package subsidy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/sourceadapter"
)

func writeCopilotFixture(t *testing.T, root, sessionID string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "copilot", "session-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "session-state", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanGitHubCopilotCLIFixture(t *testing.T) {
	root := t.TempDir()
	writeCopilotFixture(t, root, "session-fixture")
	// Other JSONL files in session-state are not Copilot session event logs.
	if err := os.WriteFile(filepath.Join(root, "session-state", "telemetry.jsonl"), []byte(`{"type":"session.shutdown"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var events []Event
	result, err := scanGitHubCopilotCLI(filepath.Join(root, "session-state"), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 1 || result.Stats.FilesScanned != 1 || !result.Stats.Partial || len(events) != 2 ||
		!slices.Contains(result.Stats.Warnings, copilotCacheTierWarning) {
		t.Fatalf("result=%+v events=%+v", result, events)
	}
	claude, gpt := events[0], events[1]
	if claude.Model != "claude-sonnet-4.5" || claude.Calls != 3 || claude.In != 600 || claude.Out != 200 || claude.CacheRead != 300 || claude.CacheWrite5m != 100 {
		t.Fatalf("Claude normalization = %+v", claude)
	}
	if gpt.Model != "gpt-5.4" || gpt.Calls != 2 || gpt.In != 450 || gpt.Out != 100 || gpt.CacheRead != 50 || gpt.CacheWrite5m != 0 {
		t.Fatalf("GPT normalization = %+v", gpt)
	}
	for _, event := range events {
		wantConfidence := sourceadapter.ConfidenceExact
		if event.Model == "claude-sonnet-4.5" {
			wantConfidence = sourceadapter.ConfidencePartial
		}
		if event.Confidence != wantConfidence || !strings.Contains(event.ID, "00000000-0000-4000-8000-000000000005") {
			t.Fatalf("identity/confidence = %+v", event)
		}
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "fixture prompt") || strings.Contains(string(encoded), "fixture response") || strings.Contains(string(encoded), "synthetic.go") {
		t.Fatalf("adapter event leaked session content: %s", encoded)
	}
}

func TestScanGitHubCopilotCLIRejectsMalformedUsage(t *testing.T) {
	tests := []struct {
		name  string
		usage string
	}{
		{"cache exceeds input", `"inputTokens":10,"outputTokens":1,"cacheReadTokens":8,"cacheWriteTokens":3,"reasoningTokens":0`},
		{"negative", `"inputTokens":10,"outputTokens":-1,"cacheReadTokens":0,"cacheWriteTokens":0,"reasoningTokens":0`},
		{"reasoning exceeds output", `"inputTokens":10,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0,"reasoningTokens":2`},
		{"composite bound", fmt.Sprintf(`"inputTokens":%d,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0,"reasoningTokens":0`, sourceadapter.MaxEventTokens)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "session")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			line := fmt.Sprintf(`{"id":"shutdown-private","timestamp":"2026-07-10T12:00:00Z","type":"session.shutdown","data":{"sessionStartTime":1783681200000,"modelMetrics":{"private-model":{"requests":{"count":1},"usage":{%s}}}}}`, tt.usage)
			if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var events []Event
			result, err := scanGitHubCopilotCLI(root, time.Time{}, DefaultScanLimits(), func(event Event) { events = append(events, event) })
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 || !result.Stats.Partial || !slices.Contains(result.Stats.Warnings, copilotMalformedWarning) {
				t.Fatalf("malformed result=%+v events=%+v", result, events)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "private-model") || strings.Contains(string(encoded), "shutdown-private") {
				t.Fatalf("diagnostic leaked source metadata: %s", encoded)
			}
		})
	}
}

func TestScanGitHubCopilotCLIRequiresSchemaUsageFields(t *testing.T) {
	validUsage := `{"inputTokens":10,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0}`
	tests := []struct {
		name string
		data string
	}{
		{"missing data", ``},
		{"missing session start", `,"data":{"modelMetrics":{}}`},
		{"missing model metrics", `,"data":{"sessionStartTime":1783681200000}`},
		{"missing requests", `,"data":{"sessionStartTime":1783681200000,"modelMetrics":{"model":{"usage":` + validUsage + `}}}`},
		{"missing usage", `,"data":{"sessionStartTime":1783681200000,"modelMetrics":{"model":{"requests":{"count":1}}}}`},
		{"missing input tokens", `,"data":{"sessionStartTime":1783681200000,"modelMetrics":{"model":{"requests":{"count":1},"usage":{"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0}}}}`},
		{"missing output tokens", `,"data":{"sessionStartTime":1783681200000,"modelMetrics":{"model":{"requests":{"count":1},"usage":{"inputTokens":10,"cacheReadTokens":0,"cacheWriteTokens":0}}}}`},
		{"missing cache read tokens", `,"data":{"sessionStartTime":1783681200000,"modelMetrics":{"model":{"requests":{"count":1},"usage":{"inputTokens":10,"outputTokens":1,"cacheWriteTokens":0}}}}`},
		{"missing cache write tokens", `,"data":{"sessionStartTime":1783681200000,"modelMetrics":{"model":{"requests":{"count":1},"usage":{"inputTokens":10,"outputTokens":1,"cacheReadTokens":0}}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "session")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			line := `{"id":"shutdown","timestamp":"2026-07-10T12:00:00Z","type":"session.shutdown"` + tt.data + `}`
			if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var events []Event
			result, err := scanGitHubCopilotCLI(root, time.Time{}, DefaultScanLimits(), func(event Event) {
				events = append(events, event)
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 || !result.Stats.Partial || !slices.Contains(result.Stats.Warnings, copilotMalformedWarning) {
				t.Fatalf("required-field result=%+v events=%+v", result, events)
			}
		})
	}
}

func TestScanGitHubCopilotCLIInvalidTrailingShutdownPreservesValidUsage(t *testing.T) {
	root := t.TempDir()
	path := writeCopilotFixture(t, root, "session-fixture")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(`{"id":"trailing","timestamp":"not-a-timestamp","type":"session.shutdown","data":{"sessionStartTime":1783681200000,"modelMetrics":{}}}` + "\n")
	closeErr := f.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	var events []Event
	result, err := scanGitHubCopilotCLI(root, time.Time{}, DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || result.Sessions != 1 || !result.Stats.Partial ||
		!slices.Contains(result.Stats.Warnings, copilotMalformedWarning) {
		t.Fatalf("trailing shutdown result=%+v events=%+v", result, events)
	}
}

func TestScanGitHubCopilotCLISurfacesBoundaryAndMissingCalls(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "session")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"id":"00000000-0000-4000-8000-000000000010","timestamp":"2026-07-10T12:00:00Z","type":"session.shutdown","data":{"sessionStartTime":1782777600000,"modelMetrics":{"gpt-5.4":{"requests":{},"usage":{"inputTokens":100,"outputTokens":20,"cacheReadTokens":10,"cacheWriteTokens":0,"reasoningTokens":5}}}}}`
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var events []Event
	result, err := scanGitHubCopilotCLI(root, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), DefaultScanLimits(), func(event Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Calls != 1 || events[0].Confidence != sourceadapter.ConfidencePartial || !result.Stats.Partial {
		t.Fatalf("partial aggregate result=%+v events=%+v", result, events)
	}
	if !slices.Contains(result.Stats.Warnings, copilotBoundaryWarning) || !slices.Contains(result.Stats.Warnings, copilotRequestCountWarning) {
		t.Fatalf("warnings = %v", result.Stats.Warnings)
	}
}

func TestScanGitHubCopilotCLISurfacesRecordLimit(t *testing.T) {
	root := t.TempDir()
	writeCopilotFixture(t, root, "session-fixture")
	limits := DefaultScanLimits()
	limits.MaxRecords = 1
	result, err := scanGitHubCopilotCLI(filepath.Join(root, "session-state"), time.Time{}, limits, func(Event) { t.Fatal("event emitted past record limit") })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.Partial || !slices.Contains(result.Stats.Warnings, "record scan limit reached") {
		t.Fatalf("record limit result = %+v", result)
	}
}

func TestBuildReportIncludesGitHubCopilotCLIAdapter(t *testing.T) {
	root := t.TempDir()
	writeCopilotFixture(t, root, "session-fixture")
	missing := filepath.Join(root, "missing")
	report, err := BuildReport(&pricing.Table{Models: map[string]pricing.Price{
		"claude-sonnet-4.5": {InputPerMTok: 1, OutputPerMTok: 2, CacheReadMult: .5, CacheWriteMult: 1.25},
		"gpt-5.4":           {InputPerMTok: 2, OutputPerMTok: 4, CacheReadMult: .5},
	}}, ReportOptions{
		Since: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
		ClaudeDir: missing, CodexDir: missing, GeminiDir: missing, CopilotDir: filepath.Join(root, "session-state"),
		CursorDB:   missing,
		OpenCodeDB: missing, HermesDB: missing, OpenClawDir: missing, GooseDB: missing,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range report.Providers {
		if provider.Provider != "github-copilot-cli" {
			continue
		}
		if !provider.Detected || provider.Sessions != 1 || provider.Calls != 5 || provider.In != 1050 || provider.Out != 300 || provider.CacheRead != 350 || provider.CacheWrite5m != 100 {
			t.Fatalf("Copilot provider = %+v", provider)
		}
		if provider.Metered || provider.AdapterVersion != sourceadapter.APIVersion || !provider.Privacy.ReadOnly {
			t.Fatalf("Copilot contract/classification = %+v", provider)
		}
		if !provider.Partial || !slices.Contains(provider.Warnings, copilotCacheTierWarning) ||
			!slices.Contains(provider.Warnings, "one or more adapter events contain partial usage") {
			t.Fatalf("Copilot confidence diagnostics = %+v", provider)
		}
		return
	}
	t.Fatal("GitHub Copilot CLI provider missing")
}

func TestDefaultCopilotDirHonorsCopilotHome(t *testing.T) {
	t.Setenv("COPILOT_HOME", filepath.Join("custom", "copilot"))
	if got, want := DefaultCopilotDir("ignored"), filepath.Join("custom", "copilot", "session-state"); got != want {
		t.Fatalf("DefaultCopilotDir() = %q, want %q", got, want)
	}
}
