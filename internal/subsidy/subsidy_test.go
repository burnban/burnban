package subsidy

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/pricing"
)

var since = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

func writeLog(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func collect(t *testing.T, scan func(string, time.Time, func(Event)) (int, error), dir string) (int, []Event) {
	t.Helper()
	var got []Event
	n, err := scan(dir, since, func(e Event) { got = append(got, e) })
	if err != nil {
		t.Fatal(err)
	}
	return n, got
}

func TestScanClaude(t *testing.T) {
	root := t.TempDir()
	// One message rendered as two JSONL lines (one per content block) plus a
	// synthetic error line, a malformed line, and a pre-window message.
	writeLog(t, filepath.Join(root, "proj-a"), "s1.jsonl", `{"type":"user","message":{"role":"user","content":"hi"},"timestamp":"2026-07-05T10:00:00.000Z"}
{"type":"assistant","requestId":"req_1","timestamp":"2026-07-05T10:00:01.123Z","message":{"id":"msg_1","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":200,"cache_creation_input_tokens":30,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":30}}}}
{"type":"assistant","requestId":"req_1","timestamp":"2026-07-05T10:00:01.123Z","message":{"id":"msg_1","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":200,"cache_creation_input_tokens":30,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":30}}}}
{"type":"assistant","requestId":"req_x","timestamp":"2026-07-05T10:01:00.000Z","message":{"id":"msg_x","model":"<synthetic>","usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"assistant","malformed "usage" json
{"type":"assistant","requestId":"req_2","timestamp":"2026-07-06T09:00:00.000Z","message":{"id":"msg_2","model":"claude-haiku-4-5-20251001","usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":40,"cache_read_input_tokens":0}}}
{"type":"assistant","requestId":"req_3","timestamp":"2026-06-01T09:00:00.000Z","message":{"id":"msg_3","model":"claude-sonnet-4-6","usage":{"input_tokens":999,"output_tokens":999,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
`)
	// A resumed session in another project dir re-lists msg_1: global dedup.
	writeLog(t, filepath.Join(root, "proj-b"), "s2.jsonl", `{"type":"assistant","requestId":"req_1","timestamp":"2026-07-05T10:00:01.123Z","message":{"id":"msg_1","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":200,"cache_creation_input_tokens":30,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":30}}}}
{"type":"assistant","requestId":"req_4","timestamp":"2026-07-07T12:00:00.000Z","message":{"id":"msg_4","model":"claude-opus-4-8","usage":{"input_tokens":5,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
`)

	sessions, got := collect(t, ScanClaude, root)
	if sessions != 2 {
		t.Fatalf("sessions = %d, want 2", sessions)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3 (dedup + synthetic + malformed + pre-window filtered): %+v", len(got), got)
	}
	byModel := map[string]Event{}
	for _, e := range got {
		if e.Provider != "claude-code" {
			t.Errorf("provider = %q", e.Provider)
		}
		byModel[e.Model] = e
	}
	s := byModel["claude-sonnet-4-6"]
	if s.In != 100 || s.Out != 200 || s.CacheRead != 50 || s.CacheWrite5m != 0 || s.CacheWrite1h != 30 {
		t.Errorf("sonnet event mispriced fields: %+v", s)
	}
	h := byModel["claude-haiku-4-5-20251001"]
	if h.CacheWrite5m != 40 || h.CacheWrite1h != 0 {
		t.Errorf("haiku cache split without detail: %+v", h)
	}
}

func TestScanCodex(t *testing.T) {
	root := t.TempDir()
	writeLog(t, filepath.Join(root, "2026", "07", "05"), "rollout-1.jsonl", `{"timestamp":"2026-07-05T10:00:00.000Z","type":"session_meta","payload":{"id":"abc","model_provider":"openai"}}
{"timestamp":"2026-07-05T10:00:01.000Z","type":"turn_context","payload":{"model":"gpt-5.1","effort":"high"}}
{"timestamp":"2026-07-05T10:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"plan_type":"team"}}}
{"timestamp":"2026-07-05T10:00:10.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"reasoning_output_tokens":80,"total_tokens":1200}}}}
{"timestamp":"2026-07-05T10:01:00.000Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}
{"timestamp":"2026-07-05T10:01:10.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1500,"cached_input_tokens":900,"output_tokens":300,"reasoning_output_tokens":100,"total_tokens":1800}}}}
{"timestamp":"2026-07-05T10:02:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":0,"total_tokens":150}}}}
`)

	sessions, got := collect(t, ScanCodex, root)
	if sessions != 1 {
		t.Fatalf("sessions = %d, want 1", sessions)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(got), got)
	}
	// First delta: full totals, cached subtracted out of input.
	if e := got[0]; e.Model != "gpt-5.1" || e.In != 400 || e.CacheRead != 600 || e.Out != 200 {
		t.Errorf("event 0: %+v", e)
	}
	// Second delta attributes to the switched model.
	if e := got[1]; e.Model != "gpt-5.6-sol" || e.In != 200 || e.CacheRead != 300 || e.Out != 100 {
		t.Errorf("event 1: %+v", e)
	}
	// Counter shrank: restart reset, the new total is the delta.
	if e := got[2]; e.Model != "gpt-5.6-sol" || e.In != 100 || e.CacheRead != 0 || e.Out != 50 {
		t.Errorf("event 2: %+v", e)
	}
}

func TestScanCodexBaselineBeforeWindow(t *testing.T) {
	root := t.TempDir()
	// The session starts before the window: the pre-window total must set
	// the baseline without being emitted, so the in-window event carries
	// only its own delta.
	writeLog(t, filepath.Join(root, "2026", "06", "30"), "rollout-2.jsonl", `{"timestamp":"2026-06-30T23:00:00.000Z","type":"turn_context","payload":{"model":"gpt-5.1"}}
{"timestamp":"2026-06-30T23:30:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200,"reasoning_output_tokens":0,"total_tokens":1200}}}}
{"timestamp":"2026-07-01T00:30:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1100,"cached_input_tokens":650,"output_tokens":210,"reasoning_output_tokens":0,"total_tokens":1310}}}}
`)

	_, got := collect(t, ScanCodex, root)
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(got), got)
	}
	if e := got[0]; e.In != 50 || e.CacheRead != 50 || e.Out != 10 {
		t.Errorf("in-window delta: %+v", e)
	}
}

func TestCostHonors1hWriteTier(t *testing.T) {
	p := pricing.Price{InputPerMTok: 10, OutputPerMTok: 50, CacheReadMult: 0.1, CacheWriteMult: 1.25}
	// 1M of everything: 10 + 50 + 1 (read) + 12.50 (5m write) + 20 (1h write at 2x).
	got := Cost(p, 1_000_000, 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	if math.Abs(got-93.50) > 1e-9 {
		t.Fatalf("cost = %v, want 93.50", got)
	}
}

func TestMissingDirIsNotAnError(t *testing.T) {
	n, err := ScanClaude(filepath.Join(t.TempDir(), "nope"), since, func(Event) { t.Fatal("emitted") })
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
