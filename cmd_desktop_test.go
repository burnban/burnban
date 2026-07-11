package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDashboardCommand(t *testing.T) {
	const url = "http://127.0.0.1:4141"
	tests := []struct {
		goos string
		name string
		args []string
	}{
		{"darwin", "open", []string{url}},
		{"linux", "xdg-open", []string{url}},
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler", url}},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			name, args, err := dashboardCommand(tt.goos, url)
			if err != nil {
				t.Fatal(err)
			}
			if name != tt.name || !reflect.DeepEqual(args, tt.args) {
				t.Fatalf("dashboardCommand(%q) = %q, %q; want %q, %q", tt.goos, name, args, tt.name, tt.args)
			}
		})
	}
	if _, _, err := dashboardCommand("plan9", url); err == nil {
		t.Fatal("dashboardCommand accepted an unsupported OS")
	}
}

func TestDashboardURLAndExistingMeter(t *testing.T) {
	if got := dashboardURL("http://127.0.0.1:4141", "a token"); got != "http://127.0.0.1:4141" {
		t.Fatalf("dashboardURL = %q", got)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/summary" || r.Header.Get("x-burnban-token") != "secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"version":"test"}`)
	}))
	t.Cleanup(srv.Close)
	if !burnbanRunning(srv.URL, "secret") {
		t.Fatal("did not recognize an existing Burnban server")
	}
	if burnbanRunning(srv.URL, "wrong") {
		t.Fatal("accepted an unauthorized server response")
	}
	notBurnban := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 10))
	}))
	t.Cleanup(notBurnban.Close)
	if burnbanRunning(notBurnban.URL, "") {
		t.Fatal("accepted a non-Burnban process")
	}
}
