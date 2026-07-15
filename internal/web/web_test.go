package web_test

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/localusage"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
	"github.com/burnban/burnban/internal/web"
)

func newServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	mux := http.NewServeMux()
	prices, err := pricing.Load()
	if err != nil {
		t.Fatal(err)
	}
	web.Register(mux, s, "test", prices)
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
	page := string(body)
	if !strings.Contains(page, "MODEL SPEND METER") {
		t.Fatal("dashboard HTML missing")
	}
	for _, want := range []string{`aria-valuenow="0"`, `aria-live="assertive"`, `aria-pressed="true"`, `focus-visible`, `DEMO DATA`, `AUTH REQUIRED`, `history.replaceState`, `trafficMode`} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard is missing accessibility/state marker %q", want)
		}
	}
}

func TestLocalSafetyRejectsDNSRebindingAndCrossSiteRequests(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := web.LocalSafety("127.0.0.1", true, inner)

	tests := []struct {
		name       string
		host       string
		origin     string
		fetchSite  string
		wantStatus int
	}{
		{name: "localhost", host: "localhost:4141", wantStatus: http.StatusNoContent},
		{name: "loopback ipv4", host: "127.8.2.1:4141", wantStatus: http.StatusNoContent},
		{name: "rebound host", host: "attacker.example", wantStatus: http.StatusMisdirectedRequest},
		{name: "evil origin", host: "localhost:4141", origin: "https://attacker.example", wantStatus: http.StatusForbidden},
		{name: "cross site metadata", host: "localhost:4141", fetchSite: "cross-site", wantStatus: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost:4141/api/summary", nil)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status=%d, want %d; body=%q", rr.Code, tt.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestConfiguredPublicOriginSurvivesTLSProxyHostRewrite(t *testing.T) {
	proxyServer := httptest.NewUnstartedServer(nil)
	publicOrigin := "https://" + proxyServer.Listener.Addr().String()
	backend := httptest.NewServer(web.LocalSafetyWithPublicOrigin(
		"127.0.0.1", false, publicOrigin,
		web.WithAuth("team-secret", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })),
	))
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	reverse := httputil.NewSingleHostReverseProxy(target)
	director := reverse.Director
	reverse.Director = func(r *http.Request) {
		director(r)
		// Simulate a common ingress default: public TLS terminates here and
		// the backend Host is rewritten to its loopback upstream authority.
		r.Host = target.Host
	}
	proxyServer.Config.Handler = reverse
	proxyServer.StartTLS()
	t.Cleanup(proxyServer.Close)
	if proxyServer.URL != publicOrigin {
		t.Fatalf("test proxy origin=%q, configured=%q", proxyServer.URL, publicOrigin)
	}

	req, _ := http.NewRequest(http.MethodGet, proxyServer.URL+"/api/summary", nil)
	req.Header.Set("Origin", proxyServer.URL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("x-burnban-token", "team-secret")
	resp, err := proxyServer.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("public-origin request through Host-rewriting TLS proxy = %d, want 204", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, proxyServer.URL+"/api/summary", nil)
	req.Header.Set("Origin", "https://attacker.example")
	resp, err = proxyServer.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unconfigured origin through proxy = %d, want 403", resp.StatusCode)
	}
}

func TestConfiguredPublicOriginNormalizesDefaultPortsDotsAndIPv6(t *testing.T) {
	tests := []struct {
		name, configured, origin string
	}{
		{name: "https explicit default", configured: "https://example.com:443", origin: "https://example.com"},
		{name: "http explicit default", configured: "http://example.com:80", origin: "http://example.com"},
		{name: "trailing dot", configured: "https://example.com.:443", origin: "https://example.com"},
		{name: "ipv6 default", configured: "https://[::1]:443", origin: "https://[::1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := web.LocalSafetyWithPublicOrigin("127.0.0.1", false, tt.configured,
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4141/api/summary", nil)
			req.Host = "127.0.0.1:4141"
			req.Header.Set("Origin", tt.origin)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestMetrics(t *testing.T) {
	srv, s := newServer(t)
	if err := s.SetSetting(budget.KeyFuseBurst, "5m:1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyFuseFanout, "1m:10"); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(store.Request{
		Ts: time.Now(), Provider: "openai", Model: "evil\"\\line\n\t\r\x1b界",
		Agent: "agent\t\r\x1b\"\\界", InTokens: 1, CostUSD: .01, Status: 200, Priced: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"alpha", "beta"} {
		if err := s.Insert(store.Request{
			Ts: time.Now(), Provider: "openai", Model: strings.Repeat("same-prefix-", 25) + suffix,
			Agent: strings.Repeat("same-agent-", 25) + suffix, CostUSD: .01, Status: 200,
			PricingState: store.PricingPriced,
		}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	metrics := string(body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(metrics, "burnban_ledger_requests") {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	for _, name := range []string{
		"burnban_ledger_unknown_pricing", "burnban_ledger_unmetered",
		"burnban_ledger_incomplete", "burnban_ledger_enforcement_gaps",
		"burnban_fuse_spend_usd", "burnban_fuse_limit_usd", "burnban_fuse_tripped",
		"burnban_fuse_requests", "burnban_fuse_request_limit",
	} {
		if !strings.Contains(metrics, name) {
			t.Errorf("metrics missing %s", name)
		}
	}
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if strings.ContainsAny(line, "\r\t\x1b") {
			t.Fatalf("metric line contains a raw control character: %q", line)
		}
		for i := 0; i < len(line); i++ {
			if line[i] != '\\' {
				continue
			}
			i++
			if i >= len(line) || !strings.ContainsRune(`\"n`, rune(line[i])) {
				t.Fatalf("metric line contains an unsupported label escape: %q", line)
			}
		}
	}
	modelSeries := map[string]bool{}
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, `burnban_ledger_model_cost_usd{model="same-prefix-`) {
			modelSeries[line] = true
		}
	}
	if len(modelSeries) != 2 {
		t.Fatalf("long distinct labels collapsed into duplicate Prometheus series: %+v", modelSeries)
	}
}

func TestWithAuth(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-burnban-token") != "" {
			t.Error("burnban token reached inner handler")
		}
		fmt.Fprintf(w, "%s|%s", r.Header.Get("Authorization"), r.URL.RawQuery)
	})
	srv := httptest.NewServer(web.WithAuth("sekret", inner))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public dashboard shell: status = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/x")
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

	resp, err = http.Get(srv.URL + "/api/x?token=sekret&provider=value")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query token: status = %d, want 401 (tokens must not travel in URLs)", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/openai/v1/models?token=sekret")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("provider query token status = %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/openai/v1/models", nil)
	req.Header.Set("Authorization", "Bearer provider-key")
	req.Header.Set("x-burnban-token", "sekret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Bearer provider-key") {
		t.Fatalf("provider authorization was consumed: %s", body)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/summary", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "sekret") {
		t.Fatalf("dashboard bearer auth status=%d body=%q", resp.StatusCode, body)
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
		TotalCost   float64 `json:"total_cost"`
		BanActive   bool    `json:"ban_active"`
		ExternalBan bool    `json:"external_ban"`
		Models      []any   `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.TotalCost != 0 || !d.BanActive || d.ExternalBan || d.Models == nil {
		t.Fatalf("summary = %+v", d)
	}
	if err := s.DeleteSetting(budget.KeyBanActive); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyExternalBanActive, "1"); err != nil {
		t.Fatal(err)
	}
	// Summary feeds coalesce synchronized dashboard tabs for one second.
	time.Sleep(1100 * time.Millisecond)
	resp2, err := http.Get(srv.URL + "/api/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if err := json.NewDecoder(resp2.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if !d.BanActive || !d.ExternalBan {
		t.Fatalf("external ban summary = %+v", d)
	}
}

func TestSummaryIncludesCappedAgentOutsideTopTwentyWithActualUsage(t *testing.T) {
	srv, s := newServer(t)
	now := time.Now()
	for i := 0; i < 25; i++ {
		if err := s.Insert(store.Request{
			Ts: now, Provider: "openai", Model: fmt.Sprintf("model-%02d", i), Agent: fmt.Sprintf("top-%02d", i),
			CostUSD: float64(100 - i), PricingState: store.PricingPriced,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, cost := range []float64{0.50, 0.75} {
		if err := s.Insert(store.Request{
			Ts: now, Provider: "openai", Model: "capped-model", Agent: "capped-low", CostUSD: cost,
			PricingState: store.PricingPriced,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetSetting(budget.KeyAgentCapPrefix+"capped-low", "1.50"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "/api/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data struct {
		TotalCost float64 `json:"total_cost"`
		Models    []struct {
			Cost    float64 `json:"cost_usd"`
			IsOther bool    `json:"is_other"`
		} `json:"models"`
		Agents []struct {
			Agent        string  `json:"agent"`
			Requests     int64   `json:"requests"`
			Cost         float64 `json:"cost_usd"`
			CapUSD       float64 `json:"cap_usd"`
			RemainingUSD float64 `json:"remaining_usd"`
			IsOther      bool    `json:"is_other"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	var modelCost, agentCost float64
	var modelOther, agentOther bool
	for _, model := range data.Models {
		modelCost += model.Cost
		modelOther = modelOther || model.IsOther
	}
	for _, agent := range data.Agents {
		agentCost += agent.Cost
		agentOther = agentOther || agent.IsOther
		if agent.Agent == "capped-low" {
			if agent.Requests != 2 || agent.Cost != 1.25 || agent.CapUSD != 1.50 || agent.RemainingUSD != 0.25 {
				t.Fatalf("capped agent usage = %+v", agent)
			}
		}
	}
	if math.Abs(modelCost-data.TotalCost) > 1e-8 || math.Abs(agentCost-data.TotalCost) > 1e-8 || !modelOther || !agentOther {
		t.Fatalf("category rows do not reconcile: total=%v models=%v other=%t agents=%v other=%t", data.TotalCost, modelCost, modelOther, agentCost, agentOther)
	}
	for _, agent := range data.Agents {
		if agent.Agent == "capped-low" {
			return
		}
	}
	t.Fatalf("capped agent outside top 20 missing from summary: %+v", data.Agents)
}

func TestSummaryIncludesAllBudgetsAndAgentCaps(t *testing.T) {
	srv, s := newServer(t)
	for key, value := range map[string]string{
		budget.KeyDailyCapUSD:              "10",
		budget.KeyWeeklyCapUSD:             "20",
		budget.KeyMonthlyCapUSD:            "30",
		budget.KeyAgentCapPrefix + "codex": "2.5",
	} {
		if err := s.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Get(srv.URL + "/api/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data struct {
		Budgets []struct {
			Name string `json:"name"`
		} `json:"budgets"`
		Agents []struct {
			Agent  string  `json:"agent"`
			CapUSD float64 `json:"cap_usd"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if len(data.Budgets) != 3 {
		t.Fatalf("budgets=%+v, want all three windows", data.Budgets)
	}
	if len(data.Agents) != 1 || data.Agents[0].Agent != "codex" || data.Agents[0].CapUSD != 2.5 {
		t.Fatalf("agents=%+v, want capped codex agent", data.Agents)
	}
}

func TestSummaryIncludesVelocityFuseAndTripState(t *testing.T) {
	srv, s := newServer(t)
	if err := s.SetSetting(budget.KeyFuseBurst, "5m:1"); err != nil {
		t.Fatal(err)
	}
	guard := &budget.Guard{S: s}
	if reservation, denial, err := guard.Admit(time.Now(), "", budget.AdmissionEstimate{
		USD: 1.1, Priced: true, Bounded: true,
	}); err != nil || reservation != nil || denial == nil || denial.Type != "burnban_fuse_tripped" {
		t.Fatalf("trip setup reservation=%v denial=%+v err=%v", reservation, denial, err)
	}
	resp, err := http.Get(srv.URL + "/api/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data struct {
		FuseTripped   bool    `json:"fuse_tripped"`
		FuseUntil     string  `json:"fuse_until"`
		FuseRule      string  `json:"fuse_rule"`
		FuseProjected float64 `json:"fuse_projected_usd"`
		FuseLimit     float64 `json:"fuse_limit_usd"`
		Budgets       []struct {
			Name      string  `json:"name"`
			Kind      string  `json:"kind"`
			CapUSD    float64 `json:"cap_usd"`
			Remaining float64 `json:"remaining_usd"`
		} `json:"budgets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if !data.FuseTripped || data.FuseUntil == "" || data.FuseRule != "burst" ||
		data.FuseProjected != 1.1 || data.FuseLimit != 1 || len(data.Budgets) != 1 ||
		data.Budgets[0].Name != "burst fuse" || data.Budgets[0].Kind != "fuse" ||
		data.Budgets[0].CapUSD != 1 || data.Budgets[0].Remaining != 1 {
		t.Fatalf("fuse summary=%+v", data)
	}
}

func TestSummaryKeepsFanoutRequestsSeparateFromDollarBudgets(t *testing.T) {
	srv, s := newServer(t)
	if err := s.SetSetting(budget.KeyFuseFanout, "1m:2"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Insert(store.Request{Ts: time.Now(), Provider: "openai", Status: 200, PricingState: store.PricingUnknown}); err != nil {
			t.Fatal(err)
		}
	}
	guard := &budget.Guard{S: s}
	if reservation, denial, err := guard.Admit(time.Now(), "", budget.AdmissionEstimate{}); err != nil || reservation != nil ||
		denial == nil || denial.Type != "burnban_fuse_tripped" {
		t.Fatalf("fanout trip reservation=%v denial=%+v err=%v", reservation, denial, err)
	}

	resp, err := http.Get(srv.URL + "/api/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data struct {
		FuseTripped           bool   `json:"fuse_tripped"`
		FuseRule              string `json:"fuse_rule"`
		FuseRequests          int64  `json:"fuse_requests"`
		FuseRequestLimit      int64  `json:"fuse_request_limit"`
		FuseRequestWindow     string `json:"fuse_request_window"`
		FuseProjectedRequests int64  `json:"fuse_projected_requests"`
		Budgets               []any  `json:"budgets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if !data.FuseTripped || data.FuseRule != "fanout" || data.FuseRequests != 2 ||
		data.FuseRequestLimit != 2 || data.FuseRequestWindow != "1m" || data.FuseProjectedRequests != 3 || len(data.Budgets) != 0 {
		t.Fatalf("fanout summary=%+v", data)
	}
}

func TestDemoSubscriptionNeverScansRealHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "private")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","requestId":"private","timestamp":%q,"message":{"id":"private","model":"claude-opus-4-8","usage":{"input_tokens":999999999,"output_tokens":999999999}}}`+"\n", time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "private.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	prices, err := pricing.Load()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	web.RegisterWithConfig(mux, s, web.Config{Version: "test", Prices: prices, Demo: true, Exposure: "localhost"})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/local-usage?window=today")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data struct {
		Calls     int64 `json:"calls"`
		Providers []struct {
			Provider string `json:"provider"`
			Dir      string `json:"dir"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data.Calls != 78 {
		t.Fatalf("demo calls=%d, want fixed fixture 78 (real log must not leak in)", data.Calls)
	}
	for _, provider := range data.Providers {
		if provider.Dir != "" {
			t.Fatalf("provider %s leaked source directory %q", provider.Provider, provider.Dir)
		}
	}
}

func TestDemoSubscriptionHeadlineMatchesTable(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	prices, err := pricing.Load()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	web.RegisterWithConfig(mux, s, web.Config{Version: "test", Prices: prices, Demo: true, Exposure: "localhost"})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// The demo fixtures model flat-rate plan usage, so the subscription
	// headline must carry the same dollars as the per-provider table; a
	// zero bucket renders a $0.00 stat above nonzero rows.
	for _, window := range []string{"today", "7d", "30d"} {
		resp, err := http.Get(srv.URL + "/api/local-usage?window=" + window)
		if err != nil {
			t.Fatal(err)
		}
		var data struct {
			SubscriptionUSD float64 `json:"subscription_usd"`
			MeteredUSD      float64 `json:"metered_usd"`
			APIUSD          float64 `json:"api_usd"`
			Providers       []struct {
				Provider        string  `json:"provider"`
				SubscriptionUSD float64 `json:"subscription_usd"`
				APIUSD          float64 `json:"api_usd"`
			} `json:"providers"`
		}
		err = json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if data.APIUSD <= 0 || data.SubscriptionUSD != data.APIUSD || data.MeteredUSD != 0 {
			t.Fatalf("window %s: subscription_usd=%v metered_usd=%v api_usd=%v, want subscription bucket to carry the whole total", window, data.SubscriptionUSD, data.MeteredUSD, data.APIUSD)
		}
		for _, provider := range data.Providers {
			if provider.SubscriptionUSD != provider.APIUSD {
				t.Fatalf("window %s: provider %s subscription_usd=%v api_usd=%v", window, provider.Provider, provider.SubscriptionUSD, provider.APIUSD)
			}
		}
	}
}

func TestTeamGatewayDisablesHostLocalUsageScanning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "private")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private.jsonl"), []byte("private operator log"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "team.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	prices, err := pricing.Load()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	web.RegisterWithConfig(mux, s, web.Config{
		Version: "test", Prices: prices, Exposure: "team/network", AuthRequired: true,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/local-usage?window=today")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || strings.Contains(string(body), "private") {
		t.Fatalf("team usage route status=%d body=%q", resp.StatusCode, body)
	}
	resp, err = http.Get(srv.URL + "/api/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var summary struct {
		LocalUsageEnabled bool `json:"local_usage_enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		t.Fatal(err)
	}
	if summary.LocalUsageEnabled {
		t.Fatal("team summary advertised host-local usage scanning")
	}
}

func TestLocalUsageAPIAutoDetectsLocalLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","requestId":"r","timestamp":%q,"message":{"id":"m","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":40,"cache_read_input_tokens":300}}}`+"\n", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, _ := newServer(t)
	resp, err := http.Get(srv.URL + "/api/local-usage?window=today")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data struct {
		Window    string `json:"window"`
		HasUsage  bool   `json:"has_usage"`
		Calls     int64  `json:"calls"`
		In        int64  `json:"in_tokens"`
		Out       int64  `json:"out_tokens"`
		Providers []struct {
			Provider string `json:"provider"`
			Detected bool   `json:"detected"`
			Sessions int    `json:"sessions"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || data.Window != "today" || !data.HasUsage || data.Calls != 1 || data.In != 100 || data.Out != 20 {
		t.Fatalf("status=%d data=%+v", resp.StatusCode, data)
	}
	if len(data.Providers) < 1 || data.Providers[0].Provider != "claude-code" || !data.Providers[0].Detected || data.Providers[0].Sessions != 1 {
		t.Fatalf("providers=%+v", data.Providers)
	}
}

func TestLocalUsageAPIHonorsConfiguredScanLimits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range []string{
		"APPDATA", "COPILOT_HOME", "GEMINI_CLI_HOME", "GOOSE_PATH_ROOT", "HERMES_HOME",
		"LOCALAPPDATA", "OPENCLAW_STATE_DIR", "OPENCODE_DB", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	} {
		t.Setenv(name, "")
	}
	dir := filepath.Join(home, ".claude", "projects", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","requestId":"r","timestamp":%q,"message":{"id":"m","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":20}}}`+"\n", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
	for i := 0; i < 2; i++ {
		body := strings.ReplaceAll(line, `"requestId":"r"`, fmt.Sprintf(`"requestId":"r%d"`, i))
		body = strings.ReplaceAll(body, `"id":"m"`, fmt.Sprintf(`"id":"m%d"`, i))
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("session-%d.jsonl", i)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	prices, err := pricing.Load()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	web.RegisterWithConfig(mux, s, web.Config{
		Version: "test", Prices: prices, Exposure: "localhost",
		LocalUsageScanLimits: localusage.ScanLimits{
			MaxFiles: 10, MaxBytes: int64(len(line)) + 16, MaxLineBytes: 1 << 20,
			MaxRecords: 10, MaxDuration: time.Second,
		},
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/local-usage?window=30d")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data struct {
		Calls     int64 `json:"calls"`
		Partial   bool  `json:"partial"`
		Providers []struct {
			Provider string   `json:"provider"`
			Partial  bool     `json:"partial"`
			Warnings []string `json:"warnings"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || data.Calls != 1 || !data.Partial {
		t.Fatalf("status=%d calls=%d partial=%t", resp.StatusCode, data.Calls, data.Partial)
	}
	claude := -1
	for i := range data.Providers {
		if data.Providers[i].Provider == "claude-code" {
			claude = i
			break
		}
	}
	if claude < 0 || !data.Providers[claude].Partial || !slices.Contains(data.Providers[claude].Warnings, "byte scan limit reached") {
		t.Fatalf("providers=%+v", data.Providers)
	}
}
