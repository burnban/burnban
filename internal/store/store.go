// Package store persists every proxied request to a local SQLite database
// and answers the aggregate questions the CLI asks of it.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL,
	provider TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	agent TEXT NOT NULL DEFAULT '',
	session TEXT NOT NULL DEFAULT '',
	in_tokens INTEGER NOT NULL DEFAULT 0,
	out_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_1h_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	status INTEGER NOT NULL DEFAULT 0,
	streamed INTEGER NOT NULL DEFAULT 0,
	estimated INTEGER NOT NULL DEFAULT 0,
	priced INTEGER NOT NULL DEFAULT 1,
	body_hash TEXT NOT NULL DEFAULT '',
	usage_state TEXT NOT NULL DEFAULT 'missing',
	pricing_state TEXT NOT NULL DEFAULT 'unmetered',
	incomplete INTEGER NOT NULL DEFAULT 0,
	enforcement_unsafe INTEGER NOT NULL DEFAULT 0,
	route TEXT NOT NULL DEFAULT '',
	service_tier TEXT NOT NULL DEFAULT '',
	inference_geo TEXT NOT NULL DEFAULT '',
	server_tool_calls INTEGER NOT NULL DEFAULT 0,
	fee_unpriced INTEGER NOT NULL DEFAULT 0,
	cost_source TEXT NOT NULL DEFAULT 'unknown',
	cost_source_ref TEXT NOT NULL DEFAULT '',
	cost_effective_from TEXT NOT NULL DEFAULT '',
	cost_valid_through TEXT NOT NULL DEFAULT '',
	cost_confidence TEXT NOT NULL DEFAULT 'unknown',
	identity_tenant TEXT NOT NULL DEFAULT '',
	identity_device TEXT NOT NULL DEFAULT '',
	principal TEXT NOT NULL DEFAULT '',
	service_account TEXT NOT NULL DEFAULT '',
	project TEXT NOT NULL DEFAULT '',
	cost_center TEXT NOT NULL DEFAULT '',
	identity_confidence TEXT NOT NULL DEFAULT 'unverified',
	requested_provider TEXT NOT NULL DEFAULT '',
	requested_model TEXT NOT NULL DEFAULT '',
	requested_route TEXT NOT NULL DEFAULT '',
	downshift_action TEXT NOT NULL DEFAULT 'none',
	downshift_rule TEXT NOT NULL DEFAULT '',
	downshift_trigger TEXT NOT NULL DEFAULT '',
	downshift_reason TEXT NOT NULL DEFAULT '',
	downshift_config_digest TEXT NOT NULL DEFAULT '',
	downshift_features TEXT NOT NULL DEFAULT '',
	downshift_source_estimated_usd REAL NOT NULL DEFAULT 0,
	downshift_target_estimated_usd REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
-- Covering indexes: budget checks SUM cost over time (and agent) ranges on
-- every request, so the sums must never leave the index for the table.
CREATE INDEX IF NOT EXISTS idx_requests_ts_cost ON requests(ts, cost_usd);
CREATE INDEX IF NOT EXISTS idx_requests_agent_ts_cost ON requests(agent, ts, cost_usd);
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS runtime_leases (
	name TEXT PRIMARY KEY,
	owner TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS identity_nonces (
	key_id TEXT NOT NULL,
	jti TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	PRIMARY KEY (key_id,jti)
);
CREATE INDEX IF NOT EXISTS idx_identity_nonces_expiry ON identity_nonces(expires_at);
`

type Store struct {
	db                *sql.DB
	snapshotReader    readQueryer
	requestMutationMu sync.Mutex
	reconciliationMu  sync.Mutex
	requestRevision   atomic.Uint64
}

func Open(path string) (*Store, error) {
	diskPath, hasDiskPath, err := sqliteDiskPath(path)
	if err != nil {
		return nil, err
	}
	if hasDiskPath {
		dir := filepath.Dir(diskPath)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		// The ledger includes agent/session names and may contain webhook URLs.
		// Pre-create and re-mode it so the process umask cannot make that data
		// readable by other local users. SQLite gives WAL/SHM files the DB mode.
		if _, statErr := os.Stat(diskPath); statErr == nil {
			if err := os.Chmod(diskPath, 0o600); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(statErr) {
			return nil, statErr
		} else if sqliteURIMode(path) != "ro" && sqliteURIMode(path) != "rw" {
			f, err := os.OpenFile(diskPath, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				return nil, err
			}
			if err := f.Close(); err != nil {
				return nil, err
			}
		}
	}
	// WAL + synchronous=NORMAL: commits append to the log without an fsync
	// each — the standard WAL setup. Worst case on an OS crash is losing
	// the final moments of the ledger, never corrupting it.
	db, err := sql.Open("sqlite", sqliteDSN(path, "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"))
	if err != nil {
		return nil, err
	}
	var existingRequestsTable int
	if err := db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM sqlite_master WHERE type='table' AND name='requests')`).Scan(&existingRequestsTable); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(policySchema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migratePolicySchema(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(reconciliationSchema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateReconciliationSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(optimizationSchema); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(downshiftSchema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateRequests(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateBodyHashes(db, existingRequestsTable != 0); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// sqliteDiskPath extracts the actual filesystem path from either a plain
// filename or SQLite's file: URI syntax. Treating a URI as a filepath would
// create stray literal "file:" directories and skip permission hardening.
func sqliteDiskPath(dsn string) (string, bool, error) {
	if dsn == ":memory:" {
		return "", false, nil
	}
	if !strings.HasPrefix(dsn, "file:") {
		return dsn, dsn != "", nil
	}
	if sqliteURIMode(dsn) == "memory" {
		return "", false, nil
	}
	raw := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" || strings.HasPrefix(raw, ":memory:") {
		return "", false, nil
	}
	path, err := url.PathUnescape(raw)
	if err != nil {
		return "", false, fmt.Errorf("invalid sqlite file URI: %w", err)
	}
	// file:///absolute/path cleans to /absolute/path on Unix while preserving
	// relative file:name.db and Windows drive paths.
	return filepath.Clean(path), true, nil
}

func sqliteURIMode(dsn string) string {
	if !strings.HasPrefix(dsn, "file:") {
		return ""
	}
	i := strings.IndexByte(dsn, '?')
	if i < 0 {
		return ""
	}
	values, err := url.ParseQuery(dsn[i+1:])
	if err != nil {
		return ""
	}
	return values.Get("mode")
}

// migrateRequests upgrades ledgers created by older Burnban releases in
// place. CREATE TABLE IF NOT EXISTS does not add columns to an existing
// SQLite table, so every additive schema change must be explicit here.
func migrateRequests(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(requests)`)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	additions := []struct {
		name string
		sql  string
	}{
		{"usage_state", `ALTER TABLE requests ADD COLUMN usage_state TEXT NOT NULL DEFAULT 'legacy'`},
		{"pricing_state", `ALTER TABLE requests ADD COLUMN pricing_state TEXT NOT NULL DEFAULT 'legacy'`},
		{"incomplete", `ALTER TABLE requests ADD COLUMN incomplete INTEGER NOT NULL DEFAULT 0`},
		{"enforcement_unsafe", `ALTER TABLE requests ADD COLUMN enforcement_unsafe INTEGER NOT NULL DEFAULT 0`},
		{"route", `ALTER TABLE requests ADD COLUMN route TEXT NOT NULL DEFAULT ''`},
		{"cache_write_1h_tokens", `ALTER TABLE requests ADD COLUMN cache_write_1h_tokens INTEGER NOT NULL DEFAULT 0`},
		{"service_tier", `ALTER TABLE requests ADD COLUMN service_tier TEXT NOT NULL DEFAULT ''`},
		{"inference_geo", `ALTER TABLE requests ADD COLUMN inference_geo TEXT NOT NULL DEFAULT ''`},
		{"server_tool_calls", `ALTER TABLE requests ADD COLUMN server_tool_calls INTEGER NOT NULL DEFAULT 0`},
		{"fee_unpriced", `ALTER TABLE requests ADD COLUMN fee_unpriced INTEGER NOT NULL DEFAULT 0`},
		{"policy_decision_id", `ALTER TABLE requests ADD COLUMN policy_decision_id INTEGER NOT NULL DEFAULT 0`},
		{"cost_source", `ALTER TABLE requests ADD COLUMN cost_source TEXT NOT NULL DEFAULT 'legacy_unknown'`},
		{"cost_source_ref", `ALTER TABLE requests ADD COLUMN cost_source_ref TEXT NOT NULL DEFAULT ''`},
		{"cost_effective_from", `ALTER TABLE requests ADD COLUMN cost_effective_from TEXT NOT NULL DEFAULT ''`},
		{"cost_valid_through", `ALTER TABLE requests ADD COLUMN cost_valid_through TEXT NOT NULL DEFAULT ''`},
		{"cost_confidence", `ALTER TABLE requests ADD COLUMN cost_confidence TEXT NOT NULL DEFAULT 'legacy'`},
		{"identity_tenant", `ALTER TABLE requests ADD COLUMN identity_tenant TEXT NOT NULL DEFAULT ''`},
		{"identity_device", `ALTER TABLE requests ADD COLUMN identity_device TEXT NOT NULL DEFAULT ''`},
		{"principal", `ALTER TABLE requests ADD COLUMN principal TEXT NOT NULL DEFAULT ''`},
		{"service_account", `ALTER TABLE requests ADD COLUMN service_account TEXT NOT NULL DEFAULT ''`},
		{"project", `ALTER TABLE requests ADD COLUMN project TEXT NOT NULL DEFAULT ''`},
		{"cost_center", `ALTER TABLE requests ADD COLUMN cost_center TEXT NOT NULL DEFAULT ''`},
		{"identity_confidence", `ALTER TABLE requests ADD COLUMN identity_confidence TEXT NOT NULL DEFAULT 'unverified'`},
		{"requested_provider", `ALTER TABLE requests ADD COLUMN requested_provider TEXT NOT NULL DEFAULT ''`},
		{"requested_model", `ALTER TABLE requests ADD COLUMN requested_model TEXT NOT NULL DEFAULT ''`},
		{"requested_route", `ALTER TABLE requests ADD COLUMN requested_route TEXT NOT NULL DEFAULT ''`},
		{"downshift_action", `ALTER TABLE requests ADD COLUMN downshift_action TEXT NOT NULL DEFAULT 'none'`},
		{"downshift_rule", `ALTER TABLE requests ADD COLUMN downshift_rule TEXT NOT NULL DEFAULT ''`},
		{"downshift_trigger", `ALTER TABLE requests ADD COLUMN downshift_trigger TEXT NOT NULL DEFAULT ''`},
		{"downshift_reason", `ALTER TABLE requests ADD COLUMN downshift_reason TEXT NOT NULL DEFAULT ''`},
		{"downshift_config_digest", `ALTER TABLE requests ADD COLUMN downshift_config_digest TEXT NOT NULL DEFAULT ''`},
		{"downshift_features", `ALTER TABLE requests ADD COLUMN downshift_features TEXT NOT NULL DEFAULT ''`},
		{"downshift_source_estimated_usd", `ALTER TABLE requests ADD COLUMN downshift_source_estimated_usd REAL NOT NULL DEFAULT 0`},
		{"downshift_target_estimated_usd", `ALTER TABLE requests ADD COLUMN downshift_target_estimated_usd REAL NOT NULL DEFAULT 0`},
	}
	for _, addition := range additions {
		if columns[addition.name] {
			continue
		}
		if _, err := db.Exec(addition.sql); err != nil {
			return fmt.Errorf("migrate requests.%s: %w", addition.name, err)
		}
	}
	// Classify legacy rows without pretending failed/no-usage responses were
	// calls on unknown-price models. The old priced bit remains for backwards
	// compatibility, while the new states carry the precise meaning.
	if _, err := db.Exec(`UPDATE requests SET usage_state = CASE
		WHEN estimated = 1 THEN 'estimated'
		WHEN model != '' OR in_tokens != 0 OR out_tokens != 0 OR cache_read_tokens != 0 OR cache_write_tokens != 0 THEN 'exact'
		ELSE 'missing' END WHERE usage_state = 'legacy'`); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE requests SET pricing_state = CASE
		WHEN priced = 1 THEN 'priced'
		WHEN usage_state != 'missing' THEN 'unknown'
		ELSE 'unmetered' END WHERE pricing_state = 'legacy'`); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE requests SET cost_source = CASE
		WHEN pricing_state='priced' THEN 'legacy_unknown'
		WHEN pricing_state='unknown' THEN 'unknown'
		ELSE 'unmetered' END WHERE cost_source='' OR cost_source='legacy'`); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE requests SET cost_confidence = CASE
		WHEN usage_state='partial' THEN 'partial'
		WHEN usage_state='estimated' THEN 'estimated'
		WHEN pricing_state='priced' THEN 'legacy'
		ELSE 'unknown' END WHERE cost_confidence='' OR cost_confidence='legacy'`); err != nil {
		return err
	}
	_, err = db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_requests_ts_budget
			ON requests(ts, cost_usd, enforcement_unsafe);
		CREATE INDEX IF NOT EXISTS idx_requests_agent_ts_budget
			ON requests(agent, ts, cost_usd, enforcement_unsafe);
		CREATE INDEX IF NOT EXISTS idx_requests_ts_body_hash
			ON requests(ts, body_hash, cost_usd) WHERE body_hash != '';
	`)
	return err
}

const bodyHashVersion = "hmac-sha256-v1"

// migrateBodyHashes removes fingerprints written by older releases. Those
// values were unsalted SHA-256 prefixes and were both dictionary-testable and
// incompatible with the keyed HMAC fingerprints used now. They cannot be
// transformed without the original request body, so the safe upgrade is a
// one-time loss of historical duplicate-receipt grouping.
func migrateBodyHashes(db *sql.DB, existingRequestsTable bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key='internal.body_hash_version'`).Scan(&current)
	if err == nil && current == bodyHashVersion {
		return tx.Commit()
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existingRequestsTable {
		if _, err := tx.Exec(`UPDATE requests SET body_hash='' WHERE body_hash != ''`); err != nil {
			return fmt.Errorf("clear legacy request fingerprints: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES('internal.body_hash_version',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, bodyHashVersion); err != nil {
		return fmt.Errorf("record request fingerprint version: %w", err)
	}
	return tx.Commit()
}

func sqliteDSN(path, query string) string {
	separator := "?"
	if strings.HasPrefix(path, "file:") && strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + query
}

func (s *Store) Close() error { return s.db.Close() }

// Request is one proxied API call. In/Out are full-price tokens; cache
// fields hold discounted reads and premium writes, already normalized by
// the meter so pricing can treat all providers the same way.
type Request struct {
	Ts                 time.Time       `json:"ts"`
	Provider           string          `json:"provider"`
	Model              string          `json:"model"`
	Agent              string          `json:"agent"`
	Session            string          `json:"session"`
	InTokens           int64           `json:"in_tokens"`
	OutTokens          int64           `json:"out_tokens"`
	CacheReadTokens    int64           `json:"cache_read_tokens"`
	CacheWriteTokens   int64           `json:"cache_write_tokens"`
	CacheWrite1hTokens int64           `json:"cache_write_1h_tokens"`
	CostUSD            float64         `json:"cost_usd"`
	LatencyMs          int64           `json:"latency_ms"`
	Status             int             `json:"status"`
	Streamed           bool            `json:"streamed"`
	Estimated          bool            `json:"estimated"`
	Priced             bool            `json:"priced"`
	BodyHash           string          `json:"-"`
	UsageState         UsageState      `json:"usage_state"`
	PricingState       PricingState    `json:"pricing_state"`
	Incomplete         bool            `json:"incomplete"`
	EnforcementUnsafe  bool            `json:"enforcement_unsafe"`
	Route              string          `json:"route"`
	ServiceTier        string          `json:"service_tier"`
	InferenceGeo       string          `json:"inference_geo"`
	ServerToolCalls    int64           `json:"server_tool_calls"`
	FeeUnpriced        bool            `json:"fee_unpriced"`
	CostSource         CostSource      `json:"cost_source"`
	CostSourceRef      string          `json:"cost_source_ref,omitempty"`
	CostEffectiveFrom  string          `json:"cost_effective_from,omitempty"`
	CostValidThrough   string          `json:"cost_valid_through,omitempty"`
	CostConfidence     CostConfidence  `json:"cost_confidence"`
	PolicyDecisionID   int64           `json:"-"`
	IdentityTenant     string          `json:"identity_tenant,omitempty"`
	IdentityDevice     string          `json:"identity_device,omitempty"`
	Principal          string          `json:"principal,omitempty"`
	ServiceAccount     string          `json:"service_account,omitempty"`
	Project            string          `json:"project,omitempty"`
	CostCenter         string          `json:"cost_center,omitempty"`
	IdentityConfidence string          `json:"identity_confidence"`
	RequestedProvider  string          `json:"requested_provider,omitempty"`
	RequestedModel     string          `json:"requested_model,omitempty"`
	RequestedRoute     string          `json:"requested_route,omitempty"`
	DownshiftAction    string          `json:"downshift_action,omitempty"`
	DownshiftRule      string          `json:"downshift_rule,omitempty"`
	DownshiftTrigger   string          `json:"downshift_trigger,omitempty"`
	DownshiftReason    string          `json:"downshift_reason,omitempty"`
	DownshiftDigest    string          `json:"downshift_config_digest,omitempty"`
	DownshiftFeatures  string          `json:"downshift_features,omitempty"`
	DownshiftSourceUSD float64         `json:"downshift_source_estimated_usd,omitempty"`
	DownshiftTargetUSD float64         `json:"downshift_target_estimated_usd,omitempty"`
	Policy             *PolicyMetadata `json:"policy,omitempty"`
}

// UsageState distinguishes exact provider accounting from estimates and
// responses that could not be fully observed. It is deliberately separate
// from pricing: an exact usage frame can still name an unknown-price model.
type UsageState string

const (
	UsageExact     UsageState = "exact"
	UsageEstimated UsageState = "estimated"
	UsagePartial   UsageState = "partial"
	UsageMissing   UsageState = "missing"
)

// PricingState says why a row did or did not contribute dollars.
type PricingState string

const (
	PricingPriced    PricingState = "priced"
	PricingUnknown   PricingState = "unknown"
	PricingUnmetered PricingState = "unmetered"
)

// CostSource records which price layer won when the request was observed.
// Legacy rows remain explicitly uncertain rather than being retroactively
// relabeled from whatever pricing configuration exists today.
type CostSource string

const (
	CostProviderFinal CostSource = "provider_final"
	CostContract      CostSource = "contract"
	CostPublicList    CostSource = "public_list"
	CostUnknown       CostSource = "unknown"
	CostUnmetered     CostSource = "unmetered"
	CostLegacyUnknown CostSource = "legacy_unknown"
)

type CostConfidence string

const (
	ConfidenceProviderFinal CostConfidence = "provider_final"
	ConfidenceContract      CostConfidence = "contract"
	ConfidenceListEstimate  CostConfidence = "list_estimate"
	ConfidenceEstimated     CostConfidence = "estimated"
	ConfidencePartial       CostConfidence = "partial"
	ConfidenceUnknown       CostConfidence = "unknown"
	ConfidenceLegacy        CostConfidence = "legacy"
)

func normalizeRequest(r *Request) {
	if r.UsageState == "" {
		switch {
		case r.Estimated:
			r.UsageState = UsageEstimated
		case r.Model != "" || r.InTokens != 0 || r.OutTokens != 0 || r.CacheReadTokens != 0 || r.CacheWriteTokens != 0:
			r.UsageState = UsageExact
		default:
			r.UsageState = UsageMissing
		}
	}
	if r.PricingState == "" {
		switch {
		case r.Priced:
			r.PricingState = PricingPriced
		case r.UsageState != UsageMissing:
			r.PricingState = PricingUnknown
		default:
			r.PricingState = PricingUnmetered
		}
	}
	r.Estimated = r.UsageState == UsageEstimated || r.UsageState == UsagePartial
	r.Priced = r.PricingState == PricingPriced
	if r.UsageState == UsagePartial {
		r.Incomplete = true
	}
	if r.CostSource == "" {
		switch r.PricingState {
		case PricingPriced:
			r.CostSource = CostLegacyUnknown
		case PricingUnknown:
			r.CostSource = CostUnknown
		default:
			r.CostSource = CostUnmetered
		}
	}
	if r.CostConfidence == "" {
		switch {
		case r.UsageState == UsagePartial:
			r.CostConfidence = ConfidencePartial
		case r.UsageState == UsageEstimated:
			r.CostConfidence = ConfidenceEstimated
		case r.PricingState != PricingPriced:
			r.CostConfidence = ConfidenceUnknown
		default:
			r.CostConfidence = ConfidenceLegacy
		}
	}
	if r.IdentityConfidence == "" {
		r.IdentityConfidence = "unverified"
	}
	if r.RequestedProvider == "" {
		r.RequestedProvider = r.Provider
	}
	if r.RequestedModel == "" {
		r.RequestedModel = r.Model
	}
	if r.RequestedRoute == "" {
		r.RequestedRoute = r.Route
	}
	if r.DownshiftAction == "" {
		r.DownshiftAction = "none"
	}
}

func (s *Store) Insert(r Request) error {
	normalizeRequest(&r)
	if err := validateDownshiftReceipt(r); err != nil {
		return err
	}
	return s.mutateRequests(func() error {
		query := `INSERT INTO requests
		(ts, provider, model, agent, session, in_tokens, out_tokens,
		 cache_read_tokens, cache_write_tokens, cache_write_1h_tokens, cost_usd, latency_ms,
		 status, streamed, estimated, priced, body_hash, usage_state,
			 pricing_state, incomplete, enforcement_unsafe, route, service_tier,
			 inference_geo, server_tool_calls, fee_unpriced, cost_source, cost_source_ref,
			 cost_effective_from, cost_valid_through, cost_confidence, policy_decision_id,
			 identity_tenant, identity_device, principal, service_account, project, cost_center, identity_confidence,
			 requested_provider, requested_model, requested_route, downshift_action, downshift_rule,
			 downshift_trigger, downshift_reason, downshift_config_digest, downshift_features,
			 downshift_source_estimated_usd, downshift_target_estimated_usd)
		VALUES (` + strings.TrimSuffix(strings.Repeat("?,", 50), ",") + `)`
		_, err := s.db.Exec(query,
			r.Ts.UTC().Format(time.RFC3339), r.Provider, r.Model, r.Agent, r.Session,
			r.InTokens, r.OutTokens, r.CacheReadTokens, r.CacheWriteTokens, r.CacheWrite1hTokens,
			r.CostUSD, r.LatencyMs, r.Status, b2i(r.Streamed), b2i(r.Estimated),
			b2i(r.Priced), r.BodyHash, string(r.UsageState), string(r.PricingState),
			b2i(r.Incomplete), b2i(r.EnforcementUnsafe), r.Route, r.ServiceTier,
			r.InferenceGeo, r.ServerToolCalls, b2i(r.FeeUnpriced), string(r.CostSource), r.CostSourceRef,
			r.CostEffectiveFrom, r.CostValidThrough, string(r.CostConfidence), r.PolicyDecisionID,
			r.IdentityTenant, r.IdentityDevice, r.Principal, r.ServiceAccount, r.Project, r.CostCenter, r.IdentityConfidence,
			r.RequestedProvider, r.RequestedModel, r.RequestedRoute, r.DownshiftAction, r.DownshiftRule,
			r.DownshiftTrigger, r.DownshiftReason, r.DownshiftDigest, r.DownshiftFeatures,
			r.DownshiftSourceUSD, r.DownshiftTargetUSD)
		return err
	})
}

func validateDownshiftReceipt(r Request) error {
	if invalidMoney(r.DownshiftSourceUSD) || invalidMoney(r.DownshiftTargetUSD) {
		return fmt.Errorf("downshift cost estimates must be finite and non-negative")
	}
	if len(r.DownshiftReason) > 1024 || len(r.DownshiftFeatures) > 4096 || len(r.DownshiftRule) > 64 {
		return fmt.Errorf("downshift receipt exceeds its bounded ledger fields")
	}
	switch r.DownshiftAction {
	case "none", "warn":
	case "downshift":
		if r.DownshiftRule == "" || r.DownshiftReason == "" || !validDigest(r.DownshiftDigest) {
			return fmt.Errorf("downshift receipt is missing rule, reason, or config digest")
		}
		if r.RequestedProvider == r.Provider && r.RequestedModel == r.Model && r.RequestedRoute == r.Route {
			return fmt.Errorf("downshift receipt did not change route or model")
		}
	default:
		return fmt.Errorf("invalid downshift action %q", r.DownshiftAction)
	}
	if r.DownshiftDigest != "" && !validDigest(r.DownshiftDigest) {
		return fmt.Errorf("invalid downshift config digest")
	}
	return nil
}

// RequestRevision is an even/odd sequence lock for request mutations. Even
// values are stable snapshots; odd values mean Insert or PruneBatch is in
// progress. Admission caches publish only unchanged even revisions. Coherence
// covers mutations made through this Store instance; Burnban's serve lease and
// live-prune refusal enforce that single-writer invariant in production.
func (s *Store) RequestRevision() uint64 { return s.requestRevision.Load() }

func (s *Store) mutateRequests(fn func() error) error {
	s.requestMutationMu.Lock()
	s.requestRevision.Add(1)
	defer func() {
		s.requestRevision.Add(1)
		s.requestMutationMu.Unlock()
	}()
	return fn()
}

func (s *Store) SpentSince(t time.Time) (float64, error) {
	var v float64
	err := s.readQueryer().QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM requests WHERE ts >= ?`,
		t.UTC().Format(time.RFC3339)).Scan(&v)
	return v, err
}

// SpentSinceMulti sums spend since each cutoff in one pass: a single range
// scan from the earliest cutoff with one conditional sum per window, so a
// request checked against three budget windows costs one query, not three.
func (s *Store) SpentSinceMulti(ts []time.Time) ([]float64, error) {
	usage, err := s.BudgetUsageSinceMulti(ts)
	if err != nil {
		return nil, err
	}
	out := make([]float64, len(usage))
	for i := range usage {
		out[i] = usage[i].SpentUSD
	}
	return out, nil
}

// BudgetUsage is the durable state a cap admission needs for one window.
// EnforcementGaps counts successful calls whose price/usage was not safe
// enough to guarantee the configured dollar cap.
type BudgetUsage struct {
	SpentUSD        float64
	EnforcementGaps int64
	Requests        int64
}

// BudgetUsageSinceMulti returns spend and enforcement gaps for several
// cutoffs in one covering-index scan.
func (s *Store) BudgetUsageSinceMulti(ts []time.Time) ([]BudgetUsage, error) {
	if len(ts) == 0 {
		return nil, nil
	}
	min := ts[0]
	for _, t := range ts[1:] {
		if t.Before(min) {
			min = t
		}
	}
	cols := make([]string, 0, len(ts)*3)
	args := make([]any, 0, len(ts)*3+1)
	for _, t := range ts {
		cols = append(cols,
			"COALESCE(SUM(CASE WHEN ts >= ? THEN cost_usd ELSE 0 END),0)",
			"COALESCE(SUM(CASE WHEN ts >= ? THEN enforcement_unsafe ELSE 0 END),0)",
			"COALESCE(SUM(CASE WHEN ts >= ? THEN 1 ELSE 0 END),0)")
		// All conditional aggregates use the same cutoff.
		args = append(args, t.UTC().Format(time.RFC3339))
		args = append(args, t.UTC().Format(time.RFC3339))
		args = append(args, t.UTC().Format(time.RFC3339))
	}
	args = append(args, min.UTC().Format(time.RFC3339))
	dests := make([]any, len(ts)*3)
	out := make([]BudgetUsage, len(ts))
	for i := range out {
		dests[i*3] = &out[i].SpentUSD
		dests[i*3+1] = &out[i].EnforcementGaps
		dests[i*3+2] = &out[i].Requests
	}
	err := s.readQueryer().QueryRow(`SELECT `+strings.Join(cols, ", ")+
		` FROM requests WHERE ts >= ?`, args...).Scan(dests...)
	return out, err
}

func (s *Store) SpentSinceForAgent(t time.Time, agent string) (float64, error) {
	var v float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM requests WHERE ts >= ? AND agent = ?`,
		t.UTC().Format(time.RFC3339), agent).Scan(&v)
	return v, err
}

// SpentSinceForAgents returns one batched spend total for every requested
// agent, including explicit zeroes for agents with no rows. Inputs are
// deduplicated and chunked so a large configured-agent set cannot exceed
// SQLite's host-parameter limit.
func (s *Store) SpentSinceForAgents(t time.Time, agents []string) (map[string]float64, error) {
	usage, err := s.UsageSinceForAgents(t, agents)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(usage))
	for agent, row := range usage {
		out[agent] = row.Cost
	}
	return out, nil
}

// UsageSinceForAgents returns one batched request count and spend total for
// every requested agent, including explicit zeroes for agents with no rows.
// It is used for configured caps that may fall outside the dashboard's top-20
// aggregate without making the UI display a false zero-call total.
func (s *Store) UsageSinceForAgents(t time.Time, agents []string) (map[string]AgentRow, error) {
	out := make(map[string]AgentRow, len(agents))
	unique := make([]string, 0, len(agents))
	for _, agent := range agents {
		if _, exists := out[agent]; exists {
			continue
		}
		out[agent] = AgentRow{Agent: agent}
		unique = append(unique, agent)
	}
	const chunkSize = 400
	for start := 0; start < len(unique); start += chunkSize {
		end := min(start+chunkSize, len(unique))
		chunk := unique[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, t.UTC().Format(time.RFC3339))
		for _, agent := range chunk {
			args = append(args, agent)
		}
		rows, err := s.readQueryer().Query(`SELECT agent, COUNT(*), COALESCE(SUM(cost_usd),0)
			FROM requests WHERE ts >= ? AND agent IN (?`+
			strings.Repeat(",?", len(chunk)-1)+`) GROUP BY agent`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var agent string
			var row AgentRow
			if err := rows.Scan(&agent, &row.Requests, &row.Cost); err != nil {
				rows.Close()
				return nil, err
			}
			row.Agent = agent
			out[agent] = row
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) BudgetUsageSinceForAgent(t time.Time, agent string) (BudgetUsage, error) {
	var out BudgetUsage
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0), COALESCE(SUM(enforcement_unsafe),0)
		FROM requests WHERE ts >= ? AND agent = ?`,
		t.UTC().Format(time.RFC3339), agent).Scan(&out.SpentUSD, &out.EnforcementGaps)
	return out, err
}

// SeriesPoint is one hour's spend; Hour is the UTC bucket "2006-01-02T15".
type SeriesPoint struct {
	Hour string
	Cost float64
}

// HourlySeries groups spend by hour. Timestamps are stored as UTC RFC3339,
// so the first 13 characters are exactly the hour bucket.
func (s *Store) HourlySeries(since time.Time) ([]SeriesPoint, error) {
	rows, err := s.db.Query(`SELECT substr(ts,1,13) h, COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts >= ? GROUP BY h ORDER BY h`,
		since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.Hour, &p.Cost); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SettingsWithPrefix returns all settings whose key starts with prefix,
// keyed by the remainder after the prefix.
func (s *Store) SettingsWithPrefix(prefix string) (map[string]string, error) {
	rows, err := s.readQueryer().Query(`SELECT key, value FROM settings WHERE key LIKE ? || '%'`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k[len(prefix):]] = v
	}
	return out, rows.Err()
}

type ModelRow struct {
	Model      string
	Requests   int64
	In         int64
	Out        int64
	CacheRead  int64
	CacheWrite int64
	Cost       float64
}

type AgentRow struct {
	Agent    string
	Requests int64
	Cost     float64
}

type Summary struct {
	Requests        int64
	Cost            float64
	In              int64
	Out             int64
	CacheRead       int64
	CacheWrite      int64
	CacheWrite1h    int64
	Unpriced        int64
	UnknownPricing  int64
	Unmetered       int64
	Incomplete      int64
	EnforcementGaps int64
	FeeUnpriced     int64
	Estimated       int64
	LastRequestAt   time.Time
	Models          []ModelRow
	Agents          []AgentRow
	ModelOther      *ModelRow
	AgentOther      *AgentRow
	DupGroups       int64
	DupWastedUSD    float64
}

// TopSummary is the deliberately small aggregate needed by the live terminal
// view. Unlike Summary it does not calculate receipt duplication, confidence
// diagnostics, unused token dimensions, or long top-20/other breakdowns.
type TopSummary struct {
	Requests  int64
	Cost      float64
	In        int64
	CacheRead int64
	Models    []ModelRow
	Agents    []AgentRow
}

// MetricsSummary is the cheap lifetime view used by Prometheus/doctor. It
// intentionally excludes token-wide and duplicate-receipt scans; those belong
// to the richer, windowed Summarize path used by reports and the dashboard.
type MetricsSummary struct {
	Requests        int64
	Cost            float64
	UnknownPricing  int64
	Unmetered       int64
	Incomplete      int64
	EnforcementGaps int64
	FeeUnpriced     int64
	LastRequestAt   time.Time
	Models          []ModelRow
	Agents          []AgentRow
}

func (s *Store) LifetimeMetrics() (*MetricsSummary, error) {
	out := &MetricsSummary{}
	var lastRequest string
	err := s.readQueryer().QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost_usd),0),
		COALESCE(SUM(CASE WHEN pricing_state='unknown' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN pricing_state='unmetered' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(incomplete),0), COALESCE(SUM(enforcement_unsafe),0),
		COALESCE(SUM(fee_unpriced),0), COALESCE(MAX(ts),'') FROM requests`).
		Scan(&out.Requests, &out.Cost, &out.UnknownPricing, &out.Unmetered,
			&out.Incomplete, &out.EnforcementGaps, &out.FeeUnpriced, &lastRequest)
	if err != nil {
		return nil, err
	}
	if lastRequest != "" {
		out.LastRequestAt, _ = time.Parse(time.RFC3339, lastRequest)
	}
	rows, err := s.readQueryer().Query(`SELECT model, COUNT(*),
		COALESCE(SUM(in_tokens),0), COALESCE(SUM(out_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		COALESCE(SUM(cost_usd),0) FROM requests WHERE model != ''
		GROUP BY model ORDER BY SUM(cost_usd) DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var model ModelRow
		if err := rows.Scan(&model.Model, &model.Requests, &model.In, &model.Out,
			&model.CacheRead, &model.CacheWrite, &model.Cost); err != nil {
			rows.Close()
			return nil, err
		}
		out.Models = append(out.Models, model)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.readQueryer().Query(`SELECT agent, COUNT(*), COALESCE(SUM(cost_usd),0)
		FROM requests WHERE agent != '' GROUP BY agent ORDER BY SUM(cost_usd) DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var agent AgentRow
		if err := rows.Scan(&agent.Agent, &agent.Requests, &agent.Cost); err != nil {
			rows.Close()
			return nil, err
		}
		out.Agents = append(out.Agents, agent)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}

// Top returns today's terminal-view totals and a bounded model/agent ranking.
// It intentionally never touches body_hash or the duplicate-receipt grouping
// used by full reports, because top refreshes continuously while it is open.
func (s *Store) Top(since time.Time, limit int) (*TopSummary, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("top limit must be between 1 and 100")
	}
	ts := since.UTC().Format(time.RFC3339)
	out := &TopSummary{}
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost_usd),0),
		COALESCE(SUM(in_tokens),0), COALESCE(SUM(cache_read_tokens),0)
		FROM requests WHERE ts >= ?`, ts).
		Scan(&out.Requests, &out.Cost, &out.In, &out.CacheRead); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`SELECT model, COUNT(*), COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts >= ? AND model != ''
		GROUP BY model ORDER BY SUM(cost_usd) DESC, model LIMIT ?`, ts, limit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var row ModelRow
		if err := rows.Scan(&row.Model, &row.Requests, &row.Cost); err != nil {
			rows.Close()
			return nil, err
		}
		out.Models = append(out.Models, row)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(`SELECT agent, COUNT(*), COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts >= ? AND agent != ''
		GROUP BY agent ORDER BY SUM(cost_usd) DESC, agent LIMIT ?`, ts, limit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var row AgentRow
		if err := rows.Scan(&row.Agent, &row.Requests, &row.Cost); err != nil {
			rows.Close()
			return nil, err
		}
		out.Agents = append(out.Agents, row)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) Summarize(since time.Time) (*Summary, error) {
	ts := since.UTC().Format(time.RFC3339)
	sum := &Summary{}
	var lastRequest string
	err := s.readQueryer().QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost_usd),0),
		COALESCE(SUM(in_tokens),0), COALESCE(SUM(out_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		COALESCE(SUM(cache_write_1h_tokens),0),
		COALESCE(SUM(CASE WHEN pricing_state='unknown' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN pricing_state='unmetered' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(estimated),0), COALESCE(SUM(incomplete),0),
		COALESCE(SUM(enforcement_unsafe),0), COALESCE(SUM(fee_unpriced),0), COALESCE(MAX(ts),'')
		FROM requests WHERE ts >= ?`, ts).
		Scan(&sum.Requests, &sum.Cost, &sum.In, &sum.Out,
			&sum.CacheRead, &sum.CacheWrite, &sum.CacheWrite1h, &sum.UnknownPricing, &sum.Unmetered,
			&sum.Estimated, &sum.Incomplete, &sum.EnforcementGaps, &sum.FeeUnpriced, &lastRequest)
	if err != nil {
		return nil, err
	}
	sum.Unpriced = sum.UnknownPricing // backwards-compatible name
	if lastRequest != "" {
		sum.LastRequestAt, _ = time.Parse(time.RFC3339, lastRequest)
	}

	rows, err := s.readQueryer().Query(`WITH grouped AS (
		SELECT model, COUNT(*) AS requests,
			COALESCE(SUM(in_tokens),0) AS in_tokens,
			COALESCE(SUM(out_tokens),0) AS out_tokens,
			COALESCE(SUM(cache_read_tokens),0) AS cache_read_tokens,
			COALESCE(SUM(cache_write_tokens),0) AS cache_write_tokens,
			COALESCE(SUM(cost_usd),0) AS cost
		FROM requests WHERE ts >= ? AND model != '' GROUP BY model
	)
	SELECT model, requests, in_tokens, out_tokens, cache_read_tokens, cache_write_tokens, cost,
		SUM(requests) OVER (), SUM(in_tokens) OVER (), SUM(out_tokens) OVER (),
		SUM(cache_read_tokens) OVER (), SUM(cache_write_tokens) OVER (), SUM(cost) OVER ()
	FROM grouped ORDER BY cost DESC, model LIMIT 20`, ts)
	if err != nil {
		return nil, err
	}
	var modelTop, modelTotal ModelRow
	for rows.Next() {
		var m ModelRow
		if err := rows.Scan(&m.Model, &m.Requests, &m.In, &m.Out, &m.CacheRead, &m.CacheWrite, &m.Cost,
			&modelTotal.Requests, &modelTotal.In, &modelTotal.Out, &modelTotal.CacheRead,
			&modelTotal.CacheWrite, &modelTotal.Cost); err != nil {
			rows.Close()
			return nil, err
		}
		sum.Models = append(sum.Models, m)
		modelTop.Requests += m.Requests
		modelTop.In += m.In
		modelTop.Out += m.Out
		modelTop.CacheRead += m.CacheRead
		modelTop.CacheWrite += m.CacheWrite
		modelTop.Cost += m.Cost
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if modelTotal.Requests > modelTop.Requests {
		sum.ModelOther = &ModelRow{
			Requests: modelTotal.Requests - modelTop.Requests,
			In:       max(0, modelTotal.In-modelTop.In), Out: max(0, modelTotal.Out-modelTop.Out),
			CacheRead:  max(0, modelTotal.CacheRead-modelTop.CacheRead),
			CacheWrite: max(0, modelTotal.CacheWrite-modelTop.CacheWrite),
			Cost:       max(0, modelTotal.Cost-modelTop.Cost),
		}
	}

	rows, err = s.readQueryer().Query(`WITH grouped AS (
		SELECT agent, COUNT(*) AS requests, COALESCE(SUM(cost_usd),0) AS cost
		FROM requests WHERE ts >= ? AND agent != '' GROUP BY agent
	)
	SELECT agent, requests, cost, SUM(requests) OVER (), SUM(cost) OVER ()
	FROM grouped ORDER BY cost DESC, agent LIMIT 20`, ts)
	if err != nil {
		return nil, err
	}
	var agentTop, agentTotal AgentRow
	for rows.Next() {
		var a AgentRow
		if err := rows.Scan(&a.Agent, &a.Requests, &a.Cost, &agentTotal.Requests, &agentTotal.Cost); err != nil {
			rows.Close()
			return nil, err
		}
		sum.Agents = append(sum.Agents, a)
		agentTop.Requests += a.Requests
		agentTop.Cost += a.Cost
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if agentTotal.Requests > agentTop.Requests {
		sum.AgentOther = &AgentRow{
			Requests: agentTotal.Requests - agentTop.Requests,
			Cost:     max(0, agentTotal.Cost-agentTop.Cost),
		}
	}

	err = s.readQueryer().QueryRow(`SELECT COUNT(*), COALESCE(SUM(wasted),0) FROM (
		SELECT SUM(cost_usd) * (COUNT(*)-1.0)/COUNT(*) AS wasted
		FROM requests WHERE ts >= ? AND body_hash != ''
		GROUP BY body_hash HAVING COUNT(*) > 1)`, ts).
		Scan(&sum.DupGroups, &sum.DupWastedUSD)
	if err != nil {
		return nil, err
	}
	return sum, nil
}

// Totals is the token mass and metered cost of the priced rows in a window,
// plus how many rows had to be excluded for lack of a known price.
type Totals struct {
	Requests     int64
	Unpriced     int64
	Unmetered    int64
	Incomplete   int64
	In           int64
	Out          int64
	CacheRead    int64
	CacheWrite   int64
	CacheWrite1h int64
	CostUSD      float64
	FeeUnpriced  int64
}

// TokenRow is the lean, privacy-minimal input for per-request what-if
// repricing. It deliberately omits model, agent, session, route, and request
// fingerprint fields.
type TokenRow struct {
	In           int64
	Out          int64
	CacheRead    int64
	CacheWrite   int64
	CacheWrite1h int64
	CostUSD      float64
	PricingState PricingState
	Incomplete   bool
	FeeUnpriced  bool
}

// TokenRows returns a single SQLite read snapshot of the numeric fields needed
// for accurate per-request repricing and its actual-mix baseline.
func (s *Store) TokenRows(t time.Time) ([]TokenRow, error) {
	rows, err := s.db.Query(`SELECT in_tokens, out_tokens, cache_read_tokens,
		cache_write_tokens, cache_write_1h_tokens, cost_usd, pricing_state,
		incomplete, fee_unpriced FROM requests WHERE ts >= ? ORDER BY id`,
		t.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TokenRow, 0)
	for rows.Next() {
		var row TokenRow
		var incomplete, feeUnpriced int
		if err := rows.Scan(&row.In, &row.Out, &row.CacheRead, &row.CacheWrite,
			&row.CacheWrite1h, &row.CostUSD, &row.PricingState, &incomplete, &feeUnpriced); err != nil {
			return nil, err
		}
		row.Incomplete = incomplete != 0
		row.FeeUnpriced = feeUnpriced != 0
		out = append(out, row)
	}
	return out, rows.Err()
}

// TokenTotals sums tokens and cost across priced rows since t in one scan and
// counts excluded confidence states on the side. It remains available for
// aggregate callers; what-if now derives its baseline, exclusions, and
// per-request repricing from one consistent TokenRows snapshot.
func (s *Store) TokenTotals(t time.Time) (*Totals, error) {
	var tot Totals
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN pricing_state='priced' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN pricing_state='unknown' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN pricing_state='unmetered' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(incomplete),0),
		COALESCE(SUM(CASE WHEN pricing_state='priced' THEN in_tokens ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN pricing_state='priced' THEN out_tokens ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN pricing_state='priced' THEN cache_read_tokens ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN pricing_state='priced' THEN cache_write_tokens ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN pricing_state='priced' THEN cache_write_1h_tokens ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN pricing_state='priced' THEN cost_usd ELSE 0 END),0),
		COALESCE(SUM(fee_unpriced),0)
		FROM requests WHERE ts >= ?`, t.UTC().Format(time.RFC3339)).
		Scan(&tot.Requests, &tot.Unpriced, &tot.Unmetered, &tot.Incomplete,
			&tot.In, &tot.Out, &tot.CacheRead, &tot.CacheWrite, &tot.CacheWrite1h,
			&tot.CostUSD, &tot.FeeUnpriced)
	if err != nil {
		return nil, err
	}
	return &tot, nil
}

// StreamExport visits raw request rows for finance/audit tooling, oldest first.
// The callback runs while the SQLite read cursor is open; returning an error
// stops the scan immediately. Memory use is bounded by one Request.
func (s *Store) StreamExport(since time.Time, visit func(Request) error) error {
	if visit == nil {
		return fmt.Errorf("export visitor must not be nil")
	}
	rows, err := s.db.Query(`SELECT r.ts, r.provider, r.model, r.agent, r.session,
		in_tokens, out_tokens, cache_read_tokens, cache_write_tokens,
		cache_write_1h_tokens, cost_usd, latency_ms, status, streamed, estimated, priced, body_hash,
		usage_state, pricing_state, incomplete, enforcement_unsafe, route,
		service_tier, inference_geo, server_tool_calls, fee_unpriced,
		cost_source,cost_source_ref,cost_effective_from,cost_valid_through,cost_confidence,
		r.policy_decision_id, r.identity_tenant, r.identity_device, r.principal,
		r.service_account, r.project, r.cost_center, r.identity_confidence,
		r.requested_provider, r.requested_model, r.requested_route, r.downshift_action,
		r.downshift_rule, r.downshift_trigger, r.downshift_reason, r.downshift_config_digest,
		r.downshift_features, r.downshift_source_estimated_usd, r.downshift_target_estimated_usd,
		COALESCE(d.policy_digest,''), COALESCE(d.policy_revision,0), COALESCE(d.policy_name,''),
		COALESCE(d.policy_namespace,''), COALESCE(d.mode,''), COALESCE(d.outcome,''), COALESCE(d.admitted,0), COALESCE(d.confidence,''),
		COALESCE(d.context_json,''), COALESCE(d.explanation_json,'')
		FROM requests r LEFT JOIN policy_decisions d ON d.id = r.policy_decision_id
		WHERE r.ts >= ? ORDER BY r.ts, r.id`,
		since.UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r Request
		var ts string
		var streamed, estimated, priced, incomplete, enforcementUnsafe, feeUnpriced int
		var policyDigest, policyName, policyNamespace, policyMode, policyOutcome, policyConfidence, policyContext, policyExplanation string
		var policyAdmitted int
		var policyRevision int64
		if err := rows.Scan(&ts, &r.Provider, &r.Model, &r.Agent, &r.Session,
			&r.InTokens, &r.OutTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.CacheWrite1hTokens,
			&r.CostUSD, &r.LatencyMs, &r.Status, &streamed, &estimated, &priced,
			&r.BodyHash, &r.UsageState, &r.PricingState, &incomplete, &enforcementUnsafe,
			&r.Route, &r.ServiceTier, &r.InferenceGeo, &r.ServerToolCalls, &feeUnpriced,
			&r.CostSource, &r.CostSourceRef, &r.CostEffectiveFrom, &r.CostValidThrough, &r.CostConfidence,
			&r.PolicyDecisionID, &r.IdentityTenant, &r.IdentityDevice, &r.Principal, &r.ServiceAccount,
			&r.Project, &r.CostCenter, &r.IdentityConfidence,
			&r.RequestedProvider, &r.RequestedModel, &r.RequestedRoute, &r.DownshiftAction,
			&r.DownshiftRule, &r.DownshiftTrigger, &r.DownshiftReason, &r.DownshiftDigest,
			&r.DownshiftFeatures, &r.DownshiftSourceUSD, &r.DownshiftTargetUSD,
			&policyDigest, &policyRevision, &policyName, &policyNamespace, &policyMode,
			&policyOutcome, &policyAdmitted, &policyConfidence, &policyContext, &policyExplanation); err != nil {
			return err
		}
		r.Ts, _ = time.Parse(time.RFC3339, ts)
		r.Streamed, r.Estimated, r.Priced = streamed == 1, estimated == 1, priced == 1
		r.Incomplete, r.EnforcementUnsafe = incomplete == 1, enforcementUnsafe == 1
		r.FeeUnpriced = feeUnpriced == 1
		if r.PolicyDecisionID != 0 {
			r.Policy = &PolicyMetadata{
				DecisionID: r.PolicyDecisionID, Digest: policyDigest, Revision: policyRevision,
				Name: policyName, Namespace: policyNamespace, Mode: policyMode, Outcome: policyOutcome,
				Admitted:   policyAdmitted != 0,
				Confidence: policyConfidence, ContextJSON: policyContext,
				ExplanationJSON: policyExplanation,
			}
		}
		if err := visit(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Export returns raw request rows for compatibility with callers that need a
// slice. Streaming consumers should use StreamExport to keep memory bounded.
func (s *Store) Export(since time.Time) ([]Request, error) {
	var out []Request
	err := s.StreamExport(since, func(r Request) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SetSettingOnce writes key=value and reports whether this call changed
// anything. False means the value was already there — which is how alert
// paths dedup atomically when concurrent requests race to send one.
func (s *Store) SetSettingOnce(key, value string) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
		WHERE settings.value <> excluded.value`, key, value)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetSetting returns "" for keys that were never set.
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.readQueryer().QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// ConsumeIdentityNonce atomically records a signed request's one-time nonce.
// False means the same key+nonce was already accepted. Expired rows are
// removed in the same short transaction so the replay table remains bounded.
func (s *Store) ConsumeIdentityNonce(keyID, jti string, expiresAt, now time.Time) (bool, error) {
	if keyID == "" || jti == "" || len(keyID) > 128 || len(jti) > 128 || !expiresAt.After(now) || expiresAt.After(now.Add(10*time.Minute)) {
		return false, fmt.Errorf("invalid identity nonce envelope")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM identity_nonces WHERE expires_at<=?`, now.UTC().Unix()); err != nil {
		return false, err
	}
	result, err := tx.Exec(`INSERT INTO identity_nonces(key_id,jti,expires_at) VALUES(?,?,?)
		ON CONFLICT(key_id,jti) DO NOTHING`, keyID, jti, expiresAt.UTC().Unix())
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return rows == 1, nil
}

// GetSettings fetches many keys in one query; absent keys are simply
// missing from the map. The budget guard runs per request, so it must not
// pay one round trip per key.
func (s *Store) GetSettings(keys ...string) (map[string]string, error) {
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	rows, err := s.readQueryer().Query(`SELECT key, value FROM settings WHERE key IN (?`+
		strings.Repeat(",?", len(keys)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string, len(keys))
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) DeleteSetting(key string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	return err
}

// DeleteSettingsWithPrefix clears a family of keys, e.g. the per-window
// warned/alerted marks when a cap is changed or removed.
func (s *Store) DeleteSettingsWithPrefix(prefix string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key LIKE ? || '%'`, prefix)
	return err
}

// Probe performs and commits a tiny durable write. It is used to recover a
// proxy that latched fail-closed after a ledger insert error; a read-only
// SELECT is insufficient because disk-full failures often leave reads healthy.
func (s *Store) Probe() error {
	return s.SetSetting("_runtime_probe", time.Now().UTC().Format(time.RFC3339Nano))
}

// Prune removes request rows and prompt-free policy decision records strictly
// older than before. Active/history policy documents and settings are kept.
// Callers choose retention explicitly; Burnban never prunes implicitly.
func (s *Store) Prune(before time.Time) (int64, error) {
	var total int64
	for {
		deleted, err := s.PruneBatch(before, 5000)
		if err != nil {
			return total, err
		}
		total += deleted
		if deleted == 0 {
			return total, nil
		}
	}
}

// PruneBatch deletes at most limit top-level ledger records (requests followed
// by policy decisions). Child rule-counter rows are removed with their decision
// and do not count against the limit. Bounded transactions prevent a large
// retention job from holding SQLite's writer lock for the entire ledger.
func (s *Store) PruneBatch(before time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 100_000 {
		return 0, fmt.Errorf("prune batch size must be between 1 and 100000")
	}
	var deleted int64
	err := s.mutateRequests(func() error {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		res, err := tx.Exec(`DELETE FROM requests WHERE id IN (
			SELECT id FROM requests WHERE ts < ? ORDER BY id LIMIT ?
		)`, before.UTC().Format(time.RFC3339), limit)
		if err != nil {
			return err
		}
		requestRows, err := res.RowsAffected()
		if err != nil {
			return err
		}
		remaining := int64(limit) - requestRows
		var decisionRows int64
		if remaining > 0 {
			cutoff := before.UTC().Format(policyTimeFormat)
			// Delete children first because policy_decision_rules intentionally
			// remains queryable without relying on connection-specific FK pragmas.
			if _, err := tx.Exec(`DELETE FROM policy_decision_rules WHERE decision_id IN (
				SELECT id FROM policy_decisions WHERE ts < ? ORDER BY id LIMIT ?
			)`, cutoff, remaining); err != nil {
				return err
			}
			res, err = tx.Exec(`DELETE FROM policy_decisions WHERE id IN (
				SELECT id FROM policy_decisions WHERE ts < ? ORDER BY id LIMIT ?
			)`, cutoff, remaining)
			if err != nil {
				return err
			}
			decisionRows, err = res.RowsAffected()
			if err != nil {
				return err
			}
		}
		deleted = requestRows + decisionRows
		return tx.Commit()
	})
	return deleted, err
}

// Checkpoint asks SQLite to move completed WAL pages into the main database
// without blocking active readers. It does not shrink the database file.
func (s *Store) Checkpoint() error {
	var busy, logFrames, checkpointed int
	return s.db.QueryRow(`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &checkpointed)
}

var ErrLeaseHeld = errors.New("runtime lease is held by another process")

// Lease is an atomic, renewable single-process claim stored in SQLite. A
// crashed owner stops renewing and the claim becomes acquirable after ttl.
type Lease struct {
	mu        sync.RWMutex
	s         *Store
	name      string
	owner     string
	ttl       time.Duration
	expiresAt time.Time
}

// AcquireLease claims name unless another unexpired owner already holds it.
// The conditional UPSERT is one SQLite statement, so separate Burnban
// processes cannot both win the same lease.
func (s *Store) AcquireLease(name string, ttl time.Duration) (*Lease, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("lease name must not be empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("lease ttl must be positive")
	}
	ownerBytes := make([]byte, 16)
	if _, err := rand.Read(ownerBytes); err != nil {
		return nil, err
	}
	lease := &Lease{s: s, name: name, owner: hex.EncodeToString(ownerBytes), ttl: ttl}
	now := time.Now()
	lease.expiresAt = now.Add(ttl)
	res, err := s.db.Exec(`INSERT INTO runtime_leases(name, owner, expires_at) VALUES(?,?,?)
		ON CONFLICT(name) DO UPDATE SET owner=excluded.owner, expires_at=excluded.expires_at
		WHERE runtime_leases.expires_at <= ? OR runtime_leases.owner = excluded.owner`,
		name, lease.owner, lease.expiresAt.UnixNano(), now.UnixNano())
	if err != nil {
		return nil, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		return nil, ErrLeaseHeld
	}
	return lease, nil
}

func (l *Lease) Owner() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.owner
}

func (l *Lease) ExpiresAt() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.expiresAt
}

// Renew extends a lease only while this owner still holds it. ErrLeaseHeld
// means the claim expired and another process took over; the server must stop.
func (l *Lease) Renew() error {
	if l == nil || l.s == nil {
		return fmt.Errorf("nil lease")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	expires := now.Add(l.ttl)
	res, err := l.s.db.Exec(`UPDATE runtime_leases SET expires_at=?
		WHERE name=? AND owner=? AND expires_at > ?`,
		expires.UnixNano(), l.name, l.owner, now.UnixNano())
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrLeaseHeld
	}
	l.expiresAt = expires
	return nil
}

// Release relinquishes a lease if this owner still holds it. It is safe to
// call after expiry or takeover.
func (l *Lease) Release() error {
	if l == nil || l.s == nil {
		return nil
	}
	_, err := l.s.db.Exec(`DELETE FROM runtime_leases WHERE name=? AND owner=?`, l.name, l.owner)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
