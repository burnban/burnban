package localusage_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/localusage"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/sourceadapter"
)

type contractFixtureAdapter struct{}

func (contractFixtureAdapter) Manifest() sourceadapter.Manifest {
	return sourceadapter.Manifest{
		APIVersion: sourceadapter.APIVersion, ID: "contract-fixture", DisplayName: "Contract Fixture",
		Store: "synthetic metadata", Privacy: sourceadapter.Privacy{ReadOnly: true},
	}
}

func (contractFixtureAdapter) DefaultPath(home string) string {
	return filepath.Join(home, ".contract-fixture")
}

func (contractFixtureAdapter) Scan(_ string, since time.Time, _ sourceadapter.ScanLimits, emit func(sourceadapter.Event)) (sourceadapter.ScanResult, error) {
	event := sourceadapter.Event{
		ID: "one", Model: "fixture-model", Time: since.Add(time.Minute),
		Calls: 1, In: 100, Out: 20, Confidence: sourceadapter.ConfidenceExact,
	}
	emit(event)
	emit(event) // The report boundary deduplicates stable non-empty IDs.
	return sourceadapter.ScanResult{Sessions: 1}, nil
}

func TestPublicAdapterContractIntegratesWithReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_CLI_HOME", "")
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"fixture-model": {InputPerMTok: 1, OutputPerMTok: 2},
	}}
	report, err := localusage.BuildReport(prices, localusage.ReportOptions{
		Since:              time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Until:              time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
		AdditionalAdapters: []sourceadapter.Adapter{contractFixtureAdapter{}},
		SourcePaths: map[string]string{
			"claude-code":        filepath.Join(home, "missing-claude"),
			"codex":              filepath.Join(home, "missing-codex"),
			"gemini-cli":         filepath.Join(home, "missing-gemini"),
			"github-copilot-cli": filepath.Join(home, "missing-copilot"),
			"cursor":             filepath.Join(home, "missing-cursor"),
			"opencode":           filepath.Join(home, "missing-opencode"),
			"hermes":             filepath.Join(home, "missing-hermes"),
			"openclaw":           filepath.Join(home, "missing-openclaw"),
			"goose":              filepath.Join(home, "missing-goose"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range report.Providers {
		if provider.Provider != "contract-fixture" {
			continue
		}
		if provider.Calls != 1 || provider.AdapterVersion != sourceadapter.APIVersion || !provider.Privacy.ReadOnly {
			t.Fatalf("contract provider = %+v", provider)
		}
		return
	}
	t.Fatal("additional public adapter missing from report")
}
