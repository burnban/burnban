package subsidy

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// ScanGoose reads Goose's per-call usage_ledger. Current Goose releases keep
// model, token-type, cost, and timestamp fields here, so no prompt or message
// content needs to be read.
func ScanGoose(path string, since time.Time, emit func(Event)) (int, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return 0, fmt.Errorf("goose sessions: %w", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT session_id, created_timestamp,
		COALESCE(model, 'unknown'), COALESCE(input_tokens, 0),
		COALESCE(output_tokens, 0), COALESCE(cache_read_tokens, 0),
		COALESCE(cache_write_tokens, 0), COALESCE(cost, 0)
		FROM usage_ledger WHERE created_timestamp >= ?
		ORDER BY created_timestamp`, since.Unix())
	if err != nil {
		return 0, fmt.Errorf("goose usage ledger: %w", err)
	}
	defer rows.Close()
	sessions := map[string]struct{}{}
	for rows.Next() {
		var sessionID, model string
		var timestamp, in, out, cacheRead, cacheWrite int64
		var cost float64
		if err := rows.Scan(&sessionID, &timestamp, &model, &in, &out, &cacheRead, &cacheWrite, &cost); err != nil {
			return 0, fmt.Errorf("goose usage row: %w", err)
		}
		if in+out+cacheRead+cacheWrite == 0 {
			continue
		}
		sessions[sessionID] = struct{}{}
		emit(Event{
			Provider: "goose", Model: model, Time: time.Unix(timestamp, 0), Calls: 1,
			In: in, Out: out, CacheRead: cacheRead, CacheWrite5m: cacheWrite,
			CostUSD: cost, CostKnown: cost > 0,
		})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("goose usage ledger: %w", err)
	}
	return len(sessions), nil
}
