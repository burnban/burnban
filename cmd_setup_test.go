package main

import (
	"bufio"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPromptGoalAndAgent(t *testing.T) {
	if got := promptGoal(bufio.NewReader(strings.NewReader("2\n")), false); got != "enforce" {
		t.Fatalf("promptGoal = %q, want enforce", got)
	}
	if got := promptAgent(bufio.NewReader(strings.NewReader("2\n")), false); got != "codex" {
		t.Fatalf("promptAgent = %q, want codex", got)
	}
	if got := promptGoal(bufio.NewReader(strings.NewReader("\n")), false); got != "observe" {
		t.Fatalf("default promptGoal = %q, want observe", got)
	}
}

func TestRouteCommand(t *testing.T) {
	if got := routeCommand("claude", "darwin"); got != "export ANTHROPIC_BASE_URL='http://127.0.0.1:4141/anthropic'" {
		t.Fatalf("Claude route = %q", got)
	}
	if got := routeCommand("codex", "windows"); got != "$env:OPENAI_BASE_URL='http://127.0.0.1:4141/openai/v1'" {
		t.Fatalf("Codex PowerShell route = %q", got)
	}
}

func TestPromptInterface(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"1\n", "desktop"},
		{"\n", "desktop"}, // empty accepts the default
		{"2\n", "web"},
		{"3\n", "terminal"},
		{"  2  \n", "web"},    // surrounding whitespace is trimmed
		{"9\n1\n", "desktop"}, // reject bad input, then accept a valid pick
		{"", "desktop"},       // EOF with no pick falls back to the default
	}
	for _, tc := range cases {
		got := promptInterfaceDefault(bufio.NewReader(strings.NewReader(tc.in)), false, "desktop")
		if got != tc.want {
			t.Errorf("promptInterface(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// promptInterface and maybeSetCap share one reader; the picker must consume
// exactly its line and leave the cap answer intact for the next step.
func TestSharedReaderOrder(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("2\n5\n"))
	if got := promptInterfaceDefault(r, false, "desktop"); got != "web" {
		t.Fatalf("promptInterface = %q, want web", got)
	}
	rest, _ := r.ReadString('\n')
	if strings.TrimSpace(rest) != "5" {
		t.Fatalf("next line after picker = %q, want 5", rest)
	}
}

func TestRecommendedInterface(t *testing.T) {
	if got := recommendedInterface("linux", "", ""); got != "terminal" {
		t.Fatalf("headless Linux recommendation = %q", got)
	}
	if got := recommendedInterface("linux", ":0", ""); got != "desktop" {
		t.Fatalf("desktop Linux recommendation = %q", got)
	}
	if got := recommendedInterface("darwin", "", ""); got != "desktop" {
		t.Fatalf("macOS recommendation = %q", got)
	}
}

func TestFmtUSD(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "$0.00"},
		{740.23, "$740.23"},
		{200, "$200.00"},
		{22206.96, "$22,206.96"},
		{1234567.5, "$1,234,567.50"},
		{-5, "-$5.00"},
	}
	for _, tc := range cases {
		if got := fmtUSD(tc.in); got != tc.want {
			t.Errorf("fmtUSD(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("BURNBAN_CONFIG", path)

	// Missing file reads as "not set up yet".
	if loadConfig().SetupDone {
		t.Fatal("expected SetupDone=false for a missing config")
	}

	want := burnbanConfig{Version: 1, SetupDone: true, Interface: "web"}
	if err := saveConfig(want); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	got := loadConfig()
	if got != want {
		t.Fatalf("loadConfig() = %+v, want %+v", got, want)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("config mode = %04o, want 0600", info.Mode().Perm())
		}
	}
}

func TestInitializeLedgerCreatesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "burnban.db")
	t.Setenv("BURNBAN_DB", path)
	if err := initializeLedger(); err != nil {
		t.Fatalf("initializeLedger: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var settings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='settings'`).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	if settings != 1 {
		t.Fatalf("settings table count = %d, want 1", settings)
	}
}

func TestInvalidSavedInterfaceIsActionable(t *testing.T) {
	err := launchConfiguredInterface(burnbanConfig{SetupDone: true, Interface: "broken"})
	if err == nil || !strings.Contains(err.Error(), "burnban setup") {
		t.Fatalf("launchConfiguredInterface error = %v", err)
	}
}
