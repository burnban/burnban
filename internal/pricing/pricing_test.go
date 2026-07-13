package pricing

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testTable() *Table {
	return &Table{Models: map[string]Price{
		"claude-opus-4-7": {InputPerMTok: 5, OutputPerMTok: 25, CacheReadMult: 0.1, CacheWriteMult: 1.25},
		"gpt-5.6-luna":    {InputPerMTok: 1, OutputPerMTok: 6, CacheReadMult: 0.1},
	}}
}

func TestLookup(t *testing.T) {
	tb := testTable()
	if _, ok := tb.Lookup("claude-opus-4-7"); !ok {
		t.Fatal("exact match failed")
	}
	p, ok := tb.Lookup("claude-opus-4-7-20260301")
	if !ok || p.InputPerMTok != 5 {
		t.Fatalf("prefix match failed: %+v ok=%v", p, ok)
	}
	if _, ok := tb.Lookup("mystery-model"); ok {
		t.Fatal("unknown model must not match — burnban never guesses prices")
	}
}

func TestLookupRefusesVariantSuffixes(t *testing.T) {
	tb := &Table{Models: map[string]Price{
		"gemini-2.5-flash": {InputPerMTok: 0.3, OutputPerMTok: 2.5},
	}}
	// A distinct cheaper tier must NOT silently bill at the base tier's
	// rates just because the ID shares a prefix.
	if _, ok := tb.Lookup("gemini-2.5-flash-lite"); ok {
		t.Fatal("-lite variant matched the base tier — that's guessing")
	}
	// Date/version tags still match.
	for _, id := range []string{"gemini-2.5-flash-001", "gemini-2.5-flash@20260601", "gemini-2.5-flash.1"} {
		if _, ok := tb.Lookup(id); !ok {
			t.Fatalf("version suffix %q should match the base entry", id)
		}
	}
}

func TestCost(t *testing.T) {
	p := testTable().Models["claude-opus-4-7"]
	got := Cost(p, 1000, 500, 2000, 0)
	want := 0.0185
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestCostLongContextAndCacheWrite(t *testing.T) {
	p := Price{
		InputPerMTok: 5, OutputPerMTok: 30, CacheReadMult: 0.1, CacheWriteMult: 1.25,
		LongContextThreshold: 272_000, LongInputMult: 2, LongOutputMult: 1.5,
	}
	got := Cost(p, 200_000, 10_000, 50_000, 30_000)
	want := (200_000*5.0*2 + 10_000*30.0*1.5 + 50_000*5.0*0.1*2 + 30_000*5.0*1.25*2) / 1e6
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("long-context cost = %v, want %v", got, want)
	}
	if got := Cost(p, -100, -100, -100, -100); got != 0 {
		t.Fatalf("negative token counts reduced cost: %v", got)
	}
}

func TestReprice(t *testing.T) {
	opus := testTable().Models["claude-opus-4-7"]
	// With cache tiers present, Reprice matches Cost exactly.
	if got, want := Reprice(opus, 1000, 500, 2000, 0), Cost(opus, 1000, 500, 2000, 0); math.Abs(got-want) > 1e-12 {
		t.Fatalf("reprice with cache tiers = %v, want %v", got, want)
	}
	// A target without a cache-write tier bills those tokens as ordinary
	// input — not zero, which is what Cost's ingest semantics would say.
	luna := testTable().Models["gpt-5.6-luna"]
	got := Reprice(luna, 1000, 500, 0, 4000)
	want := (1000*1.0 + 500*6.0 + 4000*1.0) / 1e6
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("reprice cache-write fallback = %v, want %v", got, want)
	}
	// Aggregate what-if totals must not trigger a per-request long-context tier.
	long := Price{InputPerMTok: 1, OutputPerMTok: 2, LongContextThreshold: 10, LongInputMult: 2, LongOutputMult: 1.5}
	if got, want := Reprice(long, 100, 100, 0, 0), 300.0/1e6; math.Abs(got-want) > 1e-12 {
		t.Fatalf("aggregate reprice applied request tier: %v, want %v", got, want)
	}
	if got, want := RepriceRequest(long, 100, 100, 0, 0), 500.0/1e6; math.Abs(got-want) > 1e-12 {
		t.Fatalf("per-request reprice omitted long-context tier: %v, want %v", got, want)
	}
}

func TestEmbeddedTableParses(t *testing.T) {
	var tb Table
	if err := decodeStrict(embedded, &tb); err != nil {
		t.Fatal(err)
	}
	if err := validateMetadata(tb.Metadata, true); err != nil {
		t.Fatal(err)
	}
	if err := validateModels(tb.Models); err != nil {
		t.Fatal(err)
	}
	if len(tb.Models) == 0 || tb.Metadata.VerifiedDate == "" {
		t.Fatal("embedded table is empty")
	}
	sonnet, ok := tb.Models["claude-sonnet-5"]
	if !ok || sonnet.InputPerMTok != 2 || sonnet.OutputPerMTok != 10 || sonnet.ValidThrough != "2026-08-31" {
		t.Fatalf("Sonnet 5 current pricing metadata missing: %+v", sonnet)
	}
}

func TestValidateModelsRejectsUnsafeRates(t *testing.T) {
	for name, price := range map[string]Price{
		"negative rate":        {InputPerMTok: -1, OutputPerMTok: 1},
		"missing output":       {InputPerMTok: 1},
		"negative multiplier":  {InputPerMTok: 1, OutputPerMTok: 1, CacheReadMult: -0.1},
		"incomplete long tier": {InputPerMTok: 1, OutputPerMTok: 1, LongContextThreshold: 100, LongInputMult: 2},
		"free with rate":       {Free: true, InputPerMTok: 1},
		"partial provenance":   {InputPerMTok: 1, OutputPerMTok: 1, Source: "https://example.test"},
		"secret source": {InputPerMTok: 1, OutputPerMTok: 1,
			Source: "https://user:secret@example.test/pricing?signature=secret", EffectiveFrom: "2026-01-01", VerifiedDate: "2026-01-01"},
		"deceptive source": {InputPerMTok: 1, OutputPerMTok: 1,
			Source: "https://example.test/pricing/\u202esecret", EffectiveFrom: "2026-01-01", VerifiedDate: "2026-01-01"},
		"hostless source": {InputPerMTok: 1, OutputPerMTok: 1,
			Source: "https://:443/pricing", EffectiveFrom: "2026-01-01", VerifiedDate: "2026-01-01"},
	} {
		if err := validateModels(map[string]Price{"bad": price}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := validateModels(map[string]Price{"local": {Free: true}}); err != nil {
		t.Fatalf("explicit free model rejected: %v", err)
	}
	if err := validateModels(map[string]Price{"unsafe\u202e": {Free: true}}); err == nil {
		t.Fatal("unsafe model label was accepted")
	}
}

func TestReadPricingOverrideBoundsAndStableRegularTarget(t *testing.T) {
	dir := t.TempDir()
	if _, err := readPricingOverride(dir); err == nil {
		t.Fatal("directory pricing override was accepted")
	}
	path := filepath.Join(dir, "oversized.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxPricingOverrideBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readPricingOverride(path); err == nil {
		t.Fatal("oversized pricing override was accepted")
	}

	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(`{"models":{"local":{"free":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "pricing.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if data, err := readPricingOverride(link); err != nil || len(data) == 0 {
		t.Fatalf("stable regular symlink rejected: bytes=%d err=%v", len(data), err)
	}
}

func TestLoadRejectsLooseOrAccidentallyFreeOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".burnban")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pricing.json")
	for name, body := range map[string]string{
		"unknown field":  `{"models":{"x":{"input_per_mtok":1,"output_per_mtok":2,"typo":3}}}`,
		"missing rate":   `{"models":{"x":{"input_per_mtok":1}}}`,
		"trailing JSON":  `{"models":{"x":{"input_per_mtok":1,"output_per_mtok":2}}} {}`,
		"duplicate rate": `{"models":{"x":{"input_per_mtok":1,"input_per_mtok":0,"output_per_mtok":2}}}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Errorf("%s override was accepted", name)
		}
	}
	if err := os.WriteFile(path, []byte(`{"models":{"local":{"free":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := table.Lookup("local"); !ok || !p.Free {
		t.Fatalf("explicit free override missing: %+v ok=%v", p, ok)
	}
	d := table.Diagnostics()
	if d.OverrideFile != "~/.burnban/pricing.json" || len(d.OverrideModels) != 1 || d.OverrideModels[0] != "local" {
		t.Fatalf("override diagnostics = %+v", d)
	}
	if strings.Contains(d.OverrideFile, home) {
		t.Fatalf("diagnostics leaked absolute home: %+v", d)
	}
}
