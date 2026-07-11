package subsidy

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ScanHermes reads the token counters Hermes keeps in its own state.db.
// The connection is explicitly read-only: Burnban never migrates, locks for
// writing, or otherwise owns another tool's database.
func ScanHermes(path string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanHermes(path, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

func scanHermes(path string, since time.Time, limits ScanLimits, emit func(Event)) (scanResult, error) {
	limits = normalizeScanLimits(limits)
	result := scanResult{}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	result.Stats.FilesScanned = 1
	result.Stats.BytesScanned = min(info.Size(), limits.MaxBytes)
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return result, fmt.Errorf("hermes state: %w", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), limits.MaxDuration)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(model, 'unknown'), started_at,
		COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
		COALESCE(cache_read_tokens, 0), COALESCE(cache_write_tokens, 0)
		FROM sessions WHERE started_at >= ? ORDER BY started_at LIMIT ?`, float64(since.UnixNano())/1e9, limits.MaxRecords+1)
	if err != nil {
		if ctx.Err() != nil {
			result.Stats.warn("scan time limit reached")
		}
		return result, fmt.Errorf("hermes sessions: %w", err)
	}
	defer rows.Close()
	sessions := 0
	for rows.Next() {
		if result.Stats.RecordsScanned >= limits.MaxRecords {
			result.Stats.warn("record scan limit reached")
			break
		}
		result.Stats.RecordsScanned++
		var model string
		var started float64
		var in, out, cacheRead, cacheWrite int64
		if err := rows.Scan(&model, &started, &in, &out, &cacheRead, &cacheWrite); err != nil {
			return result, fmt.Errorf("hermes session row: %w", err)
		}
		if in+out+cacheRead+cacheWrite == 0 {
			continue
		}
		sessions++
		emit(Event{
			Provider: "hermes", Model: model,
			Time: time.Unix(0, int64(started*float64(time.Second))), Calls: 1,
			In: in, Out: out, CacheRead: cacheRead, CacheWrite5m: cacheWrite,
		})
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			result.Stats.warn("scan time limit reached")
		}
		return result, fmt.Errorf("hermes sessions: %w", err)
	}
	result.Sessions = sessions
	return result, nil
}
