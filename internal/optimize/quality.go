// Package optimize derives bounded, content-free recommendations from the
// local ledger and parses a deliberately narrow external quality-score format.
package optimize

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/burnban/burnban/internal/store"
)

const (
	QualitySchema          = "burnban.external-quality/v1"
	MaxQualityPayloadBytes = 8 << 20
)

type qualityDocument struct {
	Schema    string         `json:"schema"`
	Source    string         `json:"source"`
	Metric    string         `json:"metric"`
	Cohort    string         `json:"cohort"`
	Direction string         `json:"direction"`
	Scores    []qualityScore `json:"scores"`
}

type qualityScore struct {
	ID         string `json:"id"`
	ObservedAt string `json:"observed_at"`
	Model      string `json:"model"`
	CaseHash   string `json:"case_hash"`
	Score      string `json:"score"`
}

// ParseQualityDocument accepts only the v1 content-free score envelope. The
// score is a decimal string so JSON floating-point rounding cannot change
// immutable evidence. Duplicate and case-ambiguous object fields fail closed.
func ParseQualityDocument(r io.Reader) ([]store.QualityScoreRecord, error) {
	limited := io.LimitReader(r, MaxQualityPayloadBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxQualityPayloadBytes {
		return nil, fmt.Errorf("quality payload exceeds %d bytes", MaxQualityPayloadBytes)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("quality JSON is not valid UTF-8")
	}
	if err := preflightQualityScoreCount(data); err != nil {
		return nil, err
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var document qualityDocument
	if err := dec.Decode(&document); err != nil {
		return nil, fmt.Errorf("quality JSON: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	if len(document.Scores) == 0 || len(document.Scores) > store.MaxQualityBatch {
		return nil, fmt.Errorf("quality document requires 1 to %d scores", store.MaxQualityBatch)
	}
	if err := validateQualityDocumentBounds(document); err != nil {
		return nil, err
	}
	if document.Schema != QualitySchema {
		return nil, fmt.Errorf("quality schema must be %q", QualitySchema)
	}
	records := make([]store.QualityScoreRecord, len(document.Scores))
	for i, score := range document.Scores {
		observedAt, err := time.Parse(time.RFC3339Nano, score.ObservedAt)
		if err != nil {
			return nil, fmt.Errorf("quality score %d observed_at must be RFC3339: %w", i, err)
		}
		ppm, err := ParseScorePPM(score.Score)
		if err != nil {
			return nil, fmt.Errorf("quality score %d: %w", i, err)
		}
		records[i] = store.QualityScoreRecord{
			Source: document.Source, ScoreID: score.ID, ObservedAt: observedAt.UTC(),
			Model: score.Model, Metric: document.Metric, Cohort: document.Cohort,
			CaseHash: score.CaseHash, ScorePPM: ppm, Direction: document.Direction,
		}
	}
	return records, nil
}

func preflightQualityScoreCount(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("quality JSON: %w", err)
	}
	if token != json.Delim('{') {
		return errors.New("quality JSON top level must be an object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("quality JSON: %w", err)
		}
		key, _ := keyToken.(string)
		if key != "scores" {
			var ignored json.RawMessage
			if err := decoder.Decode(&ignored); err != nil {
				return fmt.Errorf("quality JSON: %w", err)
			}
			continue
		}
		arrayToken, err := decoder.Token()
		if err != nil || arrayToken != json.Delim('[') {
			return errors.New("quality scores must be an array")
		}
		count := 0
		for decoder.More() {
			count++
			if count > store.MaxQualityBatch {
				return fmt.Errorf("quality document exceeds %d scores", store.MaxQualityBatch)
			}
			var ignored json.RawMessage
			if err := decoder.Decode(&ignored); err != nil {
				return fmt.Errorf("quality JSON: %w", err)
			}
		}
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("quality JSON: %w", err)
		}
	}
	return nil
}

func validateQualityDocumentBounds(document qualityDocument) error {
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"schema", document.Schema, 64}, {"source", document.Source, 64},
		{"metric", document.Metric, 64}, {"cohort", document.Cohort, 128},
		{"direction", document.Direction, 32},
	} {
		if len(field.value) > field.limit {
			return fmt.Errorf("quality %s exceeds %d bytes", field.name, field.limit)
		}
	}
	for i, score := range document.Scores {
		for _, field := range []struct {
			name  string
			value string
			limit int
		}{
			{"id", score.ID, 128}, {"observed_at", score.ObservedAt, 64},
			{"model", score.Model, 200}, {"case_hash", score.CaseHash, 64},
			{"score", score.Score, 8},
		} {
			if len(field.value) > field.limit {
				return fmt.Errorf("quality score %d %s exceeds %d bytes", i, field.name, field.limit)
			}
		}
	}
	return nil
}

// ParseScorePPM parses [0,1] with at most six fractional decimal digits.
// Exponents, signs, NaN, infinities, and rounding are intentionally rejected.
func ParseScorePPM(value string) (int64, error) {
	if value == "" || value != strings.TrimSpace(value) || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		return 0, errors.New("score must be a canonical decimal string between 0 and 1")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || (parts[0] != "0" && parts[0] != "1") {
		return 0, errors.New("score must be a canonical decimal string between 0 and 1")
	}
	if len(parts) == 1 {
		if value == "0" {
			return 0, nil
		}
		if value == "1" {
			return 1_000_000, nil
		}
		return 0, errors.New("score must be a canonical decimal string between 0 and 1")
	}
	if parts[1] == "" || len(parts[1]) > 6 {
		return 0, errors.New("score supports one to six fractional decimal digits")
	}
	for _, c := range parts[1] {
		if c < '0' || c > '9' {
			return 0, errors.New("score must be a canonical decimal string between 0 and 1")
		}
	}
	if parts[0] == "1" && strings.Trim(parts[1], "0") != "" {
		return 0, errors.New("score must not exceed 1")
	}
	fraction, err := strconv.ParseInt(parts[1]+strings.Repeat("0", 6-len(parts[1])), 10, 64)
	if err != nil {
		return 0, errors.New("invalid score")
	}
	if parts[0] == "1" {
		return 1_000_000, nil
	}
	return fraction, nil
}

// FormatScorePPM returns the canonical decimal form used in human receipts.
func FormatScorePPM(value int64) string {
	if value < 0 || value > 1_000_000 {
		return "invalid"
	}
	if value == 0 {
		return "0"
	}
	if value == 1_000_000 {
		return "1"
	}
	return "0." + strings.TrimRight(fmt.Sprintf("%06d", value), "0")
}

func ensureJSONEOF(dec *json.Decoder) error {
	var trailing any
	err := dec.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("quality JSON contains multiple top-level values")
	}
	return fmt.Errorf("quality JSON trailing data: %w", err)
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder, 0); err != nil {
		return fmt.Errorf("quality JSON: %w", err)
	}
	return nil
}

var canonicalQualityJSONFields = map[string]string{
	"schema": "schema", "source": "source", "metric": "metric", "cohort": "cohort",
	"direction": "direction", "scores": "scores", "id": "id", "observed_at": "observed_at",
	"model": "model", "case_hash": "case_hash", "score": "score",
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting exceeds 32 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]string{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key must be a string")
			}
			if len(key) > 128 {
				return errors.New("object field name exceeds 128 bytes")
			}
			folded := strings.ToLower(key)
			canonical, known := canonicalQualityJSONFields[folded]
			if !known || key != canonical {
				return fmt.Errorf("unknown or non-canonical JSON field %q", key)
			}
			if previous, duplicate := seen[folded]; duplicate {
				return fmt.Errorf("duplicate or case-ambiguous JSON fields %q and %q", previous, key)
			}
			seen[folded] = key
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}
