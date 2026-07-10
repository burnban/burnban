package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/syft8/burnban/internal/store"
)

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	since := fs.String("since", "today", `window: "today", "24h", "7d", or any Go duration`)
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
	sum, err := s.Summarize(from)
	if err != nil {
		return err
	}
	lastHour, err := s.SpentSince(time.Now().Add(-time.Hour))
	if err != nil {
		return err
	}

	fmt.Printf("BURNBAN REPORT — %s\n\n", label)
	fmt.Printf("total  $%.4f · %d requests · cache hit %s\n", sum.Cost, sum.Requests, cachePct(sum.CacheRead, sum.In))
	fmt.Printf("rate   $%.4f in the last hour\n", lastHour)
	if sum.Estimated > 0 {
		fmt.Printf("note   %d responses had no usage frame; output tokens were estimated (enable stream_options.include_usage)\n", sum.Estimated)
	}
	if sum.Unpriced > 0 {
		fmt.Printf("note   %d requests used models with unknown pricing (recorded at $0) — add them to ~/.burnban/pricing.json\n", sum.Unpriced)
	}

	if len(sum.Models) > 0 {
		fmt.Println("\nBY MODEL")
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "model\treq\tin\tout\tcache-r\tcache-w\tcost")
		for _, m := range sum.Models {
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t$%.4f\n",
				m.Model, m.Requests, fmtTok(m.In), fmtTok(m.Out), fmtTok(m.CacheRead), fmtTok(m.CacheWrite), m.Cost)
		}
		w.Flush()
	}

	if len(sum.Agents) > 0 {
		fmt.Println("\nBY AGENT")
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "agent\treq\tcost")
		for _, a := range sum.Agents {
			fmt.Fprintf(w, "%s\t%d\t$%.4f\n", a.Agent, a.Requests, a.Cost)
		}
		w.Flush()
	}

	fmt.Println("\nWASTE RECEIPTS")
	receipts := 0
	if sum.DupGroups > 0 {
		fmt.Printf("· %d duplicate request group(s) — $%.4f burned on identical calls\n", sum.DupGroups, sum.DupWastedUSD)
		receipts++
	}
	if rate, ok := cacheRate(sum.CacheRead, sum.In); ok && rate < 0.5 && sum.Requests >= 20 {
		fmt.Printf("· cache hit rate is %.0f%% across %d requests — you are paying full price for context the provider would re-serve at a 90%% discount\n", rate*100, sum.Requests)
		receipts++
	}
	if receipts == 0 {
		fmt.Println("· none — clean burn 🔥")
	}
	return nil
}

func parseSince(s string) (time.Time, string, error) {
	now := time.Now()
	if s == "today" {
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		return midnight, "today (" + now.Format("2006-01-02") + ")", nil
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && strings.HasSuffix(s, "d") && n > 0 {
		if n == 1 {
			return now.Add(-24 * time.Hour), "last 1 day", nil
		}
		return now.Add(-time.Duration(n) * 24 * time.Hour), fmt.Sprintf("last %d days", n), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("bad --since %q: use today, 24h, 7d, 30d, or a duration like 90m", s)
	}
	return now.Add(-d), "last " + s, nil
}

func cacheRate(read, in int64) (float64, bool) {
	total := read + in
	if total == 0 {
		return 0, false
	}
	return float64(read) / float64(total), true
}

func cachePct(read, in int64) string {
	r, ok := cacheRate(read, in)
	if !ok {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", r*100)
}

func fmtTok(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
