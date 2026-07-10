package budget_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func spend(t *testing.T, s *store.Store, ts time.Time, usd float64) {
	t.Helper()
	if err := s.Insert(store.Request{Ts: ts, Provider: "anthropic", CostUSD: usd, Priced: true}); err != nil {
		t.Fatal(err)
	}
}

// now is a Thursday mid-month so day, week, and month windows all differ.
var now = time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)

func TestWindowStarts(t *testing.T) {
	if got := budget.DayStart(now); !got.Equal(time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("DayStart = %v", got)
	}
	// 2026-07-09 is a Thursday; the week began Monday the 6th.
	if got := budget.WeekStart(now); !got.Equal(time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("WeekStart = %v", got)
	}
	// A Monday is its own week start.
	monday := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	if got := budget.WeekStart(monday); !got.Equal(time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("WeekStart(monday) = %v", got)
	}
	// Sunday still belongs to the week that began the previous Monday.
	sunday := time.Date(2026, 7, 12, 23, 0, 0, 0, time.UTC)
	if got := budget.WeekStart(sunday); !got.Equal(time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("WeekStart(sunday) = %v", got)
	}
	if got := budget.MonthStart(now); !got.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("MonthStart = %v", got)
	}
}

func TestWeeklyCapCountsWholeWeek(t *testing.T) {
	s := newStore(t)
	g := &budget.Guard{S: s}

	// $6 spent Tuesday, $5 today: over a $10 weekly cap, under any daily one.
	spend(t, s, now.AddDate(0, 0, -2), 6)
	spend(t, s, now.Add(-time.Hour), 5)
	if err := s.SetSetting(budget.KeyWeeklyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}

	d, err := g.Check(now, "")
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Type != "burnban_cap_reached" || !strings.Contains(d.Message, "weekly") {
		t.Fatalf("denial = %+v", d)
	}

	// Last week's spend must not count toward this week.
	s2 := newStore(t)
	g2 := &budget.Guard{S: s2}
	spend(t, s2, now.AddDate(0, 0, -7), 11)
	if err := s2.SetSetting(budget.KeyWeeklyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	if d, err := g2.Check(now, ""); err != nil || d != nil {
		t.Fatalf("last week's spend leaked in: %+v, %v", d, err)
	}
}

func TestMonthlyCap(t *testing.T) {
	s := newStore(t)
	g := &budget.Guard{S: s}
	spend(t, s, now.AddDate(0, 0, -8), 100) // July 1st, this month
	if err := s.SetSetting(budget.KeyMonthlyCapUSD, "99"); err != nil {
		t.Fatal(err)
	}
	d, err := g.Check(now, "")
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || !strings.Contains(d.Message, "monthly") {
		t.Fatalf("denial = %+v", d)
	}
}

func TestTightestWindowWins(t *testing.T) {
	s := newStore(t)
	g := &budget.Guard{S: s}
	spend(t, s, now.Add(-time.Hour), 20)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyMonthlyCapUSD, "15"); err != nil {
		t.Fatal(err)
	}
	d, err := g.Check(now, "")
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || !strings.Contains(d.Message, "daily") {
		t.Fatalf("want the daily denial first, got %+v", d)
	}
}

func TestWarnStatus(t *testing.T) {
	s := newStore(t)
	g := &budget.Guard{S: s}
	spend(t, s, now.Add(-time.Hour), 8.5)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}

	w, err := g.WarnStatus(now)
	if err != nil {
		t.Fatal(err)
	}
	if w == nil || w.Window != "daily" || w.Pct < 84 || w.Pct > 86 {
		t.Fatalf("warning = %+v", w)
	}

	// Marking the window silences it; a new day re-arms it.
	if err := s.SetSetting(w.MarkKey, "1"); err != nil {
		t.Fatal(err)
	}
	if w2, err := g.WarnStatus(now); err != nil || w2 != nil {
		t.Fatalf("marked window warned again: %+v, %v", w2, err)
	}
	if w3, err := g.WarnStatus(now.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	} else if w3 == nil {
		// Yesterday's $8.50 is outside the new day's window, so no warning —
		// but only because spend reset, not because the mark leaked over.
		if got, _ := s.SpentSince(budget.DayStart(now.AddDate(0, 0, 1))); got != 0 {
			t.Fatalf("expected fresh window, spent = %v", got)
		}
	}
}

func TestWarnBelowThresholdAndDisabled(t *testing.T) {
	s := newStore(t)
	g := &budget.Guard{S: s}
	spend(t, s, now.Add(-time.Hour), 5)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	if w, err := g.WarnStatus(now); err != nil || w != nil {
		t.Fatalf("50%% should not warn at the default threshold: %+v, %v", w, err)
	}

	// A lower explicit threshold fires; "0" disables outright.
	if err := s.SetSetting(budget.KeyWarnPct, "40"); err != nil {
		t.Fatal(err)
	}
	if w, err := g.WarnStatus(now); err != nil || w == nil {
		t.Fatalf("40%% threshold should warn at 50%%: %+v, %v", w, err)
	}
	if err := s.SetSetting(budget.KeyWarnPct, "0"); err != nil {
		t.Fatal(err)
	}
	if w, err := g.WarnStatus(now); err != nil || w != nil {
		t.Fatalf("warn 0 should disable: %+v, %v", w, err)
	}
}

func TestWarnPicksMostBurnedWindow(t *testing.T) {
	s := newStore(t)
	g := &budget.Guard{S: s}
	spend(t, s, now.Add(-time.Hour), 9)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil { // 90%
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyWeeklyCapUSD, "11"); err != nil { // ~82%
		t.Fatal(err)
	}
	w, err := g.WarnStatus(now)
	if err != nil {
		t.Fatal(err)
	}
	if w == nil || w.Window != "daily" {
		t.Fatalf("want daily (90%%) over weekly (82%%), got %+v", w)
	}
}
