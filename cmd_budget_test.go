package main

import (
	"path/filepath"
	"testing"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/store"
)

func TestCapRejectsSubCent(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	// $0.004 would store as a zero-looking cap; it must be refused.
	if err := cmdCap([]string{"--daily", "0.004", "--db", db}); err == nil {
		t.Fatal("sub-cent cap accepted")
	}
	if err := cmdCap([]string{"--warn", "150", "--db", db}); err == nil {
		t.Fatal("warn threshold above 100% accepted")
	}
	for _, value := range []string{"NaN", "+Inf", "-Inf"} {
		if err := cmdCap([]string{"--daily", value, "--db", db}); err == nil {
			t.Errorf("non-finite cap %q accepted", value)
		}
	}
	if err := cmdCap([]string{"--warn", "NaN", "--db", db}); err == nil {
		t.Fatal("non-finite warning threshold accepted")
	}
}

func TestCapExplicitZeroRemovesOneWindow(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	if err := cmdCap([]string{"--daily", "10", "--monthly", "100", "--db", db}); err != nil {
		t.Fatal(err)
	}
	if err := cmdCap([]string{"--daily", "0", "--db", db}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.GetSetting(budget.KeyDailyCapUSD); v != "" {
		t.Fatalf("daily cap still set: %q", v)
	}
	if v, _ := s.GetSetting(budget.KeyMonthlyCapUSD); v != "100" {
		t.Fatalf("monthly cap should survive: %q", v)
	}
}

func TestCapBareAgentShowsStatusNotError(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	// v0.3 printed status here; erroring broke scripts.
	if err := cmdCap([]string{"--agent", "claude-cli", "--db", db}); err != nil {
		t.Fatalf("bare --agent should show status, got error: %v", err)
	}
	if err := cmdCap([]string{"--agent", "claude-cli", "--warn", "50", "--db", db}); err == nil {
		t.Fatal("--warn with --agent must error, not be silently dropped")
	}
}
