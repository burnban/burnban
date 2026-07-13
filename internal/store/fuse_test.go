package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBudgetUsageWindowsUsesExactBoundariesAndCounts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fuse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	start := time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC)
	for _, row := range []Request{
		{Ts: start.Add(-time.Second), CostUSD: 50, PricingState: PricingPriced},
		{Ts: start, CostUSD: 1, PricingState: PricingPriced},
		{Ts: start.Add(30 * time.Minute), CostUSD: 2, PricingState: PricingPriced, EnforcementUnsafe: true},
		{Ts: start.Add(time.Hour), CostUSD: 100, PricingState: PricingPriced},
		{Ts: start.Add(-24 * time.Hour), CostUSD: 4, PricingState: PricingPriced},
	} {
		if err := s.Insert(row); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.BudgetUsageWindows([]time.Time{start, start.Add(-24 * time.Hour)}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SpentUSD != 3 || got[0].Requests != 2 || got[0].EnforcementGaps != 1 ||
		got[1].SpentUSD != 4 || got[1].Requests != 1 || got[1].EnforcementGaps != 0 {
		t.Fatalf("window usage = %+v", got)
	}
}

func TestBudgetUsageWindowsRejectsUnboundedInputs(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fuse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.BudgetUsageWindows([]time.Time{{}}, time.Hour); err == nil {
		t.Fatal("zero start accepted")
	}
	starts := make([]time.Time, 91)
	for i := range starts {
		starts[i] = time.Now().AddDate(0, 0, -i)
	}
	if _, err := s.BudgetUsageWindows(starts, time.Hour); err == nil {
		t.Fatal("unbounded window list accepted")
	}
}
