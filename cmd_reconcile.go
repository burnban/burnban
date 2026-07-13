package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/burnban/burnban/internal/reconcile"
	"github.com/burnban/burnban/internal/store"
)

func cmdReconcile(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: burnban reconcile <import|report> [flags]")
	}
	switch args[0] {
	case "import":
		return cmdReconcileImport(args[1:])
	case "report":
		return cmdReconcileReport(args[1:])
	default:
		return fmt.Errorf("unknown reconcile command %q: use import or report", args[0])
	}
}

func cmdReconcileImport(args []string) error {
	fs := flag.NewFlagSet("reconcile import", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	filePath := fs.String("file", "", "provider invoice CSV or canonical API JSON file")
	input := fs.String("input", "csv", "csv or json")
	format := fs.String("format", string(reconcile.FormatCanonical), "CSV mapping: canonical, openai, anthropic, or gemini")
	invoiceID := fs.String("invoice", "", "stable provider invoice ID (CSV only)")
	provider := fs.String("provider", "", "provider identity (CSV only)")
	currency := fs.String("currency", "USD", "invoice currency (USD only)")
	columns := fs.String("columns", "", "explicit CSV logical=source_header mappings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *filePath == "" {
		return errors.New("--file is required")
	}
	file, err := openRegularInvoiceFile(*filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	var invoice reconcile.Invoice
	switch *input {
	case "csv":
		mapping, err := reconcile.ParseMapping(*columns)
		if err != nil {
			return err
		}
		invoice, err = reconcile.ParseCSV(file, reconcile.CSVOptions{
			Format: reconcile.Format(*format), InvoiceID: *invoiceID, Provider: *provider,
			Currency: *currency, Mapping: mapping,
		})
		if err != nil {
			return err
		}
	case "json":
		if *invoiceID != "" || *provider != "" || *columns != "" || *currency != "USD" || *format != string(reconcile.FormatCanonical) {
			return errors.New("canonical JSON carries invoice/provider/currency/schema fields; CSV mapping flags must not be mixed with --input json")
		}
		invoice, err = reconcile.ParseJSON(file)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("bad --input %q: use csv or json", *input)
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	result, err := s.ImportInvoice(invoice, time.Now().UTC())
	if err != nil {
		return err
	}
	state := "imported"
	if result.Replayed {
		state = "already imported (exact replay)"
	}
	fmt.Printf("invoice %s: %s · %d rows · import %d\n", terminalText(invoice.InvoiceID, 200), state, result.Rows, result.ImportID)
	return nil
}

func cmdReconcileReport(args []string) error {
	fs := flag.NewFlagSet("reconcile report", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	since := fs.String("since", "30d", `window: "today", "24h", "7d", or any Go duration`)
	provider := fs.String("provider", "", "optional provider filter")
	format := fs.String("format", "table", "table, json, or csv")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	from, _, err := parseSince(*since)
	if err != nil {
		return err
	}
	through := time.Now().UTC().Add(time.Nanosecond)
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	report, err := s.Reconcile(from, through, *provider)
	if err != nil {
		return err
	}
	switch *format {
	case "table":
		writeReconciliationTable(os.Stdout, report)
		return nil
	case "json":
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	case "csv":
		return writeReconciliationCSV(os.Stdout, report)
	default:
		return fmt.Errorf("bad --format %q: use table, json, or csv", *format)
	}
}

func writeReconciliationTable(out io.Writer, report *store.ReconciliationReport) {
	provider := report.Provider
	if provider == "" {
		provider = "all providers"
	}
	fmt.Fprintf(out, "BURNBAN RECONCILIATION — %s\n\n", terminalText(provider, 100))
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintf(w, "ledger estimate\t$%s\n", reconcile.FormatMoneyMicros(report.LedgerEstimateMicros))
	fmt.Fprintf(w, "provider billed\t$%s\n", reconcile.FormatMoneyMicros(report.ProviderBilledMicros))
	fmt.Fprintf(w, "variance\t$%s\n", reconcile.FormatMoneyMicros(report.VarianceMicros))
	fmt.Fprintf(w, "unmatched ledger\t$%s · %d rows\n", reconcile.FormatMoneyMicros(report.UnmatchedLedgerMicros), report.UnmatchedLedgerRows)
	fmt.Fprintf(w, "unmatched provider\t$%s · %d lines\n", reconcile.FormatMoneyMicros(report.UnmatchedProviderMicros), report.UnmatchedProviderLines)
	fmt.Fprintf(w, "confidence\t%s\n", report.Confidence)
	last := "never"
	if report.LastReconciledAt != nil {
		last = report.LastReconciledAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(w, "last reconciled\t%s\n", last)
	for _, adjustment := range report.Adjustments {
		fmt.Fprintf(w, "adjustment %s\t$%s · %d lines\n", terminalText(adjustment.Type, 40), reconcile.FormatMoneyMicros(adjustment.AmountMicros), adjustment.Lines)
	}
	_ = w.Flush()
}

func writeReconciliationCSV(out io.Writer, report *store.ReconciliationReport) error {
	w := csv.NewWriter(out)
	if err := w.Write([]string{
		"provider", "day", "model", "service_tier", "region", "ledger_rows", "invoice_lines",
		"estimate_usd", "billed_usd", "variance_usd", "matched", "report_confidence", "last_reconciled_at",
	}); err != nil {
		return err
	}
	last := ""
	if report.LastReconciledAt != nil {
		last = report.LastReconciledAt.UTC().Format(time.RFC3339)
	}
	for _, match := range report.Matches {
		row := []string{
			reconcile.SpreadsheetText(match.Provider), match.Day, reconcile.SpreadsheetText(match.Model),
			reconcile.SpreadsheetText(match.ServiceTier), reconcile.SpreadsheetText(match.Region),
			fmt.Sprint(match.LedgerRows), fmt.Sprint(match.InvoiceLines),
			reconcile.FormatMoneyMicros(match.EstimateMicros), reconcile.FormatMoneyMicros(match.BilledMicros),
			reconcile.FormatMoneyMicros(match.VarianceMicros), fmt.Sprint(match.Matched),
			reconcile.SpreadsheetText(report.Confidence), last,
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func openRegularInvoiceFile(path string) (*os.File, error) {
	if path == "-" {
		return nil, errors.New("stdin invoice imports are disabled; use a permission-controlled regular file for replay evidence")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("invoice path must be a regular file, not a symlink, device, pipe, or directory")
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
		return nil, errors.New("invoice path changed while it was being opened")
	}
	return file, nil
}
