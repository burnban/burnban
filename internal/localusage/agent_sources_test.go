package localusage

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeHermesFixture(t *testing.T, path string) {
	t.Helper()
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
}

func TestScanHermes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	writeHermesFixture(t, path)
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

func writeGooseFixture(t *testing.T, path string) {
	t.Helper()
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
}

func TestScanGoose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	writeGooseFixture(t, path)
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

type sqliteLimitScanner func(string, time.Time, ScanLimits, func(Event)) (ScanResult, error)

func assertSQLiteByteLimits(t *testing.T, name string, writeFixture func(*testing.T, string), scan sqliteLimitScanner) {
	t.Helper()
	for _, tc := range []struct {
		name         string
		sidecar      string
		skippedFiles int
	}{
		{name: "main", skippedFiles: 1},
		{name: "wal", sidecar: "-wal", skippedFiles: 2},
		{name: "shm", sidecar: "-shm", skippedFiles: 2},
		{name: "rollback-journal", sidecar: "-journal", skippedFiles: 2},
	} {
		t.Run(name+"-oversized-"+tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name+".db")
			writeFixture(t, path)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			maxBytes := int64(1)
			if tc.sidecar != "" {
				maxBytes = info.Size()
				if err := os.WriteFile(path+tc.sidecar, []byte{0}, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			limits := DefaultScanLimits()
			limits.MaxBytes = maxBytes
			emitted := 0
			result, err := scan(path, time.Time{}, limits, func(Event) { emitted++ })
			if err != nil {
				t.Fatal(err)
			}
			if emitted != 0 || result.Sessions != 0 || !result.Stats.Partial ||
				result.Stats.FilesScanned != 0 || result.Stats.FilesSkipped != tc.skippedFiles ||
				result.Stats.BytesScanned != 0 || !hasStr(result.Stats.Warnings, "byte scan limit reached") {
				t.Fatalf("result=%+v emitted=%d", result, emitted)
			}
		})
	}
}

func TestScanHermesHonorsSQLiteByteLimits(t *testing.T) {
	assertSQLiteByteLimits(t, "hermes", writeHermesFixture, scanHermes)
}

func TestScanGooseHonorsSQLiteByteLimits(t *testing.T) {
	assertSQLiteByteLimits(t, "goose", writeGooseFixture, scanGoose)
}

func TestSQLiteSourceSizeAdditionRejectsOverflow(t *testing.T) {
	maxInt64 := int64(^uint64(0) >> 1)
	if got, err := addSQLiteSourceSize(maxInt64-1, 1); err != nil || got != maxInt64 {
		t.Fatalf("boundary addition = %d, %v", got, err)
	}
	if _, err := addSQLiteSourceSize(maxInt64, 1); err == nil {
		t.Fatal("overflowing source sizes were accepted")
	}
}
