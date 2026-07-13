package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const policyTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// policySchema is deliberately append-only. Policy documents and decisions
// never contain prompts, responses, or credentials; they retain only the
// metadata needed to explain and replay an admission decision.
const policySchema = `
CREATE TABLE IF NOT EXISTS policy_documents (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	applied_at TEXT NOT NULL,
	api_version TEXT NOT NULL,
	name TEXT NOT NULL,
	namespace TEXT NOT NULL,
	revision INTEGER NOT NULL,
	digest TEXT NOT NULL UNIQUE,
	source TEXT NOT NULL DEFAULT 'local',
	document_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS policy_active (
	slot INTEGER PRIMARY KEY CHECK (slot = 1),
	document_id INTEGER NOT NULL REFERENCES policy_documents(id)
);
CREATE TRIGGER IF NOT EXISTS policy_documents_no_update
BEFORE UPDATE ON policy_documents BEGIN SELECT RAISE(ABORT, 'policy documents are immutable'); END;
CREATE TRIGGER IF NOT EXISTS policy_documents_no_delete
BEFORE DELETE ON policy_documents BEGIN SELECT RAISE(ABORT, 'policy documents are immutable'); END;
CREATE TABLE IF NOT EXISTS policy_lineage_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL,
	action TEXT NOT NULL CHECK(action IN ('reset','takeover','unlink')),
	actor TEXT NOT NULL,
	reason TEXT NOT NULL,
	previous_document_id INTEGER NOT NULL REFERENCES policy_documents(id),
	previous_namespace TEXT NOT NULL,
	previous_source TEXT NOT NULL,
	previous_digest TEXT NOT NULL
);
CREATE TRIGGER IF NOT EXISTS policy_lineage_events_no_update
BEFORE UPDATE ON policy_lineage_events BEGIN SELECT RAISE(ABORT, 'policy lineage events are immutable'); END;
CREATE TRIGGER IF NOT EXISTS policy_lineage_events_no_delete
BEFORE DELETE ON policy_lineage_events BEGIN SELECT RAISE(ABORT, 'policy lineage events are immutable'); END;
CREATE TABLE IF NOT EXISTS policy_decisions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL,
	policy_digest TEXT NOT NULL,
	policy_revision INTEGER NOT NULL,
	policy_name TEXT NOT NULL,
	policy_namespace TEXT NOT NULL,
	mode TEXT NOT NULL,
	outcome TEXT NOT NULL,
	admitted INTEGER NOT NULL DEFAULT 0,
	http_status INTEGER NOT NULL DEFAULT 0,
	confidence TEXT NOT NULL,
	context_json TEXT NOT NULL,
	explanation_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_policy_decisions_ts ON policy_decisions(ts);
CREATE TABLE IF NOT EXISTS policy_decision_rules (
	decision_id INTEGER NOT NULL REFERENCES policy_decisions(id),
	ts TEXT NOT NULL,
	policy_digest TEXT NOT NULL,
	policy_namespace TEXT NOT NULL,
	rule_id TEXT NOT NULL,
	accepted INTEGER NOT NULL CHECK (accepted IN (0,1)),
	estimated_tokens INTEGER NOT NULL DEFAULT 0 CHECK (estimated_tokens >= 0),
	estimated_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (estimated_input_tokens >= 0),
	estimated_output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (estimated_output_tokens >= 0),
	estimated_total_tokens INTEGER NOT NULL DEFAULT 0 CHECK (estimated_total_tokens >= 0),
	estimated_cost_microusd INTEGER NOT NULL DEFAULT 0 CHECK (estimated_cost_microusd >= 0),
	PRIMARY KEY (decision_id, rule_id)
);
CREATE INDEX IF NOT EXISTS idx_policy_rule_usage
	ON policy_decision_rules(policy_digest, rule_id, ts, accepted, estimated_tokens);
CREATE INDEX IF NOT EXISTS idx_policy_rule_stable_usage
	ON policy_decision_rules(policy_namespace, rule_id, ts, accepted, estimated_tokens);
`

// PolicyMetadata is joined onto exported request rows. JSON payloads are kept
// as strings here so the store stays neutral about the policy package's schema.
type PolicyMetadata struct {
	DecisionID      int64  `json:"decision_id"`
	Digest          string `json:"digest"`
	Revision        int64  `json:"revision"`
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	Mode            string `json:"mode"`
	Outcome         string `json:"outcome"`
	Admitted        bool   `json:"admitted"`
	Confidence      string `json:"confidence"`
	ContextJSON     string `json:"context_json"`
	ExplanationJSON string `json:"explanation_json"`
}

type PolicyDocumentRecord struct {
	ID           int64     `json:"id"`
	AppliedAt    time.Time `json:"applied_at"`
	APIVersion   string    `json:"api_version"`
	Name         string    `json:"name"`
	Namespace    string    `json:"namespace"`
	Revision     int64     `json:"revision"`
	Digest       string    `json:"digest"`
	Source       string    `json:"source"`
	DocumentJSON string    `json:"document"`
}

type PolicyLineageReset struct {
	At             time.Time
	Actor          string
	Reason         string
	ExpectedDigest string
	ExpectedSource string
	Takeover       bool
}

type PolicyLineageEvent struct {
	ID                int64     `json:"id"`
	Ts                time.Time `json:"ts"`
	Action            string    `json:"action"`
	Actor             string    `json:"actor"`
	Reason            string    `json:"reason"`
	PreviousDocument  int64     `json:"previous_document_id"`
	PreviousNamespace string    `json:"previous_namespace"`
	PreviousSource    string    `json:"previous_source"`
	PreviousDigest    string    `json:"previous_digest"`
}

func migratePolicySchema(db *sql.DB) error {
	columns, err := policyTableColumns(db, "policy_documents")
	if err != nil {
		return err
	}
	if !columns["source"] {
		if _, err := db.Exec(`ALTER TABLE policy_documents ADD COLUMN source TEXT NOT NULL DEFAULT 'local'`); err != nil {
			return err
		}
	}
	columns, err = policyTableColumns(db, "policy_decision_rules")
	if err != nil {
		return err
	}
	for _, column := range []string{"estimated_input_tokens", "estimated_output_tokens", "estimated_total_tokens", "estimated_cost_microusd"} {
		if columns[column] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE policy_decision_rules ADD COLUMN ` + column + ` INTEGER NOT NULL DEFAULT 0 CHECK (` + column + ` >= 0)`); err != nil {
			return err
		}
		if column != "estimated_cost_microusd" {
			if _, err := db.Exec(`UPDATE policy_decision_rules SET ` + column + `=estimated_tokens`); err != nil {
				return err
			}
		}
	}
	return nil
}

func policyTableColumns(db *sql.DB, table string) (map[string]bool, error) {
	if table != "policy_documents" && table != "policy_decision_rules" {
		return nil, fmt.Errorf("unsupported policy migration table")
	}
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return columns, nil
}

// ApplyPolicyDocument appends a validated document and atomically points the
// active slot at it. Revisions cannot move backward or be reused for different
// content, preventing accidental rollback/replay. Reapplying identical content
// is idempotent.
func (s *Store) ApplyPolicyDocument(record PolicyDocumentRecord) error {
	record.Source = strings.TrimSpace(record.Source)
	if record.Source == "" {
		record.Source = "local"
	}
	if err := validatePolicyDocumentContent(record); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var activeRevision int64
	var activeAPIVersion, activeName, activeDigest, activeSource, activeNamespace, activeDocument string
	err = tx.QueryRow(`SELECT d.api_version,d.name,d.namespace,d.revision,d.digest,d.source,d.document_json
		FROM policy_active a JOIN policy_documents d ON d.id=a.document_id WHERE a.slot=1`).
		Scan(&activeAPIVersion, &activeName, &activeNamespace, &activeRevision, &activeDigest, &activeSource, &activeDocument)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if activeSource != record.Source {
			return fmt.Errorf("active policy is owned by %q; refusing apply from %q", activeSource, record.Source)
		}
		if activeNamespace != record.Namespace {
			return fmt.Errorf("policy namespace %q differs from active namespace %q; clear the active lineage explicitly before resetting durable counters", record.Namespace, activeNamespace)
		}
		switch {
		case record.Revision < activeRevision:
			return fmt.Errorf("policy revision %d is older than active revision %d", record.Revision, activeRevision)
		case record.Revision == activeRevision && record.Digest != activeDigest:
			return fmt.Errorf("policy revision %d is already active with different content", record.Revision)
		case record.Digest == activeDigest:
			if activeAPIVersion != record.APIVersion || activeName != record.Name ||
				activeNamespace != record.Namespace || activeRevision != record.Revision ||
				activeSource != record.Source || activeDocument != record.DocumentJSON {
				return fmt.Errorf("active policy digest conflicts with different document metadata")
			}
			return tx.Commit()
		}
	}
	if record.AppliedAt.IsZero() {
		record.AppliedAt = time.Now().UTC()
	}
	if _, err := tx.Exec(`INSERT INTO policy_documents
		(applied_at,api_version,name,namespace,revision,digest,source,document_json) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(digest) DO NOTHING`, record.AppliedAt.UTC().Format(policyTimeFormat),
		record.APIVersion, record.Name, record.Namespace, record.Revision, record.Digest, record.Source, record.DocumentJSON); err != nil {
		return err
	}
	var id int64
	var storedAPIVersion, storedName, storedNamespace, storedDigest, storedSource, storedDocument string
	var storedRevision int64
	if err := tx.QueryRow(`SELECT id,api_version,name,namespace,revision,digest,source,document_json
		FROM policy_documents WHERE digest=?`, record.Digest).
		Scan(&id, &storedAPIVersion, &storedName, &storedNamespace, &storedRevision,
			&storedDigest, &storedSource, &storedDocument); err != nil {
		return err
	}
	if storedAPIVersion != record.APIVersion || storedName != record.Name || storedNamespace != record.Namespace ||
		storedRevision != record.Revision || storedDigest != record.Digest || storedSource != record.Source ||
		storedDocument != record.DocumentJSON {
		return fmt.Errorf("stored policy digest conflicts with different document metadata")
	}
	if _, err := tx.Exec(`INSERT INTO policy_active(slot,document_id) VALUES(1,?)
		ON CONFLICT(slot) DO UPDATE SET document_id=excluded.document_id`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetPolicyLineage is the only local operation that can remove the active
// pointer and thereby permit a namespace reset. Both the currently observed
// digest and source are mandatory compare-and-swap preconditions. Takeover is
// a distinct audited action and is required when an external connector owns
// the active document; a normal reset can only clear a local lineage.
func (s *Store) ResetPolicyLineage(request PolicyLineageReset) error {
	request.Actor = strings.TrimSpace(request.Actor)
	request.Reason = strings.TrimSpace(request.Reason)
	request.ExpectedSource = strings.TrimSpace(request.ExpectedSource)
	if !validPolicyAuditText(request.Actor, 256) || !validPolicyAuditText(request.Reason, 1024) ||
		!validPolicyAuditText(request.ExpectedSource, 256) || !validDigest(request.ExpectedDigest) {
		return fmt.Errorf("policy lineage reset requires a safe actor, reason, expected source, and expected digest")
	}
	if request.At.IsZero() {
		request.At = time.Now().UTC()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var documentID int64
	var namespace, source, digest string
	err = tx.QueryRow(`SELECT d.id,d.namespace,d.source,d.digest
		FROM policy_active a JOIN policy_documents d ON d.id=a.document_id WHERE a.slot=1`).
		Scan(&documentID, &namespace, &source, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no active policy lineage to reset")
	}
	if err != nil {
		return err
	}
	if digest != request.ExpectedDigest || source != request.ExpectedSource {
		return fmt.Errorf("active policy changed; expected source %q digest %s, found source %q digest %s",
			request.ExpectedSource, request.ExpectedDigest, source, digest)
	}
	action := "reset"
	if request.Takeover {
		action = "takeover"
		if source == "local" {
			return fmt.Errorf("active policy is already locally owned; use a lineage reset instead of takeover")
		}
	} else if source != "local" {
		return fmt.Errorf("active policy is owned by %q; explicit takeover is required", source)
	}
	if _, err := tx.Exec(`DELETE FROM policy_active WHERE slot=1 AND document_id=?`, documentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO policy_lineage_events
		(ts,action,actor,reason,previous_document_id,previous_namespace,previous_source,previous_digest)
		VALUES (?,?,?,?,?,?,?,?)`, request.At.UTC().Format(policyTimeFormat), action, request.Actor, request.Reason,
		documentID, namespace, source, digest); err != nil {
		return err
	}
	return tx.Commit()
}

func validPolicyAuditText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			return false
		}
	}
	return true
}

func (s *Store) PolicyLineageEvents(limit int) ([]PolicyLineageEvent, error) {
	if limit < 1 || limit > 10_000 {
		return nil, fmt.Errorf("policy lineage event limit must be between 1 and 10000")
	}
	rows, err := s.readQueryer().Query(`SELECT id,ts,action,actor,reason,previous_document_id,
		previous_namespace,previous_source,previous_digest FROM policy_lineage_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PolicyLineageEvent, 0)
	for rows.Next() {
		var event PolicyLineageEvent
		var ts string
		if err := rows.Scan(&event.ID, &ts, &event.Action, &event.Actor, &event.Reason, &event.PreviousDocument,
			&event.PreviousNamespace, &event.PreviousSource, &event.PreviousDigest); err != nil {
			return nil, err
		}
		event.Ts, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, event)
	}
	return out, rows.Err()
}

func validatePolicyDocumentContent(record PolicyDocumentRecord) error {
	if record.Revision < 1 || record.APIVersion == "" || record.Name == "" || record.Namespace == "" ||
		!validDigest(record.Digest) || len(record.DocumentJSON) == 0 || len(record.DocumentJSON) > 1<<20 {
		return fmt.Errorf("policy document metadata is invalid")
	}
	raw := []byte(record.DocumentJSON)
	if !utf8.Valid(raw) {
		return fmt.Errorf("policy document is not valid UTF-8")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	if digest != record.Digest {
		return fmt.Errorf("policy document digest does not match its content")
	}
	if err := scanStoredDownshiftJSONDocument(raw); err != nil {
		return fmt.Errorf("policy document JSON is ambiguous: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("decode policy document metadata: %w", err)
	}
	allowedTop := map[string]struct{}{
		"apiVersion": {}, "kind": {}, "metadata": {}, "mode": {}, "rules": {},
	}
	if len(top) != len(allowedTop) {
		return fmt.Errorf("policy document has missing or unknown top-level fields")
	}
	for key := range top {
		if _, ok := allowedTop[key]; !ok {
			return fmt.Errorf("policy document has non-canonical top-level field %q", key)
		}
	}
	var apiVersion, kind string
	if json.Unmarshal(top["apiVersion"], &apiVersion) != nil || json.Unmarshal(top["kind"], &kind) != nil {
		return fmt.Errorf("policy document type fields are malformed")
	}
	if kind != "PolicySet" {
		return fmt.Errorf("policy document kind is invalid")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(top["metadata"], &metadata); err != nil {
		return fmt.Errorf("policy document metadata is malformed")
	}
	if len(metadata) != 3 || metadata["name"] == nil || metadata["namespace"] == nil || metadata["revision"] == nil {
		return fmt.Errorf("policy document metadata has missing or unknown fields")
	}
	var name, namespace string
	var revision int64
	if json.Unmarshal(metadata["name"], &name) != nil || json.Unmarshal(metadata["namespace"], &namespace) != nil ||
		json.Unmarshal(metadata["revision"], &revision) != nil {
		return fmt.Errorf("policy document metadata fields are malformed")
	}
	if apiVersion != record.APIVersion || name != record.Name || namespace != record.Namespace || revision != record.Revision {
		return fmt.Errorf("policy document metadata does not match its activation record")
	}
	return nil
}

func (s *Store) ActivePolicyDocument() (*PolicyDocumentRecord, error) {
	var out PolicyDocumentRecord
	var applied string
	err := s.readQueryer().QueryRow(`SELECT d.id,d.applied_at,d.api_version,d.name,d.namespace,d.revision,d.digest,d.source,d.document_json
		FROM policy_active a JOIN policy_documents d ON d.id=a.document_id WHERE a.slot=1`).
		Scan(&out.ID, &applied, &out.APIVersion, &out.Name, &out.Namespace, &out.Revision, &out.Digest, &out.Source, &out.DocumentJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.AppliedAt, _ = time.Parse(time.RFC3339Nano, applied)
	return &out, nil
}

type PolicyDecisionRecord struct {
	Ts              time.Time
	PolicyDigest    string
	PolicyRevision  int64
	PolicyName      string
	PolicyNamespace string
	Mode            string
	Outcome         string
	Admitted        bool
	HTTPStatus      int
	Confidence      string
	ContextJSON     string
	ExplanationJSON string
	Rules           []PolicyDecisionRule
}

type PolicyDecisionRule struct {
	RuleID                string
	Accepted              bool
	EstimatedTokens       int64
	EstimatedInputTokens  int64
	EstimatedOutputTokens int64
	EstimatedTotalTokens  int64
	EstimatedCostMicroUSD int64
}

// InsertPolicyDecision commits the explanation and all rule-counter entries in
// one transaction. The returned ID can be linked from the eventual request row.
func (s *Store) InsertPolicyDecision(record PolicyDecisionRecord) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ts := record.Ts.UTC().Format(policyTimeFormat)
	res, err := tx.Exec(`INSERT INTO policy_decisions
		(ts,policy_digest,policy_revision,policy_name,policy_namespace,mode,outcome,admitted,http_status,confidence,context_json,explanation_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, ts, record.PolicyDigest, record.PolicyRevision,
		record.PolicyName, record.PolicyNamespace, record.Mode, record.Outcome, b2i(record.Admitted), record.HTTPStatus, record.Confidence,
		record.ContextJSON, record.ExplanationJSON)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	const ruleChunk = 200
	for start := 0; start < len(record.Rules); start += ruleChunk {
		end := min(start+ruleChunk, len(record.Rules))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*11)
		for _, rule := range record.Rules[start:end] {
			if rule.EstimatedTokens < 0 || rule.EstimatedInputTokens < 0 || rule.EstimatedOutputTokens < 0 ||
				rule.EstimatedTotalTokens < 0 || rule.EstimatedCostMicroUSD < 0 {
				return 0, fmt.Errorf("policy decision estimated counters must be non-negative")
			}
			values = append(values, "(?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, id, ts, record.PolicyDigest, record.PolicyNamespace, rule.RuleID, b2i(rule.Accepted),
				rule.EstimatedTokens, rule.EstimatedInputTokens, rule.EstimatedOutputTokens,
				rule.EstimatedTotalTokens, rule.EstimatedCostMicroUSD)
		}
		if _, err := tx.Exec(`INSERT INTO policy_decision_rules
			(decision_id,ts,policy_digest,policy_namespace,rule_id,accepted,estimated_tokens,
			estimated_input_tokens,estimated_output_tokens,estimated_total_tokens,estimated_cost_microusd) VALUES `+
			strings.Join(values, ","), args...); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

type PolicyRuleUsage struct {
	Requests     int64
	Tokens       int64
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	CostMicroUSD int64
}

type PolicyDecisionSummary struct {
	Total       int64 `json:"total"`
	Allowed     int64 `json:"allowed"`
	Denied      int64 `json:"denied"`
	Admitted    int64 `json:"admitted"`
	EnforceMode int64 `json:"enforce_mode"`
}

func (s *Store) PolicyDecisionsSince(since time.Time) (PolicyDecisionSummary, error) {
	var out PolicyDecisionSummary
	err := s.readQueryer().QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN outcome='allow' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN outcome='deny' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(admitted),0),
		COALESCE(SUM(CASE WHEN mode='enforce' THEN 1 ELSE 0 END),0)
		FROM policy_decisions WHERE ts>=?`, since.UTC().Format(policyTimeFormat)).
		Scan(&out.Total, &out.Allowed, &out.Denied, &out.Admitted, &out.EnforceMode)
	return out, err
}

// PolicyCoverageLedger is a content-free coverage aggregate over routed
// request receipts. It deliberately distinguishes an evaluated request from a
// trusted identity; neither should be inferred from the other.
type PolicyCoverageLedger struct {
	RoutedRequests          int64 `json:"routed_requests"`
	EvaluatedRequests       int64 `json:"evaluated_requests"`
	EnforceModeRequests     int64 `json:"enforce_mode_requests"`
	TrustedIdentityRequests int64 `json:"trusted_identity_requests"`
	SelfReportedRequests    int64 `json:"self_reported_identity_requests"`
	UnverifiedRequests      int64 `json:"unverified_identity_requests"`
}

func (s *Store) PolicyCoverageSince(since time.Time) (PolicyCoverageLedger, error) {
	var out PolicyCoverageLedger
	err := s.readQueryer().QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN r.policy_decision_id > 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN d.mode='enforce' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.identity_confidence='authenticated' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.identity_confidence='self_reported' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.identity_confidence NOT IN ('authenticated','self_reported') THEN 1 ELSE 0 END),0)
		FROM requests r LEFT JOIN policy_decisions d ON d.id=r.policy_decision_id
		WHERE r.ts>=?`, since.UTC().Format(time.RFC3339)).Scan(
		&out.RoutedRequests, &out.EvaluatedRequests, &out.EnforceModeRequests,
		&out.TrustedIdentityRequests, &out.SelfReportedRequests, &out.UnverifiedRequests)
	return out, err
}

func (s *Store) PolicyRuleUsageSince(namespace, ruleID string, since time.Time) (PolicyRuleUsage, error) {
	rows, err := s.readQueryer().Query(`SELECT estimated_tokens,estimated_input_tokens,estimated_output_tokens,
		estimated_total_tokens,estimated_cost_microusd FROM policy_decision_rules
		WHERE policy_namespace=? AND rule_id=? AND accepted=1 AND ts>=?`,
		namespace, ruleID, since.UTC().Format(policyTimeFormat))
	if err != nil {
		return PolicyRuleUsage{}, err
	}
	defer rows.Close()
	var out PolicyRuleUsage
	for rows.Next() {
		var tokens, input, output, total, cost int64
		if err := rows.Scan(&tokens, &input, &output, &total, &cost); err != nil {
			return PolicyRuleUsage{}, err
		}
		if out.Requests < math.MaxInt64 {
			out.Requests++
		}
		if tokens > math.MaxInt64-out.Tokens {
			out.Tokens = math.MaxInt64
		} else {
			out.Tokens += tokens
		}
		out.InputTokens = saturatingPolicyCounterAdd(out.InputTokens, input)
		out.OutputTokens = saturatingPolicyCounterAdd(out.OutputTokens, output)
		out.TotalTokens = saturatingPolicyCounterAdd(out.TotalTokens, total)
		out.CostMicroUSD = saturatingPolicyCounterAdd(out.CostMicroUSD, cost)
	}
	return out, rows.Err()
}

func saturatingPolicyCounterAdd(current, value int64) int64 {
	if value < 0 || current > math.MaxInt64-value {
		return math.MaxInt64
	}
	return current + value
}
