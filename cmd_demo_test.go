package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDemoRefusesExistingCustomDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "important.db")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := cmdDemo([]string{"--db", path})
	if err == nil || !strings.Contains(err.Error(), "without --force") {
		t.Fatalf("existing database was not protected: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "keep me" {
		t.Fatalf("existing database changed: data=%q err=%v", got, err)
	}
}

func TestDemoRefusesDatabaseServedByLiveProcess(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/control/status" || r.Header.Get("x-burnban-control-token") != token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(controlStatusPayload{
			OK: true, PID: 123,
			Health: lifecycleHealth{Service: "burnban", OK: true, PersistenceOK: true, State: "healthy"},
		})
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "live.db")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeServerState(serverStatePath(path), serverState{
		Version: "test", PID: 123, URL: srv.URL, ControlURL: srv.URL,
		DBPath: path, StartedAt: time.Now().UTC(), ControlToken: token,
	}); err != nil {
		t.Fatal(err)
	}
	err := cmdDemo([]string{"--db", path, "--force", "--no-open"})
	if err == nil || !strings.Contains(err.Error(), "serving it") {
		t.Fatalf("live demo database was not protected: %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != "keep me" {
		t.Fatalf("live database changed: data=%q err=%v", data, readErr)
	}
}
