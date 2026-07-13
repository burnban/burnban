package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/burnban/burnban/internal/optimize"
	"github.com/burnban/burnban/internal/store"
)

func cmdOptimize(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: burnban optimize <cache|allocation|quality-import> [flags]")
	}
	switch args[0] {
	case "cache":
		return cmdOptimizeCache(args[1:])
	case "allocation":
		return cmdOptimizeAllocation(args[1:])
	case "quality-import":
		return cmdOptimizeQualityImport(args[1:])
	default:
		return fmt.Errorf("unknown optimize command %q: use cache, allocation, or quality-import", args[0])
	}
}

func cmdOptimizeCache(args []string) error {
	fs := flag.NewFlagSet("optimize cache", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	since := fs.String("since", "30d", `window: "24h", "7d", "30d", or another duration up to 90d`)
	format := fs.String("format", "table", "table or json")
	maxRows := fs.Int("max-rows", 50_000, fmt.Sprintf("maximum ledger rows to inspect (1-%d)", store.MaxOptimizationRows))
	largeContext := fs.Int64("large-context", 32_000, "input+cache tokens that constitute a large context")
	lowReusePercent := fs.Float64("low-reuse", 20, "cache-read percentage below which evidence is flagged")
	minRepeats := fs.Int("min-repeats", 3, "minimum large-context calls in one metadata group")
	maxReceipts := fs.Int("max-receipts", 100, "maximum recommendation receipts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *format != "table" && *format != "json" {
		return fmt.Errorf("bad --format %q: use table or json", *format)
	}
	from, _, err := parseSince(*since)
	if err != nil {
		return err
	}
	from = from.UTC().Truncate(time.Second)
	through := time.Now().UTC().Truncate(time.Second)
	if !through.After(from) {
		return errors.New("--since must span at least one complete second")
	}
	if span := through.Sub(from); span > store.MaxOptimizationRange {
		// parseSince and this command take adjacent clock samples. If the clock
		// crossed one whole-second boundary for an exact 90d request, clamp that
		// rounding sliver instead of rejecting an otherwise valid bound.
		if span <= store.MaxOptimizationRange+time.Second {
			from = through.Add(-store.MaxOptimizationRange)
		} else {
			return fmt.Errorf("--since exceeds the 90d optimization query bound")
		}
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	sample, err := s.OptimizationRows(from, through, *maxRows)
	if err != nil {
		return err
	}
	options := optimize.DefaultCacheOptions()
	options.LargeContextTokens = *largeContext
	options.LowReuseRatio = *lowReusePercent / 100
	options.MinRepeatedLarge = *minRepeats
	options.MaxReceipts = *maxReceipts
	report, err := optimize.AnalyzeCache(sample, from, through, options)
	if err != nil {
		return err
	}
	if *format == "json" {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	writeCacheRecommendations(os.Stdout, report)
	return nil
}

func writeCacheRecommendations(out io.Writer, report optimize.CacheReport) {
	fmt.Fprintf(out, "BURNBAN CACHE SHAPING — %d metadata rows%s\n\n", report.SampledRows, truncationLabel(report.Truncated))
	if len(report.Receipts) == 0 {
		fmt.Fprintln(out, "No repeated large-context, low-cache-reuse pattern met the configured evidence threshold.")
	} else {
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "scope\tprovider/model\tlarge calls\tcache reuse\tconfidence")
		for _, receipt := range report.Receipts {
			fmt.Fprintf(w, "%s:%s\t%s / %s\t%d\t%.1f%%\t%s\n",
				terminalText(receipt.ScopeType, 20), terminalText(receipt.Scope, 80),
				terminalText(receipt.Provider, 60), terminalText(receipt.Model, 80),
				receipt.LargeContextCalls, receipt.CacheReuseRatio*100, receipt.Confidence)
		}
		_ = w.Flush()
		fmt.Fprintln(out, "\nAction: where provider/model caching is documented, put reusable instructions and tool definitions before request-specific context, configure the documented TTL, then measure again.")
	}
	fmt.Fprintln(out, "\nBoundary: prefix stability is unobserved. Burnban stores token metadata, not prompt bodies or prefix fingerprints, and does not infer savings or target quality.")
}

func cmdOptimizeAllocation(args []string) error {
	fs := flag.NewFlagSet("optimize allocation", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	days := fs.Int("days", 30, "complete UTC history days (7-90)")
	dimension := fs.String("dimension", "agent", "agent, project, meter, or team")
	format := fs.String("format", "table", "table, json, or csv")
	maxRows := fs.Int("max-rows", 50_000, fmt.Sprintf("maximum ledger rows to inspect (1-%d)", store.MaxOptimizationRows))
	headroom := fs.Int("headroom", 20, "headroom percentage above max(recent velocity, historical percentile)")
	percentile := fs.Int("percentile", 90, "historical daily percentile (50-99)")
	maxScopes := fs.Int("max-scopes", 100, "maximum proposed scopes (1-500)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *format != "table" && *format != "json" && *format != "csv" {
		return fmt.Errorf("bad --format %q: use table, json, or csv", *format)
	}
	if *days < 7 || *days > 90 {
		return errors.New("--days must be between 7 and 90")
	}
	through := time.Now().UTC().Truncate(24 * time.Hour)
	from := through.Add(-time.Duration(*days) * 24 * time.Hour)
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	sample, err := s.OptimizationRows(from, through, *maxRows)
	if err != nil {
		return err
	}
	options := optimize.DefaultAllocationOptions(*dimension, *days)
	options.HeadroomPercent = *headroom
	options.Percentile = *percentile
	options.MaxScopes = *maxScopes
	report, err := optimize.RecommendAllocations(sample, from, through, options)
	if err != nil {
		return err
	}
	if report.Truncated && *format == "csv" {
		return errors.New("allocation row bound reached; no CSV emitted because blocked-call impact is incomplete (use JSON for the diagnostic)")
	}
	switch *format {
	case "json":
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	case "csv":
		return writeAllocationCSV(os.Stdout, report)
	default:
		writeAllocationRecommendations(os.Stdout, report)
		return nil
	}
}

func writeAllocationRecommendations(out io.Writer, report optimize.AllocationReport) {
	fmt.Fprintf(out, "BURNBAN ALLOCATION PROPOSALS — %s, %d complete UTC days%s\n\n",
		terminalText(report.Dimension, 20), report.HistoricalDays, truncationLabel(report.Truncated))
	if report.Truncated {
		fmt.Fprintln(out, "No proposal: the row bound made historical blocked-call simulation incomplete. Narrow the window or raise --max-rows.")
		return
	}
	if len(report.Recommendations) == 0 {
		fmt.Fprintln(out, "No priced, attributed scope had enough spend to propose an allocation.")
		return
	}
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "scope\tweight\tdaily proposal\trecent/day\theadroom\tsim blocked\tconfidence")
	for _, recommendation := range report.Recommendations {
		fmt.Fprintf(w, "%s\t%.1f%%\t$%.2f\t$%.2f\t$%.2f\t%d/%d (%.1f%%)\t%s\n",
			terminalText(recommendation.Scope, 80), recommendation.ProposedWeight*100,
			microsUSD(recommendation.ProposedDailyBudgetMicros), microsUSD(recommendation.RecentVelocityMicros),
			microsUSD(recommendation.HeadroomAtRecentVelocityMicros), recommendation.SimulatedBlockedCalls,
			recommendation.PricedCalls, recommendation.SimulatedBlockedCallRate*100, recommendation.Confidence)
	}
	_ = w.Flush()
	if report.ExcludedUnpricedRows > 0 || report.ExcludedInvalidScopeRows > 0 ||
		report.ExcludedUntrustedRows > 0 || report.UnattributedRows > 0 {
		fmt.Fprintf(out, "\nEvidence exclusions: %d unknown/unmetered price row(s), %d unsafe-scope row(s), %d untrusted fleet-identity row(s), %d unattributed row(s).\n",
			report.ExcludedUnpricedRows, report.ExcludedInvalidScopeRows,
			report.ExcludedUntrustedRows, report.UnattributedRows)
	}
	fmt.Fprintln(out, "\nNothing was applied. Review the JSON/CSV receipt and blocked-call replay before explicitly changing a cap or versioned Policy v2 allocation.")
}

func writeAllocationCSV(out io.Writer, report optimize.AllocationReport) error {
	w := csv.NewWriter(out)
	if err := w.Write([]string{
		"schema", "dimension", "from", "through", "historical_days", "sampled_rows", "truncated",
		"unattributed_rows", "excluded_invalid_scope_rows", "excluded_untrusted_scope_rows", "excluded_unpriced_rows",
		"scope", "proposed_weight", "proposed_daily_usd", "historical_average_daily_usd",
		"historical_percentile_daily_usd", "recent_velocity_daily_usd", "headroom_usd",
		"priced_calls", "excluded_calls", "enforcement_unsafe_calls", "simulated_blocked_calls", "simulated_blocked_spend_usd",
		"simulated_blocked_call_rate", "confidence", "apply_command", "operator_action",
	}); err != nil {
		return err
	}
	for _, recommendation := range report.Recommendations {
		row := []string{
			spreadsheetText(report.Schema), spreadsheetText(report.Dimension), report.From.UTC().Format(time.RFC3339), report.Through.UTC().Format(time.RFC3339),
			strconv.Itoa(report.HistoricalDays), strconv.Itoa(report.SampledRows), strconv.FormatBool(report.Truncated),
			strconv.FormatInt(report.UnattributedRows, 10), strconv.FormatInt(report.ExcludedInvalidScopeRows, 10),
			strconv.FormatInt(report.ExcludedUntrustedRows, 10), strconv.FormatInt(report.ExcludedUnpricedRows, 10),
			spreadsheetText(recommendation.Scope),
			strconv.FormatFloat(recommendation.ProposedWeight, 'f', 6, 64), formatMicros(recommendation.ProposedDailyBudgetMicros),
			formatMicros(recommendation.HistoricalAverageMicros), formatMicros(recommendation.HistoricalPercentileMicros),
			formatMicros(recommendation.RecentVelocityMicros), formatMicros(recommendation.HeadroomAtRecentVelocityMicros),
			strconv.FormatInt(recommendation.PricedCalls, 10), strconv.FormatInt(recommendation.ExcludedCalls, 10),
			strconv.FormatInt(recommendation.EnforcementUnsafeCalls, 10), strconv.FormatInt(recommendation.SimulatedBlockedCalls, 10), formatMicros(recommendation.SimulatedBlockedSpendMicros),
			strconv.FormatFloat(recommendation.SimulatedBlockedCallRate, 'f', 6, 64), spreadsheetText(recommendation.Confidence),
			spreadsheetText(recommendation.ApplyCommand), spreadsheetText(recommendation.OperatorAction),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func cmdOptimizeQualityImport(args []string) error {
	fs := flag.NewFlagSet("optimize quality-import", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	filePath := fs.String("file", "", "canonical external-quality v1 JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *filePath == "" {
		return errors.New("--file is required")
	}
	file, err := openRegularQualityFile(*filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	records, err := optimize.ParseQualityDocument(file)
	if err != nil {
		return err
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	result, err := s.ImportQualityScores(records, time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Printf("external quality evidence: %d inserted · %d exact replay(s) · immutable local ledger\n", result.Inserted, result.Replayed)
	return nil
}

func openRegularQualityFile(path string) (*os.File, error) {
	if path == "-" {
		return nil, errors.New("stdin quality imports are disabled; use a permission-controlled regular file for replay evidence")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("quality import path must be a regular file, not a symlink, device, pipe, or directory")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		file.Close()
		return nil, errors.New("quality import path changed while it was being opened")
	}
	return file, nil
}

func truncationLabel(truncated bool) string {
	if truncated {
		return " (ROW LIMIT REACHED)"
	}
	return ""
}

func microsUSD(value int64) float64 { return float64(value) / 1e6 }

func formatMicros(value int64) string { return strconv.FormatFloat(microsUSD(value), 'f', 6, 64) }
