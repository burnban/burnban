package store_test

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func testPolicyDocument(name, namespace string, revision int64, ruleID string) store.PolicyDocumentRecord {
	raw := fmt.Sprintf(`{"apiVersion":"burnban.dev/v2","kind":"PolicySet","metadata":{"name":%q,"namespace":%q,"revision":%d},"mode":"enforce","rules":[{"id":%q}]}`,
		name, namespace, revision, ruleID)
	digest := sha256.Sum256([]byte(raw))
	return store.PolicyDocumentRecord{
		APIVersion:   "burnban.dev/v2",
		Name:         name,
		Namespace:    namespace,
		Revision:     revision,
		Digest:       fmt.Sprintf("%x", digest),
		DocumentJSON: raw,
	}
}

func TestPolicyActivationIsVersionedAndPruneRemovesDecisionChildren(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "policy-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	first := testPolicyDocument("one", "one", 1, "first")
	first.AppliedAt = now
	if err := s.ApplyPolicyDocument(first); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPolicyDocument(first); err != nil {
		t.Fatalf("idempotent apply: %v", err)
	}
	conflict := testPolicyDocument("one", "one", 1, "conflict")
	if err := s.ApplyPolicyDocument(conflict); err == nil {
		t.Fatal("same revision with different content was accepted")
	}
	second := testPolicyDocument("two", "one", 2, "second")
	second.AppliedAt = now.Add(time.Minute)
	if err := s.ApplyPolicyDocument(second); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPolicyDocument(first); err == nil {
		t.Fatal("policy rollback was accepted")
	}
	metadataMismatch := second
	metadataMismatch.Revision = 3
	if err := s.ApplyPolicyDocument(metadataMismatch); err == nil {
		t.Fatal("policy activation metadata was not bound to document content")
	}
	digestMismatch := testPolicyDocument("three", "one", 3, "digest-mismatch")
	digestMismatch.DocumentJSON += " "
	if err := s.ApplyPolicyDocument(digestMismatch); err == nil {
		t.Fatal("policy activation accepted content that disagreed with its digest")
	}
	active, err := s.ActivePolicyDocument()
	if err != nil || active == nil || active.Revision != 2 || active.Digest != second.Digest {
		t.Fatalf("active=%+v err=%v", active, err)
	}

	oldDecision, err := s.InsertPolicyDecision(store.PolicyDecisionRecord{
		Ts: now.Add(-48 * time.Hour), PolicyDigest: first.Digest, PolicyRevision: 1,
		PolicyName: "one", PolicyNamespace: "one", Mode: "enforce", Outcome: "allow", Confidence: "partial",
		ContextJSON: `{}`, ExplanationJSON: `{}`, Rules: []store.PolicyDecisionRule{{RuleID: "stable", Accepted: true, EstimatedTokens: 7}},
	})
	if err != nil {
		t.Fatal(err)
	}
	newDecision, err := s.InsertPolicyDecision(store.PolicyDecisionRecord{
		Ts: now, PolicyDigest: second.Digest, PolicyRevision: 2,
		PolicyName: "two", PolicyNamespace: "one", Mode: "warn", Outcome: "allow", Confidence: "exact",
		ContextJSON: `{}`, ExplanationJSON: `{}`, Rules: []store.PolicyDecisionRule{{RuleID: "stable", Accepted: true, EstimatedTokens: 11}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(store.Request{Ts: now.Add(-48 * time.Hour), Provider: "openai", PolicyDecisionID: oldDecision}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(store.Request{Ts: now, Provider: "openai", PolicyDecisionID: newDecision}); err != nil {
		t.Fatal(err)
	}

	// The first bounded batch consumes its one-record allowance on the old
	// request; the second consumes it on the old decision and its child row.
	if deleted, err := s.PruneBatch(now.Add(-24*time.Hour), 1); err != nil || deleted != 1 {
		t.Fatalf("first prune deleted=%d err=%v", deleted, err)
	}
	if deleted, err := s.PruneBatch(now.Add(-24*time.Hour), 1); err != nil || deleted != 1 {
		t.Fatalf("second prune deleted=%d err=%v", deleted, err)
	}
	if deleted, err := s.PruneBatch(now.Add(-24*time.Hour), 1); err != nil || deleted != 0 {
		t.Fatalf("final prune deleted=%d err=%v", deleted, err)
	}
	usage, err := s.PolicyRuleUsageSince("one", "stable", time.Unix(0, 0))
	if err != nil || usage.Requests != 1 || usage.Tokens != 11 {
		t.Fatalf("retained usage=%+v err=%v", usage, err)
	}
	summary, err := s.PolicyDecisionsSince(time.Unix(0, 0))
	if err != nil || summary.Total != 1 {
		t.Fatalf("retained decisions=%+v err=%v", summary, err)
	}
	rows, err := s.Export(time.Unix(0, 0))
	if err != nil || len(rows) != 1 || rows[0].Policy == nil || rows[0].Policy.DecisionID != newDecision {
		t.Fatalf("retained rows=%+v err=%v", rows, err)
	}
	active, err = s.ActivePolicyDocument()
	if err != nil || active == nil || active.Revision != 2 {
		t.Fatalf("prune changed active policy: active=%+v err=%v", active, err)
	}
}

func TestPolicyDigestConflictCannotActivateLegacyMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "policy-digest-conflict.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	active := testPolicyDocument("active", "default", 2, "active")
	if err := s.ApplyPolicyDocument(active); err != nil {
		t.Fatal(err)
	}
	candidate := testPolicyDocument("candidate", "default", 3, "candidate")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO policy_documents
		(applied_at,api_version,name,namespace,revision,digest,source,document_json)
		VALUES(?,?,?,?,?,?,?,?)`, time.Now().UTC().Format(time.RFC3339Nano), candidate.APIVersion,
		candidate.Name, candidate.Namespace, 1, candidate.Digest, "local", candidate.DocumentJSON); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPolicyDocument(candidate); err == nil {
		t.Fatal("activated a legacy digest row with conflicting revision metadata")
	}
	got, err := s.ActivePolicyDocument()
	if err != nil || got == nil || got.Digest != active.Digest || got.Revision != active.Revision {
		t.Fatalf("digest conflict changed active policy: active=%+v err=%v", got, err)
	}
}

func TestPolicyUsageSaturatesInsteadOfSQLiteIntegerOverflow(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "overflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	active := testPolicyDocument("overflow", "overflow", 1, "overflow")
	if err := s.ApplyPolicyDocument(active); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.InsertPolicyDecision(store.PolicyDecisionRecord{
			Ts: time.Now().UTC(), PolicyDigest: active.Digest, PolicyRevision: 1,
			PolicyName: "overflow", PolicyNamespace: "overflow", Mode: "warn", Outcome: "allow", Confidence: "exact",
			ContextJSON: `{}`, ExplanationJSON: `{}`, Rules: []store.PolicyDecisionRule{{
				RuleID: "huge", Accepted: true, EstimatedTokens: 1<<63 - 1,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := s.PolicyRuleUsageSince("overflow", "huge", time.Unix(0, 0))
	if err != nil || usage.Tokens != 1<<63-1 || usage.Requests != 2 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
}

func TestPolicyOwnershipAndNamespaceCannotBeSilentlyReassigned(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "ownership.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := testPolicyDocument("fleet", "stable", 1, "owned-one")
	first.Source = "burnban-teams:meter-1"
	if err := s.ApplyPolicyDocument(first); err != nil {
		t.Fatal(err)
	}
	local := testPolicyDocument("fleet", "stable", 2, "local-two")
	local.Source = "local"
	if err := s.ApplyPolicyDocument(local); err == nil {
		t.Fatal("local apply replaced a Team-owned active policy")
	}
	reset := testPolicyDocument("fleet", "reset", 2, "owned-two")
	reset.Source = first.Source
	if err := s.ApplyPolicyDocument(reset); err == nil {
		t.Fatal("namespace change reset the active counter lineage")
	}
	active, err := s.ActivePolicyDocument()
	if err != nil || active == nil || active.Source != first.Source || active.Namespace != first.Namespace || active.Revision != 1 {
		t.Fatalf("active=%+v err=%v", active, err)
	}
}

func TestPolicyLineageResetAndTakeoverAreExplicitAuditedCASOperations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lineage.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	external := testPolicyDocument("fleet", "external-lineage", 7, "external")
	external.Source = "burnban-teams:meter-1"
	if err := s.ApplyPolicyDocument(external); err != nil {
		t.Fatal(err)
	}
	base := store.PolicyLineageReset{
		Actor: "operator@example.com", Reason: "move this ledger to local policy ownership",
		ExpectedDigest: external.Digest, ExpectedSource: external.Source,
	}
	if err := s.ResetPolicyLineage(base); err == nil {
		t.Fatal("ordinary reset took over an externally owned lineage")
	}
	wrong := base
	wrong.Takeover = true
	wrong.ExpectedDigest = strings.Repeat("0", 64)
	if err := s.ResetPolicyLineage(wrong); err == nil {
		t.Fatal("stale digest precondition was accepted")
	}
	takeover := base
	takeover.Takeover = true
	if err := s.ResetPolicyLineage(takeover); err != nil {
		t.Fatal(err)
	}
	if active, err := s.ActivePolicyDocument(); err != nil || active != nil {
		t.Fatalf("active after takeover=%+v err=%v", active, err)
	}
	local := testPolicyDocument("local", "new-local-lineage", 1, "local")
	if err := s.ApplyPolicyDocument(local); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPolicyLineage(store.PolicyLineageReset{
		Actor: "operator@example.com", Reason: "start a replacement local namespace",
		ExpectedDigest: local.Digest, ExpectedSource: "local",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := s.PolicyLineageEvents(10)
	if err != nil || len(events) != 2 || events[0].Action != "reset" || events[1].Action != "takeover" ||
		events[1].PreviousNamespace != "external-lineage" || events[1].PreviousSource != external.Source {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE policy_lineage_events SET reason='tampered'`); err == nil {
		t.Fatal("immutable lineage audit event was updated")
	}
}

func TestPolicyOwnershipColumnMigratesExistingLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE policy_documents (
		id INTEGER PRIMARY KEY AUTOINCREMENT, applied_at TEXT NOT NULL, api_version TEXT NOT NULL,
		name TEXT NOT NULL, namespace TEXT NOT NULL, revision INTEGER NOT NULL,
		digest TEXT NOT NULL UNIQUE, document_json TEXT NOT NULL
	)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record := testPolicyDocument("local", "stable", 1, "legacy-migrated")
	if err := s.ApplyPolicyDocument(record); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActivePolicyDocument()
	if err != nil || active == nil || active.Source != "local" {
		t.Fatalf("active=%+v err=%v", active, err)
	}
}
