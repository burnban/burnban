package subsidy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/sourceadapter"
)

func TestScanGeminiFixture(t *testing.T) {
	root := t.TempDir()
	chatDir := filepath.Join(root, "project-hash", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join("testdata", "gemini", "session-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chatDir, "session-fixture.jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	// A JSONL file outside a chats component must not be treated as a session.
	if err := os.WriteFile(filepath.Join(root, "telemetry.jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	var events []Event
	result, err := scanGemini(root, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 1 || result.Stats.FilesScanned != 1 || len(events) != 1 {
		t.Fatalf("result=%+v events=%+v", result, events)
	}
	event := events[0]
	if event.ID != "project-fixture/session-fixture/model-1" || event.Model != "gemini-2.5-pro" {
		t.Fatalf("identity = %+v", event)
	}
	// promptTokenCount includes cached content; tool results are additional
	// input, while thinking tokens are billed as output.
	if event.In != 725 || event.CacheRead != 300 || event.Out != 250 || event.Calls != 1 {
		t.Fatalf("normalized tokens = %+v", event)
	}
	if event.Confidence != sourceadapter.ConfidenceExact {
		t.Fatalf("confidence = %q", event.Confidence)
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "fixture prompt") || strings.Contains(string(encoded), "fixture response") {
		t.Fatalf("adapter event leaked conversation content: %s", encoded)
	}
}

func TestScanGeminiSurfacesRecordLimit(t *testing.T) {
	root := t.TempDir()
	chatDir := filepath.Join(root, "project-hash", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join("testdata", "gemini", "session-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chatDir, "session.jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	limits := DefaultScanLimits()
	limits.MaxRecords = 1
	result, err := scanGemini(root, time.Time{}, limits, func(Event) { t.Fatal("event emitted past record limit") })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.Partial || len(result.Stats.Warnings) == 0 || result.Stats.Warnings[0] != "record scan limit reached" {
		t.Fatalf("scan limit result = %+v", result)
	}
}

func TestBuildReportIncludesGeminiAdapter(t *testing.T) {
	root := t.TempDir()
	chatDir := filepath.Join(root, "gemini", "project-hash", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join("testdata", "gemini", "session-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chatDir, "session.jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing")
	report, err := BuildReport(&pricing.Table{Models: map[string]pricing.Price{
		"gemini-2.5-pro": {InputPerMTok: 1, OutputPerMTok: 2, CacheReadMult: .5},
	}}, ReportOptions{
		Since:     time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Until:     time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
		ClaudeDir: missing, CodexDir: missing, GeminiDir: filepath.Join(root, "gemini"),
		HermesDB: missing, OpenClawDir: missing, GooseDB: missing,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range report.Providers {
		if provider.Provider != "gemini-cli" {
			continue
		}
		if !provider.Detected || provider.Sessions != 1 || provider.Calls != 1 || provider.In != 725 || provider.Out != 250 || provider.CacheRead != 300 {
			t.Fatalf("Gemini provider = %+v", provider)
		}
		if provider.Metered || provider.AdapterVersion != sourceadapter.APIVersion || !provider.Privacy.ReadOnly {
			t.Fatalf("Gemini contract/classification = %+v", provider)
		}
		return
	}
	t.Fatal("Gemini provider missing")
}

func TestBuiltinAdapterManifests(t *testing.T) {
	adapters := BuiltinAdapters()
	if len(adapters) != 6 {
		t.Fatalf("built-in adapters = %d, want 6", len(adapters))
	}
	seen := map[string]bool{}
	for _, adapter := range adapters {
		manifest := adapter.Manifest()
		if err := manifest.Validate(); err != nil {
			t.Fatalf("%s: %v", manifest.ID, err)
		}
		if seen[manifest.ID] {
			t.Fatalf("duplicate adapter %q", manifest.ID)
		}
		seen[manifest.ID] = true
	}
	if !seen["gemini-cli"] {
		t.Fatal("Gemini CLI adapter not registered")
	}
}

func TestDefaultGeminiDirHonorsCLIHome(t *testing.T) {
	t.Setenv("GEMINI_CLI_HOME", filepath.Join("custom", "home"))
	if got, want := DefaultGeminiDir("ignored"), filepath.Join("custom", "home", ".gemini", "tmp"); got != want {
		t.Fatalf("DefaultGeminiDir() = %q, want %q", got, want)
	}
}
