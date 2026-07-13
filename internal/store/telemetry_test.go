package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestTelemetryRowsJoinPromptFreePolicyIdentityAndPricing(t *testing.T) {
	ledger, err := store.Open(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	now := time.Date(2026, 7, 12, 1, 2, 3, 4, time.UTC)
	decision, err := ledger.InsertPolicyDecision(store.PolicyDecisionRecord{
		Ts: now, PolicyDigest: "digest", PolicyRevision: 4, PolicyName: "default",
		PolicyNamespace: "org", Mode: "warn", Outcome: "allow", Admitted: true,
		Confidence: "exact", ContextJSON: `{"project":"legacy"}`, ExplanationJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Insert(store.Request{
		Ts: now, Provider: "openai", Model: "gpt-test", Session: "must-not-select",
		BodyHash: "must-not-select", InTokens: 3, CostUSD: .01,
		CostSource: store.CostContract, CostSourceRef: "msa-1",
		CostConfidence: store.ConfidenceContract, PolicyDecisionID: decision,
		IdentityTenant: "org:one", IdentityDevice: "device", Principal: "alice",
		Project: "project", CostCenter: "eng", IdentityConfidence: "authenticated",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := ledger.TelemetryRowsAfter(0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	r := rows[0].Request
	if rows[0].ID != 1 || r.Session != "" || r.BodyHash != "" ||
		r.CostSource != store.CostContract || r.CostSourceRef != "msa-1" ||
		r.IdentityTenant != "org:one" || r.Principal != "alice" ||
		r.Policy == nil || r.Policy.Revision != 4 || r.Policy.ContextJSON == "" {
		t.Fatalf("telemetry row projection = %+v", rows[0])
	}
}

func TestTelemetryBacklogUsesActualRowsAcrossPrunedIDGaps(t *testing.T) {
	ledger, err := store.Open(filepath.Join(t.TempDir(), "telemetry-gaps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	cutoff := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ts := cutoff.Add(time.Duration(i-3) * time.Hour)
		if err := ledger.Insert(store.Request{Ts: ts, Provider: "openai"}); err != nil {
			t.Fatal(err)
		}
	}
	if deleted, err := ledger.Prune(cutoff); err != nil || deleted != 3 {
		t.Fatalf("prune deleted=%d err=%v", deleted, err)
	}
	pending, dropThrough, err := ledger.TelemetryBacklog(0, 1)
	if err != nil || pending != 2 || dropThrough != 4 {
		t.Fatalf("backlog pending=%d dropThrough=%d err=%v", pending, dropThrough, err)
	}
	rows, err := ledger.TelemetryRowsSinceAfter(0, cutoff, 10)
	if err != nil || len(rows) != 2 || rows[0].ID != 4 || rows[1].ID != 5 {
		t.Fatalf("since rows=%+v err=%v", rows, err)
	}
}

func TestTelemetryBoundsRejectUnboundedInputs(t *testing.T) {
	ledger, err := store.Open(filepath.Join(t.TempDir(), "telemetry-bounds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	for _, limit := range []int{0, 1001} {
		if _, err := ledger.TelemetryRowsAfter(0, limit); err == nil {
			t.Errorf("batch limit %d accepted", limit)
		}
	}
	if _, _, err := ledger.TelemetryBacklog(-1, 1); err == nil {
		t.Fatal("negative cursor accepted")
	}
}
