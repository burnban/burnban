package subsidy

import (
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
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return 0, fmt.Errorf("hermes state: %w", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT COALESCE(model, 'unknown'), started_at,
		COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
		COALESCE(cache_read_tokens, 0), COALESCE(cache_write_tokens, 0)
		FROM sessions WHERE started_at >= ? ORDER BY started_at`, float64(since.UnixNano())/1e9)
	if err != nil {
		return 0, fmt.Errorf("hermes sessions: %w", err)
	}
	defer rows.Close()
	sessions := 0
	for rows.Next() {
		var model string
		var started float64
		var in, out, cacheRead, cacheWrite int64
		if err := rows.Scan(&model, &started, &in, &out, &cacheRead, &cacheWrite); err != nil {
			return 0, fmt.Errorf("hermes session row: %w", err)
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
		return 0, fmt.Errorf("hermes sessions: %w", err)
	}
	return sessions, nil
}
