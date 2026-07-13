package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/burnban/burnban/internal/store"
)

const checkpointSchemaVersion = "burnban.telemetry.checkpoint.v2"

type telemetryStore interface {
	TelemetryRowsAfter(afterID int64, limit int) ([]store.TelemetryRow, error)
	TelemetryBacklog(afterID, keep int64) (pending, dropThrough int64, err error)
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
}

type Sink interface {
	SinkID() string
	Export(context.Context, Batch) (ExportResult, error)
}

type WorkerConfig struct {
	BatchSize    int
	MaxBacklog   int64
	PollInterval time.Duration
	Logf         func(string, ...any)
}

type Stats struct {
	Enabled        bool       `json:"enabled"`
	State          string     `json:"state"`
	AckedThrough   int64      `json:"acked_through"`
	DroppedThrough int64      `json:"dropped_through"`
	TracesThrough  int64      `json:"traces_through"`
	MetricsThrough int64      `json:"metrics_through"`
	DroppedRows    int64      `json:"dropped_rows"`
	RejectedSpans  int64      `json:"rejected_spans"`
	RejectedPoints int64      `json:"rejected_data_points"`
	PendingRows    int64      `json:"pending_rows"`
	LastSuccess    *time.Time `json:"last_success,omitempty"`
	LastFailure    *time.Time `json:"last_failure,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
}

type checkpoint struct {
	SchemaVersion  string `json:"schema_version"`
	AckedThrough   int64  `json:"acked_through"`
	DroppedThrough int64  `json:"dropped_through"`
	TracesThrough  int64  `json:"traces_through,omitempty"`
	MetricsThrough int64  `json:"metrics_through,omitempty"`
	DroppedRows    int64  `json:"dropped_rows"`
	RejectedSpans  int64  `json:"rejected_spans,omitempty"`
	RejectedPoints int64  `json:"rejected_data_points,omitempty"`
	TracesFailed   bool   `json:"traces_failed,omitempty"`
	MetricsFailed  bool   `json:"metrics_failed,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

func (c checkpoint) cursor() int64 { return min(c.TracesThrough, c.MetricsThrough) }

type Worker struct {
	store         telemetryStore
	sink          Sink
	checkpointKey string
	batchSize     int
	maxBacklog    int64
	pollInterval  time.Duration
	logf          func(string, ...any)

	runMu      sync.Mutex
	loaded     bool
	checkpoint checkpoint

	statsMu sync.RWMutex
	stats   Stats

	startOnce    sync.Once
	lifecycleMu  sync.Mutex
	cancel       context.CancelFunc
	done         chan struct{}
	logMu        sync.Mutex
	lastLog      time.Time
	lastLogError string
}

func NewWorker(ledger telemetryStore, sink Sink, config WorkerConfig) (*Worker, error) {
	if ledger == nil || sink == nil {
		return nil, fmt.Errorf("telemetry store and sink are required")
	}
	if !validSinkID(sink.SinkID()) {
		return nil, fmt.Errorf("telemetry sink identity is invalid")
	}
	if config.BatchSize == 0 {
		config.BatchSize = 128
	}
	if config.BatchSize < 1 || config.BatchSize > 1000 {
		return nil, fmt.Errorf("telemetry batch size must be between 1 and 1000")
	}
	if config.MaxBacklog == 0 {
		config.MaxBacklog = 10_000
	}
	if config.MaxBacklog < int64(config.BatchSize) || config.MaxBacklog > 10_000_000 {
		return nil, fmt.Errorf("telemetry max backlog must be between batch size and 10000000")
	}
	if config.PollInterval == 0 {
		config.PollInterval = 2 * time.Second
	}
	if config.PollInterval < 100*time.Millisecond || config.PollInterval > time.Minute {
		return nil, fmt.Errorf("telemetry poll interval must be between 100ms and 1m")
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
	return &Worker{
		store: ledger, sink: sink,
		checkpointKey: "internal.telemetry.v1." + sink.SinkID() + ".checkpoint",
		batchSize:     config.BatchSize, maxBacklog: config.MaxBacklog,
		pollInterval: config.PollInterval, logf: config.Logf,
		stats: Stats{Enabled: true, State: "starting"}, done: make(chan struct{}),
	}, nil
}

func validSinkID(value string) bool {
	if len(value) < 16 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// Start launches the exporter exactly once. It returns immediately and never
// changes inference admission or provider response handling.
func (w *Worker) Start(parent context.Context) {
	w.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		w.lifecycleMu.Lock()
		w.cancel = cancel
		w.lifecycleMu.Unlock()
		go func() {
			defer close(w.done)
			w.run(ctx)
		}()
	})
}

func (w *Worker) run(ctx context.Context) {
	_ = w.DrainOnce(ctx)
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.DrainOnce(ctx)
		}
	}
}

// Stop cancels in-flight collector I/O and waits until the worker exits or the
// caller's shutdown deadline expires.
func (w *Worker) Stop(ctx context.Context) error {
	w.lifecycleMu.Lock()
	cancel := w.cancel
	w.lifecycleMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DrainOnce processes at most one bounded batch. It is exported for explicit
// diagnostics and deterministic tests; normal serving uses Start.
func (w *Worker) DrainOnce(ctx context.Context) error {
	w.runMu.Lock()
	defer w.runMu.Unlock()
	if err := w.loadCheckpoint(); err != nil {
		w.failed(err)
		return err
	}
	cursor := w.checkpoint.cursor()
	pending, dropThrough, err := w.store.TelemetryBacklog(cursor, w.maxBacklog)
	if err != nil {
		w.failed(err)
		return err
	}
	w.setPending(pending)
	if dropThrough > cursor {
		dropped := pending - w.maxBacklog
		next := w.checkpoint
		next.DroppedThrough = max(next.DroppedThrough, dropThrough)
		next.DroppedRows = saturatedCounterAdd(next.DroppedRows, dropped)
		next.TracesThrough = max(next.TracesThrough, dropThrough)
		next.MetricsThrough = max(next.MetricsThrough, dropThrough)
		if next.TracesThrough == next.MetricsThrough {
			next.TracesFailed, next.MetricsFailed = false, false
		}
		if err := w.saveCheckpoint(next); err != nil {
			w.failed(err)
			return err
		}
		w.checkpoint = next
		cursor = next.cursor()
		pending -= dropped
		w.setPending(pending)
		w.logf("burnban: telemetry backlog bound exceeded; recorded %d prompt-free ledger rows as dropped", dropped)
	}
	rows, err := w.store.TelemetryRowsAfter(cursor, w.batchSize)
	if err != nil {
		w.failed(err)
		return err
	}
	if len(rows) == 0 {
		w.healthy(false)
		return nil
	}
	traceCursor, metricCursor := w.checkpoint.TracesThrough, w.checkpoint.MetricsThrough
	if traceCursor != metricCursor {
		// Do not mix rows that are already terminal for the leading signal with
		// newer rows that still need both signals. This keeps the durable signal
		// cursors aligned at explicit batch boundaries without assuming request
		// IDs are contiguous.
		ahead := max(traceCursor, metricCursor)
		prefix := 0
		for prefix < len(rows) && rows[prefix].ID <= ahead {
			prefix++
		}
		if prefix > 0 {
			rows = rows[:prefix]
		}
	}
	events := make([]Event, len(rows))
	for i, row := range rows {
		events[i] = FromRow(row)
	}
	lastID := rows[len(rows)-1].ID
	var signals SignalMask
	if traceCursor < lastID {
		signals |= SignalTraces
	}
	if metricCursor < lastID {
		signals |= SignalMetrics
	}
	if signals == 0 {
		err := fmt.Errorf("telemetry signal cursors are inconsistent with selected rows")
		w.failed(err)
		return err
	}
	result, exportErr := w.sink.Export(ctx, Batch{
		Events: events, DroppedRows: w.checkpoint.DroppedRows, Signals: signals,
	})
	contractErr := validateExportResult(signals, result)
	if exportErr == nil {
		if signals&SignalTraces != 0 && (!result.Traces.Terminal || result.Traces.Failed) {
			contractErr = errors.Join(contractErr, fmt.Errorf("telemetry sink returned failed or non-terminal traces without an error"))
		}
		if signals&SignalMetrics != 0 && (!result.Metrics.Terminal || result.Metrics.Failed) {
			contractErr = errors.Join(contractErr, fmt.Errorf("telemetry sink returned failed or non-terminal metrics without an error"))
		}
	}
	if contractErr != nil {
		exportErr = errors.Join(exportErr, contractErr)
	}
	next := w.checkpoint
	progressed := false
	if signals&SignalTraces != 0 && result.Traces.Terminal {
		next.TracesThrough = lastID
		next.TracesFailed = next.TracesFailed || result.Traces.Failed
		next.RejectedSpans = saturatedCounterAdd(next.RejectedSpans, result.Traces.RejectedItems)
		progressed = true
	}
	if signals&SignalMetrics != 0 && result.Metrics.Terminal {
		next.MetricsThrough = lastID
		next.MetricsFailed = next.MetricsFailed || result.Metrics.Failed
		next.RejectedPoints = saturatedCounterAdd(next.RejectedPoints, result.Metrics.RejectedItems)
		progressed = true
	}
	if next.TracesThrough == next.MetricsThrough && next.cursor() > cursor {
		if next.TracesFailed || next.MetricsFailed {
			next.DroppedThrough = max(next.DroppedThrough, next.cursor())
		} else {
			next.AckedThrough = max(next.AckedThrough, next.cursor())
		}
		next.TracesFailed, next.MetricsFailed = false, false
	}
	if progressed {
		if saveErr := w.saveCheckpoint(next); saveErr != nil {
			// A terminal response may already have been observed. Without a
			// durable signal cursor the safest available behavior is still
			// at-least-once retry after reporting the checkpoint failure.
			exportErr = errors.Join(exportErr, saveErr)
		} else {
			w.checkpoint = next
			if next.cursor() > cursor {
				w.setPending(max(0, pending-int64(len(rows))))
			}
		}
	}
	w.logSignalWarning("traces", result.Traces.Warning)
	w.logSignalWarning("metrics", result.Metrics.Warning)
	if exportErr != nil {
		w.failed(exportErr)
		return exportErr
	}
	w.healthy(true)
	return nil
}

func validateExportResult(signals SignalMask, result ExportResult) error {
	checks := []struct {
		mask   SignalMask
		name   string
		result SignalExportResult
	}{
		{SignalTraces, "traces", result.Traces},
		{SignalMetrics, "metrics", result.Metrics},
	}
	for _, check := range checks {
		if signals&check.mask == 0 {
			if check.result.Attempted {
				return fmt.Errorf("telemetry sink attempted unrequested %s", check.name)
			}
			continue
		}
		if !check.result.Attempted {
			return fmt.Errorf("telemetry sink omitted requested %s", check.name)
		}
		if check.result.RejectedItems < 0 || check.result.RejectedItems > 0 && (!check.result.Terminal || !check.result.Failed) {
			return fmt.Errorf("telemetry sink returned invalid %s rejection accounting", check.name)
		}
	}
	return nil
}

func saturatedCounterAdd(current, delta int64) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	if delta <= 0 {
		return current
	}
	if current > maxInt64-delta {
		return maxInt64
	}
	return current + delta
}

func (w *Worker) logSignalWarning(signal, message string) {
	if message == "" {
		return
	}
	w.logf("burnban: OTLP collector warning for %s: %s", signal, safeLabel(message))
}

func (w *Worker) loadCheckpoint() error {
	if w.loaded {
		return nil
	}
	raw, err := w.store.GetSetting(w.checkpointKey)
	if err != nil {
		return fmt.Errorf("load telemetry checkpoint: %w", err)
	}
	legacyCheckpoint := false
	if raw != "" {
		if len(raw) > 4096 || json.Unmarshal([]byte(raw), &w.checkpoint) != nil ||
			(w.checkpoint.SchemaVersion != checkpointSchemaVersion && w.checkpoint.SchemaVersion != SchemaVersion) ||
			w.checkpoint.AckedThrough < 0 ||
			w.checkpoint.DroppedThrough < 0 || w.checkpoint.DroppedRows < 0 ||
			w.checkpoint.TracesThrough < 0 || w.checkpoint.MetricsThrough < 0 ||
			w.checkpoint.RejectedSpans < 0 || w.checkpoint.RejectedPoints < 0 {
			return fmt.Errorf("telemetry checkpoint is corrupt; remove setting %s after investigation", w.checkpointKey)
		}
		legacyCheckpoint = w.checkpoint.SchemaVersion == SchemaVersion
	}
	if legacyCheckpoint {
		// v1 checkpoints written before signal-specific progress tracked a
		// single terminal cursor. Preserve it for both signals so an upgrade
		// never re-exports or resurrects rows already classified by that worker.
		legacyCursor := max(w.checkpoint.AckedThrough, w.checkpoint.DroppedThrough)
		w.checkpoint.TracesThrough = legacyCursor
		w.checkpoint.MetricsThrough = legacyCursor
	}
	w.checkpoint.SchemaVersion = checkpointSchemaVersion
	w.loaded = true
	w.statsMu.Lock()
	w.stats.AckedThrough = w.checkpoint.AckedThrough
	w.stats.DroppedThrough = w.checkpoint.DroppedThrough
	w.stats.TracesThrough = w.checkpoint.TracesThrough
	w.stats.MetricsThrough = w.checkpoint.MetricsThrough
	w.stats.DroppedRows = w.checkpoint.DroppedRows
	w.stats.RejectedSpans = w.checkpoint.RejectedSpans
	w.stats.RejectedPoints = w.checkpoint.RejectedPoints
	w.statsMu.Unlock()
	return nil
}

func (w *Worker) saveCheckpoint(next checkpoint) error {
	next.SchemaVersion = checkpointSchemaVersion
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := w.store.SetSetting(w.checkpointKey, string(encoded)); err != nil {
		return fmt.Errorf("persist telemetry checkpoint: %w", err)
	}
	next.UpdatedAt = ""
	w.statsMu.Lock()
	w.stats.AckedThrough = next.AckedThrough
	w.stats.DroppedThrough = next.DroppedThrough
	w.stats.TracesThrough = next.TracesThrough
	w.stats.MetricsThrough = next.MetricsThrough
	w.stats.DroppedRows = next.DroppedRows
	w.stats.RejectedSpans = next.RejectedSpans
	w.stats.RejectedPoints = next.RejectedPoints
	w.statsMu.Unlock()
	return nil
}

func (w *Worker) Stats() Stats {
	w.statsMu.RLock()
	defer w.statsMu.RUnlock()
	return w.stats
}

func (w *Worker) setPending(pending int64) {
	w.statsMu.Lock()
	w.stats.PendingRows = max(pending, 0)
	w.statsMu.Unlock()
}

func (w *Worker) healthy(delivered bool) {
	w.statsMu.Lock()
	w.stats.State = "healthy"
	w.stats.LastError = ""
	if delivered {
		now := time.Now().UTC()
		w.stats.LastSuccess = &now
	}
	w.statsMu.Unlock()
}

func (w *Worker) failed(err error) {
	now := time.Now().UTC()
	message := safeLabel(err.Error())
	if len(message) > 256 {
		message = message[:256]
	}
	w.statsMu.Lock()
	w.stats.State = "degraded"
	w.stats.LastFailure = &now
	w.stats.LastError = message
	w.statsMu.Unlock()

	w.logMu.Lock()
	shouldLog := message != w.lastLogError || now.Sub(w.lastLog) >= time.Minute
	if shouldLog {
		w.lastLog, w.lastLogError = now, message
	}
	w.logMu.Unlock()
	if shouldLog {
		w.logf("burnban: optional telemetry export degraded (inference remains fail-open): %s", message)
	}
}

func (s Stats) String() string {
	parts := []string{s.State, fmt.Sprintf("pending=%d", s.PendingRows), fmt.Sprintf("dropped=%d", s.DroppedRows)}
	if s.RejectedSpans > 0 || s.RejectedPoints > 0 {
		parts = append(parts, fmt.Sprintf("rejected_spans=%d", s.RejectedSpans), fmt.Sprintf("rejected_points=%d", s.RejectedPoints))
	}
	return strings.Join(parts, " ")
}
