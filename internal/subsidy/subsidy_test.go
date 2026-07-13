package subsidy

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
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
{"type":"assistant","requestId":"req_1","timestamp":"2026-07-05T10:00:01.123Z","message":{"id":"msg_1","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":200,"cache_creation_input_tokens":30,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20},"service_tier":"standard","inference_geo":"us","server_tool_use":{"web_search_requests":2,"web_fetch_requests":1}}}}
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
	if s.In != 100 || s.Out != 200 || s.CacheRead != 50 || s.CacheWrite5m != 10 || s.CacheWrite1h != 20 {
		t.Errorf("sonnet event mispriced fields: %+v", s)
	}
	if s.ServiceTier != "standard" || s.InferenceGeo != "us" || s.ServerToolUse.WebSearchRequests != 2 || s.ServerToolUse.WebFetchRequests != 1 {
		t.Errorf("sonnet nested usage metadata missing: %+v", s)
	}
	h := byModel["claude-haiku-4-5-20251001"]
	if h.CacheWrite5m != 40 || h.CacheWrite1h != 0 {
		t.Errorf("haiku cache split without detail: %+v", h)
	}
}

func TestBoundedScanReportsPartial(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 2; i++ {
		writeLog(t, root, fmt.Sprintf("s%d.jsonl", i), fmt.Sprintf(
			`{"type":"assistant","requestId":"r%d","timestamp":"2026-07-05T10:00:01Z","message":{"id":"m%d","model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n", i, i))
	}
	var events []Event
	result, err := scanClaude(root, since, ScanLimits{MaxFiles: 1, MaxBytes: 1 << 20, MaxLineBytes: 1 << 20, MaxRecords: 100}, func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.Partial || result.Stats.FilesScanned != 1 || len(events) != 1 {
		t.Fatalf("bounded result = %+v events=%d", result, len(events))
	}
	if len(result.Stats.Warnings) == 0 || strings.Contains(strings.Join(result.Stats.Warnings, " "), root) {
		t.Fatalf("unsafe or missing scan warning: %+v", result.Stats)
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

func TestScanCodexSubagentReplayAdvancesBaselineWithoutDuplicatingParent(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "07", "05")
	writeLog(t, day, "rollout-parent.jsonl", `{"timestamp":"2026-07-05T10:00:00.000Z","type":"session_meta","payload":{"id":"parent","source":"cli"}}
{"timestamp":"2026-07-05T10:00:01.000Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}
{"timestamp":"2026-07-05T10:00:10.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200}}}}
{"timestamp":"2026-07-05T10:01:10.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1500,"cached_input_tokens":900,"output_tokens":300},"last_token_usage":{"input_tokens":500,"cached_input_tokens":300,"output_tokens":100}}}}
`)
	// Current Codex subagent rollouts write the child metadata first, replay the
	// parent's history with fresh serialization timestamps, then write a
	// trigger-turn boundary before the child's first model call. The replay must
	// establish the 1500/900/300 baseline without emitting either parent call.
	writeLog(t, day, "rollout-subagent.jsonl", `{"timestamp":"2026-07-05T11:00:00.000Z","type":"session_meta","payload":{"id":"child","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","depth":1,"agent_path":"/root/fixture"}}},"parent_thread_id":"parent","forked_from_id":"parent"}}
{"timestamp":"2026-07-05T11:00:00.001Z","type":"session_meta","payload":{"id":"parent","source":"cli"}}
{"timestamp":"2026-07-05T11:00:00.002Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}
{"timestamp":"2026-07-05T11:00:00.003Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200}}}}
{"timestamp":"2026-07-05T11:00:00.004Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1500,"cached_input_tokens":900,"output_tokens":300},"last_token_usage":{"input_tokens":500,"cached_input_tokens":300,"output_tokens":100}}}}
{"timestamp":"2026-07-05T11:00:01.000Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}
{"timestamp":"2026-07-05T11:00:01.001Z","type":"inter_agent_communication_metadata","payload":{"trigger_turn":true}}
{"timestamp":"2026-07-05T11:00:06.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1700,"cached_input_tokens":1000,"output_tokens":340},"last_token_usage":{"input_tokens":200,"cached_input_tokens":100,"output_tokens":40}}}}
`)

	sessions, got := collect(t, ScanCodex, root)
	if sessions != 2 {
		t.Fatalf("sessions = %d, want parent and live child", sessions)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want two parent calls plus one live child call: %+v", len(got), got)
	}
	var totals Event
	for _, event := range got {
		totals.In += event.In
		totals.Out += event.Out
		totals.CacheRead += event.CacheRead
	}
	if totals.In != 700 || totals.Out != 340 || totals.CacheRead != 1000 {
		t.Fatalf("deduplicated totals = %+v", totals)
	}
	if child := got[2]; child.In != 100 || child.Out != 40 || child.CacheRead != 100 {
		t.Fatalf("child delta did not use replay baseline: %+v", child)
	}
}

func TestScanCodexSubagentWithoutLiveBoundaryFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeLog(t, filepath.Join(root, "2026", "07", "05"), "rollout-subagent.jsonl", `{"timestamp":"2026-07-05T11:00:00.000Z","type":"session_meta","payload":{"id":"child","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}},"parent_thread_id":"parent"}}
{"timestamp":"2026-07-05T11:00:00.001Z","type":"session_meta","payload":{"id":"parent","source":"cli"}}
{"timestamp":"2026-07-05T11:00:00.002Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}
{"timestamp":"2026-07-05T11:00:00.003Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":200}}}}
`)

	var got []Event
	result, err := scanCodex(root, since, DefaultScanLimits(), func(event Event) {
		got = append(got, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 0 || len(got) != 0 || !result.Stats.Partial ||
		!hasStr(result.Stats.Warnings, "one or more Codex subagent logs did not expose a live-usage boundary") {
		t.Fatalf("unbounded fork replay result=%+v events=%+v", result, got)
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
