package store

import (
	"crypto/sha256"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func testDownshiftDocument(revision int64, mode, ruleID string) DownshiftDocumentRecord {
	raw := fmt.Sprintf(`{"api_version":"burnban.downshift/v1","revision":%d,"mode":%q,"warn_at_pct":70,"downshift_at_pct":80,"downshift_on_denial":true,"rules":[{"id":%q}]}`,
		revision, mode, ruleID)
	digest := sha256.Sum256([]byte(raw))
	return DownshiftDocumentRecord{
		APIVersion:   "burnban.downshift/v1",
		Revision:     revision,
		Digest:       fmt.Sprintf("%x", digest),
		Mode:         mode,
		DocumentJSON: raw,
	}
}

func TestDownshiftApplyRequiresExactMaterialSimulationOrAuditedForce(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	document := testDownshiftDocument(1, "warn_then_downshift", "material-simulation")
	document.AppliedAt = now
	digest := document.Digest
	if err := s.ApplyDownshiftDocument(document); err == nil {
		t.Fatal("enforcing document applied without simulation or force")
	}
	simulationID, err := s.InsertDownshiftSimulation(DownshiftSimulationRecord{
		CreatedAt: now, ConfigDigest: digest, Since: now.Add(-24 * time.Hour), Through: now,
		TotalRequests: 10, MatchedRequests: 5, EligibleRequests: 3,
		SourceCostUSD: 10, TargetCostUSD: 2, ReportJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	document.SimulationID = simulationID
	if err := s.ApplyDownshiftDocument(document); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActiveDownshiftDocument()
	if err != nil || active == nil || active.Digest != digest || active.SimulationID != simulationID {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	var auditCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM downshift_audit WHERE config_digest=? AND forced=0`, digest).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit=%d err=%v", auditCount, err)
	}
	for _, query := range []string{
		`UPDATE downshift_simulations SET report_json='tampered'`,
		`DELETE FROM downshift_documents`,
		`UPDATE downshift_audit SET reason='tampered'`,
	} {
		if _, err := s.db.Exec(query); err == nil {
			t.Fatalf("immutable routing evidence accepted mutation: %s", query)
		}
	}
}

func TestDownshiftForceRequiresSafeExplanationAndIsAudited(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	document := testDownshiftDocument(1, "warn_then_downshift", "forced")
	document.Forced = true
	for _, reason := range []string{"short", "contains\nnewline", strings.Repeat("x", 501)} {
		document.ForceReason = reason
		if err := s.ApplyDownshiftDocument(document); err == nil {
			t.Fatalf("accepted unsafe force reason %q", reason)
		}
	}
	document.ForceReason = "new deployment has no compatible historical feature receipts; operator reviewed the mapping"
	if err := s.ApplyDownshiftDocument(document); err != nil {
		t.Fatal(err)
	}
	var forced int
	var reason string
	if err := s.db.QueryRow(`SELECT forced,reason FROM downshift_audit WHERE config_digest=?`, document.Digest).Scan(&forced, &reason); err != nil || forced != 1 || reason != document.ForceReason {
		t.Fatalf("audit forced=%d reason=%q err=%v", forced, reason, err)
	}
}

func TestDownshiftRevisionCannotRollbackOrFork(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record := testDownshiftDocument(2, "observe", "active")
	if err := s.ApplyDownshiftDocument(record); err != nil {
		t.Fatal(err)
	}
	rollback := testDownshiftDocument(1, "observe", "rollback")
	if err := s.ApplyDownshiftDocument(rollback); err == nil {
		t.Fatal("accepted revision rollback")
	}
	fork := testDownshiftDocument(2, "observe", "fork")
	if err := s.ApplyDownshiftDocument(fork); err == nil {
		t.Fatal("accepted same-revision fork")
	}
	metadataMismatch := record
	metadataMismatch.Revision = 3
	if err := s.ApplyDownshiftDocument(metadataMismatch); err == nil {
		t.Fatal("accepted activation metadata that disagreed with the digest-bound document")
	}
	digestMismatch := testDownshiftDocument(3, "observe", "digest-mismatch")
	digestMismatch.DocumentJSON += " "
	if err := s.ApplyDownshiftDocument(digestMismatch); err == nil {
		t.Fatal("accepted document content that disagreed with its digest")
	}
	active, err := s.ActiveDownshiftDocument()
	if err != nil || active == nil || active.Digest != record.Digest || active.Revision != record.Revision {
		t.Fatalf("failed apply changed active document: active=%+v err=%v", active, err)
	}
}

func TestDownshiftDigestConflictCannotActivateLegacyMetadata(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	active := testDownshiftDocument(2, "observe", "active-before-conflict")
	if err := s.ApplyDownshiftDocument(active); err != nil {
		t.Fatal(err)
	}
	candidate := testDownshiftDocument(3, "observe", "legacy-conflict")
	if _, err := s.db.Exec(`INSERT INTO downshift_documents
		(applied_at,api_version,revision,digest,mode,document_json,forced,force_reason)
		VALUES(?,?,?,?,?,?,0,'')`, time.Now().UTC().Format(policyTimeFormat), candidate.APIVersion,
		1, candidate.Digest, candidate.Mode, candidate.DocumentJSON); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDownshiftDocument(candidate); err == nil {
		t.Fatal("activated a legacy digest row with conflicting revision metadata")
	}
	got, err := s.ActiveDownshiftDocument()
	if err != nil || got == nil || got.Digest != active.Digest || got.Revision != active.Revision {
		t.Fatalf("digest conflict changed active document: active=%+v err=%v", got, err)
	}
}

func TestDownshiftSimulationRejectsInconsistentOrNonfiniteData(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	record := DownshiftSimulationRecord{ConfigDigest: strings.Repeat("f", 64), Since: now.Add(-time.Hour), Through: now,
		TotalRequests: 1, MatchedRequests: 2, EligibleRequests: 1, ReportJSON: `{}`}
	if _, err := s.InsertDownshiftSimulation(record); err == nil {
		t.Fatal("accepted inconsistent simulation counts")
	}
	record.TotalRequests, record.MatchedRequests, record.SourceCostUSD = 2, 1, math.NaN()
	if _, err := s.InsertDownshiftSimulation(record); err == nil {
		t.Fatal("accepted NaN simulation cost")
	}
}

func TestDownshiftReceiptRoundTripsThroughImmutableRequestLedger(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	digest := strings.Repeat("a", 64)
	row := Request{Ts: time.Now().UTC(), Provider: "vllm", Model: "cheap", Route: "/v1/chat/completions",
		RequestedProvider: "openai", RequestedModel: "expensive", RequestedRoute: "/v1/chat/completions",
		DownshiftAction: "downshift", DownshiftRule: "safe", DownshiftTrigger: "budget_threshold",
		DownshiftReason: "selected compatible allowlisted target", DownshiftDigest: digest,
		DownshiftFeatures: `{"version":"burnban.features/v1"}`, DownshiftSourceUSD: .2, DownshiftTargetUSD: .02}
	if err := s.Insert(row); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Export(time.Time{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	got := rows[0]
	if got.RequestedProvider != "openai" || got.RequestedModel != "expensive" || got.DownshiftAction != "downshift" || got.DownshiftDigest != digest || got.DownshiftTargetUSD != .02 {
		t.Fatalf("receipt=%+v", got)
	}
}

func TestDownshiftReceiptRejectsMalformedClaims(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := Request{Ts: time.Now().UTC(), Provider: "openai", Model: "same", Route: "/v1",
		DownshiftAction: "downshift", DownshiftRule: "r", DownshiftReason: "reason",
		DownshiftDigest: strings.Repeat("a", 64)}
	if err := s.Insert(base); err == nil {
		t.Fatal("accepted downshift that did not change route or model")
	}
	base.RequestedModel = "source"
	base.DownshiftDigest = "not-a-digest"
	if err := s.Insert(base); err == nil {
		t.Fatal("accepted invalid config digest")
	}
}
