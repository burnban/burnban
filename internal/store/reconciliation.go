package store

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/reconcile"
)

const reconciliationSchema = `
CREATE TABLE IF NOT EXISTS invoice_imports (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	provider TEXT NOT NULL,
	invoice_id TEXT NOT NULL,
	currency TEXT NOT NULL CHECK(currency='USD'),
	source_format TEXT NOT NULL,
	content_sha256 TEXT NOT NULL,
	import_identity_sha256 TEXT NOT NULL DEFAULT '',
	imported_at TEXT NOT NULL,
	row_count INTEGER NOT NULL CHECK(row_count > 0),
	billed_micros INTEGER NOT NULL,
	UNIQUE(provider, invoice_id)
);
CREATE TABLE IF NOT EXISTS invoice_lines (
	import_id INTEGER NOT NULL REFERENCES invoice_imports(id),
	line_id TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	billed_micros INTEGER NOT NULL CHECK(billed_micros >= 0),
	model TEXT NOT NULL DEFAULT '',
	service_tier TEXT NOT NULL DEFAULT '',
	region TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(import_id, line_id)
);
CREATE INDEX IF NOT EXISTS idx_invoice_lines_time ON invoice_lines(occurred_at, import_id);
CREATE TABLE IF NOT EXISTS reconciliation_adjustments (
	import_id INTEGER NOT NULL REFERENCES invoice_imports(id),
	line_id TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	adjustment_type TEXT NOT NULL CHECK(adjustment_type IN ('delayed','credit','batch','tax','fee')),
	amount_micros INTEGER NOT NULL CHECK(amount_micros <> 0),
	reference_line_id TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	service_tier TEXT NOT NULL DEFAULT '',
	region TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(import_id, line_id)
);
CREATE INDEX IF NOT EXISTS idx_reconciliation_adjustments_time ON reconciliation_adjustments(occurred_at, import_id);

CREATE TRIGGER IF NOT EXISTS invoice_imports_no_update BEFORE UPDATE ON invoice_imports
BEGIN SELECT RAISE(ABORT, 'invoice imports are immutable'); END;
CREATE TRIGGER IF NOT EXISTS invoice_imports_no_delete BEFORE DELETE ON invoice_imports
BEGIN SELECT RAISE(ABORT, 'invoice imports are immutable'); END;
CREATE TRIGGER IF NOT EXISTS invoice_lines_no_update BEFORE UPDATE ON invoice_lines
BEGIN SELECT RAISE(ABORT, 'invoice lines are immutable'); END;
CREATE TRIGGER IF NOT EXISTS invoice_lines_no_delete BEFORE DELETE ON invoice_lines
BEGIN SELECT RAISE(ABORT, 'invoice lines are immutable'); END;
CREATE TRIGGER IF NOT EXISTS reconciliation_adjustments_no_update BEFORE UPDATE ON reconciliation_adjustments
BEGIN SELECT RAISE(ABORT, 'reconciliation adjustments are immutable'); END;
CREATE TRIGGER IF NOT EXISTS reconciliation_adjustments_no_delete BEFORE DELETE ON reconciliation_adjustments
BEGIN SELECT RAISE(ABORT, 'reconciliation adjustments are immutable'); END;
`

var ErrInvoiceConflict = errors.New("invoice ID was already imported with different content or mapping")

func migrateReconciliationSchema(db *sql.DB) error {
	var present int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('invoice_imports')
		WHERE name='import_identity_sha256'`).Scan(&present); err != nil {
		return fmt.Errorf("inspect reconciliation schema: %w", err)
	}
	if present != 0 {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE invoice_imports
		ADD COLUMN import_identity_sha256 TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("migrate invoice_imports.import_identity_sha256: %w", err)
	}
	return nil
}

type InvoiceImportResult struct {
	ImportID int64 `json:"import_id"`
	Rows     int   `json:"rows"`
	Replayed bool  `json:"replayed"`
}

// ImportInvoice appends invoice evidence and adjustments in one transaction.
// An exact replay is idempotent. Reusing an invoice identity with different
// bytes or a different effective CSV mapping fails closed and never replaces
// the first import.
func (s *Store) ImportInvoice(invoice reconcile.Invoice, importedAt time.Time) (InvoiceImportResult, error) {
	s.reconciliationMu.Lock()
	defer s.reconciliationMu.Unlock()
	if err := reconcile.ValidateInvoice(invoice); err != nil {
		return InvoiceImportResult{}, err
	}
	if importedAt.IsZero() {
		return InvoiceImportResult{}, errors.New("imported_at is required")
	}
	importIdentity, err := reconcile.ImportIdentity(invoice)
	if err != nil {
		return InvoiceImportResult{}, err
	}
	var total int64
	for _, line := range invoice.Lines {
		var err error
		total, err = reconcile.CheckedAdd(total, line.BilledMicros)
		if err != nil {
			return InvoiceImportResult{}, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return InvoiceImportResult{}, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO invoice_imports
		(provider,invoice_id,currency,source_format,content_sha256,import_identity_sha256,imported_at,row_count,billed_micros)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(provider,invoice_id) DO NOTHING`, invoice.Provider, invoice.InvoiceID,
		invoice.Currency, string(invoice.SourceFormat), invoice.ContentHash, importIdentity,
		importedAt.UTC().Format(time.RFC3339Nano), len(invoice.Lines), total)
	if err != nil {
		return InvoiceImportResult{}, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return InvoiceImportResult{}, err
	}
	if inserted == 0 {
		var existingID int64
		var existingHash, existingIdentity string
		if err := tx.QueryRow(`SELECT id, content_sha256, import_identity_sha256
			FROM invoice_imports WHERE provider=? AND invoice_id=?`, invoice.Provider, invoice.InvoiceID).
			Scan(&existingID, &existingHash, &existingIdentity); err != nil {
			return InvoiceImportResult{}, err
		}
		sameContent := sameInvoiceDigest(existingHash, invoice.ContentHash)
		sameIdentity := sameInvoiceDigest(existingIdentity, importIdentity)
		if existingIdentity == "" {
			// Legacy rows predate mapping-bound identities, so their preset or
			// custom mapping cannot be reconstructed. Raw content plus the immutable
			// normalized rows below is the strongest replay evidence they contain.
			sameIdentity = true
		}
		if sameContent && sameIdentity {
			// Do not trust a caller-supplied content digest to stand in for the
			// normalized invoice. This also supplies the compatibility proof for
			// legacy rows without weakening mapping checks on new imports.
			var sameNormalized bool
			sameNormalized, err = importedInvoiceMatches(tx, existingID, invoice)
			if err != nil {
				return InvoiceImportResult{}, err
			}
			if sameNormalized {
				return InvoiceImportResult{ImportID: existingID, Rows: len(invoice.Lines), Replayed: true}, tx.Commit()
			}
		}
		return InvoiceImportResult{}, ErrInvoiceConflict
	}
	importID, err := result.LastInsertId()
	if err != nil {
		return InvoiceImportResult{}, err
	}
	for _, line := range invoice.Lines {
		if line.Type == reconcile.LineUsage {
			_, err = tx.Exec(`INSERT INTO invoice_lines
				(import_id,line_id,occurred_at,billed_micros,model,service_tier,region,description)
				VALUES(?,?,?,?,?,?,?,?)`, importID, line.LineID, line.OccurredAt.UTC().Format(reconcile.TimestampFormat),
				line.BilledMicros, line.Model, line.ServiceTier, line.Region, line.Description)
		} else {
			_, err = tx.Exec(`INSERT INTO reconciliation_adjustments
				(import_id,line_id,occurred_at,adjustment_type,amount_micros,reference_line_id,model,service_tier,region,description)
				VALUES(?,?,?,?,?,?,?,?,?,?)`, importID, line.LineID, line.OccurredAt.UTC().Format(reconcile.TimestampFormat),
				string(line.Type), line.BilledMicros, line.ReferenceLineID, line.Model, line.ServiceTier, line.Region, line.Description)
		}
		if err != nil {
			return InvoiceImportResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return InvoiceImportResult{}, err
	}
	return InvoiceImportResult{ImportID: importID, Rows: len(invoice.Lines)}, nil
}

func sameInvoiceDigest(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func importedInvoiceMatches(tx *sql.Tx, importID int64, invoice reconcile.Invoice) (bool, error) {
	rows, err := tx.Query(`SELECT line_id,occurred_at,billed_micros,model,service_tier,region,
			line_type,reference_line_id,description FROM (
			SELECT line_id,occurred_at,billed_micros,model,service_tier,region,
				'usage' AS line_type,'' AS reference_line_id,description
			FROM invoice_lines WHERE import_id=?
			UNION ALL
			SELECT line_id,occurred_at,amount_micros,model,service_tier,region,
				adjustment_type AS line_type,reference_line_id,description
			FROM reconciliation_adjustments WHERE import_id=?
		) ORDER BY line_id`, importID, importID)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	incoming := append([]reconcile.Line(nil), invoice.Lines...)
	sort.Slice(incoming, func(i, j int) bool { return incoming[i].LineID < incoming[j].LineID })
	matched := 0
	for rows.Next() {
		if matched >= len(incoming) {
			return false, nil
		}
		var lineID, occurredAt, model, serviceTier, region, lineType, referenceLineID, description string
		var billedMicros int64
		if err := rows.Scan(&lineID, &occurredAt, &billedMicros, &model, &serviceTier, &region,
			&lineType, &referenceLineID, &description); err != nil {
			return false, err
		}
		line := incoming[matched]
		if lineID != line.LineID || occurredAt != line.OccurredAt.UTC().Format(reconcile.TimestampFormat) ||
			billedMicros != line.BilledMicros || model != line.Model || serviceTier != line.ServiceTier ||
			region != line.Region || lineType != string(line.Type) || referenceLineID != line.ReferenceLineID ||
			description != line.Description {
			return false, nil
		}
		matched++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return matched == len(incoming), nil
}

type ReconciliationReport struct {
	From                    time.Time             `json:"from"`
	Through                 time.Time             `json:"through"`
	Provider                string                `json:"provider,omitempty"`
	LedgerEstimateMicros    int64                 `json:"ledger_estimate_micros"`
	ProviderBilledMicros    int64                 `json:"provider_billed_micros"`
	VarianceMicros          int64                 `json:"variance_micros"`
	MatchedEstimateMicros   int64                 `json:"matched_estimate_micros"`
	MatchedBilledMicros     int64                 `json:"matched_billed_micros"`
	UnmatchedLedgerMicros   int64                 `json:"unmatched_ledger_micros"`
	UnmatchedProviderMicros int64                 `json:"unmatched_provider_micros"`
	UnmatchedLedgerRows     int64                 `json:"unmatched_ledger_rows"`
	UnmatchedProviderLines  int64                 `json:"unmatched_provider_lines"`
	UnknownPricingRows      int64                 `json:"unknown_pricing_rows"`
	EstimatedUsageRows      int64                 `json:"estimated_usage_rows"`
	Adjustments             []AdjustmentTotal     `json:"adjustments"`
	Matches                 []ReconciliationMatch `json:"matches"`
	Confidence              string                `json:"confidence"`
	LastReconciledAt        *time.Time            `json:"last_reconciled_at,omitempty"`
}

type AdjustmentTotal struct {
	Type         string `json:"type"`
	Lines        int64  `json:"lines"`
	AmountMicros int64  `json:"amount_micros"`
}

type ReconciliationMatch struct {
	Provider       string `json:"provider"`
	Day            string `json:"day"`
	Model          string `json:"model"`
	ServiceTier    string `json:"service_tier,omitempty"`
	Region         string `json:"region,omitempty"`
	LedgerRows     int64  `json:"ledger_rows"`
	InvoiceLines   int64  `json:"invoice_lines"`
	EstimateMicros int64  `json:"estimate_micros"`
	BilledMicros   int64  `json:"billed_micros"`
	VarianceMicros int64  `json:"variance_micros"`
	Matched        bool   `json:"matched"`
}

type reconcileKey struct {
	provider, day, model, tier, region string
}

type reconcileBucket struct {
	ledgerRows, invoiceLines int64
	estimate, billed         int64
	lowestConfidence         int
}

// Reconcile compares immutable observed rows with separately imported invoice
// evidence. through is exclusive. Matching is deliberately conservative and
// deterministic: provider + UTC day + model + service tier + region.
func (s *Store) Reconcile(from, through time.Time, provider string) (*ReconciliationReport, error) {
	if from.IsZero() || through.IsZero() || !through.After(from) {
		return nil, errors.New("reconciliation window requires from < through")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	result := &ReconciliationReport{From: from.UTC(), Through: through.UTC(), Provider: provider}
	buckets := map[reconcileKey]*reconcileBucket{}
	ledgerArgs := []any{from.UTC().Format(time.RFC3339), through.UTC().Format(time.RFC3339)}
	ledgerFilter := ""
	if provider != "" {
		ledgerFilter = " AND lower(provider)=?"
		ledgerArgs = append(ledgerArgs, provider)
	}
	rows, err := s.db.Query(`SELECT provider,substr(ts,1,10),model,service_tier,inference_geo,cost_usd,
		pricing_state,usage_state,cost_confidence FROM requests
		WHERE ts>=? AND ts<?`+ledgerFilter+` ORDER BY ts,id`, ledgerArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key reconcileKey
		var cost float64
		var pricingState, usageState, confidence string
		if err := rows.Scan(&key.provider, &key.day, &key.model, &key.tier, &key.region, &cost, &pricingState, &usageState, &confidence); err != nil {
			rows.Close()
			return nil, err
		}
		key.provider = strings.ToLower(key.provider)
		bucket := buckets[key]
		if bucket == nil {
			bucket = &reconcileBucket{}
			buckets[key] = bucket
		}
		bucket.ledgerRows++
		if pricingState != string(PricingPriced) {
			result.UnknownPricingRows++
			bucket.lowestConfidence = max(bucket.lowestConfidence, 3)
			continue
		}
		micros, err := dollarsToMicros(cost)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("ledger row has invalid cost: %w", err)
		}
		bucket.estimate, err = reconcile.CheckedAdd(bucket.estimate, micros)
		if err != nil {
			rows.Close()
			return nil, err
		}
		result.LedgerEstimateMicros, err = reconcile.CheckedAdd(result.LedgerEstimateMicros, micros)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if usageState != string(UsageExact) || confidence == "estimated" || confidence == "partial" || confidence == "unknown" {
			result.EstimatedUsageRows++
			bucket.lowestConfidence = max(bucket.lowestConfidence, 2)
		} else if confidence != "provider_final" && confidence != "contract" {
			bucket.lowestConfidence = max(bucket.lowestConfidence, 1)
		}
	}
	if err := errors.Join(rows.Close(), rows.Err()); err != nil {
		return nil, err
	}

	invoiceArgs := []any{from.UTC().Format(reconcile.TimestampFormat), through.UTC().Format(reconcile.TimestampFormat)}
	invoiceFilter := ""
	if provider != "" {
		invoiceFilter = " AND lower(i.provider)=?"
		invoiceArgs = append(invoiceArgs, provider)
	}
	rows, err = s.db.Query(`SELECT i.provider,substr(l.occurred_at,1,10),l.model,l.service_tier,l.region,l.billed_micros
		FROM invoice_lines l JOIN invoice_imports i ON i.id=l.import_id
		WHERE l.occurred_at>=? AND l.occurred_at<?`+invoiceFilter+`
		ORDER BY i.provider,l.occurred_at,l.model,l.service_tier,l.region,l.line_id`, invoiceArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key reconcileKey
		var amount int64
		if err := rows.Scan(&key.provider, &key.day, &key.model, &key.tier, &key.region, &amount); err != nil {
			rows.Close()
			return nil, err
		}
		key.provider = strings.ToLower(key.provider)
		bucket := buckets[key]
		if bucket == nil {
			bucket = &reconcileBucket{}
			buckets[key] = bucket
		}
		bucket.invoiceLines++
		bucket.billed, err = reconcile.CheckedAdd(bucket.billed, amount)
		if err != nil {
			rows.Close()
			return nil, err
		}
		result.ProviderBilledMicros, err = reconcile.CheckedAdd(result.ProviderBilledMicros, amount)
		if err != nil {
			rows.Close()
			return nil, err
		}
	}
	if err := errors.Join(rows.Close(), rows.Err()); err != nil {
		return nil, err
	}

	adjustments, adjustmentTotal, err := s.reconciliationAdjustments(from, through, provider)
	if err != nil {
		return nil, err
	}
	result.Adjustments = adjustments
	result.ProviderBilledMicros, err = reconcile.CheckedAdd(result.ProviderBilledMicros, adjustmentTotal)
	if err != nil {
		return nil, err
	}
	result.VarianceMicros, err = reconcile.CheckedAdd(result.ProviderBilledMicros, -result.LedgerEstimateMicros)
	if err != nil {
		return nil, err
	}

	keys := make([]reconcileKey, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.provider != b.provider {
			return a.provider < b.provider
		}
		if a.day != b.day {
			return a.day < b.day
		}
		if a.model != b.model {
			return a.model < b.model
		}
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		return a.region < b.region
	})
	worst := 0
	for _, key := range keys {
		bucket := buckets[key]
		matched := bucket.ledgerRows > 0 && bucket.invoiceLines > 0
		variance, err := reconcile.CheckedAdd(bucket.billed, -bucket.estimate)
		if err != nil {
			return nil, err
		}
		result.Matches = append(result.Matches, ReconciliationMatch{
			Provider: key.provider, Day: key.day, Model: key.model, ServiceTier: key.tier, Region: key.region,
			LedgerRows: bucket.ledgerRows, InvoiceLines: bucket.invoiceLines,
			EstimateMicros: bucket.estimate, BilledMicros: bucket.billed, VarianceMicros: variance, Matched: matched,
		})
		if matched {
			result.MatchedEstimateMicros, err = reconcile.CheckedAdd(result.MatchedEstimateMicros, bucket.estimate)
			if err != nil {
				return nil, err
			}
			result.MatchedBilledMicros, err = reconcile.CheckedAdd(result.MatchedBilledMicros, bucket.billed)
			if err != nil {
				return nil, err
			}
			worst = max(worst, bucket.lowestConfidence)
		} else if bucket.ledgerRows > 0 {
			result.UnmatchedLedgerRows += bucket.ledgerRows
			result.UnmatchedLedgerMicros, err = reconcile.CheckedAdd(result.UnmatchedLedgerMicros, bucket.estimate)
			if err != nil {
				return nil, err
			}
			worst = max(worst, 3)
		} else {
			result.UnmatchedProviderLines += bucket.invoiceLines
			result.UnmatchedProviderMicros, err = reconcile.CheckedAdd(result.UnmatchedProviderMicros, bucket.billed)
			if err != nil {
				return nil, err
			}
			worst = max(worst, 3)
		}
	}
	if result.UnknownPricingRows > 0 {
		worst = max(worst, 3)
	}
	last, err := s.lastReconciledAt(provider)
	if err != nil {
		return nil, err
	}
	result.LastReconciledAt = last
	switch {
	case last == nil:
		result.Confidence = "unknown"
	case worst >= 3:
		result.Confidence = "low"
	case worst == 2:
		result.Confidence = "partial"
	case worst == 1:
		result.Confidence = "list_estimate"
	default:
		result.Confidence = "high"
	}
	return result, nil
}

func (s *Store) reconciliationAdjustments(from, through time.Time, provider string) ([]AdjustmentTotal, int64, error) {
	args := []any{from.UTC().Format(reconcile.TimestampFormat), through.UTC().Format(reconcile.TimestampFormat)}
	filter := ""
	if provider != "" {
		filter = " AND lower(i.provider)=?"
		args = append(args, provider)
	}
	rows, err := s.db.Query(`SELECT a.adjustment_type,a.amount_micros
		FROM reconciliation_adjustments a JOIN invoice_imports i ON i.id=a.import_id
		WHERE a.occurred_at>=? AND a.occurred_at<?`+filter+`
		ORDER BY a.adjustment_type,a.occurred_at,a.line_id`, args...)
	if err != nil {
		return nil, 0, err
	}
	byType := map[string]*AdjustmentTotal{}
	var all int64
	for rows.Next() {
		var kind string
		var amount int64
		if err := rows.Scan(&kind, &amount); err != nil {
			rows.Close()
			return nil, 0, err
		}
		entry := byType[kind]
		if entry == nil {
			entry = &AdjustmentTotal{Type: kind}
			byType[kind] = entry
		}
		entry.Lines++
		entry.AmountMicros, err = reconcile.CheckedAdd(entry.AmountMicros, amount)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		all, err = reconcile.CheckedAdd(all, amount)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
	}
	if err := errors.Join(rows.Close(), rows.Err()); err != nil {
		return nil, 0, err
	}
	keys := make([]string, 0, len(byType))
	for key := range byType {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]AdjustmentTotal, 0, len(keys))
	for _, key := range keys {
		out = append(out, *byType[key])
	}
	return out, all, nil
}

func (s *Store) lastReconciledAt(provider string) (*time.Time, error) {
	query := `SELECT COALESCE(MAX(imported_at),'') FROM invoice_imports`
	args := []any{}
	if provider != "" {
		query += ` WHERE lower(provider)=?`
		args = append(args, provider)
	}
	var value string
	if err := s.db.QueryRow(query, args...).Scan(&value); err != nil {
		return nil, err
	}
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func dollarsToMicros(value float64) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1_000_000_000 {
		return 0, errors.New("cost is outside the supported range")
	}
	return int64(math.Round(value * 1_000_000)), nil
}
