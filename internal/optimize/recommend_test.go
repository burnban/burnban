package optimize

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestAnalyzeCacheContentFreeReceiptDoesNotInferPrefixInstability(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]store.OptimizationRow, 12)
	for i := range rows {
		rows[i] = store.OptimizationRow{
			Ts: from.Add(time.Duration(i) * time.Hour), Provider: "anthropic", Model: "claude",
			Project: "docs", Route: "/v1/messages", InTokens: 40_000, CacheReadTokens: 1_000,
		}
	}
	report, err := AnalyzeCache(store.OptimizationSample{Rows: rows}, from, from.Add(24*time.Hour), DefaultCacheOptions())
	if err != nil {
		t.Fatal(err)
	}
	if report.PromptContentStored || len(report.Receipts) != 1 {
		t.Fatalf("report = %+v", report)
	}
	receipt := report.Receipts[0]
	if receipt.PrefixStability != "unobserved" || receipt.Confidence != "high" || receipt.Pattern != "repeated_large_context_with_low_cache_reuse" {
		t.Fatalf("receipt = %+v", receipt)
	}
	joined := strings.Join(report.Limitations, " ")
	if !strings.Contains(joined, "no prompt bodies") || !strings.Contains(joined, "does not prove") {
		t.Fatalf("limitations omit privacy/inference boundary: %q", joined)
	}
}

func TestAnalyzeCacheRejectsTokenOverflow(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	_, err := AnalyzeCache(store.OptimizationSample{Rows: []store.OptimizationRow{{
		Ts: from, InTokens: math.MaxInt64, CacheReadTokens: 1,
	}}}, from, from.Add(time.Hour), DefaultCacheOptions())
	if err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("overflow err = %v", err)
	}
}

func TestAnalyzeCacheMarksTruncatedEvidenceLowConfidence(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]store.OptimizationRow, 10)
	for i := range rows {
		rows[i] = store.OptimizationRow{Ts: from.Add(time.Duration(i) * time.Minute), Agent: "a", InTokens: 50_000}
	}
	report, err := AnalyzeCache(store.OptimizationSample{Rows: rows, Truncated: true}, from, from.Add(time.Hour), DefaultCacheOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 1 || report.Receipts[0].Confidence != "low" || !strings.Contains(strings.Join(report.Limitations, " "), "row limit") {
		t.Fatalf("report = %+v", report)
	}
}

func TestRecommendAllocationsProposesOnlyAndSimulatesBlockedCalls(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	through := from.Add(10 * 24 * time.Hour)
	var rows []store.OptimizationRow
	for day := 0; day < 10; day++ {
		cost := 1.0
		if day == 0 {
			cost = 100
		}
		rows = append(rows, store.OptimizationRow{
			Ts: from.Add(time.Duration(day)*24*time.Hour + time.Hour), Agent: "worker", CostUSD: cost,
			PricingState: store.PricingPriced,
		})
	}
	options := DefaultAllocationOptions("agent", 10)
	options.HeadroomPercent = 0
	report, err := RecommendAllocations(store.OptimizationSample{Rows: rows}, from, through, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recommendations) != 1 {
		t.Fatalf("report = %+v", report)
	}
	recommendation := report.Recommendations[0]
	if recommendation.ProposedDailyBudgetMicros != 1_000_000 || recommendation.SimulatedBlockedCalls != 1 || recommendation.SimulatedBlockedSpendMicros != 100_000_000 {
		t.Fatalf("recommendation = %+v", recommendation)
	}
	if recommendation.ApplyCommand == "" || !strings.Contains(recommendation.OperatorAction, "only if") {
		t.Fatalf("proposal lacks explicit operator gate: %+v", recommendation)
	}
}

func TestRecommendAllocationsShellQuotesUntrustedScope(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	through := from.Add(7 * 24 * time.Hour)
	scope := `worker'$(touch /tmp/should-not-run)`
	report, err := RecommendAllocations(store.OptimizationSample{Rows: []store.OptimizationRow{{
		Ts: from.Add(time.Hour), Agent: scope, CostUSD: 1, PricingState: store.PricingPriced,
	}}}, from, through, DefaultAllocationOptions("agent", 7))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recommendations) != 1 {
		t.Fatalf("report = %+v", report)
	}
	want := `--agent 'worker'"'"'$(touch /tmp/should-not-run)'`
	if !strings.Contains(report.Recommendations[0].ApplyCommand, want) {
		t.Fatalf("unsafe or unexpected command quoting: %q", report.Recommendations[0].ApplyCommand)
	}
}

func TestRecommendAllocationsRefusesIncompleteSimulation(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	through := from.Add(7 * 24 * time.Hour)
	report, err := RecommendAllocations(store.OptimizationSample{Truncated: true}, from, through, DefaultAllocationOptions("project", 7))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recommendations) != 0 || !strings.Contains(strings.Join(report.Limitations, " "), "No allocation is proposed") {
		t.Fatalf("report = %+v", report)
	}
}

func TestRecommendFleetAllocationsRequireAuthenticatedAttribution(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	through := from.Add(7 * 24 * time.Hour)
	rows := []store.OptimizationRow{
		{Ts: from.Add(time.Hour), Meter: "meter-a", Team: "platform", IdentityConfidence: "authenticated", CostUSD: 2, PricingState: store.PricingPriced},
		{Ts: from.Add(2 * time.Hour), Meter: "spoofed-meter", Team: "spoofed-team", IdentityConfidence: "self_reported", CostUSD: 100, PricingState: store.PricingPriced},
		{Ts: from.Add(3 * time.Hour), Meter: "unverified-meter", Team: "unverified-team", IdentityConfidence: "unverified", CostUSD: 100, PricingState: store.PricingPriced},
	}
	for _, dimension := range []string{"meter", "team"} {
		report, err := RecommendAllocations(store.OptimizationSample{Rows: rows}, from, through,
			DefaultAllocationOptions(dimension, 7))
		if err != nil {
			t.Fatalf("%s: %v", dimension, err)
		}
		if len(report.Recommendations) != 1 || report.ExcludedUntrustedRows != 2 ||
			report.Recommendations[0].ProposedWeight != 1 {
			t.Fatalf("%s report = %+v", dimension, report)
		}
		want := "meter-a"
		if dimension == "team" {
			want = "platform"
		}
		if report.Recommendations[0].Scope != want ||
			!strings.Contains(strings.Join(report.Limitations, " "), "server-authorized") {
			t.Fatalf("%s report = %+v", dimension, report)
		}
	}
}

func TestRecommendAllocationsRejectsCostOverflowAndNonfinite(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	through := from.Add(7 * 24 * time.Hour)
	for _, cost := range []float64{math.NaN(), math.Inf(1), -1, float64(math.MaxInt64)} {
		_, err := RecommendAllocations(store.OptimizationSample{Rows: []store.OptimizationRow{{
			Ts: from, Agent: "a", CostUSD: cost, PricingState: store.PricingPriced,
		}}}, from, through, DefaultAllocationOptions("agent", 7))
		if err == nil {
			t.Errorf("cost %v accepted", cost)
		}
	}
}

func TestFixedPointHelpersRejectBoundaryOverflow(t *testing.T) {
	if _, err := dollarsToMicros(math.Nextafter(float64(math.MaxInt64)/1e6, math.Inf(1))); err == nil {
		t.Fatal("float64 2^63 conversion boundary accepted")
	}
	if _, err := multiplyDivideCeil(math.MaxInt64, 300, 100); err == nil {
		t.Fatal("fixed-point multiplication overflow accepted")
	}
	if got, err := multiplyDivideCeil(101, 120, 100); err != nil || got != 122 {
		t.Fatalf("ceil multiplication = %d, %v; want 122", got, err)
	}
}
