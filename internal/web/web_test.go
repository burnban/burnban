package web_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/store"
	"github.com/syft8/burnban/internal/web"
)

func newServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	mux := http.NewServeMux()
	web.Register(mux, s, "test")
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func TestDashboardServes(t *testing.T) {
	srv, _ := newServer(t)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "MODEL SPEND METER") {
		t.Fatal("dashboard HTML missing")
	}
}

func TestMetrics(t *testing.T) {
	srv, _ := newServer(t)
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "burnban_requests_total") {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestWithAuth(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(web.WithAuth("sekret", inner))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set("x-burnban-token", "sekret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with token: status = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/x?token=sekret")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query token: status = %d, want 200", resp.StatusCode)
	}
}

func TestSummaryAPI(t *testing.T) {
	srv, s := newServer(t)
	if err := s.SetSetting(budget.KeyBanActive, "1"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "/api/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d struct {
		TotalCost float64 `json:"total_cost"`
		BanActive bool    `json:"ban_active"`
		Models    []any   `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.TotalCost != 0 || !d.BanActive || d.Models == nil {
		t.Fatalf("summary = %+v", d)
	}
}
