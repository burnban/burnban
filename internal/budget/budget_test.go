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
	// The next day the window's spend resets to zero, so there must be no
	// warning — a non-nil result means a mark or window-start computation
	// leaked across days.
	if w3, err := g.WarnStatus(now.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	} else if w3 != nil {
		t.Fatalf("fresh day warned spuriously: %+v", w3)
	}
}

func TestZeroCapIsUnset(t *testing.T) {
	s := newStore(t)
	g := &budget.Guard{S: s}
	spend(t, s, now.Add(-time.Hour), 5)
	// A stored "0.00" (e.g. hand-edited settings) must read as no cap at
	// all — not as a cap that denies everything or divides warn math by zero.
	if err := s.SetSetting(budget.KeyDailyCapUSD, "0.00"); err != nil {
		t.Fatal(err)
	}
	if d, err := g.Check(now, ""); err != nil || d != nil {
		t.Fatalf("zero cap denied traffic: %+v, %v", d, err)
	}
	if w, err := g.WarnStatus(now); err != nil || w != nil {
		t.Fatalf("zero cap produced a warning: %+v, %v", w, err)
	}
}

func TestNonFiniteSettingsAreRejected(t *testing.T) {
	for _, value := range []string{"NaN", "+Inf", "-Inf"} {
		s := newStore(t)
		if err := s.SetSetting(budget.KeyDailyCapUSD, value); err != nil {
			t.Fatal(err)
		}
		if _, err := (&budget.Guard{S: s}).Check(now, ""); err == nil {
			t.Errorf("cap %q was accepted", value)
		}
	}
	s := newStore(t)
	if err := s.SetSetting(budget.KeyWarnPct, "101"); err != nil {
		t.Fatal(err)
	}
	if _, err := (&budget.Guard{S: s}).WarnStatus(now); err == nil {
		t.Fatal("warning threshold above 100 was accepted")
	}
}

func TestExternalPolicyIsStricterAndCannotBeOverridden(t *testing.T) {
	s := newStore(t)
	g := &budget.Guard{S: s}
	spend(t, s, now.Add(-time.Hour), 6)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyExternalDailyCapUSD, "5"); err != nil {
		t.Fatal(err)
	}
	denial, err := g.Check(now, "")
	if err != nil {
		t.Fatal(err)
	}
	if denial == nil || denial.Type != "burnban_external_cap_reached" {
		t.Fatalf("denial = %+v", denial)
	}
	if err := s.SetSetting(budget.KeyOverrideDay, now.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	denial, err = g.Check(now, "")
	if err != nil || denial == nil || denial.Type != "burnban_external_cap_reached" {
		t.Fatalf("local override bypassed external policy: denial=%+v err=%v", denial, err)
	}
	states, err := budget.Status(s, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := states[0]; got.CapUSD != 5 || got.LocalCapUSD != 10 || got.ExternalCapUSD != 5 || got.Source != "external" {
		t.Fatalf("daily state = %+v", got)
	}
}

func TestLocalCapCanRemainStricterThanExternalPolicy(t *testing.T) {
	s := newStore(t)
	g := &budget.Guard{S: s}
	spend(t, s, now.Add(-time.Hour), 6)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "5"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyExternalDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	denial, err := g.Check(now, "")
	if err != nil || denial == nil || denial.Type != "burnban_cap_reached" {
		t.Fatalf("denial=%+v err=%v", denial, err)
	}
}

func TestExternalBanCannotBeLiftedLocally(t *testing.T) {
	s := newStore(t)
	if err := s.SetSetting(budget.KeyExternalBanActive, "1"); err != nil {
		t.Fatal(err)
	}
	denial, err := (&budget.Guard{S: s}).Check(now, "")
	if err != nil || denial == nil || denial.Type != "burnban_external_ban" {
		t.Fatalf("denial=%+v err=%v", denial, err)
	}
	local, external, err := budget.BanStatus(s)
	if err != nil || local || !external {
		t.Fatalf("ban status local=%t external=%t err=%v", local, external, err)
	}
}

func TestExternalWindowsUseUTCAcrossMeters(t *testing.T) {
	s := newStore(t)
	location := time.FixedZone("Pacific", -7*60*60)
	queryNow := time.Date(2026, 7, 9, 1, 0, 0, 0, time.UTC).In(location)
	// This row belongs to the machine-local day (July 8) but precedes the
	// organization UTC day (July 9), so it must not consume the fleet cap.
	spend(t, s, time.Date(2026, 7, 8, 22, 0, 0, 0, time.UTC), 9)
	if err := s.SetSetting(budget.KeyExternalDailyCapUSD, "5"); err != nil {
		t.Fatal(err)
	}
	guard := &budget.Guard{S: s}
	if denial, err := guard.Check(queryNow, ""); err != nil || denial != nil {
		t.Fatalf("pre-UTC-boundary spend denied: denial=%+v err=%v", denial, err)
	}
	spend(t, s, time.Date(2026, 7, 9, 0, 30, 0, 0, time.UTC), 6)
	if denial, err := guard.Check(queryNow, ""); err != nil || denial == nil || denial.Type != "burnban_external_cap_reached" {
		t.Fatalf("UTC-window spend was not denied: denial=%+v err=%v", denial, err)
	}
	states, err := budget.Status(s, queryNow)
	if err != nil {
		t.Fatal(err)
	}
	if states[0].LocalSpent != 15 || states[0].ExternalSpent != 6 || states[0].Spent != 6 {
		t.Fatalf("daily state=%+v", states[0])
	}
}

func TestStatusOneShot(t *testing.T) {
	s := newStore(t)
	spend(t, s, now.Add(-time.Hour), 3)
	spend(t, s, now.AddDate(0, 0, -2), 4) // earlier this week, not today
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	states, err := budget.Status(s, now)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]budget.WindowState{}
	for _, st := range states {
		byName[st.Name] = st
	}
	if d := byName["daily"]; !d.Set || d.CapUSD != 10 || d.Spent != 3 {
		t.Fatalf("daily = %+v", d)
	}
	if w := byName["weekly"]; w.Set || w.Spent != 7 {
		t.Fatalf("weekly = %+v", w)
	}
	if m := byName["monthly"]; m.Set || m.Spent != 7 {
		t.Fatalf("monthly = %+v", m)
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
