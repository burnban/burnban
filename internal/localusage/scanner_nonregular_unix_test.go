//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package localusage

import (
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFileScannerSkipsNamedPipeJSONL(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.jsonl"), 0o600); err != nil {
		t.Skipf("named pipes unavailable: %v", err)
	}

	result, err := scanClaude(root, time.Time{}, DefaultScanLimits(), func(Event) {
		t.Fatal("event emitted from named pipe")
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stats.FilesScanned != 0 || result.Stats.FilesSkipped != 1 || !result.Stats.Partial ||
		!slices.Contains(result.Stats.Warnings, nonRegularLogWarning) {
		t.Fatalf("named-pipe scan result = %+v", result)
	}
}

func TestCursorScannerRejectsNamedPipeWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("named pipes unavailable: %v", err)
	}
	type scanOutcome struct {
		result  ScanResult
		err     error
		emitted bool
	}
	done := make(chan scanOutcome, 1)
	go func() {
		limits := DefaultScanLimits()
		limits.MaxDuration = 50 * time.Millisecond
		emitted := false
		result, err := scanCursor(path, time.Time{}, limits, func(Event) { emitted = true })
		done <- scanOutcome{result: result, err: err, emitted: emitted}
	}()
	select {
	case outcome := <-done:
		if outcome.err == nil || !strings.Contains(outcome.err.Error(), "regular") || outcome.emitted {
			t.Fatalf("named-pipe Cursor scan result=%+v emitted=%t err=%v", outcome.result, outcome.emitted, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Cursor scanner blocked while opening a named pipe")
	}
}
