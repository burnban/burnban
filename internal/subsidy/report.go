package subsidy

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/sourceadapter"
)

// Totals is the common token and API-equivalent cost shape used by the CLI
// and dashboard. Cache writes retain their two Anthropic billing tiers while
// also exposing a combined value in JSON for simple clients.
type Totals struct {
	Calls         int64            `json:"calls"`
	In            int64            `json:"in_tokens"`
	Out           int64            `json:"out_tokens"`
	CacheRead     int64            `json:"cache_read_tokens"`
	CacheWrite5m  int64            `json:"cache_write_5m_tokens"`
	CacheWrite1h  int64            `json:"cache_write_1h_tokens"`
	CacheWrite    int64            `json:"cache_write_tokens"`
	APIUSD        float64          `json:"api_usd"`
	ServerToolUSD float64          `json:"server_tool_usd"`
	ServiceTiers  map[string]int64 `json:"service_tiers,omitempty"`
	InferenceGeos map[string]int64 `json:"inference_geos,omitempty"`
	ServerToolUse ServerToolUsage  `json:"server_tool_use,omitempty"`
}

type ModelUsage struct {
	Model         string `json:"model"`
	Priced        bool   `json:"priced"`
	PricingSource string `json:"pricing_source"`
	Totals
}

type DayUsage struct {
	Day string `json:"day"`
	Totals
}

type ProviderUsage struct {
	Provider       string                `json:"provider"`
	AdapterVersion string                `json:"adapter_version"`
	Privacy        sourceadapter.Privacy `json:"privacy"`
	Dir            string                `json:"-"`
	Detected       bool                  `json:"detected"`
	// Metered is true when this source is billed per token (a BYO-key agent, or
	// a subscription CLI running on an API key), so its API-equivalent dollars
	// are a real bill rather than a subsidy comparison. BillingProvider names
	// the pay-per-token provider when the source records it (e.g. "openrouter").
	Metered         bool         `json:"metered"`
	BillingProvider string       `json:"billing_provider,omitempty"`
	Error           string       `json:"error,omitempty"`
	Detail          string       `json:"-"`
	Sessions        int          `json:"sessions"`
	Partial         bool         `json:"partial"`
	Warnings        []string     `json:"warnings,omitempty"`
	SkippedFiles    int          `json:"skipped_files,omitempty"`
	Scan            ScanStats    `json:"-"`
	Models          []ModelUsage `json:"models"`
	Days            []DayUsage   `json:"days"`
	Totals
}

type Report struct {
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	HasUsage bool      `json:"has_usage"`
	Partial  bool      `json:"partial"`
	// SubscriptionUSD is the API-equivalent value of flat-rate subscription
	// usage (the subsidy comparison). MeteredUSD is real pay-per-token spend
	// already billed. They sum to Totals.APIUSD.
	SubscriptionUSD float64         `json:"subscription_usd"`
	MeteredUSD      float64         `json:"metered_usd"`
	UnpricedCalls   int64           `json:"unpriced_calls"`
	UnpricedTokens  int64           `json:"unpriced_tokens"`
	UnpricedModels  []string        `json:"unpriced_models"`
	Providers       []ProviderUsage `json:"providers"`
	Totals
}

// SubsidyBaseUSD is the value the subsidy multiple compares against a plan: only
// flat-rate subscription usage. Reports built before billing classification
// (both buckets zero) fall back to the full total for backward compatibility.
func (r Report) SubsidyBaseUSD() float64 {
	if r.SubscriptionUSD > 0 || r.MeteredUSD > 0 {
		return r.SubscriptionUSD
	}
	return r.APIUSD
}

type ReportOptions struct {
	Since       time.Time
	Until       time.Time
	ClaudeDir   string
	CodexDir    string
	GeminiDir   string
	HermesDB    string
	OpenClawDir string
	GooseDB     string
	ScanLimits  ScanLimits
	// AdditionalAdapters are compiled-in extensions of the v1 adapter
	// contract. SourcePaths can override the path for any built-in or added
	// adapter by manifest ID.
	AdditionalAdapters []sourceadapter.Adapter
	SourcePaths        map[string]string
	// MeteredProviders forces named sources (e.g. "claude-code", "codex") into
	// the real-spend bucket, for API-key auth or Max-20x overage that the local
	// logs cannot reveal on their own.
	MeteredProviders []string
}

// BuildReport auto-detects every registered local adapter and prices its events
// in the requested window. It performs no network calls and never modifies the
// source stores.
func BuildReport(prices *pricing.Table, opts ReportOptions) (Report, error) {
	if opts.Until.IsZero() {
		opts.Until = time.Now()
	}
	if opts.Since.IsZero() {
		opts.Since = opts.Until.Add(-30 * 24 * time.Hour)
	}
	home, _ := os.UserHomeDir()
	pathOverrides := map[string]string{
		"claude-code": opts.ClaudeDir,
		"codex":       opts.CodexDir,
		"gemini-cli":  opts.GeminiDir,
		"hermes":      opts.HermesDB,
		"openclaw":    opts.OpenClawDir,
		"goose":       opts.GooseDB,
	}
	for id, path := range opts.SourcePaths {
		pathOverrides[id] = path
	}

	report := Report{
		Since: opts.Since, Until: opts.Until, Providers: []ProviderUsage{}, UnpricedModels: []string{},
	}
	unpricedModels := map[string]struct{}{}
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
		manifest sourceadapter.Manifest
		dir      string
		adapter  sourceadapter.Adapter
	}
	adapters := append(BuiltinAdapters(), opts.AdditionalAdapters...)
	sources := make([]source, 0, len(adapters))
	adapterIDs := map[string]struct{}{}
	for _, adapter := range adapters {
		if adapter == nil {
			return Report{}, fmt.Errorf("source adapter must not be nil")
		}
		manifest := adapter.Manifest()
		if err := manifest.Validate(); err != nil {
			return Report{}, err
		}
		if _, duplicate := adapterIDs[manifest.ID]; duplicate {
			return Report{}, fmt.Errorf("duplicate source adapter %q", manifest.ID)
		}
		adapterIDs[manifest.ID] = struct{}{}
		path := pathOverrides[manifest.ID]
		if path == "" {
			path = adapter.DefaultPath(home)
		}
		if path == "" {
			return Report{}, fmt.Errorf("source adapter %q returned an empty default path", manifest.ID)
		}
		sources = append(sources, source{manifest: manifest, dir: path, adapter: adapter})
	}
	for _, src := range sources {
		provider := ProviderUsage{
			Provider: src.manifest.ID, AdapterVersion: src.manifest.APIVersion,
			Privacy: src.manifest.Privacy, Dir: src.dir, Models: []ModelUsage{}, Days: []DayUsage{},
		}
		providerBilling := ""
		if _, err := os.Stat(src.dir); err == nil {
			provider.Detected = true
		}
		models := map[string]*Totals{}
		modelPriced := map[string]bool{}
		modelPricingSource := map[string]string{}
		days := map[string]*Totals{}
		add := func(t *Totals, event Event, usd, serverToolUSD float64) {
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
			t.ServerToolUSD += serverToolUSD
			if event.ServiceTier != "" {
				if t.ServiceTiers == nil {
					t.ServiceTiers = map[string]int64{}
				}
				t.ServiceTiers[event.ServiceTier] += calls
			}
			if event.InferenceGeo != "" {
				if t.InferenceGeos == nil {
					t.InferenceGeos = map[string]int64{}
				}
				t.InferenceGeos[event.InferenceGeo] += calls
			}
			t.ServerToolUse.WebSearchRequests += event.ServerToolUse.WebSearchRequests
			t.ServerToolUse.WebFetchRequests += event.ServerToolUse.WebFetchRequests
		}
		seenEvents := map[string]struct{}{}
		result, err := src.adapter.Scan(src.dir, opts.Since, opts.ScanLimits, func(event Event) {
			if event.Time.After(opts.Until) {
				return
			}
			if event.ID != "" {
				if _, duplicate := seenEvents[event.ID]; duplicate {
					return
				}
				seenEvents[event.ID] = struct{}{}
			}
			event = normalizeEvent(event)
			event.Provider = src.manifest.ID
			if event.BillingProvider != "" {
				providerBilling = event.BillingProvider
			}
			price, priced := priceFor(event.Model)
			var usd, serverToolUSD float64
			if priced {
				usd = Cost(price, event.In, event.Out, event.CacheRead, event.CacheWrite5m, event.CacheWrite1h)
				if event.InferenceGeo == "us" {
					usd *= 1.1
				}
				serverToolUSD = serverToolCost(event.ServerToolUse)
				usd += serverToolUSD
				modelPricingSource[event.Model] = "table"
			} else if event.CostKnown {
				usd = event.CostUSD
				priced = true
				modelPricingSource[event.Model] = "source"
			} else {
				calls := event.Calls
				if calls <= 0 {
					calls = 1
				}
				report.UnpricedCalls += calls
				report.UnpricedTokens += event.In + event.Out + event.CacheRead + event.CacheWrite5m + event.CacheWrite1h
				unpricedModels[event.Model] = struct{}{}
				if modelPricingSource[event.Model] == "" {
					modelPricingSource[event.Model] = "unknown"
				}
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
			add(model, event, usd, serverToolUSD)
			add(day, event, usd, serverToolUSD)
			add(&provider.Totals, event, usd, serverToolUSD)
			add(&report.Totals, event, usd, serverToolUSD)
		})
		provider.Sessions = result.Sessions
		provider.Scan = result.Stats
		provider.Partial = result.Stats.Partial
		provider.Warnings = append([]string(nil), result.Stats.Warnings...)
		provider.SkippedFiles = result.Stats.FilesSkipped
		if result.Stats.Partial {
			report.Partial = true
		}
		if err != nil {
			// One incompatible or temporarily locked agent store must not hide
			// usage from every other detected tool. Surface the source error in
			// the report and keep the successful partial data.
			provider.Error = "unable to scan " + src.manifest.ID + " usage"
			provider.Detail = err.Error()
			report.Partial = true
		}
		for model, totals := range models {
			provider.Models = append(provider.Models, ModelUsage{
				Model: model, Priced: modelPriced[model], PricingSource: modelPricingSource[model], Totals: *totals,
			})
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

		provider.Metered = providerBilling != "" || slices.Contains(opts.MeteredProviders, src.manifest.ID)
		provider.BillingProvider = providerBilling
		if provider.Metered {
			report.MeteredUSD += provider.APIUSD
		} else {
			report.SubscriptionUSD += provider.APIUSD
		}
		report.Providers = append(report.Providers, provider)
	}
	for model := range unpricedModels {
		report.UnpricedModels = append(report.UnpricedModels, model)
	}
	sort.Strings(report.UnpricedModels)
	report.HasUsage = report.Calls > 0
	return report, nil
}

func normalizeEvent(event Event) Event {
	event.In = max(event.In, 0)
	event.Out = max(event.Out, 0)
	event.CacheRead = max(event.CacheRead, 0)
	event.CacheWrite5m = max(event.CacheWrite5m, 0)
	event.CacheWrite1h = max(event.CacheWrite1h, 0)
	event.ServerToolUse.WebSearchRequests = max(event.ServerToolUse.WebSearchRequests, 0)
	event.ServerToolUse.WebFetchRequests = max(event.ServerToolUse.WebFetchRequests, 0)
	if event.Confidence == "" {
		event.Confidence = sourceadapter.ConfidenceExact
	}
	return event
}
