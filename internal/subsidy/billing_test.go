package subsidy

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/pricing"
)

func hasStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestDetectMeteredProviders(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAuth := func(mode, key string) {
		body := `{"auth_mode":"` + mode + `","OPENAI_API_KEY":"` + key + `"}`
		if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	writeAuth("chatgpt", "")
	if got := DetectMeteredProviders(home); len(got) != 0 {
		t.Fatalf("chatgpt + no env key: want none metered, got %v", got)
	}

	writeAuth("apikey", "")
	if got := DetectMeteredProviders(home); !hasStr(got, "codex") {
		t.Fatalf("codex apikey: want codex metered, got %v", got)
	}

	writeAuth("", "sk-openai-xxx") // older store: no auth_mode but a key present
	if got := DetectMeteredProviders(home); !hasStr(got, "codex") {
		t.Fatalf("codex bare key: want codex metered, got %v", got)
	}

	writeAuth("chatgpt", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-xxx")
	got := DetectMeteredProviders(home)
	if !hasStr(got, "claude-code") || hasStr(got, "codex") {
		t.Fatalf("env key set: want claude-code only, got %v", got)
	}
}

func TestScanHermesBillingProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, model TEXT, started_at REAL,
		input_tokens INTEGER, output_tokens INTEGER,
		cache_read_tokens INTEGER, cache_write_tokens INTEGER,
		billing_provider TEXT, estimated_cost_usd REAL
	);
	INSERT INTO sessions VALUES ('s1','openai/gpt-4o-mini',1783252800,100,20,0,0,'openrouter',0.0025);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	var events []Event
	if _, err := ScanHermes(path, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].BillingProvider != "openrouter" {
		t.Fatalf("billing provider = %q", events[0].BillingProvider)
	}
	if !events[0].CostKnown || events[0].CostUSD != 0.0025 {
		t.Fatalf("cost fallback: known=%v usd=%v", events[0].CostKnown, events[0].CostUSD)
	}
}

func TestReportSubsidyBaseUSD(t *testing.T) {
	// Unclassified report (both buckets zero) falls back to the full total.
	if got := (Report{Totals: Totals{APIUSD: 100}}).SubsidyBaseUSD(); got != 100 {
		t.Fatalf("unclassified fallback = %v, want 100", got)
	}
	// Classified report compares only subscription usage.
	r := Report{SubscriptionUSD: 30, MeteredUSD: 70, Totals: Totals{APIUSD: 100}}
	if got := r.SubsidyBaseUSD(); got != 30 {
		t.Fatalf("classified base = %v, want 30", got)
	}
}

func TestNewShareCardUsesSubscriptionOnly(t *testing.T) {
	report := Report{
		Since:           time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		Until:           time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		HasUsage:        true,
		SubscriptionUSD: 4173.49,
		MeteredUSD:      10000, // real spend must not inflate the subsidy multiple
		Totals:          Totals{APIUSD: 14173.49},
	}
	card := NewShareCard(report, "last 30 days", 0)
	if card.APIEquivalentUSD != 4173.49 || card.Multiplier != 20.9 {
		t.Fatalf("card ignored metered split: %+v", card)
	}
}

func TestBuildReportClassifiesMetered(t *testing.T) {
	dir := t.TempDir()
	hermes := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", hermes)
	if err != nil {
		t.Fatal(err)
	}
	// 1,000,000 input tokens of claude-opus-4-8 at $5/M = $5.00, billed via openrouter.
	_, err = db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, model TEXT, started_at REAL,
		input_tokens INTEGER, output_tokens INTEGER,
		cache_read_tokens INTEGER, cache_write_tokens INTEGER,
		billing_provider TEXT, estimated_cost_usd REAL
	);
	INSERT INTO sessions VALUES ('s1','claude-opus-4-8',1783252800,1000000,0,0,0,'openrouter',0);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	prices, err := pricing.Load()
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	rep, err := BuildReport(prices, ReportOptions{
		Since:       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Until:       time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		ClaudeDir:   filepath.Join(empty, "claude"),
		CodexDir:    filepath.Join(empty, "codex"),
		GeminiDir:   filepath.Join(empty, "gemini"),
		HermesDB:    hermes,
		OpenClawDir: filepath.Join(empty, "openclaw"),
		GooseDB:     filepath.Join(empty, "goose.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.MeteredUSD <= 0 {
		t.Fatalf("expected metered spend, got %v", rep.MeteredUSD)
	}
	if rep.SubscriptionUSD != 0 {
		t.Fatalf("expected no subscription usage, got %v", rep.SubscriptionUSD)
	}
	var hermesProvider *ProviderUsage
	for i := range rep.Providers {
		if rep.Providers[i].Provider == "hermes" {
			hermesProvider = &rep.Providers[i]
		}
	}
	if hermesProvider == nil || !hermesProvider.Metered || hermesProvider.BillingProvider != "openrouter" {
		t.Fatalf("hermes classification = %+v", hermesProvider)
	}
}
