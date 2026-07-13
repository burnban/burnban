package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/identity"
	"github.com/burnban/burnban/internal/policy"
	"github.com/burnban/burnban/internal/store"
)

type policyCoverageReport struct {
	GeneratedAt time.Time `json:"generated_at"`
	From        time.Time `json:"from"`

	Configured      bool          `json:"configured"`
	Routed          bool          `json:"routed"`
	Observed        bool          `json:"observed"`
	Enforcing       bool          `json:"enforcing"`
	PolicyStale     bool          `json:"policy_stale"`
	PolicyClockSkew bool          `json:"policy_clock_skew"`
	StaleAfter      time.Duration `json:"-"`
	StaleAfterText  string        `json:"stale_after"`

	PolicyName              string     `json:"policy_name,omitempty"`
	PolicyNamespace         string     `json:"policy_namespace,omitempty"`
	PolicyRevision          int64      `json:"policy_revision,omitempty"`
	PolicyDigest            string     `json:"policy_digest,omitempty"`
	PolicyAppliedAt         *time.Time `json:"policy_applied_at,omitempty"`
	PolicyMode              string     `json:"policy_mode,omitempty"`
	ExternalPolicySource    string     `json:"external_policy_source,omitempty"`
	ExternalPolicyUpdatedAt *time.Time `json:"external_policy_updated_at,omitempty"`

	Ledger                store.PolicyCoverageLedger  `json:"ledger"`
	Decisions             store.PolicyDecisionSummary `json:"decisions"`
	UncoveredRouted       int64                       `json:"uncovered_routed_requests"`
	EvaluationCoveragePct float64                     `json:"evaluation_coverage_pct"`
	TrustedIdentityPct    float64                     `json:"trusted_identity_pct"`

	TrustGrantState      string     `json:"trust_grant_state"`
	TrustGrantValidUntil *time.Time `json:"trust_grant_valid_until,omitempty"`
	ProjectTrustState    string     `json:"project_trust_state"`
	ExactProjectGrants   int        `json:"exact_project_grants"`

	SuspectedBypass          bool     `json:"suspected_bypass"`
	BypassEvidence           string   `json:"bypass_evidence"`
	UnmatchedProviderLines   int64    `json:"unmatched_provider_lines,omitempty"`
	UnmatchedProviderMicros  int64    `json:"unmatched_provider_micros,omitempty"`
	InvoiceEvidenceAvailable bool     `json:"invoice_evidence_available"`
	Notes                    []string `json:"notes"`
}

func cmdPolicyCoverage(args []string) error {
	fs := flag.NewFlagSet("policy coverage", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	since := fs.String("since", "7d", `window: "today", "24h", "7d", or any Go duration`)
	format := fs.String("format", "text", "text or json")
	staleAfter := fs.Duration("stale-after", 24*time.Hour, "mark an unchanged active policy stale after this duration")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("bad --format %q: use text or json", *format)
	}
	if *staleAfter < time.Minute || *staleAfter > 366*24*time.Hour {
		return fmt.Errorf("--stale-after must be between 1m and 8784h")
	}
	from, _, err := parseSince(*since)
	if err != nil {
		return err
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	report, err := buildPolicyCoverage(s, from, time.Now().UTC(), *staleAfter)
	if err != nil {
		return err
	}
	if *format == "json" {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	printPolicyCoverage(report)
	return nil
}

func buildPolicyCoverage(s *store.Store, requestedFrom, now time.Time, staleAfter time.Duration) (policyCoverageReport, error) {
	report := policyCoverageReport{
		GeneratedAt: now.UTC(), From: requestedFrom.UTC(), StaleAfter: staleAfter,
		StaleAfterText: staleAfter.String(), TrustGrantState: "not_enrolled", ProjectTrustState: "not_enrolled",
		BypassEvidence: "unknown_without_provider_invoice_evidence",
		Notes: []string{
			"direct provider traffic is invisible to a local proxy; unmatched provider invoice lines are a suspicion signal, not proof of bypass",
		},
	}
	active, err := s.ActivePolicyDocument()
	if err != nil {
		return report, err
	}
	coverageFrom := requestedFrom.UTC()
	if active != nil {
		report.Configured = true
		report.PolicyName, report.PolicyNamespace = active.Name, active.Namespace
		report.PolicyRevision, report.PolicyDigest = active.Revision, active.Digest
		applied := active.AppliedAt.UTC()
		report.PolicyAppliedAt = &applied
		if applied.After(coverageFrom) {
			coverageFrom = applied
			report.From = coverageFrom
		}
		compiled, parseErr := policy.Parse([]byte(active.DocumentJSON))
		if parseErr != nil {
			return report, fmt.Errorf("active policy is invalid: %w", parseErr)
		}
		report.PolicyMode = string(compiled.Document.Mode)
		report.Enforcing = policyDocumentEnforces(compiled.Document)
	}
	externalSettings, err := s.GetSettings(
		budget.KeyExternalPolicySource, budget.KeyExternalPolicyAt,
		budget.KeyExternalDailyCapUSD, budget.KeyExternalWeeklyCapUSD,
		budget.KeyExternalMonthlyCapUSD, budget.KeyExternalBanActive,
	)
	if err != nil {
		return report, err
	}
	report.ExternalPolicySource = externalSettings[budget.KeyExternalPolicySource]
	if report.ExternalPolicySource != "" {
		report.Configured = true
		rawUpdated := externalSettings[budget.KeyExternalPolicyAt]
		updated, parseErr := time.Parse(time.RFC3339, rawUpdated)
		if parseErr != nil {
			report.PolicyStale = true
		} else {
			updated = updated.UTC()
			report.ExternalPolicyUpdatedAt = &updated
			switch {
			case updated.After(now.Add(time.Minute)):
				report.PolicyClockSkew = true
				report.PolicyStale = true
			case now.Sub(updated) > staleAfter:
				report.PolicyStale = true
			}
		}
		if externalSettings[budget.KeyExternalDailyCapUSD] != "" ||
			externalSettings[budget.KeyExternalWeeklyCapUSD] != "" ||
			externalSettings[budget.KeyExternalMonthlyCapUSD] != "" ||
			externalSettings[budget.KeyExternalBanActive] == "1" {
			report.Enforcing = true
		}
	}
	report.Ledger, err = s.PolicyCoverageSince(coverageFrom)
	if err != nil {
		return report, err
	}
	report.Decisions, err = s.PolicyDecisionsSince(coverageFrom)
	if err != nil {
		return report, err
	}
	report.Routed = report.Ledger.RoutedRequests > 0
	report.Observed = report.Decisions.Total > 0
	report.UncoveredRouted = max(0, report.Ledger.RoutedRequests-report.Ledger.EvaluatedRequests)
	if report.Ledger.RoutedRequests > 0 {
		report.EvaluationCoveragePct = float64(report.Ledger.EvaluatedRequests) / float64(report.Ledger.RoutedRequests) * 100
		report.TrustedIdentityPct = float64(report.Ledger.TrustedIdentityRequests) / float64(report.Ledger.RoutedRequests) * 100
	}
	identitySettings, err := s.GetSettings(identity.KeyTrustGrant, identity.KeyTrustSource)
	if err != nil {
		return report, err
	}
	if identitySettings[identity.KeyTrustGrant] != "" || identitySettings[identity.KeyTrustSource] != "" {
		grant, grantErr := identity.LoadTrustGrant(s, now.UTC())
		if grantErr != nil {
			report.TrustGrantState = "stale_or_invalid"
			report.ProjectTrustState = "stale_or_invalid"
		} else {
			report.TrustGrantState = "valid"
			report.ProjectTrustState, report.ExactProjectGrants = projectTrustCoverage(grant.Attribution.Projects)
			if report.ProjectTrustState == "self_reported_wildcard" || report.ProjectTrustState == "mixed" {
				report.Notes = append(report.Notes,
					"wildcard project grants permit signed attribution but do not authenticate a project for enforcing policy")
			}
			validUntil, parseErr := time.Parse(time.RFC3339, grant.ValidUntil)
			if parseErr == nil {
				validUntil = validUntil.UTC()
				report.TrustGrantValidUntil = &validUntil
			}
		}
	}
	reconciliation, err := s.Reconcile(coverageFrom, now.Add(time.Nanosecond), "")
	if err != nil {
		return report, err
	}
	if reconciliation.LastReconciledAt != nil {
		report.InvoiceEvidenceAvailable = true
		report.UnmatchedProviderLines = reconciliation.UnmatchedProviderLines
		report.UnmatchedProviderMicros = reconciliation.UnmatchedProviderMicros
		if reconciliation.UnmatchedProviderLines > 0 {
			report.SuspectedBypass = true
			report.BypassEvidence = "unmatched_provider_invoice_lines"
		} else {
			report.BypassEvidence = "no_unmatched_provider_invoice_lines"
		}
	}
	return report, nil
}

func projectTrustCoverage(projects []string) (string, int) {
	wildcard, exact := false, 0
	for _, project := range projects {
		if project == "*" {
			wildcard = true
		} else {
			exact++
		}
	}
	switch {
	case wildcard && exact != 0:
		return "mixed", exact
	case wildcard:
		return "self_reported_wildcard", 0
	case exact != 0:
		return "exact_allowlist", exact
	default:
		return "none", 0
	}
}

func policyDocumentEnforces(document policy.Document) bool {
	for _, rule := range document.Rules {
		mode := rule.Mode
		if mode == "" {
			mode = document.Mode
		}
		if mode == policy.ModeEnforce {
			return true
		}
	}
	return false
}

func printPolicyCoverage(report policyCoverageReport) {
	fmt.Printf("policy coverage from %s\n", report.From.Format(time.RFC3339))
	fmt.Printf("pipeline  configured=%t routed=%t observed=%t enforcing=%t stale=%t\n",
		report.Configured, report.Routed, report.Observed, report.Enforcing, report.PolicyStale)
	if report.PolicyAppliedAt != nil {
		fmt.Printf("policy    %s/%s revision %d · mode %s · applied %s\n", report.PolicyNamespace,
			report.PolicyName, report.PolicyRevision, report.PolicyMode, report.PolicyAppliedAt.Format(time.RFC3339))
	}
	if report.ExternalPolicySource != "" {
		updated := "invalid or missing timestamp"
		if report.ExternalPolicyUpdatedAt != nil {
			updated = report.ExternalPolicyUpdatedAt.Format(time.RFC3339)
		}
		fmt.Printf("external  source %s · updated %s\n", terminalText(report.ExternalPolicySource, 200), updated)
	}
	fmt.Printf("traffic   %d routed · %d evaluated (%.1f%%) · %d uncovered\n",
		report.Ledger.RoutedRequests, report.Ledger.EvaluatedRequests, report.EvaluationCoveragePct, report.UncoveredRouted)
	fmt.Printf("identity  %d trusted (%.1f%%) · %d self-reported · %d unverified · grant %s · project %s (%d exact)\n",
		report.Ledger.TrustedIdentityRequests, report.TrustedIdentityPct, report.Ledger.SelfReportedRequests,
		report.Ledger.UnverifiedRequests, report.TrustGrantState, report.ProjectTrustState, report.ExactProjectGrants)
	fmt.Printf("bypass    suspected=%t · %s", report.SuspectedBypass, report.BypassEvidence)
	if report.InvoiceEvidenceAvailable {
		fmt.Printf(" · %d unmatched provider lines ($%.6f)", report.UnmatchedProviderLines,
			float64(report.UnmatchedProviderMicros)/1_000_000)
	}
	fmt.Println()
	for _, note := range report.Notes {
		fmt.Printf("note: %s\n", note)
	}
}
