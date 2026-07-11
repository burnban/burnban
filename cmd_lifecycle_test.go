package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestControlURLRequiresPlainHTTPLoopbackOriginWithPort(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:4141",
		"http://localhost:1/",
		"http://[::1]:65535",
	} {
		if _, err := parseControlURL(raw); err != nil {
			t.Errorf("parseControlURL(%q) = %v", raw, err)
		}
	}
	for _, raw := range []string{
		"https://127.0.0.1:4141",
		"http://127.0.0.1",
		"http://127.0.0.1:0",
		"http://127.0.0.1:65536",
		"http://192.168.1.10:4141",
		"http://example.com:4141",
		"http://user@127.0.0.1:4141",
		"http://127.0.0.1:4141/control",
		"http://127.0.0.1:4141?",
		"http://127.0.0.1:4141?token=secret",
		"http://127.0.0.1:4141#fragment",
	} {
		if _, err := parseControlURL(raw); err == nil {
			t.Errorf("parseControlURL(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestReadServerStateRejectsUnsafeControlURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state := serverState{
		Version: "test", PID: 123, URL: "https://burnban.example", DBPath: "test.db",
		StartedAt: time.Now().UTC(), ControlToken: strings.Repeat("a", 64),
		ControlURL: "https://127.0.0.1:4141",
	}
	if err := writeServerState(path, state); err != nil {
		t.Fatal(err)
	}
	if _, err := readServerState(path); err == nil || !strings.Contains(err.Error(), "control URL is invalid") {
		t.Fatalf("unsafe lifecycle state was accepted: %v", err)
	}
}

func TestControlClientNeverUsesEnvironmentProxy(t *testing.T) {
	transport, ok := controlHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("control transport type = %T", controlHTTPClient.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("private lifecycle client has a proxy function")
	}
}

func TestServerStateAliveRejectsUnrelatedProcessOnReusedPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("some unrelated local service"))
	}))
	t.Cleanup(srv.Close)
	if serverStateAlive(serverState{
		PID: 123, ControlURL: srv.URL, ControlToken: strings.Repeat("a", 64),
	}) {
		t.Fatal("an arbitrary HTTP 200 on a reused loopback port was treated as the live Burnban server")
	}
}

func TestStatusJSONIsNonzeroWhenInactive(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "not-running.db")
	var out bytes.Buffer
	err := cmdStatusTo([]string{"--db", dbPath, "--json"}, &out)
	if err == nil {
		t.Fatal("inactive status returned success")
	}
	var got statusResult
	if decodeErr := json.Unmarshal(out.Bytes(), &got); decodeErr != nil {
		t.Fatalf("decode status JSON: %v; output=%q", decodeErr, out.String())
	}
	if got.OK || got.Active || got.Healthy || !strings.Contains(got.Issue, "not running") {
		t.Fatalf("inactive status = %+v", got)
	}
}

func TestStatusJSONUsesAuthenticatedControlHealth(t *testing.T) {
	const token = "burnban-test-status-control-token-not-a-secret"
	var authenticated atomic.Bool
	statePID := os.Getpid()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/control/status" || r.Header.Get("x-burnban-control-token") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		authenticated.Store(true)
		_ = json.NewEncoder(w).Encode(controlStatusPayload{
			OK: true, PID: statePID, Version: "test", StartedAt: time.Now().UTC(),
			Health: lifecycleHealth{Service: "burnban", OK: true, PersistenceOK: true, State: "healthy", InFlight: 2, ReservedUSD: 1.25},
		})
	}))
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	state := serverState{
		Version: "test\x1b[31m", PID: statePID, URL: "https://burnban.example/\x1b[32m",
		ControlURL: srv.URL, DBPath: "ledger\ninjected.db", StartedAt: time.Now().UTC(), ControlToken: token,
	}
	if err := writeServerState(serverStatePath(dbPath), state); err != nil {
		t.Fatal(err)
	}

	var jsonOutput bytes.Buffer
	if err := cmdStatusTo([]string{"--db", dbPath, "--json"}, &jsonOutput); err != nil {
		t.Fatal(err)
	}
	var got statusResult
	if err := json.Unmarshal(jsonOutput.Bytes(), &got); err != nil {
		t.Fatalf("decode status JSON: %v", err)
	}
	if !got.OK || !got.Active || !got.Healthy || got.Health == nil || got.Health.InFlight != 2 {
		t.Fatalf("healthy status = %+v", got)
	}
	if !authenticated.Load() {
		t.Fatal("status did not authenticate to the private control endpoint")
	}

	var human bytes.Buffer
	if err := cmdStatusTo([]string{"--db", dbPath}, &human); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(human.String(), "\x1b") || strings.Contains(human.String(), "ledger\ninjected") {
		t.Fatalf("status retained terminal controls: %q", human.String())
	}
	if !strings.Contains(human.String(), "2 in flight") {
		t.Fatalf("human status missing health detail: %q", human.String())
	}
}

func TestStatusReturnsFailureForUnhealthyServer(t *testing.T) {
	const token = "burnban-test-unhealthy-control-token-not-secret"
	statePID := os.Getpid()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(controlStatusPayload{
			OK: true, PID: statePID,
			Health: lifecycleHealth{
				Service: "burnban", State: "fail_closed", Detail: "database unavailable",
			},
		})
	}))
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	if err := writeServerState(serverStatePath(dbPath), serverState{
		Version: "test", PID: statePID, URL: "http://127.0.0.1:4141", ControlURL: srv.URL,
		DBPath: dbPath, StartedAt: time.Now().UTC(), ControlToken: token,
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := cmdStatusTo([]string{"--db", dbPath, "--json"}, &out)
	if err == nil {
		t.Fatal("unhealthy status returned success")
	}
	var got statusResult
	if decodeErr := json.Unmarshal(out.Bytes(), &got); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if got.OK || !got.Active || got.Healthy || got.Health == nil || got.Health.State != "fail_closed" {
		t.Fatalf("unhealthy status = %+v", got)
	}
}
