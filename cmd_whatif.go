package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

// cmdWhatif reprices the window's traffic onto other models: same token
// counts, each candidate's rates and cache economics. It answers the
// question every bill triggers — "what would this have cost on X?" — from
// your own ledger instead of a pricing page.
func cmdWhatif(args []string) error {
	fs := flag.NewFlagSet("whatif", flag.ExitOnError)
	since := fs.String("since", "7d", `window: "today", "24h", "7d", or any Go duration`)
	model := fs.String("model", "", "compare against one model only")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	from, label, err := parseSince(*since)
	if err != nil {
		return err
	}
	requests, err := s.TokenRows(from)
	if err != nil {
		return err
	}
	tot := tokenTotalsFromRows(requests)
	if tot.Requests == 0 {
		fmt.Printf("no priced traffic in %s — nothing to reprice\n", label)
		return nil
	}
	prices, err := pricing.Load()
	if err != nil {
		return err
	}

	type row struct {
		model string
		cost  float64
	}
	var rows []row
	if *model != "" {
		p, ok := prices.Lookup(*model)
		if !ok {
			return fmt.Errorf("no pricing for %q — add it to ~/.burnban/pricing.json", *model)
		}
		rows = append(rows, row{*model, repriceRequests(*model, p, requests)})
	} else {
		for name, p := range prices.Models {
			rows = append(rows, row{name, repriceRequests(name, p, requests)})
		}
		// Name tiebreak keeps equal-priced models in a stable order run to run.
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].cost != rows[j].cost {
				return rows[i].cost < rows[j].cost
			}
			return rows[i].model < rows[j].model
		})
	}

	fmt.Printf("BURNBAN WHAT-IF — %s, same tokens on one model\n\n", label)
	fmt.Printf("  your actual mix   $%.2f  (%d requests)\n", tot.CostUSD, tot.Requests)
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "model\twould cost\tvs actual\t")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t$%.2f\t%s\t\n", terminalText(r.model, 100), r.cost, deltaPct(r.cost, tot.CostUSD))
	}
	w.Flush()

	if *model == "" && len(rows) > 1 && tot.CostUSD > 0 {
		best := rows[0]
		if best.cost < tot.CostUSD {
			fmt.Printf("\n  cheapest: %s would have cut $%.2f (%s)\n", terminalText(best.model, 100), tot.CostUSD-best.cost, deltaPct(best.cost, tot.CostUSD))
		}
	}
	fmt.Println("\n  each request is repriced separately, including target long-context tiers;")
	fmt.Println("  tokenizers and model verbosity differ, so this is an estimate, not a quote.")
	fmt.Println("  standard target rates are used; service-tier, geography, and hosted-tool fees are not projected.")
	if tot.Unpriced > 0 {
		fmt.Printf("  %d unpriced request(s) excluded (unknown models, recorded at $0).\n", tot.Unpriced)
	}
	if tot.Unmetered > 0 {
		fmt.Printf("  %d unmetered response(s) excluded because no usable token accounting was available.\n", tot.Unmetered)
	}
	if tot.Incomplete > 0 {
		fmt.Printf("  %d partial/cancelled response(s) are represented by lower-bound token estimates.\n", tot.Incomplete)
	}
	if tot.FeeUnpriced > 0 {
		fmt.Printf("  %d call(s) had provider-hosted fee dimensions excluded from this token-only comparison.\n", tot.FeeUnpriced)
	}
	return nil
}

func repriceRequests(model string, p pricing.Price, requests []store.TokenRow) float64 {
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

func tokenTotalsFromRows(requests []store.TokenRow) *store.Totals {
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

func deltaPct(cost, actual float64) string {
	if actual <= 0 {
		return "–"
	}
	d := (cost - actual) / actual * 100
	switch {
	case d < 0:
		return fmt.Sprintf("−%.0f%%", -d)
	case d > 0:
		return fmt.Sprintf("+%.0f%%", d)
	default:
		return "±0%"
	}
}
