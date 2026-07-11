package budget_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/store"
)

func TestDirectInsertAndPruneInvalidateAdmissionCache(t *testing.T) {
	s := newStore(t)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	g := &budget.Guard{S: s}
	if denial, err := g.Check(now, ""); err != nil || denial != nil {
		t.Fatalf("cold check denial=%+v err=%v", denial, err)
	}
	if denial, err := g.Check(now, ""); err != nil || denial != nil {
		t.Fatalf("warm check denial=%+v err=%v", denial, err)
	}
	spend(t, s, now.Add(-time.Minute), 1.1)
	if denial, err := g.Check(now, ""); err != nil || denial == nil || denial.Type != "burnban_cap_reached" {
		t.Fatalf("direct insert did not invalidate cache: denial=%+v err=%v", denial, err)
	}
	if deleted, err := s.Prune(now.Add(time.Hour)); err != nil || deleted != 1 {
		t.Fatalf("prune deleted=%d err=%v", deleted, err)
	}
	if denial, err := g.Check(now, ""); err != nil || denial != nil {
		t.Fatalf("prune did not invalidate cache: denial=%+v err=%v", denial, err)
	}
}

func TestSettlementUpdatesWarmGlobalAndAgentCachesExactlyOnce(t *testing.T) {
	s := newStore(t)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyAgentCapPrefix+"alpha", "2"); err != nil {
		t.Fatal(err)
	}
	g := &budget.Guard{S: s}
	if denial, err := g.Check(now, "alpha"); err != nil || denial != nil {
		t.Fatalf("warm cache check denial=%+v err=%v", denial, err)
	}
	reservation, denial, err := g.Admit(now, "alpha", budget.AdmissionEstimate{USD: 1, Priced: true, Bounded: true})
	if err != nil || denial != nil || reservation == nil {
		t.Fatalf("admit reservation=%v denial=%+v err=%v", reservation, denial, err)
	}
	if err := reservation.Settle(store.Request{
		Ts: now, Provider: "openai", Agent: "alpha", CostUSD: 0.75,
		UsageState: store.UsageExact, PricingState: store.PricingPriced,
	}); err != nil {
		t.Fatal(err)
	}
	// Exactly $1.25 remains on the agent cap. This succeeds only if settlement
	// replaced the reservation with durable spend exactly once (not zero/twice).
	next, denial, err := g.Admit(now, "alpha", budget.AdmissionEstimate{USD: 1.25, Priced: true, Bounded: true})
	if err != nil || denial != nil || next == nil {
		t.Fatalf("post-settlement admit=%v denial=%+v err=%v", next, denial, err)
	}
	next.Release()
	if snapshot := g.Reservations(); snapshot.InFlight != 0 || snapshot.ReservedUSD != 0 {
		t.Fatalf("settlement leaked/double-counted reservation: %+v", snapshot)
	}

	unsafe, denial, err := g.Admit(now, "alpha", budget.AdmissionEstimate{USD: 0.1, Priced: true, Bounded: true})
	if err != nil || denial != nil || unsafe == nil {
		t.Fatalf("unsafe admit=%v denial=%+v err=%v", unsafe, denial, err)
	}
	if err := unsafe.Settle(store.Request{
		Ts: now, Provider: "openai", Agent: "alpha", CostUSD: 0.01,
		UsageState: store.UsagePartial, PricingState: store.PricingPriced,
		EnforcementUnsafe: true,
	}); err != nil {
		t.Fatal(err)
	}
	if denial, err := g.Check(now, "alpha"); err != nil || denial == nil || denial.Type != "burnban_metering_gap" {
		t.Fatalf("settled accounting gap was not fail-closed: denial=%+v err=%v", denial, err)
	}
}

func TestCacheSeparatesLocalUTCAndAgentWindows(t *testing.T) {
	s := newStore(t)
	location := time.FixedZone("Pacific", -7*60*60)
	queryNow := time.Date(2026, 7, 9, 1, 0, 0, 0, time.UTC).In(location)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyExternalDailyCapUSD, "1.5"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyAgentCapPrefix+"alpha", "5"); err != nil {
		t.Fatal(err)
	}
	spendAgent := func(ts time.Time, usd float64) {
		t.Helper()
		if err := s.Insert(store.Request{
			Ts: ts, Provider: "openai", Agent: "alpha", CostUSD: usd,
			UsageState: store.UsageExact, PricingState: store.PricingPriced,
		}); err != nil {
			t.Fatal(err)
		}
	}
	spendAgent(time.Date(2026, 7, 8, 22, 0, 0, 0, time.UTC), 4)
	g := &budget.Guard{S: s}
	if denial, err := g.Check(queryNow, "alpha"); err != nil || denial != nil {
		t.Fatalf("pre-UTC row leaked into external window: denial=%+v err=%v", denial, err)
	}
	spendAgent(time.Date(2026, 7, 9, 0, 30, 0, 0, time.UTC), 2)
	if denial, err := g.Check(queryNow, "alpha"); err != nil || denial == nil || denial.Type != "burnban_external_cap_reached" {
		t.Fatalf("UTC cache/invalidation denial=%+v err=%v", denial, err)
	}
	if err := s.SetSetting(budget.KeyExternalDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	if denial, err := g.Check(queryNow, "alpha"); err != nil || denial == nil || denial.Type != "burnban_agent_cap_reached" {
		t.Fatalf("agent cache/settings update denial=%+v err=%v", denial, err)
	}
}

func TestConcurrentSettlementAndAdmissionNeverSeesInsertReservationDoubleCount(t *testing.T) {
	s := newStore(t)
	g := &budget.Guard{S: s}
	durable := 0.0
	for i := 0; i < 100; i++ {
		if err := s.SetSetting(budget.KeyDailyCapUSD, fmt.Sprintf("%.2f", durable+2)); err != nil {
			t.Fatal(err)
		}
		first, denial, err := g.Admit(now, "", budget.AdmissionEstimate{USD: 1, Priced: true, Bounded: true})
		if err != nil || denial != nil || first == nil {
			t.Fatalf("iteration %d first admit=%v denial=%+v err=%v", i, first, denial, err)
		}
		start := make(chan struct{})
		settleErr := make(chan error, 1)
		admitResult := make(chan error, 1)
		go func() {
			<-start
			settleErr <- first.Settle(store.Request{
				Ts: now, Provider: "openai", CostUSD: 0.5,
				UsageState: store.UsageExact, PricingState: store.PricingPriced,
			})
		}()
		go func() {
			<-start
			reservation, denial, err := g.Admit(now, "", budget.AdmissionEstimate{USD: 1, Priced: true, Bounded: true})
			if reservation != nil {
				reservation.Release()
			}
			if err != nil {
				admitResult <- err
			} else if denial != nil {
				admitResult <- denial
			} else {
				admitResult <- nil
			}
		}()
		close(start)
		if err := <-settleErr; err != nil {
			t.Fatalf("iteration %d settle: %v", i, err)
		}
		if err := <-admitResult; err != nil {
			t.Fatalf("iteration %d concurrent admission observed double count: %v", i, err)
		}
		durable += 0.5
	}
}

func TestSameGuardLoadsFreshCalendarRolloverCutoffs(t *testing.T) {
	tests := []struct {
		name, key string
		advance   func(time.Time) time.Time
	}{
		{name: "day", key: budget.KeyDailyCapUSD, advance: func(v time.Time) time.Time { return v.AddDate(0, 0, 1) }},
		{name: "week", key: budget.KeyWeeklyCapUSD, advance: func(v time.Time) time.Time { return v.AddDate(0, 0, 7) }},
		{name: "month", key: budget.KeyMonthlyCapUSD, advance: func(v time.Time) time.Time { return v.AddDate(0, 1, 0) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			if err := s.SetSetting(tt.key, "5"); err != nil {
				t.Fatal(err)
			}
			spend(t, s, now.Add(-time.Minute), 6)
			g := &budget.Guard{S: s}
			if denial, err := g.Check(now, ""); err != nil || denial == nil {
				t.Fatalf("initial window denial=%+v err=%v", denial, err)
			}
			// No ledger mutation/revision change occurs between checks. Only the
			// calendar cutoff changes, so a stale key would incorrectly deny.
			if denial, err := g.Check(tt.advance(now), ""); err != nil || denial != nil {
				t.Fatalf("new %s reused stale cached cutoff: denial=%+v err=%v", tt.name, denial, err)
			}
		})
	}
}
