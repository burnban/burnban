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

func TestCapSetAndBanClearTodayOverride(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	overrideActive := func() bool {
		t.Helper()
		s, err := store.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		v, err := s.GetSetting(budget.KeyOverrideDay)
		if err != nil {
			t.Fatal(err)
		}
		return v != ""
	}
	lift := func() {
		t.Helper()
		if err := cmdLift([]string{"--today", "--db", db}); err != nil {
			t.Fatal(err)
		}
		if !overrideActive() {
			t.Fatal("lift --today did not record an override")
		}
	}

	// A cap set after `lift --today` must enforce, not stay silently
	// suspended until midnight.
	lift()
	if err := cmdCap([]string{"--daily", "5", "--db", db}); err != nil {
		t.Fatal(err)
	}
	if overrideActive() {
		t.Fatal("setting a cap did not clear the today override")
	}

	lift()
	if err := cmdCap([]string{"--agent", "claude-cli", "--daily", "3", "--db", db}); err != nil {
		t.Fatal(err)
	}
	if overrideActive() {
		t.Fatal("setting a per-agent cap did not clear the today override")
	}

	// Ban then lift must return to enforced budgets, not resurrect the
	// earlier override.
	lift()
	if err := cmdBan([]string{"--db", db}); err != nil {
		t.Fatal(err)
	}
	if overrideActive() {
		t.Fatal("ban did not clear the today override")
	}

	// Removing a cap is not a request for enforcement; the override stays.
	lift()
	if err := cmdCap([]string{"--daily", "0", "--db", db}); err != nil {
		t.Fatal(err)
	}
	if !overrideActive() {
		t.Fatal("removing a cap should not clear the today override")
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
