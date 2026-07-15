// Package whatif reprices a window's recorded traffic onto other models:
// same token counts, each candidate's rates and cache economics. The CLI
// `burnban whatif` and the dashboard share this math so both surfaces answer
// "what would this have cost on X?" identically.
package whatif

import (
	"sort"
	"strings"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

// Row is one candidate model's repriced total for the window.
type Row struct {
	Model   string  `json:"model"`
	CostUSD float64 `json:"cost_usd"`
}

// Reprice totals what the window's priced requests would cost on one model.
func Reprice(model string, p pricing.Price, requests []store.TokenRow) float64 {
	var total float64
	claude := strings.HasPrefix(strings.ToLower(model), "claude")
	for _, request := range requests {
		if request.PricingState != store.PricingPriced {
			continue
		}
		cost := pricing.RepriceRequest(p, request.In, request.Out, request.CacheRead, request.CacheWrite)
		oneHour := min(max(request.CacheWrite1h, 0), max(request.CacheWrite, 0))
		if oneHour > 0 && claude {
			// Anthropic's 1-hour cache tier is 2x input. RepriceRequest has
			// already applied the ordinary cache-write multiplier to this subset.
			writeMult := p.CacheWriteMult
			if writeMult <= 0 {
				writeMult = 1
			}
			cost += float64(oneHour) * p.InputPerMTok * (2 - writeMult) / 1e6
		}
		total += max(0, cost)
	}
	return max(0, total)
}

// Rows reprices the window onto every priced model (or one, when only is
// set), cheapest first with a stable name tiebreak.
func Rows(prices *pricing.Table, requests []store.TokenRow, only string) ([]Row, bool) {
	var rows []Row
	if only != "" {
		p, ok := prices.Lookup(only)
		if !ok {
			return nil, false
		}
		rows = append(rows, Row{Model: only, CostUSD: Reprice(only, p, requests)})
	} else {
		for name, p := range prices.Models {
			rows = append(rows, Row{Model: name, CostUSD: Reprice(name, p, requests)})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CostUSD != rows[j].CostUSD {
			return rows[i].CostUSD < rows[j].CostUSD
		}
		return rows[i].Model < rows[j].Model
	})
	return rows, true
}

// Totals aggregates the window's raw token rows into the priced totals and
// exclusion counts every consumer reports alongside a repricing table.
func Totals(requests []store.TokenRow) *store.Totals {
	totals := &store.Totals{}
	for _, request := range requests {
		if request.Incomplete {
			totals.Incomplete++
		}
		if request.FeeUnpriced {
			totals.FeeUnpriced++
		}
		switch request.PricingState {
		case store.PricingPriced:
			totals.Requests++
			totals.In += request.In
			totals.Out += request.Out
			totals.CacheRead += request.CacheRead
			totals.CacheWrite += request.CacheWrite
			totals.CacheWrite1h += request.CacheWrite1h
			totals.CostUSD += request.CostUSD
		case store.PricingUnknown:
			totals.Unpriced++
		case store.PricingUnmetered:
			totals.Unmetered++
		}
	}
	return totals
}
