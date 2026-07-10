package pricing

import (
	"encoding/json"
	"math"
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
}

func TestEmbeddedTableParses(t *testing.T) {
	var tb Table
	if err := json.Unmarshal(embedded, &tb); err != nil {
		t.Fatal(err)
	}
	if len(tb.Models) == 0 {
		t.Fatal("embedded table is empty")
	}
}

func TestValidateModelsRejectsUnsafeRates(t *testing.T) {
	for name, price := range map[string]Price{
		"negative rate":       {InputPerMTok: -1},
		"negative multiplier": {InputPerMTok: 1, CacheReadMult: -0.1},
		"incomplete long tier": {InputPerMTok: 1, LongContextThreshold: 100, LongInputMult: 2},
	} {
		if err := validateModels(map[string]Price{"bad": price}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
