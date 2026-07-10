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
