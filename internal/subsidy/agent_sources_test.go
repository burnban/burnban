package subsidy

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestScanHermes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, model TEXT, started_at REAL,
		input_tokens INTEGER, output_tokens INTEGER,
		cache_read_tokens INTEGER, cache_write_tokens INTEGER
	);
	INSERT INTO sessions VALUES ('old', 'gpt-5.6-sol', 1, 9, 9, 9, 9);
	INSERT INTO sessions VALUES ('new', 'openai/gpt-5.6-sol', 1783252800, 100, 20, 300, 40);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var events []Event
	n, err := ScanHermes(path, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(events) != 1 {
		t.Fatalf("sessions=%d events=%+v", n, events)
	}
	if got := events[0]; got.Provider != "hermes" || got.Model != "openai/gpt-5.6-sol" || got.In != 100 || got.Out != 20 || got.CacheRead != 300 || got.CacheWrite5m != 40 {
		t.Fatalf("event = %+v", got)
	}
}

func TestScanOpenClaw(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agents", "main", "sessions")
	writeLog(t, dir, "session.jsonl", `{"type":"session","id":"s","timestamp":"2026-07-05T10:00:00Z"}
{"type":"message","id":"a","timestamp":"2026-07-05T10:00:01Z","message":{"role":"assistant","provider":"openai","model":"gpt-5.6-sol","usage":{"input":100,"output":20,"cacheRead":300,"cacheWrite":40,"cost":{"total":0.0123}},"timestamp":1783245601000}}
{"type":"message","id":"u","timestamp":"2026-07-05T10:00:02Z","message":{"role":"user","content":[]}}
`)
	writeLog(t, dir, "session.trajectory.jsonl", `{"type":"message","timestamp":"2026-07-05T10:00:01Z","message":{"role":"assistant","model":"duplicate","usage":{"input":999}}}
`)
	var events []Event
	n, err := ScanOpenClaw(root, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(events) != 1 {
		t.Fatalf("sessions=%d events=%+v", n, events)
	}
	if got := events[0]; got.Provider != "openclaw" || got.Model != "openai/gpt-5.6-sol" || got.In != 100 || got.Out != 20 || got.CacheRead != 300 || got.CacheWrite5m != 40 || !got.CostKnown {
		t.Fatalf("event = %+v", got)
	}
}

func TestScanGoose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE usage_ledger (
		id INTEGER PRIMARY KEY, session_id TEXT, created_timestamp INTEGER,
		model TEXT, input_tokens INTEGER, output_tokens INTEGER,
		total_tokens INTEGER, cache_read_tokens INTEGER,
		cache_write_tokens INTEGER, cost REAL, cost_source TEXT,
		is_compaction INTEGER DEFAULT 0
	);
	INSERT INTO usage_ledger VALUES (1, 'old', 1, 'gpt-old', 9, 9, 18, 0, 0, .1, 'known', 0);
	INSERT INTO usage_ledger VALUES (2, 's1', 1783245601, 'openai/gpt-5.6-sol', 100, 20, 460, 300, 40, .0123, 'known', 0);
	INSERT INTO usage_ledger VALUES (3, 's1', 1783245602, 'openai/gpt-5.6-sol', 50, 10, 60, 0, 0, .004, 'known', 1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var events []Event
	n, err := ScanGoose(path, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(events) != 2 {
		t.Fatalf("sessions=%d events=%+v", n, events)
	}
	if got := events[0]; got.Provider != "goose" || got.Model != "openai/gpt-5.6-sol" || got.In != 100 || got.Out != 20 || got.CacheRead != 300 || got.CacheWrite5m != 40 || !got.CostKnown {
		t.Fatalf("event = %+v", got)
	}
}
