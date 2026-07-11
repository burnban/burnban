package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestEndpointConfigurationRequiresExactProviderRoute(t *testing.T) {
	const meter = "http://127.0.0.1:4141"
	tests := []struct {
		name, raw, route string
		want             bool
	}{
		{name: "anthropic", raw: meter + "/anthropic", route: "/anthropic", want: true},
		{name: "trailing slash", raw: meter + "/openai/v1/", route: "/openai/v1", want: true},
		{name: "missing route", raw: meter, route: "/anthropic"},
		{name: "route prefix only", raw: meter + "/openai", route: "/openai/v1"},
		{name: "extra route", raw: meter + "/gemini/v1", route: "/gemini"},
		{name: "wrong origin", raw: "http://localhost:4141/anthropic", route: "/anthropic"},
		{name: "case sensitive path", raw: meter + "/Anthropic", route: "/anthropic"},
		{name: "credentials", raw: "http://user:secret@127.0.0.1:4141/anthropic", route: "/anthropic"},
		{name: "query secret", raw: meter + "/anthropic?token=secret", route: "/anthropic"},
		{name: "empty query", raw: meter + "/anthropic?", route: "/anthropic"},
		{name: "fragment", raw: meter + "/anthropic#secret", route: "/anthropic"},
		{name: "invalid", raw: "://secret", route: "/anthropic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, got := endpointConfigurationState(tt.raw, meter, tt.route)
			if got != tt.want {
				t.Fatalf("match=%t, want %t; state=%q", got, tt.want, state)
			}
			if strings.Contains(state, "secret") || strings.Contains(state, "user") {
				t.Fatalf("configuration state leaked input: %q", state)
			}
		})
	}
	for _, pair := range [][2]string{
		{"https://burnban.example:443/anthropic", "https://burnban.example"},
		{"https://burnban.example./anthropic", "https://burnban.example:443"},
		{"https://[::1]:443/anthropic", "https://[::1]"},
	} {
		if state, ok := endpointConfigurationState(pair[0], pair[1], "/anthropic"); !ok {
			t.Errorf("normalized origin %q vs %q did not match: %s", pair[0], pair[1], state)
		}
	}
}

func TestDoctorUsesPrivateAuthenticatedControlState(t *testing.T) {
	for _, spec := range providerBaseEnvs {
		t.Setenv(spec.Key, "")
	}
	t.Setenv("BURNBAN_TOKEN", "")

	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(store.Request{
		Ts: time.Now(), Provider: "anthropic", Model: "claude-sonnet-5", CostUSD: 0.01, Priced: true,
	}); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	const controlToken = "burnban-test-doctor-control-token-not-a-secret"
	pid := os.Getpid()
	var authenticated atomic.Bool
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/control/status" || r.Header.Get("x-burnban-control-token") != controlToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		authenticated.Store(true)
		_ = json.NewEncoder(w).Encode(controlStatusPayload{
			OK: true, PID: pid, Version: "test", StartedAt: time.Now().UTC(),
			Health: lifecycleHealth{Service: "burnban", OK: true, PersistenceOK: true, State: "healthy"},
		})
	}))
	t.Cleanup(control.Close)

	// This advertised team URL is intentionally unreachable from the test.
	// Doctor can only pass by using the authenticated private control state.
	if err := writeServerState(serverStatePath(dbPath), serverState{
		Version: "test", PID: pid, URL: "https://unreachable.invalid", ControlURL: control.URL,
		DBPath: dbPath, StartedAt: time.Now().UTC(), ControlToken: controlToken,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cmdDoctor([]string{
		"--db", dbPath, "--recent", "1h", "--pricing-max-age", "100000h", "--json",
	}); err != nil {
		t.Fatalf("doctor failed despite healthy private control state: %v", err)
	}
	if !authenticated.Load() {
		t.Fatal("doctor did not authenticate to the private control endpoint")
	}

	// A configured SDK base that misses the exact provider route must make the
	// overall diagnostic fail, even though the process and ledger are healthy.
	t.Setenv("OPENAI_BASE_URL", "https://unreachable.invalid/openai")
	if err := cmdDoctor([]string{
		"--db", dbPath, "--recent", "1h", "--pricing-max-age", "100000h", "--json",
	}); err == nil {
		t.Fatal("doctor accepted a provider base URL on the wrong route")
	}
}

func TestProbeHealthRequiresOriginURL(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:4141/anthropic",
		"http://127.0.0.1:4141?token=secret",
		"http://user:secret@127.0.0.1:4141",
	} {
		if err := probeHealth(raw, false); err == nil || !strings.Contains(err.Error(), "origin") {
			t.Errorf("probeHealth(%q) error=%v", raw, err)
		}
	}
}

func TestDiagnosticServerURLRedactsCredentialsAndSuffixes(t *testing.T) {
	for _, raw := range []string{
		"https://user:secret@burnban.example",
		"https://burnban.example/private-secret",
		"https://burnban.example?token=secret",
		"://secret",
	} {
		got := diagnosticServerURL(raw)
		if strings.Contains(got, "secret") || strings.Contains(got, "user") || strings.Contains(got, "token") {
			t.Errorf("diagnosticServerURL(%q) leaked data as %q", raw, got)
		}
	}
	if got := diagnosticServerURL("https://burnban.example/"); got != "https://burnban.example" {
		t.Fatalf("valid diagnostic URL = %q", got)
	}
}
