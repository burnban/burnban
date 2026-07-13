package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestTelemetryExportCommandPublishesDataset(t *testing.T) {
	db := filepath.Join(t.TempDir(), "telemetry-command.db")
	ledger, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Insert(store.Request{
		Ts: time.Now().UTC(), Provider: "openai", Model: "test",
		UsageState: store.UsageExact, PricingState: store.PricingPriced,
	}); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "objects")
	if err := cmdTelemetry([]string{"export", "--since", "24h", "--db", db, "--out", out, "--max-rows", "10", "--max-bytes", "1048576"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("warehouse datasets=%v err=%v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(out, entries[0].Name(), "manifest.json")); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryCommandRequiresKnownSubcommand(t *testing.T) {
	if err := cmdTelemetry(nil); err == nil {
		t.Fatal("missing telemetry subcommand accepted")
	}
	if err := cmdTelemetry([]string{"upload"}); err == nil {
		t.Fatal("unknown telemetry subcommand accepted")
	}
}
