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

func TestCost(t *testing.T) {
	p := testTable().Models["claude-opus-4-7"]
	got := Cost(p, 1000, 500, 2000, 0)
	want := 0.0185
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", got, want)
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
