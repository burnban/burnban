package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// readQueryer is the common query surface implemented by both sql.DB and
// sql.Tx. Store methods use the transaction-backed implementation only while
// a ReadSnapshot callback is running.
type readQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func (s *Store) readQueryer() readQueryer {
	if s.snapshotReader != nil {
		return s.snapshotReader
	}
	return s.db
}

// ReadSnapshot is a read-only view of the ledger and settings at one SQLite
// point in time. It is valid only for the duration of the Store.ReadSnapshot
// callback and must not be retained by callers.
type ReadSnapshot struct {
	store *Store
}

// ReadSnapshot runs fn inside one SQLite read transaction. SQLite establishes
// the snapshot on fn's first query; every later query in the callback observes
// that same ledger and settings state while WAL writers continue normally.
func (s *Store) ReadSnapshot(fn func(*ReadSnapshot) error) error {
	if fn == nil {
		return errors.New("read snapshot callback is required")
	}
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	view := &ReadSnapshot{store: &Store{snapshotReader: tx}}
	if err := fn(view); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *ReadSnapshot) Summarize(since time.Time) (*Summary, error) {
	return r.store.Summarize(since)
}

func (r *ReadSnapshot) LifetimeMetrics() (*MetricsSummary, error) {
	return r.store.LifetimeMetrics()
}

func (r *ReadSnapshot) SpentSince(since time.Time) (float64, error) {
	return r.store.SpentSince(since)
}

func (r *ReadSnapshot) SpentSinceMulti(since []time.Time) ([]float64, error) {
	return r.store.SpentSinceMulti(since)
}

func (r *ReadSnapshot) SettingsWithPrefix(prefix string) (map[string]string, error) {
	return r.store.SettingsWithPrefix(prefix)
}

func (r *ReadSnapshot) UsageSinceForAgents(since time.Time, agents []string) (map[string]AgentRow, error) {
	return r.store.UsageSinceForAgents(since, agents)
}

func (r *ReadSnapshot) GetSetting(key string) (string, error) {
	return r.store.GetSetting(key)
}

func (r *ReadSnapshot) GetSettings(keys ...string) (map[string]string, error) {
	return r.store.GetSettings(keys...)
}
