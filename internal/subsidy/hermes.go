package subsidy

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/burnban/burnban/sourceadapter"
	_ "modernc.org/sqlite"
)

// ScanHermes reads the token counters Hermes keeps in its own state.db.
// The connection is explicitly read-only: Burnban never migrates, locks for
// writing, or otherwise owns another tool's database.
func ScanHermes(path string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanHermes(path, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

// hermesColumns returns the set of column names on the sessions table so the
// scanner can adapt to Hermes schema changes without failing outright.
func hermesColumns(ctx context.Context, db *sql.DB) map[string]bool {
	cols := map[string]bool{}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(sessions)`)
	if err != nil {
		return cols
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil {
			cols[name] = true
		}
	}
	return cols
}

func scanHermes(path string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error) {
	limits = normalizeScanLimits(limits)
	result := ScanResult{}
	stats, ready, err := preflightSQLiteSource(path, limits)
	if err != nil {
		return result, fmt.Errorf("hermes state: %w", err)
	}
	result.Stats = stats
	if !ready {
		return result, nil
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)")
	if err != nil {
		return result, fmt.Errorf("hermes state: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), limits.MaxDuration)
	defer cancel()
	// billing_provider and estimated_cost_usd are newer Hermes columns; older
	// stores omit them, so select them only when present and fall back to
	// neutral literals otherwise. This keeps the scan working across versions.
	cols := hermesColumns(ctx, db)
	billingExpr, costExpr := "''", "0"
	if cols["billing_provider"] {
		billingExpr = "COALESCE(billing_provider, '')"
	}
	if cols["estimated_cost_usd"] {
		costExpr = "COALESCE(estimated_cost_usd, 0)"
	}
	query := fmt.Sprintf(`SELECT COALESCE(model, 'unknown'), started_at,
		COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
		COALESCE(cache_read_tokens, 0), COALESCE(cache_write_tokens, 0),
		%s, %s
		FROM sessions WHERE started_at >= ? ORDER BY started_at LIMIT ?`, billingExpr, costExpr)
	rows, err := db.QueryContext(ctx, query, float64(since.UnixNano())/1e9, limits.MaxRecords+1)
	if err != nil {
		if ctx.Err() != nil {
			result.Stats.Warn("scan time limit reached")
		}
		return result, fmt.Errorf("hermes sessions: %w", err)
	}
	defer rows.Close()
	sessions := 0
	for rows.Next() {
		if result.Stats.RecordsScanned >= limits.MaxRecords {
			result.Stats.Warn("record scan limit reached")
			break
		}
		result.Stats.RecordsScanned++
		var model, billingProvider string
		var started, estimatedCostUSD float64
		var in, out, cacheRead, cacheWrite int64
		if err := rows.Scan(&model, &started, &in, &out, &cacheRead, &cacheWrite, &billingProvider, &estimatedCostUSD); err != nil {
			return result, fmt.Errorf("hermes session row: %w", err)
		}
		if in+out+cacheRead+cacheWrite == 0 {
			continue
		}
		sessions++
		event := Event{
			Provider: "hermes", Model: model,
			Time: time.Unix(0, int64(started*float64(time.Second))), Calls: 1,
			In: in, Out: out, CacheRead: cacheRead, CacheWrite5m: cacheWrite,
			BillingProvider: billingProvider,
			Confidence:      sourceadapter.ConfidenceExact,
		}
		// Hermes prices via the provider's live models API, so its own estimate
		// covers models outside Burnban's table (used only as a fallback there).
		if estimatedCostUSD > 0 {
			event.CostUSD = estimatedCostUSD
			event.CostKnown = true
		}
		emit(event)
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			result.Stats.Warn("scan time limit reached")
		}
		return result, fmt.Errorf("hermes sessions: %w", err)
	}
	result.Sessions = sessions
	return result, nil
}
