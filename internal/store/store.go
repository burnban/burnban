// Package store persists every proxied request to a local SQLite database
// and answers the aggregate questions the CLI asks of it.
package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
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
	cost_usd REAL NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	status INTEGER NOT NULL DEFAULT 0,
	streamed INTEGER NOT NULL DEFAULT 0,
	estimated INTEGER NOT NULL DEFAULT 0,
	priced INTEGER NOT NULL DEFAULT 1,
	body_hash TEXT NOT NULL DEFAULT ''
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
`

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// WAL + synchronous=NORMAL: commits append to the log without an fsync
	// each — the standard WAL setup. Worst case on an OS crash is losing
	// the final moments of the ledger, never corrupting it.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Request is one proxied API call. In/Out are full-price tokens; cache
// fields hold discounted reads and premium writes, already normalized by
// the meter so pricing can treat all providers the same way.
type Request struct {
	Ts               time.Time
	Provider         string
	Model            string
	Agent            string
	Session          string
	InTokens         int64
	OutTokens        int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
	LatencyMs        int64
	Status           int
	Streamed         bool
	Estimated        bool
	Priced           bool
	BodyHash         string
}

func (s *Store) Insert(r Request) error {
	_, err := s.db.Exec(`INSERT INTO requests
		(ts, provider, model, agent, session, in_tokens, out_tokens,
		 cache_read_tokens, cache_write_tokens, cost_usd, latency_ms,
		 status, streamed, estimated, priced, body_hash)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Ts.UTC().Format(time.RFC3339), r.Provider, r.Model, r.Agent, r.Session,
		r.InTokens, r.OutTokens, r.CacheReadTokens, r.CacheWriteTokens,
		r.CostUSD, r.LatencyMs, r.Status, b2i(r.Streamed), b2i(r.Estimated),
		b2i(r.Priced), r.BodyHash)
	return err
}

func (s *Store) SpentSince(t time.Time) (float64, error) {
	var v float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM requests WHERE ts >= ?`,
		t.UTC().Format(time.RFC3339)).Scan(&v)
	return v, err
}

// SpentSinceMulti sums spend since each cutoff in one pass: a single range
// scan from the earliest cutoff with one conditional sum per window, so a
// request checked against three budget windows costs one query, not three.
func (s *Store) SpentSinceMulti(ts []time.Time) ([]float64, error) {
	if len(ts) == 0 {
		return nil, nil
	}
	min := ts[0]
	for _, t := range ts[1:] {
		if t.Before(min) {
			min = t
		}
	}
	cols := make([]string, len(ts))
	args := make([]any, 0, len(ts)+1)
	for i, t := range ts {
		cols[i] = "COALESCE(SUM(CASE WHEN ts >= ? THEN cost_usd ELSE 0 END),0)"
		args = append(args, t.UTC().Format(time.RFC3339))
	}
	args = append(args, min.UTC().Format(time.RFC3339))
	dests := make([]any, len(ts))
	out := make([]float64, len(ts))
	for i := range out {
		dests[i] = &out[i]
	}
	err := s.db.QueryRow(`SELECT `+strings.Join(cols, ", ")+
		` FROM requests WHERE ts >= ?`, args...).Scan(dests...)
	return out, err
}

func (s *Store) SpentSinceForAgent(t time.Time, agent string) (float64, error) {
	var v float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM requests WHERE ts >= ? AND agent = ?`,
		t.UTC().Format(time.RFC3339), agent).Scan(&v)
	return v, err
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
	rows, err := s.db.Query(`SELECT key, value FROM settings WHERE key LIKE ? || '%'`, prefix)
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
	Requests     int64
	Cost         float64
	In           int64
	Out          int64
	CacheRead    int64
	CacheWrite   int64
	Unpriced     int64
	Estimated    int64
	Models       []ModelRow
	Agents       []AgentRow
	DupGroups    int64
	DupWastedUSD float64
}

func (s *Store) Summarize(since time.Time) (*Summary, error) {
	ts := since.UTC().Format(time.RFC3339)
	sum := &Summary{}
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost_usd),0),
		COALESCE(SUM(in_tokens),0), COALESCE(SUM(out_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		COALESCE(SUM(CASE WHEN priced=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(estimated),0)
		FROM requests WHERE ts >= ?`, ts).
		Scan(&sum.Requests, &sum.Cost, &sum.In, &sum.Out,
			&sum.CacheRead, &sum.CacheWrite, &sum.Unpriced, &sum.Estimated)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`SELECT model, COUNT(*),
		COALESCE(SUM(in_tokens),0), COALESCE(SUM(out_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts >= ? AND model != ''
		GROUP BY model ORDER BY SUM(cost_usd) DESC LIMIT 20`, ts)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m ModelRow
		if err := rows.Scan(&m.Model, &m.Requests, &m.In, &m.Out, &m.CacheRead, &m.CacheWrite, &m.Cost); err != nil {
			rows.Close()
			return nil, err
		}
		sum.Models = append(sum.Models, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(`SELECT agent, COUNT(*), COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts >= ? AND agent != ''
		GROUP BY agent ORDER BY SUM(cost_usd) DESC LIMIT 20`, ts)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a AgentRow
		if err := rows.Scan(&a.Agent, &a.Requests, &a.Cost); err != nil {
			rows.Close()
			return nil, err
		}
		sum.Agents = append(sum.Agents, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(wasted),0) FROM (
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
	Requests   int64
	Unpriced   int64
	In         int64
	Out        int64
	CacheRead  int64
	CacheWrite int64
	CostUSD    float64
}

// TokenTotals sums tokens and cost across priced rows since t in one scan,
// counting unpriced rows on the side. Repricing math is linear in token
// counts, so these sums are all what-if needs.
func (s *Store) TokenTotals(t time.Time) (*Totals, error) {
	var tot Totals
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(priced),0),
		COALESCE(SUM(CASE WHEN priced=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN priced=1 THEN in_tokens ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN priced=1 THEN out_tokens ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN priced=1 THEN cache_read_tokens ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN priced=1 THEN cache_write_tokens ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN priced=1 THEN cost_usd ELSE 0 END),0)
		FROM requests WHERE ts >= ?`, t.UTC().Format(time.RFC3339)).
		Scan(&tot.Requests, &tot.Unpriced, &tot.In, &tot.Out, &tot.CacheRead, &tot.CacheWrite, &tot.CostUSD)
	if err != nil {
		return nil, err
	}
	return &tot, nil
}

// Export returns raw request rows for finance/audit tooling, oldest first.
func (s *Store) Export(since time.Time) ([]Request, error) {
	rows, err := s.db.Query(`SELECT ts, provider, model, agent, session,
		in_tokens, out_tokens, cache_read_tokens, cache_write_tokens,
		cost_usd, latency_ms, status, streamed, estimated, priced
		FROM requests WHERE ts >= ? ORDER BY ts`,
		since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		var ts string
		var streamed, estimated, priced int
		if err := rows.Scan(&ts, &r.Provider, &r.Model, &r.Agent, &r.Session,
			&r.InTokens, &r.OutTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.CostUSD, &r.LatencyMs, &r.Status, &streamed, &estimated, &priced); err != nil {
			return nil, err
		}
		r.Ts, _ = time.Parse(time.RFC3339, ts)
		r.Streamed, r.Estimated, r.Priced = streamed == 1, estimated == 1, priced == 1
		out = append(out, r)
	}
	return out, rows.Err()
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
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
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
	rows, err := s.db.Query(`SELECT key, value FROM settings WHERE key IN (?`+
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

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
