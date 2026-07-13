package downshift

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func validConfigJSON() []byte {
	return []byte(`{
  "api_version":"burnban.downshift/v1",
  "revision":1,
  "mode":"warn_then_downshift",
  "warn_at_pct":70,
  "downshift_at_pct":80,
  "downshift_on_denial":true,
  "rules":[{
    "id":"coding-safe",
    "source":{"route":"openai","model":"gpt-expensive","family":"coding","dialect":"openai","context_tokens":200000},
    "target":{"route":"vllm","model":"gpt-cheap","family":"coding","dialect":"openai","context_tokens":128000},
    "scope":{"project":"oss"},
    "capabilities":{"tools":true,"structured_output":true,"modalities":["text","image"]}
  }]
}`)
}

func TestParseStrictCanonicalConfig(t *testing.T) {
	compiled, err := Parse(validConfigJSON())
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Digest == "" || len(compiled.Canonical) == 0 || compiled.Rule("openai", "gpt-expensive") == nil {
		t.Fatalf("compiled=%+v", compiled)
	}
	reparsed, err := Parse(compiled.Canonical)
	if err != nil || reparsed.Digest != compiled.Digest {
		t.Fatalf("canonical round trip digest=%q err=%v", reparsed.Digest, err)
	}
}

func TestParseRejectsAmbiguityAndUnsafeEquivalence(t *testing.T) {
	tests := []struct {
		name, replace, with string
	}{
		{"duplicate JSON field", `"revision":1,`, `"revision":1,"revision":2,`},
		{"case alias", `"revision":1,`, `"revision":1,"Revision":2,`},
		{"unknown field", `"mode":"warn_then_downshift",`, `"mode":"warn_then_downshift","url":"https://user:secret@example/",`},
		{"dialect translation", `"dialect":"openai","context_tokens":128000`, `"dialect":"anthropic","context_tokens":128000`},
		{"family mismatch", `"family":"coding","dialect":"openai","context_tokens":128000`, `"family":"vision","dialect":"openai","context_tokens":128000`},
		{"target same", `"route":"vllm","model":"gpt-cheap"`, `"route":"openai","model":"gpt-expensive"`},
		{"bidi", `"model":"gpt-cheap"`, `"model":"gpt-\u202echeap"`},
		{"bad threshold", `"downshift_at_pct":80`, `"downshift_at_pct":101`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := strings.Replace(string(validConfigJSON()), test.replace, test.with, 1)
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatalf("accepted unsafe config: %s", raw)
			}
		})
	}
}

func TestAnalyzeAndEligibilityGatesCapabilities(t *testing.T) {
	compiled, err := Parse(validConfigJSON())
	if err != nil {
		t.Fatal(err)
	}
	rule := compiled.Rule("openai", "gpt-expensive")
	body := []byte(`{"model":"gpt-expensive","max_output_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"redacted"}}]}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"}}}}`)
	features := Analyze(body, "openai")
	if ok, reason := Eligible(rule, features); !ok {
		t.Fatalf("expected eligible: %+v reason=%s", features, reason)
	}
	rule.Capabilities.Tools = false
	if ok, _ := Eligible(rule, features); ok {
		t.Fatal("tool request passed target without tool capability")
	}
	rule.Capabilities.Tools = true
	rule.Capabilities.StructuredOutput = false
	if ok, _ := Eligible(rule, features); ok {
		t.Fatal("structured request passed target without structured-output capability")
	}
	rule.Capabilities.StructuredOutput = true
	rule.Capabilities.Modalities = []string{"text"}
	if ok, _ := Eligible(rule, features); ok {
		t.Fatal("image request passed text-only target")
	}
}

func TestAnalyzeRejectsUnknownToolSchemaAndMissingOutputBound(t *testing.T) {
	for _, body := range []string{
		`{"model":"x","max_tokens":4,"tools":[{"type":"web_search"}]}`,
		`{"model":"x","max_tokens":4,"tools":[{"type":"function","function":{"name":"f","parameters":[]}}]}`,
		`{"model":"x","messages":[]}`,
		`{"model":"x","max_tokens":4,"messages":[{"content":[{"type":"future_media"}]}]}`,
	} {
		features := Analyze([]byte(body), "openai")
		if features.Compatible {
			t.Fatalf("unsafe request accepted: %s -> %+v", body, features)
		}
	}
}

func TestAnalyzeAnthropicAndGeminiSchemas(t *testing.T) {
	anthropic := Analyze([]byte(`{"model":"claude","max_tokens":32,"messages":[{"content":[{"type":"image","source":{"type":"base64"}}]}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`), "anthropic")
	if !anthropic.Compatible || !anthropic.UsesTools || strings.Join(anthropic.Modalities, ",") != "image,text" {
		t.Fatalf("anthropic=%+v", anthropic)
	}
	gemini := Analyze([]byte(`{"generationConfig":{"maxOutputTokens":32,"responseMimeType":"application/json"},"contents":[{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"redacted"}}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object"}}]}]}`), "gemini")
	if !gemini.Compatible || !gemini.UsesTools || !gemini.StructuredOutput || strings.Join(gemini.Modalities, ",") != "audio,text" {
		t.Fatalf("gemini=%+v", gemini)
	}
}

func TestAnalyzeAnthropicThinkingAndNestedToolResultsFailClosed(t *testing.T) {
	for name, body := range map[string]string{
		"thinking config": `{"model":"claude","max_tokens":32,"thinking":{"type":"enabled","budget_tokens":16}}`,
		"thinking block":  `{"model":"claude","max_tokens":32,"messages":[{"content":[{"type":"thinking","thinking":"private","signature":"signed"}]}]}`,
		"redacted block":  `{"model":"claude","max_tokens":32,"messages":[{"content":[{"type":"redacted_thinking","data":"private"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if features := Analyze([]byte(body), "anthropic"); features.Compatible {
				t.Fatalf("unsafe Anthropic request was compatible: %+v", features)
			}
		})
	}
	features := Analyze([]byte(`{"model":"claude","max_tokens":32,"messages":[{"content":[{"type":"tool_result","tool_use_id":"one","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"redacted"}}]}]}]}`), "anthropic")
	if !features.Compatible || strings.Join(features.Modalities, ",") != "image,text" {
		t.Fatalf("nested tool-result image was not classified: %+v", features)
	}
}

func TestAnalyzeAtRejectsLookalikeGeminiOperations(t *testing.T) {
	body := []byte(`{"generationConfig":{"maxOutputTokens":32},"contents":[{"parts":[{"text":"x"}]}]}`)
	for _, path := range []string{
		"/custom/models/gemini:generateContent",
		"/v1beta/models/gemini/other:generateContent",
		"/v1beta/models/gemini:generateContent:other",
	} {
		if features := AnalyzeAt(body, "gemini", path); features.Compatible {
			t.Fatalf("lookalike Gemini path %q was compatible: %+v", path, features)
		}
	}
	if features := AnalyzeAt(body, "gemini", "/v1beta/models/gemini:generateContent"); !features.Compatible {
		t.Fatalf("canonical Gemini operation rejected: %+v", features)
	}
}

func TestRewriteOnlyModelSelector(t *testing.T) {
	body := []byte(`{"model":"old","max_tokens":4,"messages":[{"content":"private"}]}`)
	path, rewritten, err := RewriteRequest("/v1/chat/completions", body, "openai", "new")
	if err != nil || path != "/v1/chat/completions" {
		t.Fatalf("rewrite path=%q err=%v", path, err)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(rewritten, &object) != nil || string(object["model"]) != `"new"` || !strings.Contains(string(object["messages"]), "private") {
		t.Fatalf("rewritten=%s", rewritten)
	}
	path, rewritten, err = RewriteRequest("/v1beta/models/gemini-old:streamGenerateContent", body, "gemini", "gemini-new")
	if err != nil || path != "/v1beta/models/gemini-new:streamGenerateContent" || string(rewritten) != string(body) {
		t.Fatalf("Gemini rewrite path=%q body=%q err=%v", path, rewritten, err)
	}
}

func TestDecisionIdentityBudgetAndObserveMode(t *testing.T) {
	compiled, err := Parse(validConfigJSON())
	if err != nil {
		t.Fatal(err)
	}
	features := Analyze([]byte(`{"model":"gpt-expensive","max_tokens":10}`), "openai")
	base := Input{Route: "openai", Model: "gpt-expensive", Dialect: "openai", TargetRouteExists: true,
		TargetDialect: "openai", BudgetPct: 85, Features: features,
		Identity: Identity{Project: "oss", Confidence: "authenticated", ProjectConfidence: "authenticated"}}
	decision := Decide(compiled, base)
	if decision.Action != ActionDownshift || decision.Trigger != TriggerThreshold {
		t.Fatalf("decision=%+v", decision)
	}
	spoofed := base
	spoofed.Identity.ProjectConfidence = "self_reported"
	if decision := Decide(compiled, spoofed); decision.Action != ActionNone || decision.CompatibilityOK {
		t.Fatalf("self-reported identity selected scoped route: %+v", decision)
	}
	spoofed.Identity.ProjectConfidence = ""
	if decision := Decide(compiled, spoofed); decision.Action != ActionNone || decision.CompatibilityOK {
		t.Fatalf("aggregate authentication promoted an unproven project scope: %+v", decision)
	}
	compiled.Config.Mode = ModeObserve
	if decision := Decide(compiled, base); decision.Action != ActionWarn {
		t.Fatalf("observe decision=%+v", decision)
	}
}

func TestConcurrentDecisionIsPureAndDeterministic(t *testing.T) {
	compiled, err := Parse(validConfigJSON())
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Route: "openai", Model: "gpt-expensive", Dialect: "openai", TargetRouteExists: true,
		TargetDialect: "openai", BudgetPct: 90, Features: Analyze([]byte(`{"model":"gpt-expensive","max_tokens":10}`), "openai"),
		Identity: Identity{Project: "oss", Confidence: "authenticated", ProjectConfidence: "authenticated"}}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if decision := Decide(compiled, input); decision.Action != ActionDownshift || decision.TargetModel != "gpt-cheap" {
				t.Errorf("decision=%+v", decision)
			}
		}()
	}
	wg.Wait()
}

func TestRuntimeFailsClosedOnMalformedOrMetadataMismatchedActiveRecord(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "runtime.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ApplyDownshiftDocument(store.DownshiftDocumentRecord{
		APIVersion: APIVersion, Revision: 1, Digest: strings.Repeat("a", 64),
		Mode: string(ModeWarnThenDownshift), DocumentJSON: `{}`,
		Forced: true, ForceReason: "intentionally malformed durable record for fail-closed runtime test",
	}); err == nil {
		t.Fatal("store accepted malformed digest-bound downshift document")
	}
	// Simulate a malformed row written by an older release or out-of-band DB
	// tooling. Runtime validation remains a second fail-closed boundary.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result, err := db.Exec(`INSERT INTO downshift_documents
		(applied_at,api_version,revision,digest,mode,document_json,forced,force_reason)
		VALUES(?,?,?,?,?,?,1,?)`, time.Now().UTC().Format(time.RFC3339Nano), APIVersion, 1,
		strings.Repeat("a", 64), string(ModeWarnThenDownshift), `{}`,
		"intentionally malformed durable record for fail-closed runtime test")
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO downshift_active(slot,document_id) VALUES(1,?)`, id); err != nil {
		t.Fatal(err)
	}
	if active, err := NewRuntime(s).Active(); err == nil || active != nil {
		t.Fatalf("malformed active record did not fail closed: active=%+v err=%v", active, err)
	}
}
