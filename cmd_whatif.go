package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/burnban/burnban/internal/optimize"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
	"github.com/burnban/burnban/internal/whatif"
)

type whatifRow struct {
	model   string
	cost    float64
	quality *store.QualitySummary
}

type whatifQualityConstraint struct {
	source      string
	metric      string
	cohort      string
	minimumPPM  int64
	minSamples  int64
	minCoverage float64
}

// cmdWhatif reprices the window's traffic onto other models: same token
// counts, each candidate's rates and cache economics. It answers the
// question every bill triggers — "what would this have cost on X?" — from
// your own ledger instead of a pricing page.
func cmdWhatif(args []string) error {
	fs := flag.NewFlagSet("whatif", flag.ExitOnError)
	since := fs.String("since", "7d", `window: "today", "24h", "7d", or any Go duration`)
	model := fs.String("model", "", "compare against one model only")
	qualitySource := fs.String("quality-source", "", "external quality-score source (requires all quality flags)")
	qualityMetric := fs.String("quality-metric", "", "external higher-is-better metric")
	qualityCohort := fs.String("quality-cohort", "", "external evaluation cohort")
	minimumQuality := fs.String("min-quality", "", "minimum external score, exact decimal 0-1")
	qualityMinSamples := fs.Int64("quality-min-samples", 10, "minimum scored cases per candidate")
	qualityMinCoverage := fs.Float64("quality-min-coverage", 0.80, "minimum fraction of scored cohort cases per candidate (0.5-1)")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	qualityPassed := false
	fs.Visit(func(flag *flag.Flag) {
		if strings.HasPrefix(flag.Name, "quality-") || flag.Name == "min-quality" {
			qualityPassed = true
		}
	})
	qualityConstraint, err := parseWhatifQualityConstraint(qualityPassed, *qualitySource, *qualityMetric, *qualityCohort,
		*minimumQuality, *qualityMinSamples, *qualityMinCoverage)
	if err != nil {
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
	tot := whatif.Totals(requests)
	if tot.Requests == 0 {
		fmt.Print(noPricedTrafficMessage(label, tot))
		return nil
	}
	prices, err := pricing.Load()
	if err != nil {
		return err
	}

	priced, ok := whatif.Rows(prices, requests, *model)
	if !ok {
		return fmt.Errorf("no pricing for %q — add it to ~/.burnban/pricing.json", *model)
	}
	rows := make([]whatifRow, 0, len(priced))
	for _, row := range priced {
		rows = append(rows, whatifRow{model: row.Model, cost: row.CostUSD})
	}
	qualityExcluded := 0
	if qualityConstraint != nil {
		through := time.Now().UTC().Add(time.Nanosecond)
		if through.Sub(from) > 366*24*time.Hour {
			return fmt.Errorf("quality-constrained what-if is bounded to 366 days; narrow --since")
		}
		models := make([]string, len(rows))
		for i := range rows {
			models[i] = rows[i].model
		}
		summaries, err := s.QualitySummaries(from, through, qualityConstraint.source, qualityConstraint.metric, qualityConstraint.cohort, models)
		if err != nil {
			return err
		}
		rows, qualityExcluded = filterWhatifQuality(rows, summaries, *qualityConstraint)
		if *model != "" && qualityExcluded != 0 {
			return fmt.Errorf("model %q lacks sufficient supplied external quality evidence", *model)
		}
		if len(rows) == 0 {
			return fmt.Errorf("no candidate model has supplied external score coverage meeting --min-quality=%s, --quality-min-samples=%d, and --quality-min-coverage=%.0f%%",
				optimize.FormatScorePPM(qualityConstraint.minimumPPM), qualityConstraint.minSamples, qualityConstraint.minCoverage*100)
		}
	}
	// Name tiebreak keeps equal-priced models in a stable order run to run.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].cost != rows[j].cost {
			return rows[i].cost < rows[j].cost
		}
		return rows[i].model < rows[j].model
	})

	fmt.Printf("BURNBAN WHAT-IF — %s, same tokens on one model\n\n", label)
	fmt.Printf("  your actual mix   $%.2f  (%d requests)\n", tot.CostUSD, tot.Requests)
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', tabwriter.AlignRight)
	if qualityConstraint == nil {
		fmt.Fprintln(w, "model\twould cost\tvs actual\t")
	} else {
		fmt.Fprintln(w, "model\twould cost\tvs actual\texternal score\tsamples\tcoverage\t")
	}
	for _, row := range rows {
		if row.quality == nil {
			fmt.Fprintf(w, "%s\t$%.2f\t%s\t\n", terminalText(row.model, 100), row.cost, deltaPct(row.cost, tot.CostUSD))
		} else {
			fmt.Fprintf(w, "%s\t$%.2f\t%s\t%s\t%d\t%.0f%%\t\n", terminalText(row.model, 100), row.cost,
				deltaPct(row.cost, tot.CostUSD), optimize.FormatScorePPM(row.quality.AverageScorePPM),
				row.quality.Samples, row.quality.Coverage*100)
		}
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
	if qualityConstraint != nil {
		fmt.Printf("  quality constraint uses externally supplied %s/%s scores for cohort %s; %d priced candidate(s) without sufficient score, sample, or cohort coverage were excluded.\n",
			terminalText(qualityConstraint.source, 64), terminalText(qualityConstraint.metric, 64), terminalText(qualityConstraint.cohort, 128), qualityExcluded)
		fmt.Println("  Burnban does not infer target quality; cohort comparability and score validity remain the external evaluator's responsibility.")
	}
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

func parseWhatifQualityConstraint(enabled bool, source, metric, cohort, minimum string, minSamples int64, minCoverage float64) (*whatifQualityConstraint, error) {
	if !enabled {
		return nil, nil
	}
	if source == "" || metric == "" || cohort == "" || minimum == "" {
		return nil, fmt.Errorf("quality-constrained what-if requires --quality-source, --quality-metric, --quality-cohort, and --min-quality together")
	}
	minimumPPM, err := optimize.ParseScorePPM(minimum)
	if err != nil {
		return nil, fmt.Errorf("--min-quality: %w", err)
	}
	if minSamples < 1 || minSamples > store.MaxQualityBatch {
		return nil, fmt.Errorf("--quality-min-samples must be between 1 and %d", store.MaxQualityBatch)
	}
	if math.IsNaN(minCoverage) || math.IsInf(minCoverage, 0) || minCoverage < .5 || minCoverage > 1 {
		return nil, fmt.Errorf("--quality-min-coverage must be between 0.5 and 1")
	}
	return &whatifQualityConstraint{
		source: source, metric: metric, cohort: cohort, minimumPPM: minimumPPM,
		minSamples: minSamples, minCoverage: minCoverage,
	}, nil
}

func filterWhatifQuality(rows []whatifRow, summaries map[string]store.QualitySummary, constraint whatifQualityConstraint) ([]whatifRow, int) {
	filtered := make([]whatifRow, 0, len(rows))
	excluded := 0
	for _, row := range rows {
		summary, ok := summaries[row.model]
		invalidCoverage := math.IsNaN(summary.Coverage) || math.IsInf(summary.Coverage, 0) || summary.Coverage < 0 || summary.Coverage > 1
		if !ok || summary.Samples < constraint.minSamples || invalidCoverage || summary.Coverage < constraint.minCoverage ||
			summary.AverageScorePPM < constraint.minimumPPM || summary.AverageScorePPM > 1_000_000 {
			excluded++
			continue
		}
		row.quality = &summary
		filtered = append(filtered, row)
	}
	return filtered, excluded
}

func noPricedTrafficMessage(label string, totals *store.Totals) string {
	var message strings.Builder
	fmt.Fprintf(&message, "no priced traffic in %s — nothing to reprice\n", label)
	if totals.Unpriced > 0 {
		fmt.Fprintf(&message, "%d unknown-price request(s) were excluded.\n", totals.Unpriced)
	}
	if totals.Unmetered > 0 {
		fmt.Fprintf(&message, "%d unmetered response(s) had no usable token accounting.\n", totals.Unmetered)
	}
	if totals.Incomplete > 0 {
		fmt.Fprintf(&message, "%d response(s) were partial or cancelled.\n", totals.Incomplete)
	}
	if totals.FeeUnpriced > 0 {
		fmt.Fprintf(&message, "%d call(s) had unpriced provider-hosted fee dimensions.\n", totals.FeeUnpriced)
	}
	return message.String()
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
