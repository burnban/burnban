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

func TestInvoiceImportReplayBindsEffectiveMapping(t *testing.T) {
	body := "id,usage_start_time,cost_usd,model,alternate_id,alternate_time,alternate_cost,alternate_model\n" +
		"original,2026-07-12T00:00:00Z,1,gpt,corrected,2026-07-13T00:00:00Z,2,gpt\n"
	correctedMapping := map[string]string{
		"line_id": "alternate_id", "occurred_at": "alternate_time", "billed_usd": "alternate_cost",
	}
	parse := func(t *testing.T, invoiceID string, mapping map[string]string) reconcile.Invoice {
		t.Helper()
		invoice, err := reconcile.ParseCSV(strings.NewReader(body), reconcile.CSVOptions{
			Format: reconcile.FormatOpenAI, InvoiceID: invoiceID, Provider: "openai", Currency: "USD", Mapping: mapping,
		})
		if err != nil {
			t.Fatal(err)
		}
		return invoice
	}

	for _, test := range []struct {
		name            string
		first, replay   map[string]string
		conflictMapping map[string]string
	}{
		{
			name:            "changed interpretation",
			conflictMapping: correctedMapping,
		},
		{
			name:            "corrected mapping imported first",
			first:           correctedMapping,
			replay:          correctedMapping,
			conflictMapping: nil,
		},
		{
			name: "same normalized rows but different mapping",
			conflictMapping: map[string]string{
				"model": "alternate_model",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, err := Open(t.TempDir() + "/ledger.db")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			firstInvoice := parse(t, "inv-mapping", test.first)
			replayInvoice := parse(t, "inv-mapping", test.replay)
			conflictingInvoice := parse(t, "inv-mapping", test.conflictMapping)
			if firstInvoice.ContentHash != conflictingInvoice.ContentHash {
				t.Fatal("mapping changed the raw content hash")
			}
			firstIdentity, err := reconcile.ImportIdentity(firstInvoice)
			if err != nil {
				t.Fatal(err)
			}
			conflictingIdentity, err := reconcile.ImportIdentity(conflictingInvoice)
			if err != nil {
				t.Fatal(err)
			}
			if firstIdentity == conflictingIdentity {
				t.Fatal("different effective mappings have the same import identity")
			}

			first, err := s.ImportInvoice(firstInvoice, time.Now().UTC())
			if err != nil || first.Replayed {
				t.Fatalf("first import = %+v, %v", first, err)
			}
			replay, err := s.ImportInvoice(replayInvoice, time.Now().UTC())
			if err != nil || !replay.Replayed || replay.ImportID != first.ImportID {
				t.Fatalf("exact replay = %+v, %v", replay, err)
			}
			mutatedInvoice := replayInvoice
			mutatedInvoice.Lines = append([]reconcile.Line(nil), replayInvoice.Lines...)
			mutatedInvoice.Lines[0].Model += "-mutated"
			if _, err := s.ImportInvoice(mutatedInvoice, time.Now().UTC()); !errors.Is(err, ErrInvoiceConflict) {
				t.Fatalf("mutated normalized replay error = %v", err)
			}
			if _, err := s.ImportInvoice(conflictingInvoice, time.Now().UTC()); !errors.Is(err, ErrInvoiceConflict) {
				t.Fatalf("corrected mapping error = %v", err)
			}

			var storedContent, storedIdentity string
			if err := s.db.QueryRow(`SELECT content_sha256,import_identity_sha256 FROM invoice_imports WHERE id=?`, first.ImportID).
				Scan(&storedContent, &storedIdentity); err != nil {
				t.Fatal(err)
			}
			if storedContent != firstInvoice.ContentHash || storedIdentity != firstIdentity {
				t.Fatalf("stored content=%q identity=%q", storedContent, storedIdentity)
			}
		})
	}
}

func TestInvoiceImportIdentityMigrationPreservesLegacyReplays(t *testing.T) {
	body := "id,usage_start_time,cost_usd,actual_id,actual_time,actual_cost\n" +
		"preset,2026-07-12T00:00:00Z,1,custom,2026-07-13T00:00:00Z,2\n"
	customMapping := map[string]string{
		"line_id": "actual_id", "occurred_at": "actual_time", "billed_usd": "actual_cost",
	}
	for _, test := range []struct {
		name          string
		mapping       map[string]string
		conflicting   map[string]string
		checkConflict bool
	}{
		{name: "preset mapping", conflicting: customMapping, checkConflict: true},
		{name: "custom mapping", mapping: customMapping, checkConflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := t.TempDir() + "/ledger.db"
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			parse := func(mapping map[string]string) reconcile.Invoice {
				invoice, err := reconcile.ParseCSV(strings.NewReader(body), reconcile.CSVOptions{
					Format: reconcile.FormatOpenAI, InvoiceID: "legacy", Provider: "openai", Currency: "USD", Mapping: mapping,
				})
				if err != nil {
					t.Fatal(err)
				}
				return invoice
			}
			invoice := parse(test.mapping)
			first, err := s.ImportInvoice(invoice, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			// Removing the additive column reproduces a database written by the
			// prior schema while retaining its immutable normalized invoice rows.
			if _, err := s.db.Exec(`ALTER TABLE invoice_imports DROP COLUMN import_identity_sha256`); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var migratedIdentity string
			if err := s.db.QueryRow(`SELECT import_identity_sha256 FROM invoice_imports WHERE id=?`, first.ImportID).
				Scan(&migratedIdentity); err != nil {
				t.Fatal(err)
			}
			if migratedIdentity != "" {
				t.Fatalf("legacy identity was fabricated: %q", migratedIdentity)
			}
			replay, err := s.ImportInvoice(parse(test.mapping), time.Now().UTC())
			if err != nil || !replay.Replayed || replay.ImportID != first.ImportID {
				t.Fatalf("legacy replay = %+v, %v", replay, err)
			}
			if test.checkConflict {
				if _, err := s.ImportInvoice(parse(test.conflicting), time.Now().UTC()); !errors.Is(err, ErrInvoiceConflict) {
					t.Fatalf("changed legacy interpretation error = %v", err)
				}
			}
		})
	}
}

func TestConcurrentInvoiceReplayIsIdempotent(t *testing.T) {
	path := t.TempDir() + "/ledger.db"
	firstStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer firstStore.Close()
	secondStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	stores := []*Store{firstStore, secondStore}
	invoice := parsedInvoice(t, "inv-race", "line_id,occurred_at,billed_usd\nl,2026-07-12T00:00:00Z,1\n")
	const workers = 16
	var wg sync.WaitGroup
	results := make(chan InvoiceImportResult, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := stores[i%len(stores)].ImportInvoice(invoice, time.Date(2026, 7, 12, 0, 0, i, 0, time.UTC))
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
	if err := firstStore.db.QueryRow(`SELECT COUNT(*) FROM invoice_imports`).Scan(&imports); err != nil {
		t.Fatal(err)
	}
	if err := firstStore.db.QueryRow(`SELECT COUNT(*) FROM invoice_lines`).Scan(&lines); err != nil {
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
