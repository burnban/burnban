package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const downshiftSchema = `
CREATE TABLE IF NOT EXISTS downshift_simulations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at TEXT NOT NULL,
	config_digest TEXT NOT NULL,
	since_at TEXT NOT NULL,
	through_at TEXT NOT NULL,
	total_requests INTEGER NOT NULL CHECK(total_requests >= 0),
	matched_requests INTEGER NOT NULL CHECK(matched_requests >= 0),
	eligible_requests INTEGER NOT NULL CHECK(eligible_requests >= 0),
	indeterminate_requests INTEGER NOT NULL CHECK(indeterminate_requests >= 0),
	source_cost_usd REAL NOT NULL CHECK(source_cost_usd >= 0),
	target_cost_usd REAL NOT NULL CHECK(target_cost_usd >= 0),
	report_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_downshift_simulation_digest
	ON downshift_simulations(config_digest, created_at);
CREATE TABLE IF NOT EXISTS downshift_documents (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	applied_at TEXT NOT NULL,
	api_version TEXT NOT NULL,
	revision INTEGER NOT NULL CHECK(revision >= 1),
	digest TEXT NOT NULL UNIQUE,
	mode TEXT NOT NULL,
	document_json TEXT NOT NULL,
	simulation_id INTEGER REFERENCES downshift_simulations(id),
	forced INTEGER NOT NULL DEFAULT 0 CHECK(forced IN (0,1)),
	force_reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS downshift_active (
	slot INTEGER PRIMARY KEY CHECK(slot = 1),
	document_id INTEGER NOT NULL REFERENCES downshift_documents(id)
);
CREATE TABLE IF NOT EXISTS downshift_audit (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL,
	action TEXT NOT NULL,
	config_digest TEXT NOT NULL,
	config_revision INTEGER NOT NULL,
	simulation_id INTEGER,
	forced INTEGER NOT NULL CHECK(forced IN (0,1)),
	reason TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_downshift_audit_ts ON downshift_audit(ts);
CREATE TRIGGER IF NOT EXISTS downshift_simulations_no_update
	BEFORE UPDATE ON downshift_simulations BEGIN SELECT RAISE(ABORT,'downshift simulations are immutable'); END;
CREATE TRIGGER IF NOT EXISTS downshift_simulations_no_delete
	BEFORE DELETE ON downshift_simulations BEGIN SELECT RAISE(ABORT,'downshift simulations are immutable'); END;
CREATE TRIGGER IF NOT EXISTS downshift_documents_no_update
	BEFORE UPDATE ON downshift_documents BEGIN SELECT RAISE(ABORT,'downshift documents are immutable'); END;
CREATE TRIGGER IF NOT EXISTS downshift_documents_no_delete
	BEFORE DELETE ON downshift_documents BEGIN SELECT RAISE(ABORT,'downshift documents are immutable'); END;
CREATE TRIGGER IF NOT EXISTS downshift_audit_no_update
	BEFORE UPDATE ON downshift_audit BEGIN SELECT RAISE(ABORT,'downshift audit is immutable'); END;
CREATE TRIGGER IF NOT EXISTS downshift_audit_no_delete
	BEFORE DELETE ON downshift_audit BEGIN SELECT RAISE(ABORT,'downshift audit is immutable'); END;
`

type DownshiftSimulationRecord struct {
	ID                    int64     `json:"id"`
	CreatedAt             time.Time `json:"created_at"`
	ConfigDigest          string    `json:"config_digest"`
	Since                 time.Time `json:"since"`
	Through               time.Time `json:"through"`
	TotalRequests         int64     `json:"total_requests"`
	MatchedRequests       int64     `json:"matched_requests"`
	EligibleRequests      int64     `json:"eligible_requests"`
	IndeterminateRequests int64     `json:"indeterminate_requests"`
	SourceCostUSD         float64   `json:"source_cost_usd"`
	TargetCostUSD         float64   `json:"target_cost_usd"`
	ReportJSON            string    `json:"report"`
}

type DownshiftDocumentRecord struct {
	ID           int64     `json:"id"`
	AppliedAt    time.Time `json:"applied_at"`
	APIVersion   string    `json:"api_version"`
	Revision     int64     `json:"revision"`
	Digest       string    `json:"digest"`
	Mode         string    `json:"mode"`
	DocumentJSON string    `json:"document"`
	SimulationID int64     `json:"simulation_id,omitempty"`
	Forced       bool      `json:"forced"`
	ForceReason  string    `json:"force_reason,omitempty"`
}

func (s *Store) InsertDownshiftSimulation(record DownshiftSimulationRecord) (int64, error) {
	if !validDigest(record.ConfigDigest) {
		return 0, fmt.Errorf("downshift simulation config digest is invalid")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if record.Since.IsZero() || record.Through.IsZero() || record.Through.Before(record.Since) {
		return 0, fmt.Errorf("downshift simulation window is invalid")
	}
	for _, value := range []int64{record.TotalRequests, record.MatchedRequests, record.EligibleRequests, record.IndeterminateRequests} {
		if value < 0 {
			return 0, fmt.Errorf("downshift simulation counts must be non-negative")
		}
	}
	if record.MatchedRequests > record.TotalRequests || record.EligibleRequests > record.MatchedRequests || record.IndeterminateRequests > record.MatchedRequests {
		return 0, fmt.Errorf("downshift simulation counts are inconsistent")
	}
	if invalidMoney(record.SourceCostUSD) || invalidMoney(record.TargetCostUSD) {
		return 0, fmt.Errorf("downshift simulation costs must be finite and non-negative")
	}
	if len(record.ReportJSON) == 0 || len(record.ReportJSON) > 1<<20 {
		return 0, fmt.Errorf("downshift simulation report must be between 1 byte and 1 MiB")
	}
	res, err := s.db.Exec(`INSERT INTO downshift_simulations
		(created_at,config_digest,since_at,through_at,total_requests,matched_requests,
		 eligible_requests,indeterminate_requests,source_cost_usd,target_cost_usd,report_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, record.CreatedAt.UTC().Format(policyTimeFormat), record.ConfigDigest,
		record.Since.UTC().Format(policyTimeFormat), record.Through.UTC().Format(policyTimeFormat),
		record.TotalRequests, record.MatchedRequests, record.EligibleRequests, record.IndeterminateRequests,
		record.SourceCostUSD, record.TargetCostUSD, record.ReportJSON)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) LatestDownshiftSimulation(digest string) (*DownshiftSimulationRecord, error) {
	var out DownshiftSimulationRecord
	var created, since, through string
	err := s.readQueryer().QueryRow(`SELECT id,created_at,config_digest,since_at,through_at,total_requests,
		matched_requests,eligible_requests,indeterminate_requests,source_cost_usd,target_cost_usd,report_json
		FROM downshift_simulations WHERE config_digest=? ORDER BY id DESC LIMIT 1`, digest).
		Scan(&out.ID, &created, &out.ConfigDigest, &since, &through, &out.TotalRequests,
			&out.MatchedRequests, &out.EligibleRequests, &out.IndeterminateRequests,
			&out.SourceCostUSD, &out.TargetCostUSD, &out.ReportJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	out.Since, _ = time.Parse(time.RFC3339Nano, since)
	out.Through, _ = time.Parse(time.RFC3339Nano, through)
	return &out, nil
}

// ApplyDownshiftDocument appends a version and atomically activates it.
// Enforcing mode requires a material historical simulation for the exact
// digest unless an operator supplies a durable force reason.
func (s *Store) ApplyDownshiftDocument(record DownshiftDocumentRecord) error {
	if record.Revision < 1 || !validDigest(record.Digest) || record.APIVersion == "" || len(record.DocumentJSON) == 0 || len(record.DocumentJSON) > 1<<20 {
		return fmt.Errorf("downshift document metadata is invalid")
	}
	if record.Mode != "observe" && record.Mode != "warn_then_downshift" {
		return fmt.Errorf("downshift document mode is invalid")
	}
	if err := validateDownshiftDocumentContent(record); err != nil {
		return err
	}
	if record.AppliedAt.IsZero() {
		record.AppliedAt = time.Now().UTC()
	}
	if record.Forced {
		if err := validateForceReason(record.ForceReason); err != nil {
			return err
		}
	} else if record.ForceReason != "" {
		return fmt.Errorf("force reason requires forced=true")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var activeRevision int64
	var activeAPIVersion, activeDigest, activeMode, activeDocument string
	err = tx.QueryRow(`SELECT d.api_version,d.revision,d.digest,d.mode,d.document_json FROM downshift_active a
		JOIN downshift_documents d ON d.id=a.document_id WHERE a.slot=1`).
		Scan(&activeAPIVersion, &activeRevision, &activeDigest, &activeMode, &activeDocument)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		switch {
		case record.Revision < activeRevision:
			return fmt.Errorf("downshift revision %d is older than active revision %d", record.Revision, activeRevision)
		case record.Revision == activeRevision && record.Digest != activeDigest:
			return fmt.Errorf("downshift revision %d is already active with different content", record.Revision)
		case record.Digest == activeDigest:
			if activeAPIVersion != record.APIVersion || activeRevision != record.Revision ||
				activeMode != record.Mode || activeDocument != record.DocumentJSON {
				return fmt.Errorf("active downshift digest conflicts with different document metadata")
			}
			return tx.Commit()
		}
	}

	if record.Mode == "warn_then_downshift" && !record.Forced {
		var simulationDigest string
		var simulationCreated, simulationThrough string
		var total, matched, eligible int64
		if record.SimulationID == 0 {
			return fmt.Errorf("enabling downshift requires a historical simulation for this exact config or an explicit force reason")
		}
		if err := tx.QueryRow(`SELECT config_digest,created_at,through_at,total_requests,matched_requests,eligible_requests
			FROM downshift_simulations WHERE id=?`, record.SimulationID).
			Scan(&simulationDigest, &simulationCreated, &simulationThrough, &total, &matched, &eligible); err != nil {
			return fmt.Errorf("load downshift simulation: %w", err)
		}
		if simulationDigest != record.Digest || total == 0 || matched == 0 || eligible == 0 {
			return fmt.Errorf("simulation must match the config and quantify at least one eligible historical request; use --force with an audit reason only after review")
		}
		createdAt, createdErr := time.Parse(time.RFC3339Nano, simulationCreated)
		throughAt, throughErr := time.Parse(time.RFC3339Nano, simulationThrough)
		if createdErr != nil || throughErr != nil || createdAt.After(record.AppliedAt.Add(5*time.Minute)) ||
			record.AppliedAt.Sub(createdAt) > 24*time.Hour || record.AppliedAt.Sub(throughAt) > 24*time.Hour {
			return fmt.Errorf("simulation is stale or has invalid timestamps; run a fresh exact-digest simulation")
		}
	}

	if _, err := tx.Exec(`INSERT INTO downshift_documents
		(applied_at,api_version,revision,digest,mode,document_json,simulation_id,forced,force_reason)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(digest) DO NOTHING`,
		record.AppliedAt.UTC().Format(policyTimeFormat), record.APIVersion, record.Revision,
		record.Digest, record.Mode, record.DocumentJSON, nullableID(record.SimulationID), b2i(record.Forced), record.ForceReason); err != nil {
		return err
	}
	var id int64
	var storedAPIVersion, storedDigest, storedMode, storedDocument string
	var storedRevision int64
	if err := tx.QueryRow(`SELECT id,api_version,revision,digest,mode,document_json
		FROM downshift_documents WHERE digest=?`, record.Digest).
		Scan(&id, &storedAPIVersion, &storedRevision, &storedDigest, &storedMode, &storedDocument); err != nil {
		return err
	}
	if storedAPIVersion != record.APIVersion || storedRevision != record.Revision || storedDigest != record.Digest ||
		storedMode != record.Mode || storedDocument != record.DocumentJSON {
		return fmt.Errorf("stored downshift digest conflicts with different document metadata")
	}
	if _, err := tx.Exec(`INSERT INTO downshift_active(slot,document_id) VALUES(1,?)
		ON CONFLICT(slot) DO UPDATE SET document_id=excluded.document_id`, id); err != nil {
		return err
	}
	reason := "historical simulation reviewed"
	if record.Mode == "observe" {
		reason = "observe-only configuration activated"
	}
	if record.Forced {
		reason = record.ForceReason
	}
	if _, err := tx.Exec(`INSERT INTO downshift_audit
		(ts,action,config_digest,config_revision,simulation_id,forced,reason) VALUES(?,?,?,?,?,?,?)`,
		record.AppliedAt.UTC().Format(policyTimeFormat), "apply", record.Digest, record.Revision,
		nullableID(record.SimulationID), b2i(record.Forced), reason); err != nil {
		return err
	}
	return tx.Commit()
}

func validateDownshiftDocumentContent(record DownshiftDocumentRecord) error {
	raw := []byte(record.DocumentJSON)
	if !utf8.Valid(raw) {
		return fmt.Errorf("downshift document is not valid UTF-8")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	if digest != record.Digest {
		return fmt.Errorf("downshift document digest does not match its content")
	}
	if err := scanStoredDownshiftJSONDocument(raw); err != nil {
		return fmt.Errorf("downshift document JSON is ambiguous: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("decode downshift document metadata: %w", err)
	}
	allowed := map[string]struct{}{
		"api_version": {}, "revision": {}, "mode": {}, "warn_at_pct": {},
		"downshift_at_pct": {}, "downshift_on_denial": {}, "rules": {},
	}
	if len(top) != len(allowed) {
		return fmt.Errorf("downshift document has missing or unknown top-level fields")
	}
	for key := range top {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("downshift document has non-canonical top-level field %q", key)
		}
	}
	var apiVersion, mode string
	var revision int64
	if json.Unmarshal(top["api_version"], &apiVersion) != nil || json.Unmarshal(top["revision"], &revision) != nil ||
		json.Unmarshal(top["mode"], &mode) != nil {
		return fmt.Errorf("downshift document metadata fields are malformed")
	}
	if apiVersion != record.APIVersion || revision != record.Revision || mode != record.Mode {
		return fmt.Errorf("downshift document metadata does not match its activation record")
	}
	return nil
}

func scanStoredDownshiftJSONDocument(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := scanStoredDownshiftJSONValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func scanStoredDownshiftJSONValue(dec *json.Decoder, depth int) error {
	if depth > 128 {
		return fmt.Errorf("nesting exceeds 128 levels")
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]string)
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			folded := strings.ToLower(key)
			if previous, exists := seen[folded]; exists {
				return fmt.Errorf("duplicate or case-ambiguous fields %q and %q", previous, key)
			}
			seen[folded] = key
			if err := scanStoredDownshiftJSONValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := scanStoredDownshiftJSONValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func (s *Store) ActiveDownshiftDocument() (*DownshiftDocumentRecord, error) {
	var out DownshiftDocumentRecord
	var applied string
	var simulation sql.NullInt64
	var forced int
	err := s.readQueryer().QueryRow(`SELECT d.id,d.applied_at,d.api_version,d.revision,d.digest,d.mode,
		d.document_json,d.simulation_id,d.forced,d.force_reason FROM downshift_active a
		JOIN downshift_documents d ON d.id=a.document_id WHERE a.slot=1`).
		Scan(&out.ID, &applied, &out.APIVersion, &out.Revision, &out.Digest, &out.Mode,
			&out.DocumentJSON, &simulation, &forced, &out.ForceReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.AppliedAt, _ = time.Parse(time.RFC3339Nano, applied)
	out.SimulationID, out.Forced = simulation.Int64, forced != 0
	return &out, nil
}

func (s *Store) DisableDownshift(now time.Time, reason string) error {
	if err := validateForceReason(reason); err != nil {
		return fmt.Errorf("disable reason: %w", err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var digest string
	var revision int64
	err = tx.QueryRow(`SELECT d.digest,d.revision FROM downshift_active a
		JOIN downshift_documents d ON d.id=a.document_id WHERE a.slot=1`).Scan(&digest, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM downshift_active WHERE slot=1`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO downshift_audit
		(ts,action,config_digest,config_revision,simulation_id,forced,reason) VALUES(?,?,?,?,NULL,0,?)`,
		now.UTC().Format(policyTimeFormat), "disable", digest, revision, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func invalidMoney(value float64) bool {
	return value < 0 || value > 1e12 || math.IsNaN(value) || math.IsInf(value, 0)
}

func validateForceReason(value string) error {
	if len(value) < 10 || len(value) > 500 || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return fmt.Errorf("--force-reason must be a trimmed UTF-8 explanation between 10 and 500 bytes")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control) {
			return fmt.Errorf("--force-reason contains a forbidden control character")
		}
	}
	return nil
}
