package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/store"
)

func TestFuseCommandConfiguresResetsAndRemovesRules(t *testing.T) {
	db := filepath.Join(t.TempDir(), "fuse.db")
	if err := cmdFuse([]string{"--hourly", "20", "--burst", "5m:4", "--cooldown", "10m", "--db", db}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSetting(budget.KeyFuseHourlyUSD); got != "20" {
		t.Fatalf("hourly=%q", got)
	}
	if got, _ := s.GetSetting(budget.KeyFuseBurst); got != "5m:4" {
		t.Fatalf("burst=%q", got)
	}
	if got, _ := s.GetSetting(budget.KeyFuseCooldown); got != "10m" {
		t.Fatalf("cooldown=%q", got)
	}
	guard := &budget.Guard{S: s}
	if reservation, denial, err := guard.Admit(time.Now(), "", budget.AdmissionEstimate{
		USD: 5, Priced: true, Bounded: true,
	}); err != nil || reservation != nil || denial == nil || denial.Type != "burnban_fuse_tripped" {
		t.Fatalf("trip setup reservation=%v denial=%+v err=%v", reservation, denial, err)
	}
	frame, err := renderTop(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frame, "SPEND-VELOCITY FUSE TRIPPED") || !strings.Contains(frame, "burst*") || !strings.Contains(frame, "rolling 5m") {
		t.Fatalf("top omitted fuse state:\n%s", frame)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmdFuse([]string{"--cooldown", "20m", "--db", db}); err != nil {
		t.Fatalf("change cooldown: %v", err)
	}
	s, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if trip, _ := s.GetSetting(budget.KeyFuseTrip); trip == "" {
		t.Fatal("policy edit silently cleared an active fuse trip")
	}
	if cooldown, _ := s.GetSetting(budget.KeyFuseCooldown); cooldown != "20m" {
		t.Fatalf("updated cooldown=%q", cooldown)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if err := cmdFuse([]string{"--db", db}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := cmdFuse([]string{"--reset", "--db", db}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := cmdFuse([]string{"--off", "--db", db}); err != nil {
		t.Fatalf("off: %v", err)
	}
	s, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, key := range []string{budget.KeyFuseHourlyUSD, budget.KeyFuseBurst, budget.KeyFuseCooldown, budget.KeyFuseTrip} {
		if got, _ := s.GetSetting(key); got != "" {
			t.Errorf("%s survived --off: %q", key, got)
		}
	}
}

func TestFuseCommandRejectsUnsafeConfiguration(t *testing.T) {
	db := filepath.Join(t.TempDir(), "fuse.db")
	for _, args := range [][]string{
		{"--hourly", "NaN", "--db", db},
		{"--hourly", "0.001", "--db", db},
		{"--burst", "5m:0.001", "--db", db},
		{"--burst", "2h:4", "--db", db},
		{"--cooldown", "30s", "--db", db},
		{"--cooldown", "25h", "--db", db},
		{"--off", "--hourly", "10", "--db", db},
		{"--reset", "--burst", "5m:4", "--db", db},
	} {
		if err := cmdFuse(args); err == nil {
			t.Errorf("unsafe fuse args accepted: %v", args)
		}
	}
}

func TestFuseCommandRemovesRulesIndependently(t *testing.T) {
	db := filepath.Join(t.TempDir(), "fuse.db")
	if err := cmdFuse([]string{"--hourly", "20", "--burst", "5m:4", "--db", db}); err != nil {
		t.Fatal(err)
	}
	if err := cmdFuse([]string{"--hourly", "0", "--db", db}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if hourly, _ := s.GetSetting(budget.KeyFuseHourlyUSD); hourly != "" {
		t.Errorf("hourly rule survived explicit zero: %q", hourly)
	}
	if burst, _ := s.GetSetting(budget.KeyFuseBurst); burst != "5m:4" {
		t.Errorf("burst rule was removed with hourly: %q", burst)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmdFuse([]string{"--burst", "off", "--db", db}); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if burst, _ := s.GetSetting(budget.KeyFuseBurst); burst != "" {
		t.Errorf("burst rule survived off: %q", burst)
	}
}
