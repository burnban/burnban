package telemetry

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/burnban/burnban/internal/store"
)

type warehouseStore interface {
	TelemetryRowsSinceAfter(afterID int64, since time.Time, limit int) ([]store.TelemetryRow, error)
}

type WarehouseConfig struct {
	OutputDir string
	Since     time.Time
	BatchRows int
	MaxRows   int64
	MaxBytes  int64
	Now       func() time.Time
	Random    io.Reader
}

type Manifest struct {
	SchemaVersion string           `json:"schema_version"`
	DatasetID     string           `json:"dataset_id"`
	CreatedAt     string           `json:"created_at"`
	Since         string           `json:"since"`
	Format        string           `json:"format"`
	Partitioning  []string         `json:"partitioning"`
	Rows          int64            `json:"rows"`
	Bytes         int64            `json:"bytes"`
	Objects       []ManifestObject `json:"objects"`
}

type ManifestObject struct {
	Path          string `json:"path"`
	Rows          int64  `json:"rows"`
	Bytes         int64  `json:"bytes"`
	SHA256        string `json:"sha256"`
	MinObservedAt string `json:"min_observed_at"`
	MaxObservedAt string `json:"max_observed_at"`
}

// ExportWarehouse creates one immutable, partitioned NDJSON dataset and a
// checksum manifest. The staging directory is removed on any failure and
// renamed atomically only after every object and the manifest are durable.
func ExportWarehouse(ctx context.Context, ledger warehouseStore, config WarehouseConfig) (Manifest, string, error) {
	if ledger == nil {
		return Manifest{}, "", fmt.Errorf("warehouse store is required")
	}
	if config.OutputDir == "" {
		return Manifest{}, "", fmt.Errorf("warehouse output directory is required")
	}
	if config.BatchRows == 0 {
		config.BatchRows = 1000
	}
	if config.BatchRows < 1 || config.BatchRows > 1000 {
		return Manifest{}, "", fmt.Errorf("warehouse batch rows must be between 1 and 1000")
	}
	if config.MaxRows == 0 {
		config.MaxRows = 100_000
	}
	if config.MaxRows < 1 || config.MaxRows > 10_000_000 {
		return Manifest{}, "", fmt.Errorf("warehouse max rows must be between 1 and 10000000")
	}
	if config.MaxBytes == 0 {
		config.MaxBytes = 256 << 20
	}
	if config.MaxBytes < 1<<20 || config.MaxBytes > 10<<30 {
		return Manifest{}, "", fmt.Errorf("warehouse max bytes must be between 1 MiB and 10 GiB")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	base, err := filepath.Abs(config.OutputDir)
	if err != nil {
		return Manifest{}, "", err
	}
	if err := ensureSafeOutputBase(base); err != nil {
		return Manifest{}, "", err
	}
	random := make([]byte, 8)
	if _, err := io.ReadFull(config.Random, random); err != nil {
		return Manifest{}, "", fmt.Errorf("generate warehouse dataset id: %w", err)
	}
	created := config.Now().UTC()
	datasetID := "burnban-" + created.Format("20060102T150405Z") + "-" + hex.EncodeToString(random)
	staging := filepath.Join(base, "."+datasetID+".tmp")
	final := filepath.Join(base, datasetID)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return Manifest{}, "", fmt.Errorf("create warehouse staging directory: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(staging)
		}
	}()

	manifest := Manifest{
		SchemaVersion: SchemaVersion, DatasetID: datasetID,
		CreatedAt: created.Format(time.RFC3339Nano),
		Since:     config.Since.UTC().Format(time.RFC3339Nano), Format: "ndjson",
		Partitioning: []string{"date", "hour"},
	}
	var cursor int64
	var partSequence int64
	for {
		if err := ctx.Err(); err != nil {
			return Manifest{}, "", err
		}
		rows, err := ledger.TelemetryRowsSinceAfter(cursor, config.Since, config.BatchRows)
		if err != nil {
			return Manifest{}, "", fmt.Errorf("read warehouse rows: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		if manifest.Rows+int64(len(rows)) > config.MaxRows {
			return Manifest{}, "", fmt.Errorf("warehouse export exceeds --max-rows=%d; no dataset was published", config.MaxRows)
		}
		byPartition := map[string][]Event{}
		for _, row := range rows {
			event := FromRow(row)
			observed := parseEventTime(event.ObservedAt)
			partition := "date=" + observed.Format("2006-01-02") + "/hour=" + observed.Format("15")
			byPartition[partition] = append(byPartition[partition], event)
			cursor = row.ID
		}
		partitions := make([]string, 0, len(byPartition))
		for partition := range byPartition {
			partitions = append(partitions, partition)
		}
		sort.Strings(partitions)
		for _, partition := range partitions {
			partSequence++
			object, err := writeNDJSONObject(staging, partition, partSequence, byPartition[partition])
			if err != nil {
				return Manifest{}, "", err
			}
			manifest.Rows += object.Rows
			manifest.Bytes += object.Bytes
			if manifest.Bytes > config.MaxBytes {
				return Manifest{}, "", fmt.Errorf("warehouse export exceeds --max-bytes=%d; no dataset was published", config.MaxBytes)
			}
			manifest.Objects = append(manifest.Objects, object)
		}
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, "", err
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := writeExclusive(filepath.Join(staging, "manifest.json"), manifestBytes); err != nil {
		return Manifest{}, "", fmt.Errorf("write warehouse manifest: %w", err)
	}
	if err := syncDirectoryTree(staging); err != nil {
		return Manifest{}, "", err
	}
	if err := os.Rename(staging, final); err != nil {
		return Manifest{}, "", fmt.Errorf("publish warehouse dataset: %w", err)
	}
	if err := syncDir(base); err != nil {
		return Manifest{}, "", fmt.Errorf("sync warehouse output directory: %w", err)
	}
	complete = true
	return manifest, final, nil
}

func ensureSafeOutputBase(base string) error {
	info, err := os.Lstat(base)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(base, 0o700); err != nil {
			return fmt.Errorf("create warehouse output directory: %w", err)
		}
		info, err = os.Lstat(base)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("warehouse output must be a real directory, not a symlink or special file")
	}
	return nil
}

func writeNDJSONObject(root, partition string, sequence int64, events []Event) (ManifestObject, error) {
	dir := filepath.Join(root, filepath.FromSlash(partition))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ManifestObject{}, err
	}
	name := fmt.Sprintf("part-%06d.ndjson", sequence)
	fullPath := filepath.Join(dir, name)
	file, err := os.OpenFile(fullPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ManifestObject{}, err
	}
	hasher := sha256.New()
	counter := &countWriter{}
	buffer := bufio.NewWriterSize(io.MultiWriter(file, hasher, counter), 64<<10)
	object := ManifestObject{Path: filepath.ToSlash(filepath.Join(partition, name)), Rows: int64(len(events))}
	for i, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			file.Close()
			return ManifestObject{}, err
		}
		if _, err := buffer.Write(encoded); err != nil {
			file.Close()
			return ManifestObject{}, err
		}
		if err := buffer.WriteByte('\n'); err != nil {
			file.Close()
			return ManifestObject{}, err
		}
		if i == 0 || event.ObservedAt < object.MinObservedAt {
			object.MinObservedAt = event.ObservedAt
		}
		if event.ObservedAt > object.MaxObservedAt {
			object.MaxObservedAt = event.ObservedAt
		}
	}
	if err := buffer.Flush(); err != nil {
		file.Close()
		return ManifestObject{}, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return ManifestObject{}, err
	}
	if err := file.Close(); err != nil {
		return ManifestObject{}, err
	}
	object.Bytes = counter.n
	object.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	return object, nil
}

type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

func writeExclusive(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func syncDirectoryTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return syncDir(path)
		}
		return nil
	})
}

func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
