package budget_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/store"
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

func TestConcurrentAdmissionsReserveCapHeadroom(t *testing.T) {
	s := newStore(t)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "0.05"); err != nil {
		t.Fatal(err)
	}
	g := &budget.Guard{S: s}
	estimate := budget.AdmissionEstimate{USD: 0.01, Priced: true, Bounded: true}

	var wg sync.WaitGroup
	reservations := make(chan *budget.Reservation, 20)
	denials := make(chan *budget.Denial, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reservation, denial, err := g.Admit(now, "agent", estimate)
			if err != nil {
				t.Errorf("admit: %v", err)
				return
			}
			if reservation != nil {
				reservations <- reservation
			} else {
				denials <- denial
			}
		}()
	}
	wg.Wait()
	close(reservations)
	close(denials)
	var admitted int
	for reservation := range reservations {
		admitted++
		reservation.Release()
	}
	if admitted != 5 || len(denials) != 15 {
		t.Fatalf("admitted=%d denied=%d, want 5/15", admitted, len(denials))
	}
	if snapshot := g.Reservations(); snapshot.InFlight != 0 || snapshot.ReservedUSD != 0 {
		t.Fatalf("reservations leaked: %+v", snapshot)
	}
}

func TestUnboundedAdmissionIsExclusiveAndUnknownPriceFailsClosed(t *testing.T) {
	s := newStore(t)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	g := &budget.Guard{S: s}
	first, denial, err := g.Admit(now, "", budget.AdmissionEstimate{USD: 0.1, Priced: true, Bounded: false})
	if err != nil || denial != nil || first == nil {
		t.Fatalf("first unbounded admission = reservation=%v denial=%v err=%v", first, denial, err)
	}
	if got := first.AmountUSD(); got != 10 {
		t.Fatalf("exclusive reservation = %v, want 10", got)
	}
	if second, denial, err := g.Admit(now, "", budget.AdmissionEstimate{USD: 0.1, Priced: true, Bounded: false}); err != nil || second != nil || denial == nil || denial.Type != "burnban_inflight_headroom" || denial.AlertMark() != "" {
		t.Fatalf("second admission = reservation=%v denial=%v err=%v", second, denial, err)
	}
	first.Release()

	if reservation, denial, err := g.Admit(now, "", budget.AdmissionEstimate{Priced: false, Description: `model "future"`}); err != nil || reservation != nil || denial == nil || denial.Type != "burnban_unpriced_request" {
		t.Fatalf("unknown-price admission = reservation=%v denial=%+v err=%v", reservation, denial, err)
	}
}

func TestVelocityFuseTripsBeforeForwardingAndSurvivesRestart(t *testing.T) {
	s := newStore(t)
	spend(t, s, now.Add(-2*time.Minute), 3.5)
	if err := s.SetSetting(budget.KeyFuseBurst, "5m:4"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyFuseCooldown, "10m"); err != nil {
		t.Fatal(err)
	}

	guard := &budget.Guard{S: s}
	reservation, denial, err := guard.Admit(now, "agent", budget.AdmissionEstimate{
		USD: 0.6, Priced: true, Bounded: true,
	})
	if err != nil || reservation != nil || denial == nil || denial.Type != "burnban_fuse_tripped" {
		t.Fatalf("crossing admission reservation=%v denial=%+v err=%v", reservation, denial, err)
	}
	if !strings.Contains(denial.Message, "rolling 5m") || !strings.Contains(denial.Message, "$4.00") || denial.AlertMark() == "" {
		t.Fatalf("fuse denial is not explainable/alertable: %+v", denial)
	}
	if raw, err := s.GetSetting(budget.KeyFuseTrip); err != nil || raw == "" {
		t.Fatalf("persisted trip=%q err=%v", raw, err)
	}

	// A fresh guard simulates a proxy restart. The automatic cooldown remains.
	restarted := &budget.Guard{S: s}
	if got, err := restarted.Check(now.Add(time.Minute), ""); err != nil || got == nil || got.Type != "burnban_fuse_tripped" {
		t.Fatalf("restart bypassed cooldown: denial=%+v err=%v", got, err)
	}
	snapshot, err := budget.FuseStatus(s, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Tripped || snapshot.TripRule != "burst" || snapshot.TripProjected < 4.09 || snapshot.TripProjected > 4.11 {
		t.Fatalf("fuse snapshot=%+v", snapshot)
	}

	// Once both the cooldown and rolling spend have aged out, traffic resumes.
	if got, err := restarted.Check(now.Add(11*time.Minute), ""); err != nil || got != nil {
		t.Fatalf("expired fuse still denied: denial=%+v err=%v", got, err)
	}
}

func TestVelocityFuseCountsInFlightReservations(t *testing.T) {
	s := newStore(t)
	if err := s.SetSetting(budget.KeyFuseHourlyUSD, "1"); err != nil {
		t.Fatal(err)
	}
	guard := &budget.Guard{S: s}
	first, denial, err := guard.Admit(now, "", budget.AdmissionEstimate{USD: 0.6, Priced: true, Bounded: true})
	if err != nil || denial != nil || first == nil || !first.CapActive() {
		t.Fatalf("first admission=%v denial=%+v err=%v", first, denial, err)
	}
	second, denial, err := guard.Admit(now, "", budget.AdmissionEstimate{USD: 0.5, Priced: true, Bounded: true})
	if err != nil || second != nil || denial == nil || denial.Type != "burnban_fuse_tripped" {
		t.Fatalf("fan-out admission=%v denial=%+v err=%v", second, denial, err)
	}
	if !strings.Contains(denial.Message, "$1.1000") {
		t.Fatalf("projected reservation missing from denial: %s", denial.Message)
	}
	first.Release()
	if snapshot := guard.Reservations(); snapshot.InFlight != 0 || snapshot.ReservedUSD != 0 {
		t.Fatalf("reservations leaked: %+v", snapshot)
	}
}

func TestVelocityFuseSettlementUpdatesWarmRollingCache(t *testing.T) {
	s := newStore(t)
	if err := s.SetSetting(budget.KeyFuseBurst, "5m:1"); err != nil {
		t.Fatal(err)
	}
	guard := &budget.Guard{S: s}
	if denial, err := guard.Check(now, ""); err != nil || denial != nil {
		t.Fatalf("warm check denial=%+v err=%v", denial, err)
	}
	first, denial, err := guard.Admit(now, "", budget.AdmissionEstimate{USD: 0.6, Priced: true, Bounded: true})
	if err != nil || denial != nil || first == nil {
		t.Fatalf("first admission=%v denial=%+v err=%v", first, denial, err)
	}
	if err := first.Settle(store.Request{
		Ts: now, Provider: "openai", CostUSD: 0.6,
		UsageState: store.UsageExact, PricingState: store.PricingPriced,
	}); err != nil {
		t.Fatal(err)
	}
	if next, denial, err := guard.Admit(now, "", budget.AdmissionEstimate{USD: 0.5, Priced: true, Bounded: true}); err != nil || next != nil || denial == nil || denial.Type != "burnban_fuse_tripped" {
		t.Fatalf("settled spend was absent/double-counted: admission=%v denial=%+v err=%v", next, denial, err)
	}
}

func TestVelocityFuseRetripsAfterCooldownWhileSpendRemainsHigh(t *testing.T) {
	s := newStore(t)
	spend(t, s, now.Add(-time.Minute), 1.1)
	if err := s.SetSetting(budget.KeyFuseBurst, "5m:1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyFuseCooldown, "1m"); err != nil {
		t.Fatal(err)
	}
	guard := &budget.Guard{S: s}
	first, err := guard.Check(now, "")
	if err != nil || first == nil || first.Type != "burnban_fuse_tripped" {
		t.Fatalf("first trip=%+v err=%v", first, err)
	}
	secondAt := now.Add(2 * time.Minute)
	second, err := guard.Check(secondAt, "")
	if err != nil || second == nil || second.Type != "burnban_fuse_tripped" {
		t.Fatalf("high velocity did not retrip: denial=%+v err=%v", second, err)
	}
	snapshot, err := budget.FuseStatus(s, secondAt)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Tripped || !snapshot.TripStartedAt.Equal(secondAt) || !snapshot.TrippedUntil.Equal(secondAt.Add(time.Minute)) {
		t.Fatalf("retrip snapshot=%+v", snapshot)
	}
}

func TestVelocityFuseRollingWindowAndAccountingSafety(t *testing.T) {
	s := newStore(t)
	spend(t, s, now.Add(-6*time.Minute), 20) // outside the rolling burst
	spend(t, s, now.Add(-time.Minute), 1)
	if err := s.SetSetting(budget.KeyFuseBurst, "5m:2"); err != nil {
		t.Fatal(err)
	}
	guard := &budget.Guard{S: s}
	if denial, err := guard.Check(now, ""); err != nil || denial != nil {
		t.Fatalf("old spend leaked into rolling window: denial=%+v err=%v", denial, err)
	}
	if reservation, denial, err := guard.Admit(now, "", budget.AdmissionEstimate{Priced: false}); err != nil || reservation != nil || denial == nil || denial.Type != "burnban_unpriced_request" {
		t.Fatalf("unknown pricing did not fail closed: reservation=%v denial=%+v err=%v", reservation, denial, err)
	}

	if err := s.Insert(store.Request{
		Ts: now.Add(-30 * time.Second), Provider: "openai", Status: 200,
		UsageState: store.UsagePartial, PricingState: store.PricingPriced,
		EnforcementUnsafe: true,
	}); err != nil {
		t.Fatal(err)
	}
	if denial, err := guard.Check(now, ""); err != nil || denial == nil || denial.Type != "burnban_metering_gap" || !strings.Contains(denial.Message, "spend-velocity fuse") {
		t.Fatalf("unsafe rolling accounting denial=%+v err=%v", denial, err)
	}
}

func TestFuseSettingValidation(t *testing.T) {
	for _, raw := range []string{"", "5m", "0:4", "5m:0.001", "2h:4", "5m:NaN"} {
		if _, _, err := budget.ParseFuseBurst(raw); err == nil {
			t.Errorf("burst %q accepted", raw)
		}
	}
	s := newStore(t)
	if err := s.SetSetting(budget.KeyFuseCooldown, "25h"); err != nil {
		t.Fatal(err)
	}
	if _, err := budget.FuseStatus(s, now); err == nil {
		t.Fatal("invalid stored fuse cooldown accepted")
	}
	s2 := newStore(t)
	if err := s2.SetSetting(budget.KeyFuseHourlyUSD, "0.001"); err != nil {
		t.Fatal(err)
	}
	if _, err := budget.FuseStatus(s2, now); err == nil {
		t.Fatal("sub-cent stored hourly fuse accepted")
	}
}

func TestFanoutFuseCountsInFlightWithoutDollarPricing(t *testing.T) {
	s := newStore(t)
	if err := s.SetSetting(budget.KeyFuseFanout, "1m:2"); err != nil {
		t.Fatal(err)
	}
	guard := &budget.Guard{S: s}
	first, denial, err := guard.Admit(now, "", budget.AdmissionEstimate{})
	if err != nil || denial != nil || first == nil {
		t.Fatalf("first fanout admission=%v denial=%+v err=%v", first, denial, err)
	}
	second, denial, err := guard.Admit(now, "", budget.AdmissionEstimate{})
	if err != nil || denial != nil || second == nil {
		t.Fatalf("second fanout admission=%v denial=%+v err=%v", second, denial, err)
	}
	third, denial, err := guard.Admit(now, "", budget.AdmissionEstimate{})
	if err != nil || third != nil || denial == nil || denial.Type != "burnban_fuse_tripped" ||
		!strings.Contains(denial.Message, "projected 3 requests") {
		t.Fatalf("third fanout admission=%v denial=%+v err=%v", third, denial, err)
	}
	first.Release()
	second.Release()

	snapshot, err := budget.FuseStatus(s, now)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Tripped || snapshot.TripRule != "fanout" || snapshot.TripLimitRequests != 2 || snapshot.TripProjectedRequests != 3 {
		t.Fatalf("fanout snapshot=%+v", snapshot)
	}
}

func TestFanoutFuseSerializesConcurrentAdmissionsAtExactLimit(t *testing.T) {
	s := newStore(t)
	if err := s.SetSetting(budget.KeyFuseFanout, "1m:10"); err != nil {
		t.Fatal(err)
	}
	guard := &budget.Guard{S: s}
	reservations := make(chan *budget.Reservation, 100)
	denials := make(chan *budget.Denial, 100)
	errors := make(chan error, 100)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reservation, denial, err := guard.Admit(now, "", budget.AdmissionEstimate{})
			switch {
			case err != nil:
				errors <- err
			case reservation != nil:
				reservations <- reservation
			default:
				denials <- denial
			}
		}()
	}
	wg.Wait()
	close(reservations)
	close(denials)
	close(errors)
	if len(errors) != 0 {
		t.Fatalf("concurrent admission errors=%d first=%v", len(errors), <-errors)
	}
	if len(reservations) != 10 || len(denials) != 90 {
		t.Fatalf("concurrent fanout admitted=%d denied=%d, want 10/90", len(reservations), len(denials))
	}
	for reservation := range reservations {
		reservation.Release()
	}
	for denial := range denials {
		if denial == nil || denial.Type != "burnban_fuse_tripped" {
			t.Fatalf("unexpected concurrent fanout denial=%+v", denial)
		}
	}
	if snapshot := guard.Reservations(); snapshot.InFlight != 0 || snapshot.ReservedUSD != 0 {
		t.Fatalf("concurrent fanout leaked reservations: %+v", snapshot)
	}
}

func TestBaselineFuseUsesSameUTCSlotMedianAndMinimumFloor(t *testing.T) {
	s := newStore(t)
	at := now.Add(30 * time.Minute)
	currentStart := at.UTC().Truncate(time.Hour)
	for day, usd := range []float64{1, 1, 10, 0, 1, 1, 1} {
		if usd != 0 {
			spend(t, s, currentStart.Add(-time.Duration(day+1)*24*time.Hour).Add(10*time.Minute), usd)
		}
	}
	// Large traffic in a different slot must not inflate the comparator.
	spend(t, s, currentStart.Add(-24*time.Hour).Add(-time.Minute), 100)
	spend(t, s, currentStart.Add(5*time.Minute), 1.9)

	raw, err := budget.EncodeFuseBaseline(budget.FuseBaselinePolicy{
		Version: 1, Window: time.Hour, Multiplier: 2, LookbackDays: 7, MinimumUSD: 0.25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyFuseBaseline, raw); err != nil {
		t.Fatal(err)
	}
	guard := &budget.Guard{S: s}
	reservation, denial, err := guard.Admit(at, "", budget.AdmissionEstimate{USD: 0.2, Priced: true, Bounded: true})
	if err != nil || reservation != nil || denial == nil || denial.Type != "burnban_fuse_tripped" {
		t.Fatalf("baseline crossing=%v denial=%+v err=%v", reservation, denial, err)
	}
	snapshot, err := budget.FuseStatus(s, at)
	if err != nil {
		t.Fatal(err)
	}
	var baseline budget.FuseRuleState
	for _, rule := range snapshot.Rules {
		if rule.Name == "baseline" {
			baseline = rule
		}
	}
	if baseline.CapUSD != 2 || baseline.BaselineMedianUSD != 1 || baseline.BaselineMultiplier != 2 || baseline.SpentUSD != 1.9 ||
		baseline.ProjectedTimeToLimit < 90*time.Second || baseline.ProjectedTimeToLimit > 2*time.Minute {
		t.Fatalf("baseline status=%+v", baseline)
	}

	// A completely idle history still gets the explicit absolute floor.
	s2 := newStore(t)
	raw, err = budget.EncodeFuseBaseline(budget.FuseBaselinePolicy{
		Version: 1, Window: time.Hour, Multiplier: 3, LookbackDays: 7, MinimumUSD: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.SetSetting(budget.KeyFuseBaseline, raw); err != nil {
		t.Fatal(err)
	}
	snapshot, err = budget.FuseStatus(s2, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Rules) != 1 || snapshot.Rules[0].CapUSD != 0.5 || snapshot.Rules[0].BaselineMedianUSD != 0 {
		t.Fatalf("idle baseline floor=%+v", snapshot.Rules)
	}
}

func TestBaselineFuseRefreshesAfterBackdatedLedgerMutation(t *testing.T) {
	s := newStore(t)
	at := now.Add(30 * time.Minute)
	currentStart := at.UTC().Truncate(time.Hour)
	raw, err := budget.EncodeFuseBaseline(budget.FuseBaselinePolicy{
		Version: 1, Window: time.Hour, Multiplier: 2, LookbackDays: 7, MinimumUSD: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyFuseBaseline, raw); err != nil {
		t.Fatal(err)
	}
	guard := &budget.Guard{S: s}
	if denial, err := guard.Check(at, ""); err != nil || denial != nil {
		t.Fatalf("warm empty baseline denial=%+v err=%v", denial, err)
	}
	// Four populated same-time slots make the seven-day median $10. If the
	// historical cache remained at its old zero value, this $1 estimate would
	// cross the $0.50 floor and trip instead of fitting under the new $20 cap.
	for day := 1; day <= 4; day++ {
		spend(t, s, currentStart.Add(-time.Duration(day)*24*time.Hour).Add(10*time.Minute), 10)
	}
	reservation, denial, err := guard.Admit(at, "", budget.AdmissionEstimate{USD: 1, Priced: true, Bounded: true})
	if err != nil || denial != nil || reservation == nil {
		t.Fatalf("backdated refresh reservation=%v denial=%+v err=%v", reservation, denial, err)
	}
	reservation.Release()
	snapshot, err := budget.FuseStatus(s, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Rules) != 1 || snapshot.Rules[0].BaselineMedianUSD != 10 || snapshot.Rules[0].CapUSD != 20 {
		t.Fatalf("refreshed baseline=%+v", snapshot.Rules)
	}
}

func TestFanoutAndBaselineSettingValidation(t *testing.T) {
	for _, raw := range []string{"", "1m", "0:1", "1m:0", "2h:4", "1m:1000001", "1m:1.5"} {
		if _, _, err := budget.ParseFuseFanout(raw); err == nil {
			t.Errorf("fanout %q accepted", raw)
		}
	}
	for _, policy := range []budget.FuseBaselinePolicy{
		{Version: 2, Window: time.Hour, Multiplier: 3, LookbackDays: 14, MinimumUSD: 1},
		{Version: 1, Window: 7 * time.Minute, Multiplier: 3, LookbackDays: 14, MinimumUSD: 1},
		{Version: 1, Window: time.Hour, Multiplier: 1, LookbackDays: 14, MinimumUSD: 1},
		{Version: 1, Window: time.Hour, Multiplier: 3, LookbackDays: 6, MinimumUSD: 1},
		{Version: 1, Window: time.Hour, Multiplier: 3, LookbackDays: 14, MinimumUSD: 0},
	} {
		if _, err := budget.EncodeFuseBaseline(policy); err == nil {
			t.Errorf("invalid baseline accepted: %+v", policy)
		}
	}
	s := newStore(t)
	if err := s.SetSetting(budget.KeyFuseBaseline, "{\"version\":1,\"Version\":1,\"window\":\"1h\",\"multiplier\":3,\"lookback_days\":14,\"minimum_usd\":1}"); err != nil {
		t.Fatal(err)
	}
	if _, err := budget.FuseStatus(s, now); err == nil {
		t.Fatal("case-ambiguous baseline setting accepted")
	}
}

func TestEnforcementGapFailsClosedAndTinyGuidanceKeepsCents(t *testing.T) {
	s := newStore(t)
	if err := s.Insert(store.Request{
		Ts: now.Add(-time.Minute), Provider: "openai", Model: "known", Status: 200,
		UsageState: store.UsagePartial, PricingState: store.PricingPriced,
		EnforcementUnsafe: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	g := &budget.Guard{S: s}
	if denial, err := g.Check(now, ""); err != nil || denial == nil || denial.Type != "burnban_metering_gap" {
		t.Fatalf("metering gap denial=%+v err=%v", denial, err)
	}

	s2 := newStore(t)
	spend(t, s2, now.Add(-time.Minute), 0.02)
	if err := s2.SetSetting(budget.KeyDailyCapUSD, "0.01"); err != nil {
		t.Fatal(err)
	}
	denial, err := (&budget.Guard{S: s2}).Check(now, "")
	if err != nil || denial == nil {
		t.Fatalf("tiny cap denial=%+v err=%v", denial, err)
	}
	if !strings.Contains(denial.Message, "--daily 0.02") || strings.Contains(denial.Message, "--daily 0)") {
		t.Fatalf("unsafe tiny-cap guidance: %s", denial.Message)
	}
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

func TestTemporaryExternalIncreaseExpiresLocallyWhileOffline(t *testing.T) {
	s := newStore(t)
	guard := &budget.Guard{S: s}
	queryNow := now.UTC().Truncate(time.Second)
	spend(t, s, queryNow.Add(-time.Hour), 12)
	if err := s.SetSetting(budget.KeyExternalDailyCapUSD, "15"); err != nil {
		t.Fatal(err)
	}
	expires := queryNow.Add(10 * time.Minute)
	raw := fmt.Sprintf(`{"version":1,"exceptions":[{"request_id":"apr_one","window":"daily","amount_usd":5,"valid_until":%q}]}`,
		expires.Format(time.RFC3339))
	if err := s.SetSetting(budget.KeyExternalPolicyExceptions, raw); err != nil {
		t.Fatal(err)
	}
	if denial, err := guard.Check(queryNow, ""); err != nil || denial != nil {
		t.Fatalf("active temporary increase denied: denial=%+v err=%v", denial, err)
	}
	states, err := budget.Status(s, queryNow)
	if err != nil || states[0].ExternalCapUSD != 15 {
		t.Fatalf("active state=%+v err=%v", states, err)
	}
	denial, err := guard.Check(expires, "")
	if err != nil || denial == nil || denial.Type != "burnban_external_cap_reached" {
		t.Fatalf("offline expiry denial=%+v err=%v", denial, err)
	}
	states, err = budget.Status(s, expires)
	if err != nil || states[0].ExternalCapUSD != 10 || states[0].CapUSD != 10 {
		t.Fatalf("expired state=%+v err=%v", states, err)
	}
}

func TestTemporaryExternalIncreaseScheduleFailsClosed(t *testing.T) {
	s := newStore(t)
	if err := s.SetSetting(budget.KeyExternalDailyCapUSD, "15"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"version":2,"exceptions":[]}`,
		`{"version":1,"exceptions":[{"request_id":"same","window":"daily","amount_usd":5,"valid_until":"2026-07-01T00:00:00Z"},{"request_id":"same","window":"daily","amount_usd":5,"valid_until":"2026-07-01T00:00:00Z"}]}`,
		`{"version":1,"exceptions":[{"request_id":"apr","window":"daily","amount_usd":20,"valid_until":"2026-07-01T00:00:00Z"}]}`,
		`{"version":1,"version":1,"exceptions":[]}`,
		`{"Version":1,"exceptions":[]}`,
		`{"version":1,"exceptions":[{"request_id":"apr","window":"daily","amount_usd":1,"amount_usd":5,"valid_until":"2026-07-01T00:00:00Z"}]}`,
		string([]byte{'{', '"', 'v', 'e', 'r', 's', 'i', 'o', 'n', '"', ':', '1', ',', '"', 'e', 'x', 'c', 'e', 'p', 't', 'i', 'o', 'n', 's', '"', ':', '[', ']', ',', '"', 0xff, '"', ':', '0', '}'}),
	} {
		if err := s.SetSetting(budget.KeyExternalPolicyExceptions, raw); err != nil {
			t.Fatal(err)
		}
		if denial, err := (&budget.Guard{S: s}).Check(now, ""); err == nil || denial != nil {
			t.Fatalf("unsafe schedule did not fail closed: denial=%+v err=%v raw=%s", denial, err, raw)
		}
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
	// external-policy UTC day (July 9), so it must not consume the shared cap.
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
