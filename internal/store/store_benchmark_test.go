package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

const largeLedgerRows = 100_000

func benchmarkLedger(b *testing.B, count int) (*Store, time.Time) {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "ledger.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })

	tx, err := s.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO requests
		(ts, provider, model, agent, in_tokens, cache_read_tokens, cost_usd,
		 body_hash, usage_state, pricing_state)
		VALUES (?, 'openai', ?, ?, ?, ?, ?, ?, 'exact', 'priced')`)
	if err != nil {
		_ = tx.Rollback()
		b.Fatal(err)
	}
	models := make([]string, 100)
	for i := range models {
		models[i] = fmt.Sprintf("model-%03d", i)
	}
	agents := make([]string, 250)
	for i := range agents {
		agents[i] = fmt.Sprintf("agent-%03d", i)
	}
	hashes := make([]string, 5_000)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("fingerprint-%05d", i)
	}
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < count; i++ {
		ts := started.Add(time.Duration(i%3600) * time.Second).Format(time.RFC3339)
		if _, err := stmt.Exec(ts, models[i%len(models)], agents[i%len(agents)],
			100+(i%1000), i%100, float64(i%1000)/100_000, hashes[i%len(hashes)]); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			b.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return s, started
}

func BenchmarkTop100KRows(b *testing.B) {
	s, started := benchmarkLedger(b, largeLedgerRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		top, err := s.Top(started, 5)
		if err != nil {
			b.Fatal(err)
		}
		if top.Requests != largeLedgerRows || len(top.Models) != 5 || len(top.Agents) != 5 {
			b.Fatalf("unexpected top aggregate: %+v", top)
		}
	}
	b.ReportMetric(largeLedgerRows, "rows/op")
}

func BenchmarkStreamExport100KRows(b *testing.B) {
	s, started := benchmarkLedger(b, largeLedgerRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		if err := s.StreamExport(started, func(Request) error {
			count++
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if count != largeLedgerRows {
			b.Fatalf("streamed %d rows, want %d", count, largeLedgerRows)
		}
	}
	b.ReportMetric(largeLedgerRows, "rows/op")
}
