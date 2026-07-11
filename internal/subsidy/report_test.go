package subsidy

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/pricing"
)

func TestBuildReportAutoDetectsAndAggregates(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, ".claude", "projects")
	codexDir := filepath.Join(root, ".codex", "sessions")
	writeLog(t, filepath.Join(claudeDir, "p"), "s.jsonl", `{"type":"assistant","requestId":"r","timestamp":"2026-07-05T10:00:01Z","message":{"id":"m","model":"claude-test","usage":{"input_tokens":100,"output_tokens":200,"cache_creation_input_tokens":30,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_1h_input_tokens":10}}}}
`)
	writeLog(t, filepath.Join(codexDir, "2026", "07", "05"), "rollout.jsonl", `{"timestamp":"2026-07-05T10:00:00Z","type":"turn_context","payload":{"model":"gpt-test"}}
{"timestamp":"2026-07-05T10:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200}}}}
`)
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"claude-test": {InputPerMTok: 10, OutputPerMTok: 50, CacheReadMult: .1, CacheWriteMult: 1.25},
		"gpt-test":    {InputPerMTok: 2, OutputPerMTok: 8, CacheReadMult: .5},
	}}
	report, err := BuildReport(prices, ReportOptions{
		Since:     time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Until:     time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC),
		ClaudeDir: claudeDir, CodexDir: codexDir,
		HermesDB:    filepath.Join(root, "missing-hermes.db"),
		OpenClawDir: filepath.Join(root, "missing-openclaw"),
		GooseDB:     filepath.Join(root, "missing-goose.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasUsage || report.Calls != 2 || report.In != 500 || report.Out != 400 || report.CacheRead != 650 || report.CacheWrite != 30 {
		t.Fatalf("totals = %+v", report.Totals)
	}
	if len(report.Providers) != 5 || !report.Providers[0].Detected || !report.Providers[1].Detected {
		t.Fatalf("providers = %+v", report.Providers)
	}
	// Claude: .001 + .010 + .00005 + .00025 + .0002 = .0115.
	// Codex: .0008 + .0016 + .0006 = .003.
	if math.Abs(report.APIUSD-.0145) > 1e-9 {
		t.Fatalf("APIUSD = %.9f, want .0145", report.APIUSD)
	}
}

func TestReportTracksAnthropicDimensionsAndUnknownPricing(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, "private-home", ".claude", "projects")
	writeLog(t, claudeDir, "known.jsonl", `{"type":"assistant","requestId":"r1","timestamp":"2026-07-05T10:00:01Z","message":{"id":"m1","model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":100,"cache_creation_input_tokens":30,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20},"service_tier":"standard","inference_geo":"us","server_tool_use":{"web_search_requests":2,"web_fetch_requests":1}}}}
`)
	writeLog(t, claudeDir, "unknown.jsonl", `{"type":"assistant","requestId":"r2","timestamp":"2026-07-05T10:00:02Z","message":{"id":"m2","model":"claude-unknown","usage":{"input_tokens":7,"output_tokens":3}}}
`)
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"claude-sonnet-5": {InputPerMTok: 2, OutputPerMTok: 10, CacheReadMult: .1, CacheWriteMult: 1.25},
	}}
	report, err := BuildReport(prices, ReportOptions{
		Since: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC),
		ClaudeDir: claudeDir, CodexDir: filepath.Join(root, "missing-codex"),
		HermesDB: filepath.Join(root, "missing-hermes"), OpenClawDir: filepath.Join(root, "missing-openclaw"),
		GooseDB: filepath.Join(root, "missing-goose"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.UnpricedCalls != 1 || report.UnpricedTokens != 10 || len(report.UnpricedModels) != 1 || report.UnpricedModels[0] != "claude-unknown" {
		t.Fatalf("unknown pricing diagnostics = %+v", report)
	}
	if report.ServiceTiers["standard"] != 1 || report.InferenceGeos["us"] != 1 ||
		report.ServerToolUse.WebSearchRequests != 2 || report.ServerToolUse.WebFetchRequests != 1 || report.ServerToolUSD != .02 {
		t.Fatalf("nested Anthropic totals = %+v", report.Totals)
	}
	if want := .0214465; math.Abs(report.APIUSD-want) > 1e-12 {
		t.Fatalf("APIUSD = %.9f, want %.9f with US geo and server-tool pricing", report.APIUSD, want)
	}
	var unknown ModelUsage
	for _, model := range report.Providers[0].Models {
		if model.Model == "claude-unknown" {
			unknown = model
		}
	}
	if unknown.PricingSource != "unknown" || unknown.Priced {
		t.Fatalf("unknown model was not explicit: %+v", unknown)
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), root) || strings.Contains(string(b), "private-home") {
		t.Fatalf("web-facing report leaked an absolute source path: %s", b)
	}
}

func TestReportSurfacesScanLimitWithoutPaths(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, "logs")
	for i := 0; i < 2; i++ {
		writeLog(t, claudeDir, string(rune('a'+i))+".jsonl", `{"type":"assistant","requestId":"r","timestamp":"2026-07-05T10:00:01Z","message":{"id":"m","model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}
`)
	}
	report, err := BuildReport(&pricing.Table{Models: map[string]pricing.Price{
		"claude-sonnet-5": {InputPerMTok: 2, OutputPerMTok: 10},
	}}, ReportOptions{
		Since: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC),
		ClaudeDir: claudeDir, CodexDir: filepath.Join(root, "missing-codex"), HermesDB: filepath.Join(root, "missing-hermes"),
		OpenClawDir: filepath.Join(root, "missing-openclaw"), GooseDB: filepath.Join(root, "missing-goose"),
		ScanLimits: ScanLimits{MaxFiles: 1, MaxBytes: 1 << 20, MaxLineBytes: 1 << 20, MaxRecords: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Partial || !report.Providers[0].Scan.Partial || len(report.Providers[0].Scan.Warnings) == 0 {
		t.Fatalf("scan limit was not surfaced: %+v", report.Providers[0])
	}
	b, _ := json.Marshal(report)
	if strings.Contains(string(b), root) {
		t.Fatalf("partial diagnostics leaked path: %s", b)
	}
}
