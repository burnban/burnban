package main

import (
	"math"
	"strings"
	"testing"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

func TestRepriceRequestsAppliesLongContextPerRequest(t *testing.T) {
	p := pricing.Price{
		InputPerMTok: 1, OutputPerMTok: 2,
		LongContextThreshold: 100, LongInputMult: 2, LongOutputMult: 1.5,
	}
	shortRows := []store.TokenRow{
		{In: 60, PricingState: store.PricingPriced},
		{In: 60, PricingState: store.PricingPriced},
		{In: 1000, PricingState: store.PricingUnknown},
	}
	if got, want := repriceRequests("target", p, shortRows), 120.0/1e6; math.Abs(got-want) > 1e-12 {
		t.Fatalf("separate short requests incorrectly triggered long tier: got %v want %v", got, want)
	}
	longRows := []store.TokenRow{{In: 120, Out: 10, PricingState: store.PricingPriced}}
	if got, want := repriceRequests("target", p, longRows), (120.0*2+10.0*2*1.5)/1e6; math.Abs(got-want) > 1e-12 {
		t.Fatalf("long request omitted tier: got %v want %v", got, want)
	}
}

func TestNoPricedTrafficMessageDisclosesEveryExclusion(t *testing.T) {
	message := noPricedTrafficMessage("today", &store.Totals{
		Unpriced: 2, Unmetered: 3, Incomplete: 4, FeeUnpriced: 5,
	})
	for _, want := range []string{"2 unknown-price", "3 unmetered", "4 response(s) were partial", "5 call(s) had unpriced"} {
		if !strings.Contains(message, want) {
			t.Errorf("message missing %q: %s", want, message)
		}
	}
}

func TestTokenTotalsFromRowsUsesSameSnapshotAsRepricing(t *testing.T) {
	rows := []store.TokenRow{
		{In: 10, CostUSD: 1.25, PricingState: store.PricingPriced, Incomplete: true},
		{In: 100, PricingState: store.PricingUnknown, FeeUnpriced: true},
		{PricingState: store.PricingUnmetered},
	}
	totals := tokenTotalsFromRows(rows)
	if totals.Requests != 1 || totals.In != 10 || totals.CostUSD != 1.25 || totals.Unpriced != 1 ||
		totals.Unmetered != 1 || totals.Incomplete != 1 || totals.FeeUnpriced != 1 {
		t.Fatalf("totals = %+v", totals)
	}
}
