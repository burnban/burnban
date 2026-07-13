package subsidy

import (
	"fmt"
	"os"
)

// preflightSQLiteSource enforces the shared scanner envelope before an
// adapter opens a foreign SQLite database. SQLite may keep live state in WAL,
// shared-memory, or rollback-journal sidecars, so all stable regular files
// count toward both the file and byte bounds.
func preflightSQLiteSource(path string, limits ScanLimits) (ScanStats, bool, error) {
	limits = normalizeScanLimits(limits)
	stats := ScanStats{}
	files, bytes, err := sqliteSourceSize(path)
	if os.IsNotExist(err) {
		return stats, false, nil
	}
	if err != nil {
		return stats, false, err
	}
	if files > limits.MaxFiles {
		stats.FilesSkipped = files
		stats.Warn("file scan limit reached")
		return stats, false, nil
	}
	if bytes > limits.MaxBytes {
		stats.FilesSkipped = files
		stats.Warn("byte scan limit reached")
		return stats, false, nil
	}
	stats.FilesScanned = files
	stats.BytesScanned = bytes
	return stats, true, nil
}

func sqliteSourceSize(path string) (int, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, 0, fmt.Errorf("expected a stable regular database file")
	}
	bytes, err := addSQLiteSourceSize(0, info.Size())
	if err != nil {
		return 0, 0, err
	}
	files := 1
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		aux, err := os.Lstat(path + suffix)
		if err == nil && aux.Mode().IsRegular() {
			bytes, err = addSQLiteSourceSize(bytes, aux.Size())
			if err != nil {
				return 0, 0, err
			}
			files++
			continue
		}
		if err == nil {
			return 0, 0, fmt.Errorf("expected stable regular database sidecars")
		}
		if !os.IsNotExist(err) {
			return 0, 0, err
		}
	}
	return files, bytes, nil
}

func addSQLiteSourceSize(total, size int64) (int64, error) {
	if total < 0 || size < 0 || size > int64(^uint64(0)>>1)-total {
		return 0, fmt.Errorf("database and sidecar sizes overflow the scanner byte counter")
	}
	return total + size, nil
}
