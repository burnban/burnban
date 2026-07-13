package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/burnban/burnban/internal/store"
	"github.com/burnban/burnban/internal/telemetry"
)

func cmdTelemetry(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: burnban telemetry export [--since 7d] [--out DIRECTORY]")
	}
	switch args[0] {
	case "export":
		return cmdTelemetryExport(args[1:])
	default:
		return fmt.Errorf("unknown telemetry subcommand %q (use export)", args[0])
	}
}

func cmdTelemetryExport(args []string) error {
	fs := flag.NewFlagSet("telemetry export", flag.ExitOnError)
	since := fs.String("since", "7d", `window: "today", "24h", "7d", or any Go duration`)
	out := fs.String("out", ".", "parent directory for the immutable partitioned dataset")
	batchRows := fs.Int("batch-rows", 1000, "maximum rows read and written per bounded batch (1-1000)")
	maxRows := fs.Int64("max-rows", 100_000, "refuse to publish a dataset larger than this many rows")
	maxBytes := fs.Int64("max-bytes", 256<<20, "refuse to publish a dataset larger than this many bytes")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	from, _, err := parseSince(*since)
	if err != nil {
		return err
	}
	ledger, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer ledger.Close()
	manifest, dataset, err := telemetry.ExportWarehouse(context.Background(), ledger, telemetry.WarehouseConfig{
		OutputDir: *out, Since: from, BatchRows: *batchRows,
		MaxRows: *maxRows, MaxBytes: *maxBytes,
	})
	if err != nil {
		return err
	}
	info, err := os.Stat(dataset)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("published warehouse dataset is unavailable: %w", err)
	}
	fmt.Printf("warehouse dataset %s\n  rows     %d\n  objects  %d\n  bytes    %d\n  schema   %s\n",
		terminalText(dataset, 500), manifest.Rows, len(manifest.Objects), manifest.Bytes, telemetry.SchemaVersion)
	return nil
}
