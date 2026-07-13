package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/downshift"
	"github.com/burnban/burnban/internal/store"
)

func historicalDownshiftConfig(t *testing.T) *downshift.Compiled {
	t.Helper()
	config := downshift.Config{
		APIVersion: downshift.APIVersion, Revision: 1, Mode: downshift.ModeWarnThenDownshift,
		WarnAtPct: 70, DownshiftAtPct: 80, DownshiftOnDenial: true,
		Rules: []downshift.Rule{{ID: "openai-safe",
			Source:       downshift.Endpoint{Route: "openai", Model: "gpt-5", Family: "coding", Dialect: "openai", ContextTokens: 200000},
			Target:       downshift.Endpoint{Route: "openai", Model: "gpt-5-mini", Family: "coding", Dialect: "openai", ContextTokens: 128000},
			Capabilities: downshift.Capabilities{Tools: true, StructuredOutput: true, Modalities: []string{"text"}},
		}},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := downshift.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func TestHistoricalDownshiftSimulationIsPersistedAndGatesActivation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "simulation.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	compiled := historicalDownshiftConfig(t)
	now := time.Now().UTC()
	features := downshift.AnalyzeAt([]byte(`{"model":"gpt-5","max_tokens":100,"messages":[]}`), "openai", "/v1/chat/completions")
	if err := s.Insert(store.Request{
		Ts: now.Add(-time.Hour), Provider: "openai", Model: "gpt-5", Route: "/v1/chat/completions",
		InTokens: 1000, OutTokens: 100, CostUSD: .02, PricingState: store.PricingPriced,
		DownshiftFeatures: downshift.FeatureJSON(features),
	}); err != nil {
		t.Fatal(err)
	}
	report, simulationID, err := runDownshiftSimulation(s, compiled, now.Add(-24*time.Hour), now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if simulationID == 0 || report.EligibleRequests != 1 || report.TargetCostUSD <= 0 || report.TargetCostUSD >= report.SourceCostUSD || report.SavingsUSD <= 0 {
		t.Fatalf("simulation id=%d report=%+v", simulationID, report)
	}
	if err := s.ApplyDownshiftDocument(store.DownshiftDocumentRecord{
		APIVersion: compiled.Config.APIVersion, Revision: compiled.Config.Revision, Digest: compiled.Digest,
		Mode: string(compiled.Config.Mode), DocumentJSON: string(compiled.Canonical), SimulationID: simulationID,
	}); err != nil {
		t.Fatalf("material exact simulation did not unlock activation: %v", err)
	}
}

func TestHistoricalDownshiftSimulationTreatsMissingFeaturesAsIndeterminate(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "simulation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	compiled := historicalDownshiftConfig(t)
	now := time.Now().UTC()
	if err := s.Insert(store.Request{Ts: now.Add(-time.Hour), Provider: "openai", Model: "gpt-5",
		CostUSD: .02, PricingState: store.PricingPriced}); err != nil {
		t.Fatal(err)
	}
	report, simulationID, err := runDownshiftSimulation(s, compiled, now.Add(-24*time.Hour), now, 100)
	if err != nil || report.IndeterminateRequests != 1 || report.EligibleRequests != 0 {
		t.Fatalf("id=%d report=%+v err=%v", simulationID, report, err)
	}
	if err := s.ApplyDownshiftDocument(store.DownshiftDocumentRecord{
		APIVersion: compiled.Config.APIVersion, Revision: compiled.Config.Revision, Digest: compiled.Digest,
		Mode: string(compiled.Config.Mode), DocumentJSON: string(compiled.Canonical), SimulationID: simulationID,
	}); err == nil {
		t.Fatal("indeterminate-only dry run unlocked enforcing activation")
	}
}

func TestReadDownshiftFileRejectsSymlinkAndOversize(t *testing.T) {
	dir := t.TempDir()
	compiled := historicalDownshiftConfig(t)
	regular := filepath.Join(dir, "config.json")
	if err := os.WriteFile(regular, compiled.Canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "config-link.json")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := readDownshiftFile(symlink); err == nil {
		t.Fatal("accepted symlinked routing config")
	}
	oversize := filepath.Join(dir, "oversize.json")
	if err := os.WriteFile(oversize, make([]byte, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDownshiftFile(oversize); err == nil {
		t.Fatal("accepted oversized routing config")
	}
}
