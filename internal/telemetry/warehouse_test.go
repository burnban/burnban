package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestWarehouseExportIsAtomicPartitionedChecksummedAndContentFree(t *testing.T) {
	ledger := openWorkerStore(t)
	start := time.Date(2026, 7, 12, 1, 30, 0, 0, time.UTC)
	for i, ts := range []time.Time{start, start.Add(40 * time.Minute), start.Add(25 * time.Hour)} {
		if err := ledger.Insert(store.Request{
			Ts: ts, Provider: "openai", Model: "gpt-test", Agent: "ci",
			Session: "private-session", BodyHash: "private-fingerprint",
			Principal: "svc", Project: "oss", IdentityConfidence: "authenticated",
			InTokens: int64(i + 1), OutTokens: 2, CostUSD: .01,
			UsageState: store.UsageExact, PricingState: store.PricingPriced,
			CostSource: store.CostPublicList, CostConfidence: store.ConfidenceListEstimate,
		}); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "warehouse")
	manifest, dataset, err := ExportWarehouse(context.Background(), ledger, WarehouseConfig{
		OutputDir: out, Since: start.Add(-time.Second), BatchRows: 2,
		Now:    func() time.Time { return time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC) },
		Random: bytes.NewReader(make([]byte, 8)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Rows != 3 || len(manifest.Objects) != 3 || !strings.HasSuffix(dataset, "burnban-20260713T000000Z-0000000000000000") {
		t.Fatalf("manifest=%+v dataset=%q", manifest, dataset)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(dataset, "manifest.json"))
	if err != nil || !json.Valid(manifestBytes) {
		t.Fatalf("manifest file err=%v body=%s", err, manifestBytes)
	}
	for _, object := range manifest.Objects {
		if !strings.HasPrefix(object.Path, "date=") || !strings.Contains(object.Path, "/hour=") || strings.Contains(object.Path, "..") {
			t.Errorf("unsafe/nonpartitioned object path %q", object.Path)
		}
		full := filepath.Join(dataset, filepath.FromSlash(object.Path))
		contents, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(contents)
		if object.SHA256 != hex.EncodeToString(digest[:]) || object.Bytes != int64(len(contents)) {
			t.Errorf("object checksum/size mismatch: %+v", object)
		}
		if strings.Contains(string(contents), "private-session") || strings.Contains(string(contents), "private-fingerprint") {
			t.Fatalf("warehouse object leaked private ledger fields: %s", contents)
		}
		scanner := bufio.NewScanner(bytes.NewReader(contents))
		for scanner.Scan() {
			var event Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.SchemaVersion != SchemaVersion {
				t.Fatalf("warehouse row=%s err=%v", scanner.Bytes(), err)
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(full)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Errorf("object permissions mode=%v err=%v", info.Mode(), err)
		}
	}
}

func TestWarehouseLimitFailurePublishesNothing(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 2)
	out := filepath.Join(t.TempDir(), "warehouse")
	_, _, err := ExportWarehouse(context.Background(), ledger, WarehouseConfig{
		OutputDir: out, MaxRows: 1, MaxBytes: 1 << 20,
		Now: func() time.Time { return time.Unix(1, 0).UTC() }, Random: bytes.NewReader(make([]byte, 8)),
	})
	if err == nil || !strings.Contains(err.Error(), "max-rows") {
		t.Fatalf("row limit error = %v", err)
	}
	entries, readErr := os.ReadDir(out)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed export published files entries=%v err=%v", entries, readErr)
	}
}

func TestWarehouseRejectsSymlinkOutput(t *testing.T) {
	ledger := openWorkerStore(t)
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "linked-output")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, _, err := ExportWarehouse(context.Background(), ledger, WarehouseConfig{
		OutputDir: link, MaxRows: 1, MaxBytes: 1 << 20,
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink output error = %v", err)
	}
}

func TestWarehouseCancellationRemovesStagingDirectory(t *testing.T) {
	ledger := openWorkerStore(t)
	insertWorkerRows(t, ledger, 1)
	out := filepath.Join(t.TempDir(), "warehouse")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := ExportWarehouse(ctx, ledger, WarehouseConfig{
		OutputDir: out, MaxRows: 10, MaxBytes: 1 << 20,
		Now: func() time.Time { return time.Unix(1, 0).UTC() }, Random: bytes.NewReader(make([]byte, 8)),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	entries, readErr := os.ReadDir(out)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("canceled export left staging entries=%v err=%v", entries, readErr)
	}
}
