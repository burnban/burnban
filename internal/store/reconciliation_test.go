package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/reconcile"
)

func parsedInvoice(t *testing.T, invoiceID, body string) reconcile.Invoice {
	t.Helper()
	invoice, err := reconcile.ParseCSV(strings.NewReader(body), reconcile.CSVOptions{
		Format: reconcile.FormatCanonical, InvoiceID: invoiceID, Provider: "openai", Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	return invoice
}

func TestInvoiceImportReplayConflictImmutabilityAndReport(t *testing.T) {
	s, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	day := func(day int) time.Time { return time.Date(2026, 7, day, 12, 0, 0, 0, time.UTC) }
	if err := s.Insert(Request{
		Ts: day(12), Provider: "openai", Model: "gpt", ServiceTier: "priority", InferenceGeo: "us",
		CostUSD: 1, UsageState: UsageExact, PricingState: PricingPriced,
		CostSource: CostContract, CostSourceRef: "msa", CostEffectiveFrom: "2026-07-01", CostConfidence: ConfidenceContract,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(Request{
		Ts: day(13), Provider: "openai", Model: "unknown", UsageState: UsageExact,
		PricingState: PricingUnknown, CostSource: CostUnknown, CostConfidence: ConfidenceUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	body := "line_id,occurred_at,billed_usd,model,service_tier,region,line_type,reference_line_id\n" +
		"matched,2026-07-12T12:00:00Z,1.1,gpt,priority,us,usage,\n" +
		"unmatched,2026-07-14T12:00:00Z,0.4,gpt,priority,us,usage,\n" +
		"credit,2026-07-12T12:00:00Z,-0.1,gpt,priority,us,credit,matched\n"
	invoice := parsedInvoice(t, "inv-1", body)
	importedAt := time.Date(2026, 7, 15, 1, 2, 3, 4, time.UTC)
	first, err := s.ImportInvoice(invoice, importedAt)
	if err != nil || first.ImportID == 0 || first.Replayed || first.Rows != 3 {
		t.Fatalf("first import = %+v err=%v", first, err)
	}
	replay, err := s.ImportInvoice(invoice, importedAt.Add(time.Hour))
	if err != nil || !replay.Replayed || replay.ImportID != first.ImportID {
		t.Fatalf("replay = %+v err=%v", replay, err)
	}
	changed := parsedInvoice(t, "inv-1", strings.Replace(body, "1.1", "1.2", 1))
	if _, err := s.ImportInvoice(changed, importedAt); !errors.Is(err, ErrInvoiceConflict) {
		t.Fatalf("changed replay error = %v", err)
	}

	report, err := s.Reconcile(day(12).Add(-time.Hour), day(16), "OPENAI")
	if err != nil {
		t.Fatal(err)
	}
	if report.LedgerEstimateMicros != 1_000_000 || report.ProviderBilledMicros != 1_400_000 ||
		report.VarianceMicros != 400_000 || report.MatchedEstimateMicros != 1_000_000 ||
		report.MatchedBilledMicros != 1_100_000 || report.UnmatchedProviderMicros != 400_000 ||
		report.UnmatchedProviderLines != 1 || report.UnmatchedLedgerRows != 1 ||
		report.UnknownPricingRows != 1 || report.Confidence != "low" || report.LastReconciledAt == nil ||
		!report.LastReconciledAt.Equal(importedAt) {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Adjustments) != 1 || report.Adjustments[0].Type != "credit" || report.Adjustments[0].AmountMicros != -100_000 {
		t.Fatalf("adjustments = %+v", report.Adjustments)
	}
	if len(report.Matches) != 3 || !report.Matches[0].Matched {
		t.Fatalf("deterministic matches = %+v", report.Matches)
	}

	for _, statement := range []string{
		`UPDATE invoice_imports SET invoice_id='changed' WHERE id=1`,
		`DELETE FROM invoice_lines`,
		`UPDATE reconciliation_adjustments SET amount_micros=-1`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatalf("immutable mutation succeeded: %s", statement)
		}
	}
}

func TestConcurrentInvoiceReplayIsIdempotent(t *testing.T) {
	s, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	invoice := parsedInvoice(t, "inv-race", "line_id,occurred_at,billed_usd\nl,2026-07-12T00:00:00Z,1\n")
	const workers = 16
	var wg sync.WaitGroup
	results := make(chan InvoiceImportResult, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := s.ImportInvoice(invoice, time.Date(2026, 7, 12, 0, 0, i, 0, time.UTC))
			results <- result
			errs <- err
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id int64
	created := 0
	for result := range results {
		if id == 0 {
			id = result.ImportID
		}
		if result.ImportID != id {
			t.Fatalf("import IDs diverged: got %d want %d", result.ImportID, id)
		}
		if !result.Replayed {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created imports = %d, want 1", created)
	}
	var imports, lines int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM invoice_imports`).Scan(&imports); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM invoice_lines`).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if imports != 1 || lines != 1 {
		t.Fatalf("imports=%d lines=%d", imports, lines)
	}
}

func TestPricingProvenanceMigrationAndExport(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	request := Request{
		Ts: time.Now(), Provider: "anthropic", Model: "claude", CostUSD: .25,
		UsageState: UsageExact, PricingState: PricingPriced, CostSource: CostPublicList,
		CostSourceRef: "https://example.test/pricing", CostEffectiveFrom: "2026-01-01",
		CostValidThrough: "2026-12-31", CostConfidence: ConfidenceListEstimate,
	}
	if err := s.Insert(request); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Export(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CostSource != CostPublicList || rows[0].CostSourceRef != request.CostSourceRef ||
		rows[0].CostEffectiveFrom != "2026-01-01" || rows[0].CostValidThrough != "2026-12-31" ||
		rows[0].CostConfidence != ConfidenceListEstimate {
		t.Fatalf("exported row = %+v", rows)
	}
	var columns int
	for _, name := range []string{"cost_source", "cost_source_ref", "cost_effective_from", "cost_valid_through", "cost_confidence"} {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests') WHERE name=?`, name).Scan(&columns); err != nil || columns != 1 {
			t.Fatalf("column %s missing: count=%d err=%v", name, columns, err)
		}
	}
}
