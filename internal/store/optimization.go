package store

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const optimizationSchema = `
CREATE TABLE IF NOT EXISTS external_quality_scores (
	source TEXT NOT NULL CHECK(length(source) BETWEEN 1 AND 64),
	score_id TEXT NOT NULL CHECK(length(score_id) BETWEEN 1 AND 128),
	content_sha256 TEXT NOT NULL CHECK(length(content_sha256)=64),
	observed_unix_nano INTEGER NOT NULL,
	observed_at TEXT NOT NULL,
	model TEXT NOT NULL CHECK(length(model) BETWEEN 1 AND 200),
	metric TEXT NOT NULL CHECK(length(metric) BETWEEN 1 AND 64),
	cohort TEXT NOT NULL CHECK(length(cohort) BETWEEN 1 AND 128),
	case_hash TEXT NOT NULL CHECK(length(case_hash)=64),
	score_ppm INTEGER NOT NULL CHECK(score_ppm BETWEEN 0 AND 1000000),
	direction TEXT NOT NULL CHECK(direction='higher_is_better'),
	imported_at TEXT NOT NULL,
	PRIMARY KEY(source, score_id),
	UNIQUE(source, metric, cohort, model, case_hash)
);
CREATE INDEX IF NOT EXISTS idx_quality_scope_time_model
	ON external_quality_scores(source, metric, cohort, observed_unix_nano, model);

CREATE TRIGGER IF NOT EXISTS external_quality_scores_no_update
	BEFORE UPDATE ON external_quality_scores
	BEGIN SELECT RAISE(ABORT, 'external quality scores are immutable'); END;
CREATE TRIGGER IF NOT EXISTS external_quality_scores_no_delete
	BEFORE DELETE ON external_quality_scores
	BEGIN SELECT RAISE(ABORT, 'external quality scores are immutable'); END;
`

const (
	MaxOptimizationRows  = 100_000
	MaxOptimizationRange = 90 * 24 * time.Hour
	MaxQualityBatch      = 10_000
	MaxQualityModels     = 1_000
)

// OptimizationRow is the bounded, content-free ledger projection consumed by
// recommendation algorithms. It intentionally excludes request bodies,
// response bodies, body fingerprints, session IDs, and identity principals.
type OptimizationRow struct {
	ID                 int64
	Ts                 time.Time
	Provider           string
	Model              string
	Agent              string
	Project            string
	Meter              string
	Team               string
	IdentityConfidence string
	Route              string
	InTokens           int64
	CacheReadTokens    int64
	CacheWriteTokens   int64
	CostUSD            float64
	PricingState       PricingState
	UsageState         UsageState
	EnforcementUnsafe  bool
}

type OptimizationSample struct {
	Rows      []OptimizationRow
	Truncated bool
}

// OptimizationRows returns at most maxRows recent metadata rows from a
// bounded, half-open window. Asking for one extra row makes truncation explicit
// without ever allowing recommendation commands to read an unbounded ledger.
func (s *Store) OptimizationRows(from, through time.Time, maxRows int) (OptimizationSample, error) {
	if from.IsZero() || through.IsZero() || !through.After(from) {
		return OptimizationSample{}, errors.New("optimization window requires from < through")
	}
	if through.Sub(from) > MaxOptimizationRange {
		return OptimizationSample{}, fmt.Errorf("optimization window exceeds %s", MaxOptimizationRange)
	}
	if maxRows < 1 || maxRows > MaxOptimizationRows {
		return OptimizationSample{}, fmt.Errorf("max rows must be between 1 and %d", MaxOptimizationRows)
	}
	rows, err := s.readQueryer().Query(`SELECT id,ts,provider,model,agent,project,
		identity_device,cost_center,identity_confidence,route,
		in_tokens,cache_read_tokens,cache_write_tokens,cost_usd,pricing_state,
		usage_state,enforcement_unsafe
		FROM requests WHERE ts>=? AND ts<? ORDER BY ts DESC,id DESC LIMIT ?`,
		from.UTC().Format(time.RFC3339), through.UTC().Format(time.RFC3339), maxRows+1)
	if err != nil {
		return OptimizationSample{}, err
	}
	defer rows.Close()
	out := OptimizationSample{Rows: make([]OptimizationRow, 0, min(maxRows, 4096))}
	for rows.Next() {
		if len(out.Rows) == maxRows {
			out.Truncated = true
			break
		}
		var row OptimizationRow
		var ts string
		var unsafe int
		if err := rows.Scan(&row.ID, &ts, &row.Provider, &row.Model, &row.Agent, &row.Project,
			&row.Meter, &row.Team, &row.IdentityConfidence, &row.Route,
			&row.InTokens, &row.CacheReadTokens, &row.CacheWriteTokens,
			&row.CostUSD, &row.PricingState, &row.UsageState, &unsafe); err != nil {
			return OptimizationSample{}, err
		}
		parsed, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return OptimizationSample{}, fmt.Errorf("invalid ledger timestamp: %w", err)
		}
		row.Ts = parsed
		row.EnforcementUnsafe = unsafe != 0
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return OptimizationSample{}, err
	}
	return out, nil
}

// QualityScoreRecord is a normalized external observation. ScorePPM is an
// exact fixed-point value in [0,1] at six-decimal precision. CaseHash must be
// a caller-created SHA-256 digest of an evaluation case ID; raw case IDs and
// evaluation content are deliberately outside this API.
type QualityScoreRecord struct {
	Source     string
	ScoreID    string
	ObservedAt time.Time
	Model      string
	Metric     string
	Cohort     string
	CaseHash   string
	ScorePPM   int64
	Direction  string
}

var (
	ErrQualityInvalid      = errors.New("invalid external quality evidence")
	ErrQualityConflict     = errors.New("quality score identity already contains different immutable evidence")
	ErrQualityCaseConflict = errors.New("quality score case already has evidence for this source, metric, cohort, and model")
)

type QualityImportResult struct {
	Inserted int `json:"inserted"`
	Replayed int `json:"replayed"`
}

// ImportQualityScores atomically appends normalized external evidence. An
// exact replay of source+score ID is idempotent; identity reuse or a second
// score for the same model/case conflicts rather than silently overwriting the
// first observation. The database remains immutable even for direct UPDATE or
// DELETE statements because schema triggers reject them.
func (s *Store) ImportQualityScores(records []QualityScoreRecord, importedAt time.Time) (QualityImportResult, error) {
	if len(records) == 0 || len(records) > MaxQualityBatch {
		return QualityImportResult{}, fmt.Errorf("%w: import requires 1 to %d scores", ErrQualityInvalid, MaxQualityBatch)
	}
	if importedAt.IsZero() || importedAt.Before(time.Unix(0, 0)) || importedAt.After(time.Now().UTC().Add(24*time.Hour)) {
		return QualityImportResult{}, fmt.Errorf("%w: imported_at must be between 1970-01-01 and 24 hours in the future", ErrQualityInvalid)
	}
	seenIDs := make(map[string]struct{}, len(records))
	seenCases := make(map[string]struct{}, len(records))
	for i := range records {
		if err := validateQualityRecord(records[i]); err != nil {
			return QualityImportResult{}, fmt.Errorf("%w: score %d: %v", ErrQualityInvalid, i, err)
		}
		idKey := records[i].Source + "\x00" + records[i].ScoreID
		if _, duplicate := seenIDs[idKey]; duplicate {
			return QualityImportResult{}, fmt.Errorf("%w: score %d duplicates source+id in this import", ErrQualityInvalid, i)
		}
		seenIDs[idKey] = struct{}{}
		caseKey := records[i].Source + "\x00" + records[i].Metric + "\x00" + records[i].Cohort + "\x00" + records[i].Model + "\x00" + records[i].CaseHash
		if _, duplicate := seenCases[caseKey]; duplicate {
			return QualityImportResult{}, fmt.Errorf("%w: score %d duplicates a model evaluation case in this import", ErrQualityInvalid, i)
		}
		seenCases[caseKey] = struct{}{}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return QualityImportResult{}, err
	}
	defer tx.Rollback()
	var result QualityImportResult
	for _, record := range records {
		hash := hashQualityRecord(record)
		inserted, err := tx.Exec(`INSERT OR IGNORE INTO external_quality_scores
			(source,score_id,content_sha256,observed_unix_nano,observed_at,model,
			 metric,cohort,case_hash,score_ppm,direction,imported_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, record.Source, record.ScoreID, hash,
			record.ObservedAt.UTC().UnixNano(), record.ObservedAt.UTC().Format(time.RFC3339Nano),
			record.Model, record.Metric, record.Cohort, record.CaseHash, record.ScorePPM,
			record.Direction, importedAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return QualityImportResult{}, err
		}
		rows, err := inserted.RowsAffected()
		if err != nil {
			return QualityImportResult{}, err
		}
		if rows == 1 {
			result.Inserted++
			continue
		}
		var existingHash string
		err = tx.QueryRow(`SELECT content_sha256 FROM external_quality_scores
			WHERE source=? AND score_id=?`, record.Source, record.ScoreID).Scan(&existingHash)
		if err == nil {
			if len(existingHash) == len(hash) && subtle.ConstantTimeCompare([]byte(existingHash), []byte(hash)) == 1 {
				result.Replayed++
				continue
			}
			return QualityImportResult{}, fmt.Errorf("%w: source=%q id=%q", ErrQualityConflict, record.Source, record.ScoreID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return QualityImportResult{}, err
		}
		return QualityImportResult{}, fmt.Errorf("%w: source=%q model=%q case_hash=%q", ErrQualityCaseConflict, record.Source, record.Model, record.CaseHash)
	}
	if err := tx.Commit(); err != nil {
		return QualityImportResult{}, err
	}
	return result, nil
}

func validateQualityRecord(record QualityScoreRecord) error {
	for name, value := range map[string]string{
		"source": record.Source, "score_id": record.ScoreID, "model": record.Model,
		"metric": record.Metric, "cohort": record.Cohort,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("%s is required without surrounding whitespace", name)
		}
		limit := 128
		switch name {
		case "source", "metric":
			limit = 64
		case "model":
			limit = 200
		}
		if len(value) > limit || !utf8.ValidString(value) {
			return fmt.Errorf("%s must be valid UTF-8 and at most %d bytes", name, limit)
		}
		for _, r := range value {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
				return fmt.Errorf("%s contains a control or invisible formatting character", name)
			}
		}
	}
	if !safeQualitySlug(record.Source) || !safeQualitySlug(record.Metric) || !safeQualitySlug(record.Cohort) || !safeQualitySlug(record.ScoreID) {
		return errors.New("source, metric, cohort, and score_id may contain only letters, numbers, dot, underscore, colon, slash, and hyphen")
	}
	if record.ObservedAt.IsZero() || record.ObservedAt.Before(time.Unix(0, 0)) || record.ObservedAt.After(time.Now().UTC().Add(24*time.Hour)) {
		return errors.New("observed_at must be between 1970-01-01 and 24 hours in the future")
	}
	if len(record.CaseHash) != 64 {
		return errors.New("case_hash must be exactly 64 lowercase hexadecimal characters")
	}
	for _, c := range record.CaseHash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return errors.New("case_hash must be exactly 64 lowercase hexadecimal characters")
		}
	}
	if record.ScorePPM < 0 || record.ScorePPM > 1_000_000 {
		return errors.New("score must be between 0 and 1")
	}
	if record.Direction != "higher_is_better" {
		return errors.New("direction must be higher_is_better; transform lower-is-better metrics before import")
	}
	return nil
}

func safeQualitySlug(value string) bool {
	for _, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("._:/-", c) {
			continue
		}
		return false
	}
	return value != ""
}

func hashQualityRecord(record QualityScoreRecord) string {
	h := sha256.New()
	writeString := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(value))
	}
	for _, value := range []string{
		"burnban.external-quality/v1", record.Source, record.ScoreID,
		record.ObservedAt.UTC().Format(time.RFC3339Nano), record.Model, record.Metric,
		record.Cohort, record.CaseHash, record.Direction,
	} {
		writeString(value)
	}
	var score [8]byte
	binary.BigEndian.PutUint64(score[:], uint64(record.ScorePPM))
	_, _ = h.Write(score[:])
	return fmt.Sprintf("%x", h.Sum(nil))
}

type QualitySummary struct {
	Model           string  `json:"model"`
	Samples         int64   `json:"samples"`
	CohortCases     int64   `json:"cohort_cases"`
	Coverage        float64 `json:"coverage"`
	AverageScorePPM int64   `json:"average_score_ppm"`
}

// QualitySummaries returns bounded aggregate coverage for exact candidate
// model IDs in one external source/metric/cohort and time window. CohortCases
// is the distinct union of privacy-safe case hashes; callers can require a
// minimum coverage before treating an average score as decision evidence.
func (s *Store) QualitySummaries(from, through time.Time, source, metric, cohort string, models []string) (map[string]QualitySummary, error) {
	if from.IsZero() || through.IsZero() || !through.After(from) {
		return nil, errors.New("quality window requires from < through")
	}
	if len(models) == 0 || len(models) > MaxQualityModels {
		return nil, fmt.Errorf("quality query requires 1 to %d candidate models", MaxQualityModels)
	}
	probe := QualityScoreRecord{Source: source, ScoreID: "probe", ObservedAt: from, Model: models[0], Metric: metric, Cohort: cohort, CaseHash: strings.Repeat("0", 64), Direction: "higher_is_better"}
	if err := validateQualityRecord(probe); err != nil {
		return nil, fmt.Errorf("quality selector: %w", err)
	}
	if through.Sub(from) > 366*24*time.Hour {
		return nil, errors.New("quality query window exceeds 366 days")
	}
	unique := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		probe.Model = model
		if err := validateQualityRecord(probe); err != nil {
			return nil, fmt.Errorf("quality candidate model: %w", err)
		}
		if _, ok := seen[model]; !ok {
			seen[model] = struct{}{}
			unique = append(unique, model)
		}
	}
	fromNano, throughNano := from.UTC().UnixNano(), through.UTC().UnixNano()
	var cohortCases int64
	if err := s.readQueryer().QueryRow(`SELECT COUNT(DISTINCT case_hash)
		FROM external_quality_scores WHERE source=? AND metric=? AND cohort=?
		AND observed_unix_nano>=? AND observed_unix_nano<?`,
		source, metric, cohort, fromNano, throughNano).Scan(&cohortCases); err != nil {
		return nil, err
	}
	out := make(map[string]QualitySummary, len(unique))
	const chunkSize = 400
	for start := 0; start < len(unique); start += chunkSize {
		end := min(start+chunkSize, len(unique))
		chunk := unique[start:end]
		args := make([]any, 0, len(chunk)+5)
		args = append(args, source, metric, cohort, fromNano, throughNano)
		for _, model := range chunk {
			args = append(args, model)
		}
		rows, err := s.readQueryer().Query(`SELECT model,COUNT(*),AVG(CAST(score_ppm AS REAL))
			FROM external_quality_scores WHERE source=? AND metric=? AND cohort=?
			AND observed_unix_nano>=? AND observed_unix_nano<? AND model IN (?`+
			strings.Repeat(",?", len(chunk)-1)+`) GROUP BY model ORDER BY model`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var summary QualitySummary
			var average float64
			if err := rows.Scan(&summary.Model, &summary.Samples, &average); err != nil {
				rows.Close()
				return nil, err
			}
			if math.IsNaN(average) || math.IsInf(average, 0) || average < 0 || average > 1_000_000 {
				rows.Close()
				return nil, errors.New("quality score aggregate is corrupt")
			}
			summary.AverageScorePPM = int64(math.Round(average))
			summary.CohortCases = cohortCases
			if cohortCases > 0 {
				summary.Coverage = float64(summary.Samples) / float64(cohortCases)
			}
			out[summary.Model] = summary
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
