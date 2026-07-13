package main

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestReconcileImportCommandAndSafePath(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "ledger.db")
	invoice := filepath.Join(dir, "invoice.csv")
	body := "line_id,occurred_at,billed_usd,model\nline-1,2026-07-12T00:00:00Z,1.25,gpt\n"
	if err := os.WriteFile(invoice, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--db", db, "--file", invoice, "--invoice", "inv-1", "--provider", "openai", "--format", "canonical"}
	if err := cmdReconcileImport(args); err != nil {
		t.Fatal(err)
	}
	if err := cmdReconcileImport(args); err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	report, err := s.Reconcile(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), "openai")
	if err != nil || report.ProviderBilledMicros != 1_250_000 || report.UnmatchedProviderLines != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := openRegularInvoiceFile(dir); err == nil {
		t.Fatal("directory accepted as invoice")
	}
	if _, err := openRegularInvoiceFile("-"); err == nil {
		t.Fatal("stdin accepted as replay evidence")
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "invoice-link.csv")
		if err := os.Symlink(invoice, link); err != nil {
			t.Fatal(err)
		}
		if _, err := openRegularInvoiceFile(link); err == nil {
			t.Fatal("symlink accepted as invoice")
		}
	}
}

func TestReconcileCanonicalJSONFlagSeparation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invoice.json")
	body := `{"schema":"burnban.invoice/v1","invoice_id":"inv","provider":"openai","currency":"USD","lines":[{"line_id":"line","occurred_at":"2026-07-12T00:00:00Z","billed_usd":"1"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdReconcileImport([]string{"--db", filepath.Join(t.TempDir(), "db"), "--file", path, "--input", "json", "--provider", "openai"}); err == nil {
		t.Fatal("JSON accepted conflicting CSV identity flags")
	}
	if err := cmdReconcileImport([]string{"--db", filepath.Join(t.TempDir(), "db"), "--file", path, "--input", "json"}); err != nil {
		t.Fatal(err)
	}
}

func TestReconciliationCSVNeutralizesFormulaDimensions(t *testing.T) {
	report := &store.ReconciliationReport{
		Confidence: "low",
		Matches: []store.ReconciliationMatch{{
			Provider: "=provider", Day: "2026-07-12", Model: "\u00a0-cmd", ServiceTier: "+tier", Region: "@region",
		}},
	}
	var output bytes.Buffer
	if err := writeReconciliationCSV(&output, report); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(output.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []int{0, 2, 3, 4} {
		if !strings.HasPrefix(rows[1][column], "'") {
			t.Errorf("column %d was not neutralized: %q", column, rows[1][column])
		}
	}
}
