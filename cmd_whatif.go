package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
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

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	from, label, err := parseSince(*since)
	if err != nil {
		return err
	}
	tot, err := s.TokenTotals(from)
	if err != nil {
		return err
	}
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
		rows = append(rows, row{*model, pricing.Reprice(p, tot.In, tot.Out, tot.CacheRead, tot.CacheWrite)})
	} else {
		for name, p := range prices.Models {
			rows = append(rows, row{name, pricing.Reprice(p, tot.In, tot.Out, tot.CacheRead, tot.CacheWrite)})
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
		fmt.Fprintf(w, "%s\t$%.2f\t%s\t\n", r.model, r.cost, deltaPct(r.cost, tot.CostUSD))
	}
	w.Flush()

	if *model == "" && len(rows) > 1 && tot.CostUSD > 0 {
		best := rows[0]
		if best.cost < tot.CostUSD {
			fmt.Printf("\n  cheapest: %s would have cut $%.2f (%s)\n", best.model, tot.CostUSD-best.cost, deltaPct(best.cost, tot.CostUSD))
		}
	}
	fmt.Println("\n  same token counts assumed — tokenizers and model verbosity differ,")
	fmt.Println("  so read this as a floor estimate, not a quote.")
	if tot.Unpriced > 0 {
		fmt.Printf("  %d unpriced request(s) excluded (unknown models, recorded at $0).\n", tot.Unpriced)
	}
	return nil
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
