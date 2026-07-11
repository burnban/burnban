package store_test

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestTopReturnsOnlyLeanBoundedRankings(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "top.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 7; i++ {
		if err := s.Insert(store.Request{
			Ts: now.Add(time.Duration(i) * time.Second), Provider: "openai",
			Model: fmt.Sprintf("model-%02d", i), Agent: fmt.Sprintf("agent-%02d", i),
			InTokens: int64(10 * (i + 1)), CacheReadTokens: int64(2 * (i + 1)),
			CostUSD: float64(i + 1), BodyHash: "same-duplicate-fingerprint",
			UsageState: store.UsageExact, PricingState: store.PricingPriced,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Insert(store.Request{
		Ts: now.Add(-48 * time.Hour), Provider: "openai", Model: "old", Agent: "old",
		CostUSD: 100, UsageState: store.UsageExact, PricingState: store.PricingPriced,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Top(now.Add(-time.Minute), 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 7 || math.Abs(got.Cost-28) > 1e-9 || got.In != 280 || got.CacheRead != 56 {
		t.Fatalf("top totals = %+v", got)
	}
	if len(got.Models) != 5 || got.Models[0].Model != "model-06" || got.Models[4].Model != "model-02" {
		t.Fatalf("top models = %+v", got.Models)
	}
	if len(got.Agents) != 5 || got.Agents[0].Agent != "agent-06" || got.Agents[4].Agent != "agent-02" {
		t.Fatalf("top agents = %+v", got.Agents)
	}
	if _, err := s.Top(now, 0); err == nil {
		t.Fatal("top accepted an unbounded/empty limit")
	}

	// The fixture really is a duplicate-heavy ledger. Full Summary sees that
	// receipt analysis; Top deliberately exposes no duplicate fields or scan.
	full, err := s.Summarize(now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if full.DupGroups != 1 {
		t.Fatalf("duplicate-heavy fixture was not established: %+v", full)
	}
}

func TestStreamExportOrderingEarlyStopAndCompatibility(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ts := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		if err := s.Insert(store.Request{
			Ts: ts, Provider: fmt.Sprintf("provider-%d", i), Model: "model", CostUSD: float64(i),
			UsageState: store.UsageExact, PricingState: store.PricingPriced,
		}); err != nil {
			t.Fatal(err)
		}
	}

	stop := errors.New("stop after two")
	var streamed []store.Request
	err = s.StreamExport(ts.Add(-time.Second), func(r store.Request) error {
		streamed = append(streamed, r)
		if len(streamed) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || len(streamed) != 2 || streamed[0].Provider != "provider-0" || streamed[1].Provider != "provider-1" {
		t.Fatalf("early stream = rows=%+v err=%v", streamed, err)
	}
	if err := s.StreamExport(ts, nil); err == nil {
		t.Fatal("nil export visitor was accepted")
	}

	rows, err := s.Export(ts.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[2].Provider != "provider-2" {
		t.Fatalf("compatible Export rows = %+v", rows)
	}
}
