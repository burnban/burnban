package budget_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/store"
)

const budgetBenchmarkRows = 100_000

func BenchmarkGuardAdmission100KRows(b *testing.B) {
	path := filepath.Join(b.TempDir(), "budget.db")
	created, err := store.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := created.Close(); err != nil {
		b.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO requests
		(ts, provider, agent, cost_usd, usage_state, pricing_state)
		VALUES (?, 'openai', 'alpha', 0.000001, 'exact', 'priced')`)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	ts := now.Add(-time.Hour).Format(time.RFC3339)
	for i := 0; i < budgetBenchmarkRows; i++ {
		if _, err := stmt.Exec(ts); err != nil {
			b.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	s, err := store.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if err := s.SetSetting(budget.KeyDailyCapUSD, "1000"); err != nil {
		b.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyAgentCapPrefix+"alpha", "1000"); err != nil {
		b.Fatal(err)
	}
	estimate := budget.AdmissionEstimate{Priced: true, Bounded: true}
	run := func(b *testing.B, guard *budget.Guard) {
		b.Helper()
		reservation, denial, err := guard.Admit(now, "alpha", estimate)
		if err != nil || denial != nil || reservation == nil {
			b.Fatalf("admit reservation=%v denial=%+v err=%v", reservation, denial, err)
		}
		reservation.Release()
	}
	b.Run("cold", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			run(b, &budget.Guard{S: s})
		}
		b.ReportMetric(budgetBenchmarkRows, "ledger_rows/op")
	})
	b.Run("warm", func(b *testing.B) {
		guard := &budget.Guard{S: s}
		run(b, guard)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			run(b, guard)
		}
		b.ReportMetric(0, "ledger_rows/op")
	})
	b.Run("warm_velocity_fuse", func(b *testing.B) {
		if err := s.SetSetting(budget.KeyFuseBurst, "5m:1000"); err != nil {
			b.Fatal(err)
		}
		guard := &budget.Guard{S: s}
		run(b, guard)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			run(b, guard)
		}
		b.ReportMetric(0, "ledger_rows/op")
	})
}
