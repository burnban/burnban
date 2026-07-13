package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

func newWebhookQueueTestProxy(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "webhook-queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p, err := New(s, &pricing.Table{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Logf = func(string, ...any) {}
	return p, s
}

// waitForQueueWebhookMutexWait makes the old stale-read interleaving
// deterministic. queueWebhook used to read the durable mark before taking
// alertMu, so by the time it blocked on this mutex it had already captured a
// stale value. The fixed implementation blocks before that read.
func waitForQueueWebhookMutexWait(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	buffer := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buffer, true)
		for _, stack := range bytes.Split(buffer[:n], []byte("\n\n")) {
			proxyFrame := bytes.Index(stack, []byte("(*Proxy).queueWebhook"))
			if proxyFrame < 0 {
				continue
			}
			lockFrame := bytes.LastIndex(stack[:proxyFrame], []byte("sync.(*Mutex).Lock"))
			if lockFrame >= 0 && !bytes.Contains(stack[lockFrame:proxyFrame], []byte("database/sql")) {
				return
			}
		}
		runtime.Gosched()
	}
	t.Fatal("queueWebhook did not block on alertMu")
}

func TestQueueWebhookDoesNotEnqueueFromDurableMarkReadBeforeClaim(t *testing.T) {
	p, s := newWebhookQueueTestProxy(t)
	var attempts atomic.Int64
	attempted := make(chan struct{}, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		select {
		case attempted <- struct{}{}:
		default:
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)

	const mark = "_test_webhook_stale_read"
	p.alertMu.Lock()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		p.queueWebhook(mark, hook.URL, "must not be delivered")
	}()
	waitForQueueWebhookMutexWait(t)

	// This models the first delivery committing its durable success while the
	// second caller waits to claim the mark. The waiting caller must read this
	// value only after acquiring alertMu.
	if err := s.SetSetting(mark, "1"); err != nil {
		p.alertMu.Unlock()
		t.Fatal(err)
	}
	p.alertMu.Unlock()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("queueWebhook did not return after alertMu was released")
	}

	select {
	case <-attempted:
		t.Fatalf("durably delivered webhook was enqueued again: attempts=%d", attempts.Load())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestQueueWebhookCanRetryAfterCompleteDeliveryFailure(t *testing.T) {
	p, s := newWebhookQueueTestProxy(t)
	var attempts atomic.Int64
	var succeed atomic.Bool
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		if !succeed.Load() {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)

	const mark = "_test_webhook_failure_retry"
	p.queueWebhook(mark, hook.URL, "first delivery fails")
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		p.alertMu.Lock()
		inFlight := p.alertsInFlight[mark]
		p.alertMu.Unlock()
		if attempts.Load() == 3 && !inFlight {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("failed delivery attempts=%d, want 3", got)
	}
	if delivered, err := s.GetSetting(mark); err != nil || delivered != "" {
		t.Fatalf("failed delivery durable mark=%q err=%v", delivered, err)
	}

	succeed.Store(true)
	p.queueWebhook(mark, hook.URL, "second delivery succeeds")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		delivered, err := s.GetSetting(mark)
		if err != nil {
			t.Fatal(err)
		}
		if delivered == "1" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := attempts.Load(); got != 4 {
		t.Fatalf("retry attempts=%d, want 4 total", got)
	}
	if delivered, err := s.GetSetting(mark); err != nil || strings.TrimSpace(delivered) != "1" {
		t.Fatalf("successful retry durable mark=%q err=%v", delivered, err)
	}
}
