package localusage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/burnban/burnban/sourceadapter"
)

// ScanGoose reads Goose's per-call usage_ledger. Current Goose releases keep
// model, token-type, cost, and timestamp fields here, so no prompt or message
// content needs to be read.
func ScanGoose(path string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanGoose(path, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

func scanGoose(path string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error) {
	limits = normalizeScanLimits(limits)
	result := ScanResult{}
	stats, ready, err := preflightSQLiteSource(path, limits)
	if err != nil {
		return result, fmt.Errorf("goose sessions: %w", err)
	}
	result.Stats = stats
	if !ready {
		return result, nil
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)")
	if err != nil {
		return result, fmt.Errorf("goose sessions: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), limits.MaxDuration)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT session_id, created_timestamp,
		COALESCE(model, 'unknown'), COALESCE(input_tokens, 0),
		COALESCE(output_tokens, 0), COALESCE(cache_read_tokens, 0),
		COALESCE(cache_write_tokens, 0), COALESCE(cost, 0)
		FROM usage_ledger WHERE created_timestamp >= ?
		ORDER BY created_timestamp LIMIT ?`, since.Unix(), limits.MaxRecords+1)
	if err != nil {
		if ctx.Err() != nil {
			result.Stats.Warn("scan time limit reached")
		}
		return result, fmt.Errorf("goose usage ledger: %w", err)
	}
	defer rows.Close()
	sessions := map[string]struct{}{}
	for rows.Next() {
		if result.Stats.RecordsScanned >= limits.MaxRecords {
			result.Stats.Warn("record scan limit reached")
			break
		}
		result.Stats.RecordsScanned++
		var sessionID, model string
		var timestamp, in, out, cacheRead, cacheWrite int64
		var cost float64
		if err := rows.Scan(&sessionID, &timestamp, &model, &in, &out, &cacheRead, &cacheWrite, &cost); err != nil {
			return result, fmt.Errorf("goose usage row: %w", err)
		}
		if in+out+cacheRead+cacheWrite == 0 {
			continue
		}
		sessions[sessionID] = struct{}{}
		emit(Event{
			Provider: "goose", Model: model, Time: time.Unix(timestamp, 0), Calls: 1,
			In: in, Out: out, CacheRead: cacheRead, CacheWrite5m: cacheWrite,
			CostUSD: cost, CostKnown: cost > 0,
			Confidence: sourceadapter.ConfidenceExact,
		})
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			result.Stats.Warn("scan time limit reached")
		}
		return result, fmt.Errorf("goose usage ledger: %w", err)
	}
	result.Sessions = len(sessions)
	return result, nil
}
