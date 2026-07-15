package localusage

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const nonRegularLogWarning = "one or more non-regular log files were skipped"

func TestFileScannerSkipsSymlinkedJSONL(t *testing.T) {
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "private.jsonl")
	if err := os.WriteFile(target, []byte(`{"type":"assistant","timestamp":"2026-07-10T12:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "linked.jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result, err := scanClaude(root, time.Time{}, DefaultScanLimits(), func(Event) {
		t.Fatal("event emitted from symlinked log")
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stats.FilesScanned != 0 || result.Stats.FilesSkipped != 1 || !result.Stats.Partial ||
		!slices.Contains(result.Stats.Warnings, nonRegularLogWarning) {
		t.Fatalf("symlink scan result = %+v", result)
	}
}
