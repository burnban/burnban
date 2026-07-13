package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/policy"
	"github.com/burnban/burnban/internal/reconcile"
	"github.com/burnban/burnban/internal/store"
)

func TestPolicyCoverageReportsEvaluationIdentityStalenessAndInvoiceSignals(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "coverage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	document, _ := policyTemplate("individual-coding-agent")
	raw, _ := json.Marshal(document)
	compiled, err := policy.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPolicyDocument(store.PolicyDocumentRecord{
		AppliedAt: now.Add(-2 * time.Hour), APIVersion: document.APIVersion,
		Name: document.Metadata.Name, Namespace: document.Metadata.Namespace,
		Revision: document.Metadata.Revision, Digest: compiled.Digest, DocumentJSON: string(compiled.Canonical),
	}); err != nil {
		t.Fatal(err)
	}
	engine := policy.NewEngine(s)
	reservation, decision, err := engine.Admit(now.Add(-30*time.Minute), policy.Context{
		Provider: "openai", Model: "gpt-5-mini", EstimatedInput: 100,
		OutputBound: 100, OutputBoundPresent: true, EstimatedCostUSD: 0.01, CostKnown: true,
		IdentityConfidence: "authenticated",
	})
	if err != nil || reservation == nil || decision == nil || decision.ID == 0 {
		t.Fatalf("policy admission reservation=%v decision=%+v err=%v", reservation, decision, err)
	}
	reservation.Release()
	for _, row := range []store.Request{
		{Ts: now.Add(-30 * time.Minute), Provider: "openai", Model: "gpt-5-mini", CostUSD: 0.01,
			PricingState: store.PricingPriced, UsageState: store.UsageExact,
			IdentityConfidence: "authenticated", PolicyDecisionID: decision.ID},
		{Ts: now.Add(-20 * time.Minute), Provider: "openai", Model: "gpt-5-mini", CostUSD: 0.01,
			PricingState: store.PricingPriced, UsageState: store.UsageExact,
			IdentityConfidence: "self_reported"},
	} {
		if err := s.Insert(row); err != nil {
			t.Fatal(err)
		}
	}

	report, err := buildPolicyCoverage(s, now.Add(-time.Hour), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Configured || !report.Routed || !report.Observed || !report.Enforcing || report.PolicyStale {
		t.Fatalf("pipeline flags=%+v", report)
	}
	if report.Ledger.RoutedRequests != 2 || report.Ledger.EvaluatedRequests != 1 ||
		report.Ledger.TrustedIdentityRequests != 1 || report.Ledger.SelfReportedRequests != 1 ||
		report.UncoveredRouted != 1 || report.EvaluationCoveragePct != 50 || report.TrustedIdentityPct != 50 {
		t.Fatalf("coverage aggregates=%+v", report)
	}
	if report.TrustGrantState != "not_enrolled" || report.InvoiceEvidenceAvailable || report.SuspectedBypass {
		t.Fatalf("empty evidence state=%+v", report)
	}

	for key, value := range map[string]string{
		budget.KeyExternalPolicySource: "fleet:meter-1",
		budget.KeyExternalPolicyAt:     now.Add(-2 * time.Hour).Format(time.RFC3339),
		budget.KeyExternalDailyCapUSD:  "5",
	} {
		if err := s.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	report, err = buildPolicyCoverage(s, now.Add(-time.Hour), now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !report.PolicyStale || !report.Enforcing || report.ExternalPolicyUpdatedAt == nil {
		t.Fatalf("external stale state=%+v", report)
	}

	invoice, err := reconcile.ParseCSV(strings.NewReader(
		"line_id,occurred_at,billed_usd,model\nprovider-only,2026-07-12T11:45:00Z,1,claude-other\n"),
		reconcile.CSVOptions{Format: reconcile.FormatCanonical, InvoiceID: "inv-coverage", Provider: "anthropic", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportInvoice(invoice, now); err != nil {
		t.Fatal(err)
	}
	report, err = buildPolicyCoverage(s, now.Add(-time.Hour), now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !report.InvoiceEvidenceAvailable || !report.SuspectedBypass || report.UnmatchedProviderLines != 1 ||
		report.BypassEvidence != "unmatched_provider_invoice_lines" {
		t.Fatalf("provider evidence=%+v", report)
	}
}

func TestPolicyCoverageExternalOnlyTextDoesNotRequireLocalPolicy(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "coverage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	if err := s.SetSetting(budget.KeyExternalPolicySource, "fleet:meter-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyExternalPolicyAt, now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	report, err := buildPolicyCoverage(s, now.Add(-time.Hour), now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Configured || report.PolicyAppliedAt != nil {
		t.Fatalf("external-only report=%+v", report)
	}
	// Exercise the text renderer's external-only branch; this previously
	// risked dereferencing a missing local-policy timestamp.
	printPolicyCoverage(report)
}

func TestProjectTrustCoverageDistinguishesWildcardFromExactAssignments(t *testing.T) {
	for name, test := range map[string]struct {
		projects []string
		state    string
		exact    int
	}{
		"none":     {state: "none"},
		"wildcard": {projects: []string{"*"}, state: "self_reported_wildcard"},
		"exact":    {projects: []string{"project-a", "project-b"}, state: "exact_allowlist", exact: 2},
		"mixed":    {projects: []string{"*", "project-a"}, state: "mixed", exact: 1},
	} {
		t.Run(name, func(t *testing.T) {
			state, exact := projectTrustCoverage(test.projects)
			if state != test.state || exact != test.exact {
				t.Fatalf("state=%q exact=%d", state, exact)
			}
		})
	}
}
