package subsidy

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/syft8/burnban/internal/pricing"
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
