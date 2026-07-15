package localusage

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/sourceadapter"
)

func openCodeFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	writeOpenCodeFixture(t, path)
	return path
}

func writeOpenCodeFixture(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "opencode", "session-v1.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestScanOpenCodeCompatibilityFixture(t *testing.T) {
	path := openCodeFixtureDB(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	result, err := scanOpenCode(
		path,
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		DefaultScanLimits(),
		func(event Event) { events = append(events, event) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 3 || len(events) != 4 {
		t.Fatalf("result=%+v events=%+v", result, events)
	}
	if !result.Stats.Partial || !hasStr(result.Stats.Warnings, openCodeMalformedWarning) {
		t.Fatalf("malformed compatibility record was not surfaced: %+v", result.Stats)
	}

	exact := events[0]
	if exact.ID != "ses_resumed/msg_exact" || exact.Model != "anthropic/claude-sonnet-5" ||
		!exact.Time.Equal(time.UnixMilli(1783252800000)) {
		t.Fatalf("exact identity = %+v", exact)
	}
	if exact.In != 725 || exact.Out != 250 || exact.CacheRead != 300 || exact.CacheWrite5m != 25 || exact.Calls != 1 {
		t.Fatalf("exact normalization = %+v", exact)
	}
	if exact.Confidence != sourceadapter.ConfidenceExact || !exact.CostKnown || exact.CostUSD != 0.0042 {
		t.Fatalf("exact confidence/cost = %+v", exact)
	}
	if exact.BillingProvider != "" {
		t.Fatalf("OpenCode history guessed billing provider %q", exact.BillingProvider)
	}

	ids := map[string]int{}
	for _, event := range events {
		ids[event.ID]++
	}
	if ids["ses_bridge/msg_bridge"] != 1 || ids["ses_legacy/msg_legacy"] != 1 {
		t.Fatalf("stable event IDs/deduplication = %+v", ids)
	}
	if ids["ses_old/msg_outside"] != 0 || ids["ses_bad/msg_missing_tokens"] != 0 {
		t.Fatalf("outside/malformed records emitted: %+v", ids)
	}

	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture prompt", "fixture response"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("adapter leaked conversation content %q: %s", secret, encoded)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("OpenCode source database changed during read-only scan")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("read-only scan created %s sidecar: %v", suffix, err)
		}
	}
}

func TestScanOpenCodeSurfacesLimits(t *testing.T) {
	path := openCodeFixtureDB(t)
	limits := DefaultScanLimits()
	limits.MaxRecords = 1
	var events []Event
	result, err := scanOpenCode(path, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), limits, func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !result.Stats.Partial || !hasStr(result.Stats.Warnings, "record scan limit reached") {
		t.Fatalf("record limit result=%+v events=%+v", result, events)
	}

	limits = DefaultScanLimits()
	limits.MaxBytes = 1
	result, err = scanOpenCode(path, time.Time{}, limits, func(Event) { t.Fatal("event emitted past byte limit") })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.Partial || !hasStr(result.Stats.Warnings, "byte scan limit reached") {
		t.Fatalf("byte limit result=%+v", result)
	}

	limits = DefaultScanLimits()
	limits.MaxLineBytes = 1
	result, err = scanOpenCode(path, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), limits, func(Event) {
		t.Fatal("event emitted past record size limit")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.Partial || !hasStr(result.Stats.Warnings, openCodeRecordSizeWarning) {
		t.Fatalf("record size limit result=%+v", result)
	}
}

func TestDefaultOpenCodeDB(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join("custom", "data"))
	t.Setenv("OPENCODE_DB", "")
	want := filepath.Join("custom", "data", "opencode", "opencode.db")
	if got := DefaultOpenCodeDB("ignored"); got != want {
		t.Fatalf("DefaultOpenCodeDB() = %q, want %q", got, want)
	}

	t.Setenv("OPENCODE_DB", "channel.db")
	want = filepath.Join("custom", "data", "opencode", "channel.db")
	if got := DefaultOpenCodeDB("ignored"); got != want {
		t.Fatalf("relative OPENCODE_DB = %q, want %q", got, want)
	}

	absolute := filepath.Join(t.TempDir(), "explicit.db")
	t.Setenv("OPENCODE_DB", absolute)
	if got := DefaultOpenCodeDB("ignored"); got != absolute {
		t.Fatalf("absolute OPENCODE_DB = %q, want %q", got, absolute)
	}
}

func TestBuildReportIncludesOpenCodeAndRequiresManualBilling(t *testing.T) {
	dataRoot := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataRoot)
	t.Setenv("OPENCODE_DB", "")
	path := DefaultOpenCodeDB("ignored")
	writeOpenCodeFixture(t, path)
	missing := filepath.Join(t.TempDir(), "missing")
	options := ReportOptions{
		Since:       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Until:       time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
		ClaudeDir:   missing,
		CodexDir:    missing,
		GeminiDir:   missing,
		CopilotDir:  missing,
		CursorDB:    missing,
		HermesDB:    missing,
		OpenClawDir: missing,
		GooseDB:     missing,
	}
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"claude-sonnet-5": {InputPerMTok: 1, OutputPerMTok: 2, CacheReadMult: .5, CacheWriteMult: 1.25},
		"gpt-fixture":     {InputPerMTok: 1, OutputPerMTok: 2, CacheReadMult: .5},
		"gemini-fixture":  {InputPerMTok: 1, OutputPerMTok: 2, CacheReadMult: .5, CacheWriteMult: 1},
	}}
	report, err := BuildReport(prices, options)
	if err != nil {
		t.Fatal(err)
	}
	provider := providerByName(t, report, "opencode")
	if !provider.Detected || provider.Sessions != 3 || provider.Calls != 4 || provider.Metered {
		t.Fatalf("OpenCode default classification = %+v", provider)
	}
	if provider.AdapterVersion != sourceadapter.APIVersion || !provider.Privacy.ReadOnly || provider.Privacy.NetworkAccess {
		t.Fatalf("OpenCode contract = %+v", provider)
	}

	options.MeteredProviders = []string{"opencode"}
	report, err = BuildReport(prices, options)
	if err != nil {
		t.Fatal(err)
	}
	provider = providerByName(t, report, "opencode")
	if !provider.Metered || report.MeteredUSD <= 0 {
		t.Fatalf("manual OpenCode billing classification = provider=%+v report=%+v", provider, report)
	}
}

func providerByName(t *testing.T, report Report, name string) ProviderUsage {
	t.Helper()
	for _, provider := range report.Providers {
		if provider.Provider == name {
			return provider
		}
	}
	t.Fatalf("provider %q missing", name)
	return ProviderUsage{}
}
