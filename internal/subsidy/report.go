package subsidy

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/pricing"
)

// Totals is the common token and API-equivalent cost shape used by the CLI
// and dashboard. Cache writes retain their two Anthropic billing tiers while
// also exposing a combined value in JSON for simple clients.
type Totals struct {
	Calls        int64   `json:"calls"`
	In           int64   `json:"in_tokens"`
	Out          int64   `json:"out_tokens"`
	CacheRead    int64   `json:"cache_read_tokens"`
	CacheWrite5m int64   `json:"cache_write_5m_tokens"`
	CacheWrite1h int64   `json:"cache_write_1h_tokens"`
	CacheWrite   int64   `json:"cache_write_tokens"`
	APIUSD       float64 `json:"api_usd"`
}

type ModelUsage struct {
	Model  string `json:"model"`
	Priced bool   `json:"priced"`
	Totals
}

type DayUsage struct {
	Day string `json:"day"`
	Totals
}

type ProviderUsage struct {
	Provider string       `json:"provider"`
	Dir      string       `json:"dir"`
	Detected bool         `json:"detected"`
	Error    string       `json:"error,omitempty"`
	Sessions int          `json:"sessions"`
	Models   []ModelUsage `json:"models"`
	Days     []DayUsage   `json:"days"`
	Totals
}

type Report struct {
	Since          time.Time       `json:"since"`
	Until          time.Time       `json:"until"`
	HasUsage       bool            `json:"has_usage"`
	UnpricedTokens int64           `json:"unpriced_tokens"`
	Providers      []ProviderUsage `json:"providers"`
	Totals
}

type ReportOptions struct {
	Since       time.Time
	Until       time.Time
	ClaudeDir   string
	CodexDir    string
	HermesDB    string
	OpenClawDir string
	GooseDB     string
}

// BuildReport auto-detects Claude Code, Codex, Hermes, OpenClaw, and Goose logs and
// prices every event in the requested window. It performs no network calls
// and never modifies the source logs.
func BuildReport(prices *pricing.Table, opts ReportOptions) (Report, error) {
	if opts.Until.IsZero() {
		opts.Until = time.Now()
	}
	if opts.Since.IsZero() {
		opts.Since = opts.Until.Add(-30 * 24 * time.Hour)
	}
	if opts.ClaudeDir == "" || opts.CodexDir == "" || opts.HermesDB == "" || opts.OpenClawDir == "" || opts.GooseDB == "" {
		home, _ := os.UserHomeDir()
		if opts.ClaudeDir == "" {
			opts.ClaudeDir = filepath.Join(home, ".claude", "projects")
		}
		if opts.CodexDir == "" {
			opts.CodexDir = filepath.Join(home, ".codex", "sessions")
		}
		if opts.HermesDB == "" {
			hermesHome := os.Getenv("HERMES_HOME")
			if hermesHome == "" {
				hermesHome = filepath.Join(home, ".hermes")
				if local := os.Getenv("LOCALAPPDATA"); local != "" {
					native := filepath.Join(local, "hermes")
					if _, err := os.Stat(filepath.Join(native, "state.db")); err == nil {
						hermesHome = native
					}
				}
			}
			opts.HermesDB = filepath.Join(hermesHome, "state.db")
		}
		if opts.OpenClawDir == "" {
			opts.OpenClawDir = os.Getenv("OPENCLAW_STATE_DIR")
			if opts.OpenClawDir == "" {
				opts.OpenClawDir = filepath.Join(home, ".openclaw")
			}
		}
		if opts.GooseDB == "" {
			opts.GooseDB = DefaultGooseDB(home)
		}
	}

	report := Report{Since: opts.Since, Until: opts.Until, Providers: []ProviderUsage{}}
	type priced struct {
		price pricing.Price
		ok    bool
	}
	lookup := map[string]priced{}
	priceFor := func(model string) (pricing.Price, bool) {
		if cached, ok := lookup[model]; ok {
			return cached.price, cached.ok
		}
		price, ok := prices.Lookup(model)
		if !ok {
			// Hermes/OpenClaw/Goose commonly qualify models as provider/model (and
			// OpenRouter may use provider/vendor/model). Burnban prices the
			// terminal model ID while preserving the full source label in UI.
			if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
				price, ok = prices.Lookup(model[i+1:])
			}
		}
		lookup[model] = priced{price: price, ok: ok}
		return price, ok
	}

	type source struct {
		name string
		dir  string
		scan func(string, time.Time, func(Event)) (int, error)
	}
	sources := []source{
		{name: "claude-code", dir: opts.ClaudeDir, scan: ScanClaude},
		{name: "codex", dir: opts.CodexDir, scan: ScanCodex},
		{name: "hermes", dir: opts.HermesDB, scan: ScanHermes},
		{name: "openclaw", dir: opts.OpenClawDir, scan: ScanOpenClaw},
		{name: "goose", dir: opts.GooseDB, scan: ScanGoose},
	}
	for _, src := range sources {
		provider := ProviderUsage{Provider: src.name, Dir: src.dir, Models: []ModelUsage{}, Days: []DayUsage{}}
		if _, err := os.Stat(src.dir); err == nil {
			provider.Detected = true
		}
		models := map[string]*Totals{}
		modelPriced := map[string]bool{}
		days := map[string]*Totals{}
		add := func(t *Totals, event Event, usd float64) {
			calls := event.Calls
			if calls <= 0 {
				calls = 1
			}
			t.Calls += calls
			t.In += event.In
			t.Out += event.Out
			t.CacheRead += event.CacheRead
			t.CacheWrite5m += event.CacheWrite5m
			t.CacheWrite1h += event.CacheWrite1h
			t.CacheWrite += event.CacheWrite5m + event.CacheWrite1h
			t.APIUSD += usd
		}
		var err error
		provider.Sessions, err = src.scan(src.dir, opts.Since, func(event Event) {
			if event.Time.After(opts.Until) {
				return
			}
			price, priced := priceFor(event.Model)
			var usd float64
			if priced {
				usd = Cost(price, event.In, event.Out, event.CacheRead, event.CacheWrite5m, event.CacheWrite1h)
			} else if event.CostKnown {
				usd = event.CostUSD
				priced = true
			} else {
				report.UnpricedTokens += event.In + event.Out + event.CacheRead + event.CacheWrite5m + event.CacheWrite1h
			}
			if priced {
				modelPriced[event.Model] = true
			}
			model := models[event.Model]
			if model == nil {
				model = &Totals{}
				models[event.Model] = model
			}
			dayKey := event.Time.Local().Format("2006-01-02")
			day := days[dayKey]
			if day == nil {
				day = &Totals{}
				days[dayKey] = day
			}
			add(model, event, usd)
			add(day, event, usd)
			add(&provider.Totals, event, usd)
			add(&report.Totals, event, usd)
		})
		if err != nil {
			// One incompatible or temporarily locked agent store must not hide
			// usage from every other detected tool. Surface the source error in
			// the report and keep the successful partial data.
			provider.Error = err.Error()
		}
		for model, totals := range models {
			provider.Models = append(provider.Models, ModelUsage{Model: model, Priced: modelPriced[model], Totals: *totals})
		}
		sort.Slice(provider.Models, func(i, j int) bool {
			if provider.Models[i].APIUSD != provider.Models[j].APIUSD {
				return provider.Models[i].APIUSD > provider.Models[j].APIUSD
			}
			return provider.Models[i].Model < provider.Models[j].Model
		})
		for day, totals := range days {
			provider.Days = append(provider.Days, DayUsage{Day: day, Totals: *totals})
		}
		sort.Slice(provider.Days, func(i, j int) bool { return provider.Days[i].Day < provider.Days[j].Day })
		report.Providers = append(report.Providers, provider)
	}
	report.HasUsage = report.Calls > 0
	return report, nil
}

// DefaultGooseDB returns Goose's current per-OS data path, honoring its
// documented GOOSE_PATH_ROOT override and legacy locations.
func DefaultGooseDB(home string) string {
	if root := os.Getenv("GOOSE_PATH_ROOT"); root != "" {
		return filepath.Join(root, "data", "sessions", "sessions.db")
	}
	var candidates []string
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		candidates = append(candidates,
			filepath.Join(data, "goose", "sessions", "sessions.db"),
			filepath.Join(data, "Block", "goose", "sessions", "sessions.db"))
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		candidates = append(candidates, filepath.Join(appData, "Block", "goose", "sessions", "sessions.db"))
	}
	candidates = append(candidates,
		filepath.Join(home, ".local", "share", "goose", "sessions", "sessions.db"),
		filepath.Join(home, ".local", "share", "Block", "goose", "sessions", "sessions.db"),
		filepath.Join(home, "Library", "Application Support", "Block", "goose", "sessions", "sessions.db"),
		filepath.Join(home, "Library", "Application Support", "goose", "sessions", "sessions.db"),
		filepath.Join(home, ".config", "goose", "sessions.db"),
	)
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Block", "goose", "sessions", "sessions.db")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Block", "goose", "sessions", "sessions.db")
		}
	}
	return filepath.Join(home, ".local", "share", "goose", "sessions", "sessions.db")
}
