package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSinceRejectsNonPositiveWindows(t *testing.T) {
	for _, value := range []string{"0h", "-1h", "-30m", "0d", "-1d", "106752d", "999999999999999999999d"} {
		if _, _, err := parseSince(value); err == nil {
			t.Errorf("parseSince(%q) accepted a non-positive window", value)
		}
	}
}

func TestCommandSafetyValidation(t *testing.T) {
	if err := cmdTop([]string{"--interval=0"}); err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("top interval validation error=%v", err)
	}
	if err := cmdDoctor([]string{"--recent=0"}); err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("doctor recent validation error=%v", err)
	}
	if err := cmdPrune([]string{"--older-than=90d"}); err == nil || !strings.Contains(err.Error(), "without --yes") {
		t.Fatalf("prune confirmation error=%v", err)
	}
	if err := cmdPrune([]string{"--older-than=90d", "--yes", "typo"}); err == nil || !strings.Contains(err.Error(), "unexpected positional") {
		t.Fatalf("prune positional-argument error=%v", err)
	}
}

func TestPrivateServerStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	state := serverState{
		Version: "test", PID: 123, URL: "http://localhost:4141",
		ControlURL: "http://127.0.0.1:4141", DBPath: "test.db",
		StartedAt: time.Now().UTC(), ControlToken: strings.Repeat("a", 64),
	}
	if err := writeServerState(path, state); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("server state mode=%#o, want private", info.Mode().Perm())
	}
	got, err := readServerState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ControlToken != state.ControlToken || got.PID != state.PID || got.ControlURL != state.ControlURL {
		t.Fatalf("state round trip=%+v, want %+v", got, state)
	}
	removeServerState(path, strings.Repeat("b", 64))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("wrong token removed server state: %v", err)
	}
	removeServerState(path, state.ControlToken)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("matching token did not remove server state: %v", err)
	}
}

func TestControlTokenCannotFallThrough(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	handler := withControlToken("secret", control, next)

	for _, tt := range []struct {
		path, token string
		want        int
	}{
		{path: "/api/control/stop", token: "secret", want: http.StatusAccepted},
		{path: "/api/control/stop", token: "wrong", want: http.StatusTeapot},
		{path: "/openai/v1/chat", token: "secret", want: http.StatusTeapot},
	} {
		req := httptest.NewRequest(http.MethodPost, tt.path, nil)
		req.Header.Set("x-burnban-control-token", tt.token)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != tt.want {
			t.Errorf("path=%s token=%s status=%d want=%d", tt.path, tt.token, rr.Code, tt.want)
		}
	}
}

func TestFileIsTerminalRejectsFilesPipesAndDevNull(t *testing.T) {
	regular, err := os.CreateTemp(t.TempDir(), "output-*")
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	if fileIsTerminal(regular) {
		t.Fatal("regular file was treated as a terminal")
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	if fileIsTerminal(write) {
		t.Fatal("pipe was treated as a terminal")
	}
	if devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		defer devNull.Close()
		if fileIsTerminal(devNull) {
			t.Fatal("os.DevNull was treated as a terminal")
		}
	}
}
