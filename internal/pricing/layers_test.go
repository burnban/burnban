package pricing

import (
	"math"
	"os"
	"testing"
	"time"
)

func TestResolveLayerPrecedenceAndDates(t *testing.T) {
	table := &Table{
		Models: map[string]Price{
			"model-1": {
				InputPerMTok: 1, OutputPerMTok: 2,
				Source: "https://provider.test/pricing", EffectiveFrom: "2026-01-01", ValidThrough: "2026-12-31", VerifiedDate: "2026-01-01",
			},
		},
		Contracts: []ContractPrice{
			{ID: "global", Provider: "openai", Model: "model-1", EffectiveFrom: "2026-03-01", Price: Price{InputPerMTok: .8, OutputPerMTok: 1.5}},
			{ID: "us-priority", Provider: "openai", Model: "model-1", Region: "us", ServiceTier: "priority", EffectiveFrom: "2026-06-01", ValidThrough: "2026-08-31", Price: Price{InputPerMTok: .5, OutputPerMTok: 1}},
		},
	}
	at := time.Date(2026, 7, 12, 23, 59, 0, 0, time.FixedZone("west", -7*3600))
	resolved, ok := table.Resolve("OPENAI", "model-1-202607", "US", "PRIORITY", at, 0, false)
	if !ok || resolved.Source != SourceContract || resolved.SourceRef != "us-priority" || resolved.Price.InputPerMTok != .5 {
		t.Fatalf("specific contract resolution = %+v ok=%v", resolved, ok)
	}
	resolved, ok = table.Resolve("openai", "model-1", "eu", "priority", at, 0, false)
	if !ok || resolved.SourceRef != "global" {
		t.Fatalf("global contract resolution = %+v ok=%v", resolved, ok)
	}
	resolved, ok = table.Resolve("openai", "model-1", "us", "priority", at, 0.03125, true)
	if !ok || resolved.Source != SourceProviderFinal || !resolved.HasFinalCost || resolved.FinalCostUSD != .03125 {
		t.Fatalf("provider-final resolution = %+v ok=%v", resolved, ok)
	}
	resolved, ok = table.Resolve("anthropic", "model-1", "", "", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 0, false)
	if ok || resolved.Source != SourceUnknown {
		t.Fatalf("expired public list must be unknown: %+v ok=%v", resolved, ok)
	}
}

func TestResolveMalformedProviderFinalDoesNotFallBack(t *testing.T) {
	table := testTable()
	for _, amount := range []float64{math.NaN(), math.Inf(1), -1, 1_000_000_001} {
		resolved, ok := table.Resolve("anthropic", "claude-opus-4-7", "", "", time.Now(), amount, true)
		if ok || resolved.Source != SourceUnknown {
			t.Fatalf("malformed final %v fell back: %+v ok=%v", amount, resolved, ok)
		}
	}
}

func TestValidateContractsRejectsAmbiguityAndUnsafeInputs(t *testing.T) {
	price := Price{InputPerMTok: 1, OutputPerMTok: 2}
	base := ContractPrice{ID: "one", Provider: "openai", Model: "m", EffectiveFrom: "2026-01-01", ValidThrough: "2026-12-31", Price: price}
	for name, contracts := range map[string][]ContractPrice{
		"duplicate id": {base, {ID: "one", Provider: "anthropic", Model: "m", EffectiveFrom: "2026-01-01", Price: price}},
		"overlap":      {base, {ID: "two", Provider: "OPENAI", Model: "m", EffectiveFrom: "2026-06-01", Price: price}},
		"version prefix overlap": {
			{ID: "family", Provider: "openai", Model: "gpt-5", EffectiveFrom: "2026-01-01", Price: price},
			{ID: "minor", Provider: "OPENAI", Model: "gpt-5.4", EffectiveFrom: "2026-06-01", Price: price},
		},
		"control":           {{ID: "bad", Provider: "openai\n", Model: "m", EffectiveFrom: "2026-01-01", Price: price}},
		"bidi selector":     {{ID: "bad\u202e", Provider: "openai", Model: "m", EffectiveFrom: "2026-01-01", Price: price}},
		"bad range":         {{ID: "bad", Provider: "openai", Model: "m", EffectiveFrom: "2026-02-01", ValidThrough: "2026-01-01", Price: price}},
		"nested provenance": {{ID: "bad", Provider: "openai", Model: "m", EffectiveFrom: "2026-01-01", Price: Price{InputPerMTok: 1, OutputPerMTok: 2, Source: "https://example.test", EffectiveFrom: "2026-01-01", VerifiedDate: "2026-01-01"}}},
	} {
		if err := validateContracts(contracts); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	valid := []ContractPrice{base, {ID: "specific", Provider: "openai", Model: "m", Region: "us", EffectiveFrom: "2026-01-01", Price: price}}
	if err := validateContracts(valid); err != nil {
		t.Fatalf("different specificity rejected: %v", err)
	}
}

func TestLoadContractOnlyOverlay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := home + "/.burnban"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"contracts":[{"id":"msa-7","provider":"openai","model":"gpt-5.4","effective_from":"2026-07-01","price":{"input_per_mtok":1,"output_per_mtok":2}}]}`
	if err := os.WriteFile(dir+"/pricing.json", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Contracts) != 1 || table.Contracts[0].ID != "msa-7" {
		t.Fatalf("contracts = %+v", table.Contracts)
	}
}
