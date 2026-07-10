package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/syft8/burnban/internal/pricing"
	"github.com/syft8/burnban/internal/subsidy"
)

// cmdSubsidy prices subscription traffic that never touches the proxy.
// Claude Code on Pro/Max and Codex on ChatGPT plans bill nothing per token,
// so there is no spend to meter — but both write session logs, and those
// logs carry real token counts. Pricing them at API rates shows what the
// flat plan is actually worth (and when a fleet would outgrow it).
func cmdSubsidy(args []string) error {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("subsidy", flag.ExitOnError)
	since := fs.String("since", "30d", `window: "today", "24h", "7d", "30d", or any Go duration`)
	claudeDir := fs.String("claude-dir", filepath.Join(home, ".claude", "projects"), "Claude Code session logs")
	codexDir := fs.String("codex-dir", filepath.Join(home, ".codex", "sessions"), "Codex rollout logs")
	planCost := fs.Float64("plan-cost", 0, "what you pay per month across plans, for a single subsidy multiple")
	daily := fs.Bool("daily", false, "per-day breakdown")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Parse(args)

	from, label, err := parseSince(*since)
	if err != nil {
		return err
	}
	prices, err := pricing.Load()
	if err != nil {
		return err
	}
	type priced struct {
		p  pricing.Price
		ok bool
	}
	lookup := map[string]priced{} // memoized: log-derived model IDs repeat millions of times
	price := func(model string) (pricing.Price, bool) {
		if c, ok := lookup[model]; ok {
			return c.p, c.ok
		}
		p, ok := prices.Lookup(model)
		lookup[model] = priced{p, ok}
		return p, ok
	}

	type totals struct {
		calls, in, out, cacheRead, w5m, w1h int64
		usd                                 float64
	}
	type prov struct {
		name, dir string
		sessions  int
		models    map[string]*totals
		days      map[string]*totals
	}
	provs := []*prov{
		{name: "claude-code", dir: *claudeDir},
		{name: "codex", dir: *codexDir},
	}
	scanners := map[string]func(string, time.Time, func(subsidy.Event)) (int, error){
		"claude-code": subsidy.ScanClaude,
		"codex":       subsidy.ScanCodex,
	}
	var unpricedTokens int64
	for _, p := range provs {
		p.models = map[string]*totals{}
		p.days = map[string]*totals{}
		bump := func(m map[string]*totals, key string, e subsidy.Event, usd float64) {
			t := m[key]
			if t == nil {
				t = &totals{}
				m[key] = t
			}
			t.calls++
			t.in += e.In
			t.out += e.Out
			t.cacheRead += e.CacheRead
			t.w5m += e.CacheWrite5m
			t.w1h += e.CacheWrite1h
			t.usd += usd
		}
		p.sessions, err = scanners[p.name](p.dir, from, func(e subsidy.Event) {
			var usd float64
			if pr, ok := price(e.Model); ok {
				usd = subsidy.Cost(pr, e.In, e.Out, e.CacheRead, e.CacheWrite5m, e.CacheWrite1h)
			} else {
				unpricedTokens += e.In + e.Out + e.CacheRead + e.CacheWrite5m + e.CacheWrite1h
			}
			bump(p.models, e.Model, e, usd)
			bump(p.days, e.Time.Local().Format("2006-01-02"), e, usd)
		})
		if err != nil {
			return err
		}
	}

	windowDays := time.Since(from).Hours() / 24
	if windowDays <= 0 {
		windowDays = 1
	}
	pace := func(usd float64) float64 { return usd / windowDays * 30 }
	round := func(v float64) float64 { return math.Round(v*100) / 100 }

	// Sticker prices, July 2026, monthly billing (Team is per seat). These
	// are display-only comparisons — override with --plan-cost.
	type plan struct {
		name string
		usd  float64
	}
	providerPlans := map[string][]plan{
		"claude-code": {{"Claude Pro", 20}, {"Claude Max 5x", 100}, {"Claude Max 20x", 200}},
		"codex":       {{"ChatGPT Plus", 20}, {"ChatGPT Team", 30}, {"ChatGPT Pro", 200}},
	}

	type modelRow struct {
		Model        string  `json:"model"`
		Calls        int64   `json:"calls"`
		In           int64   `json:"in"`
		Out          int64   `json:"out"`
		CacheRead    int64   `json:"cache_read"`
		CacheWrite5m int64   `json:"cache_write_5m,omitempty"`
		CacheWrite1h int64   `json:"cache_write_1h,omitempty"`
		APIUSD       float64 `json:"api_usd"`
		Priced       bool    `json:"priced"`
	}
	type dayRow struct {
		Day    string  `json:"day"`
		Calls  int64   `json:"calls"`
		APIUSD float64 `json:"api_usd"`
	}
	type planRow struct {
		Name       string  `json:"name"`
		MonthlyUSD float64 `json:"monthly_usd"`
		Multiple   float64 `json:"multiple"`
	}
	type provOut struct {
		Provider       string     `json:"provider"`
		Dir            string     `json:"dir"`
		Sessions       int        `json:"sessions"`
		Models         []modelRow `json:"models"`
		Days           []dayRow   `json:"days,omitempty"`
		APIUSD         float64    `json:"api_usd"`
		MonthlyPaceUSD float64    `json:"monthly_pace_usd"`
		Plans          []planRow  `json:"plans,omitempty"`
	}

	var out []provOut
	var grand float64
	for _, p := range provs {
		po := provOut{Provider: p.name, Dir: p.dir, Sessions: p.sessions}
		for model, t := range p.models {
			_, ok := price(model)
			po.Models = append(po.Models, modelRow{Model: model, Calls: t.calls,
				In: t.in, Out: t.out, CacheRead: t.cacheRead,
				CacheWrite5m: t.w5m, CacheWrite1h: t.w1h,
				APIUSD: round(t.usd), Priced: ok})
			po.APIUSD += t.usd
		}
		sort.Slice(po.Models, func(i, j int) bool {
			if po.Models[i].APIUSD != po.Models[j].APIUSD {
				return po.Models[i].APIUSD > po.Models[j].APIUSD
			}
			return po.Models[i].Model < po.Models[j].Model
		})
		for day, t := range p.days {
			po.Days = append(po.Days, dayRow{Day: day, Calls: t.calls, APIUSD: round(t.usd)})
		}
		sort.Slice(po.Days, func(i, j int) bool { return po.Days[i].Day < po.Days[j].Day })
		po.APIUSD = round(po.APIUSD)
		po.MonthlyPaceUSD = round(pace(po.APIUSD))
		if po.APIUSD > 0 {
			for _, pl := range providerPlans[p.name] {
				po.Plans = append(po.Plans, planRow{pl.name, pl.usd, math.Round(po.MonthlyPaceUSD/pl.usd*10) / 10})
			}
		}
		grand += po.APIUSD
		out = append(out, po)
	}
	grand = round(grand)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"since": from.Format(time.RFC3339), "until": time.Now().Format(time.RFC3339),
			"providers": out, "api_usd_total": grand, "unpriced_tokens": unpricedTokens,
		})
	}

	if out[0].Sessions == 0 && out[1].Sessions == 0 {
		fmt.Printf("no local agent logs in %s — looked in:\n  %s\n  %s\n", label, *claudeDir, *codexDir)
		return nil
	}

	fmt.Printf("BURNBAN SUBSIDY — %s of subscription traffic at API prices\n\n", label)
	fmt.Println("Claude Code and Codex ran on flat-rate plans here; none of this was")
	fmt.Println("billed per token. Same tokens, priced at what API keys would pay:")
	for _, po := range out {
		if po.Sessions == 0 {
			continue
		}
		fmt.Printf("\n%s  %s · %d sessions\n", subsidyTitle(po.Provider), po.Dir, po.Sessions)
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  model\tcalls\tin\tout\tcache-r\tcache-w\tAPI price")
		for _, r := range po.Models {
			cost := fmt.Sprintf("$%.2f", r.APIUSD)
			if !r.Priced {
				cost = "unpriced"
			}
			fmt.Fprintf(w, "  %s\t%d\t%s\t%s\t%s\t%s\t%s\n", r.Model, r.Calls,
				fmtTok(r.In), fmtTok(r.Out), fmtTok(r.CacheRead), fmtTok(r.CacheWrite5m+r.CacheWrite1h), cost)
		}
		fmt.Fprintf(w, "  subtotal\t\t\t\t\t\t$%.2f\n", po.APIUSD)
		w.Flush()
		if *daily {
			dw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(dw, "\n  day\tcalls\tAPI price")
			for _, d := range po.Days {
				fmt.Fprintf(dw, "  %s\t%d\t$%.2f\n", d.Day, d.Calls, d.APIUSD)
			}
			dw.Flush()
		}
	}

	fmt.Printf("\nTOTAL  $%.2f at API prices", grand)
	if math.Abs(pace(grand)-grand) > 0.5 {
		fmt.Printf("  ·  ≈ $%.2f/mo pace", pace(grand))
	}
	fmt.Println()
	if *planCost > 0 {
		fmt.Printf("\n  vs your $%.0f/mo in plans → %.1fx the sticker price in API value\n", *planCost, pace(grand)/(*planCost))
	} else {
		for _, po := range out {
			if len(po.Plans) == 0 {
				continue
			}
			fmt.Printf("\n  %s pace ≈ $%.2f/mo vs", po.Provider, po.MonthlyPaceUSD)
			for i, pl := range po.Plans {
				if i > 0 {
					fmt.Print("  ·")
				}
				fmt.Printf("  %s $%.0f → %.1fx", pl.Name, pl.MonthlyUSD, pl.Multiple)
			}
			fmt.Println()
		}
	}
	if unpricedTokens > 0 {
		fmt.Printf("\n  %s tokens ran on models with no known price — counted above, priced at\n", fmtTok(unpricedTokens))
		fmt.Println("  nothing. Add them to ~/.burnban/pricing.json to include them.")
	}
	fmt.Println("\n  priced with burnban's own table (override: ~/.burnban/pricing.json).")
	fmt.Println("  running agents on API keys too? `burnban serve` meters those live.")
	return nil
}

func subsidyTitle(provider string) string {
	switch provider {
	case "claude-code":
		return "CLAUDE CODE"
	case "codex":
		return "CODEX"
	}
	return provider
}
