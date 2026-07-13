package telemetry

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

type fakeSink struct {
	id      string
	mu      sync.Mutex
	err     error
	results []fakeExport
	calls   []Batch
}

type fakeExport struct {
	result ExportResult
	err    error
}

func (s *fakeSink) SinkID() string { return s.id }
func (s *fakeSink) Export(_ context.Context, batch Batch) (ExportResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, batch)
	if len(s.results) > 0 {
		out := s.results[0]
		s.results = s.results[1:]
		return out.result, out.err
	}
	result := successfulExportResult(batch.Signals)
	if s.err == nil {
		return result, nil
	}
	var partial *PartialRejectError
	var permanent *permanentExportError
	switch {
	case errors.As(s.err, &partial):
		signal := &result.Traces
		if partial.Signal == "metrics" {
			signal = &result.Metrics
		}
		signal.Terminal, signal.Failed, signal.RejectedItems = true, true, partial.RejectedItems
	case errors.As(s.err, &permanent):
		if result.Traces.Attempted {
			result.Traces.Terminal, result.Traces.Failed = true, true
		}
		if result.Metrics.Attempted {
			result.Metrics.Terminal, result.Metrics.Failed = true, true
		}
	default:
		if result.Traces.Attempted {
			result.Traces.Terminal, result.Traces.Failed = false, true
		}
		if result.Metrics.Attempted {
			result.Metrics.Terminal, result.Metrics.Failed = false, true
		}
	}
	return result, s.err
}

func successfulExportResult(signals SignalMask) ExportResult {
	if signals == 0 {
		signals = signalAll
	}
	return ExportResult{
		Traces:  SignalExportResult{Attempted: signals&SignalTraces != 0, Terminal: signals&SignalTraces != 0},
		Metrics: SignalExportResult{Attempted: signals&SignalMetrics != 0, Terminal: signals&SignalMetrics != 0},
	}
}
func (s *fakeSink) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *fakeSink) batches() []Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Batch(nil), s.calls...)
}

func openWorkerStore(t *testing.T) *store.Store {
	t.Helper()
	ledger, err := store.Open(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return ledger
}

func insertWorkerRows(t *testing.T, ledger *store.Store, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if err := ledger.Insert(store.Request{
			Ts: time.Unix(int64(i+1), 0).UTC(), Provider: "openai", Model: "test",
			UsageState: store.UsageExact, PricingState: store.PricingPriced,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkerPersistsSinkBoundAtLeastOnceCursor(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 3)
	sink := &fakeSink{id: "0123456789abcdef0123456789abcdef"}
	worker, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 2, MaxBacklog: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := worker.Stats(); stats.AckedThrough != 2 || stats.PendingRows != 1 || stats.DroppedRows != 0 {
		t.Fatalf("first drain stats = %+v", stats)
	}

	// A fresh worker for the same sink resumes after the durable ACK and does
	// not resend the first two rows.
	restarted, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 2, MaxBacklog: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := restarted.Stats(); stats.AckedThrough != 3 || stats.PendingRows != 0 {
		t.Fatalf("restart drain stats = %+v", stats)
	}
	if sink.callCount() != 2 {
		t.Fatalf("sink calls = %d", sink.callCount())
	}
}

func TestWorkerMigratesLegacySingleCursorWithoutReplay(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 3)
	sink := &fakeSink{id: "12121212121212121212121212121212"}
	key := "internal.telemetry.v1." + sink.id + ".checkpoint"
	if err := ledger.SetSetting(key, `{"schema_version":"burnban.telemetry.v1","acked_through":2,"dropped_through":0,"dropped_rows":0,"updated_at":"2026-07-13T00:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 2, MaxBacklog: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	batches := sink.batches()
	if len(batches) != 1 || len(batches[0].Events) != 1 || batches[0].Events[0].RequestID != 3 {
		t.Fatalf("legacy cursor replayed rows: %+v", batches)
	}
	if stats := worker.Stats(); stats.TracesThrough != 3 || stats.MetricsThrough != 3 || stats.AckedThrough != 3 {
		t.Fatalf("migrated cursor stats = %+v", stats)
	}
	raw, err := ledger.GetSetting(key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"schema_version":"burnban.telemetry.checkpoint.v2"`) ||
		!strings.Contains(raw, `"traces_through":3`) || !strings.Contains(raw, `"metrics_through":3`) {
		t.Fatalf("legacy checkpoint was not rewritten safely: %s", raw)
	}
}

type checkpointFailStore struct {
	*store.Store
	fail atomic.Bool
}

func (s *checkpointFailStore) SetSetting(key, value string) error {
	if s.fail.Load() {
		return errors.New("synthetic checkpoint disk failure")
	}
	return s.Store.SetSetting(key, value)
}

func TestWorkerNeverAcknowledgesBeforeDurableCheckpoint(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 1)
	wrapped := &checkpointFailStore{Store: ledger}
	wrapped.fail.Store(true)
	sink := &fakeSink{id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	worker, err := NewWorker(wrapped, sink, WorkerConfig{BatchSize: 1, MaxBacklog: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); err == nil {
		t.Fatal("checkpoint failure was ignored")
	}
	if worker.Stats().AckedThrough != 0 {
		t.Fatalf("failed checkpoint was reported as delivered: %+v", worker.Stats())
	}
	wrapped.fail.Store(false)
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.callCount() != 2 || worker.Stats().AckedThrough != 1 {
		t.Fatalf("at-least-once retry calls=%d stats=%+v", sink.callCount(), worker.Stats())
	}
}

func TestWorkerBoundsBacklogWithSeparateDroppedCursor(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 5)
	sink := &fakeSink{id: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	worker, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 2, MaxBacklog: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := worker.Stats()
	if stats.DroppedRows != 3 || stats.DroppedThrough != 3 || stats.AckedThrough != 5 || stats.PendingRows != 0 {
		t.Fatalf("overflow stats = %+v", stats)
	}
	if sink.callCount() != 1 || len(sink.calls[0].Events) != 2 || sink.calls[0].Events[0].RequestID != 4 {
		t.Fatalf("overflow batch = %+v", sink.calls)
	}
}

func TestWorkerDoesNotRetryCollectorPartialSuccess(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 2)
	sink := &fakeSink{id: "cccccccccccccccccccccccccccccccc", err: &PartialRejectError{Signal: "traces", RejectedItems: 1}}
	worker, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 2, MaxBacklog: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); err == nil {
		t.Fatal("partial success was hidden")
	}
	stats := worker.Stats()
	if stats.AckedThrough != 0 || stats.DroppedThrough != 2 || stats.DroppedRows != 0 || stats.RejectedSpans != 1 || sink.callCount() != 1 {
		t.Fatalf("partial-success accounting stats=%+v calls=%d", stats, sink.callCount())
	}
	sink.mu.Lock()
	sink.err = nil
	sink.mu.Unlock()
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.callCount() != 1 {
		t.Fatalf("partially rejected batch was retried, calls=%d", sink.callCount())
	}
}

func TestWorkerPersistsIndependentSignalProgress(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 2)
	retryable := errors.New("synthetic retryable traces failure")
	sink := &fakeSink{
		id: "abababababababababababababababab",
		results: []fakeExport{
			{
				result: ExportResult{
					Traces:  SignalExportResult{Attempted: true, Failed: true},
					Metrics: SignalExportResult{Attempted: true, Terminal: true},
				},
				err: retryable,
			},
			{result: ExportResult{Traces: SignalExportResult{Attempted: true, Terminal: true}}},
		},
	}
	worker, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 2, MaxBacklog: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); !errors.Is(err, retryable) {
		t.Fatalf("first drain error = %v", err)
	}
	if stats := worker.Stats(); stats.TracesThrough != 0 || stats.MetricsThrough != 2 || stats.PendingRows != 2 {
		t.Fatalf("first signal checkpoint = %+v", stats)
	}
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	batches := sink.batches()
	if len(batches) != 2 || batches[0].Signals != signalAll || batches[1].Signals != SignalTraces {
		t.Fatalf("signal retries = %+v", batches)
	}
	if stats := worker.Stats(); stats.AckedThrough != 2 || stats.TracesThrough != 2 || stats.MetricsThrough != 2 || stats.PendingRows != 0 {
		t.Fatalf("completed signal checkpoint = %+v", stats)
	}
}

func TestWorkerNeverRetriesTerminalPartialWhileLaggingSignalRecovers(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 1)
	retryable := errors.New("synthetic retryable metrics failure")
	sink := &fakeSink{
		id: "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
		results: []fakeExport{
			{
				result: ExportResult{
					Traces:  SignalExportResult{Attempted: true, Terminal: true, Failed: true, RejectedItems: 1},
					Metrics: SignalExportResult{Attempted: true, Failed: true},
				},
				err: errors.Join(&PartialRejectError{Signal: "traces", RejectedItems: 1}, retryable),
			},
			{result: ExportResult{Metrics: SignalExportResult{Attempted: true, Terminal: true}}},
		},
	}
	worker, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 1, MaxBacklog: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); err == nil {
		t.Fatal("partial/retryable result was hidden")
	}
	if stats := worker.Stats(); stats.TracesThrough != 1 || stats.MetricsThrough != 0 || stats.RejectedSpans != 1 || stats.DroppedRows != 0 {
		t.Fatalf("partial checkpoint = %+v", stats)
	}
	restarted, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 1, MaxBacklog: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	batches := sink.batches()
	if len(batches) != 2 || batches[1].Signals != SignalMetrics {
		t.Fatalf("terminal trace request was retried: %+v", batches)
	}
	if stats := restarted.Stats(); stats.DroppedThrough != 1 || stats.DroppedRows != 0 || stats.RejectedSpans != 1 {
		t.Fatalf("terminal partial accounting = %+v", stats)
	}
}

func TestWorkerTracksMetricRejectionsWithoutInflatingBacklogDrops(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 2)
	sink := &fakeSink{
		id:  "efefefefefefefefefefefefefefefef",
		err: &PartialRejectError{Signal: "metrics", RejectedItems: 2},
	}
	worker, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 2, MaxBacklog: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); err == nil {
		t.Fatal("metric partial success was hidden")
	}
	stats := worker.Stats()
	if stats.DroppedThrough != 2 || stats.DroppedRows != 0 || stats.RejectedSpans != 0 || stats.RejectedPoints != 2 {
		t.Fatalf("metric partial accounting = %+v", stats)
	}
	if batches := sink.batches(); len(batches) != 1 || batches[0].DroppedRows != 0 {
		t.Fatalf("collector rejection polluted backlog-drop metric: %+v", batches)
	}
}

func TestWorkerRecordsPermanentCollectorFailureAsDrop(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 1)
	sink := &fakeSink{id: "ffffffffffffffffffffffffffffffff", err: &permanentExportError{message: "synthetic bad payload"}}
	worker, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 1, MaxBacklog: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); err == nil {
		t.Fatal("permanent failure was hidden")
	}
	if stats := worker.Stats(); stats.AckedThrough != 0 || stats.DroppedThrough != 1 || stats.DroppedRows != 0 {
		t.Fatalf("permanent failure stats = %+v", stats)
	}
	if err := worker.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.callCount() != 1 {
		t.Fatalf("permanently rejected batch was retried, calls=%d", sink.callCount())
	}
}

func TestWorkerStatsAndLifecycleAreRaceSafe(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 20)
	sink := &fakeSink{id: "dddddddddddddddddddddddddddddddd"}
	worker, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 2, MaxBacklog: 20, PollInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker.Start(ctx)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = worker.Stats().String()
			}
		}()
	}
	wg.Wait()
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptCheckpointFailsObservableWithoutSending(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 1)
	sink := &fakeSink{id: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}
	key := "internal.telemetry.v1." + sink.id + ".checkpoint"
	if err := ledger.SetSetting(key, `{"schema_version":"wrong","acked_through":999}`); err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorker(ledger, sink, WorkerConfig{BatchSize: 1, MaxBacklog: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DrainOnce(context.Background()); err == nil {
		t.Fatal("corrupt checkpoint was accepted")
	}
	if sink.callCount() != 0 || worker.Stats().State != "degraded" {
		t.Fatalf("corrupt checkpoint calls=%d stats=%+v", sink.callCount(), worker.Stats())
	}
}
