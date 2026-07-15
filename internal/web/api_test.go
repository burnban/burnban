package web_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
	"github.com/burnban/burnban/internal/web"
)

// newControlServer mounts the dashboard the way `burnban serve` does on a
// loopback listener: admin controls enabled.
func newControlServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
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
		Version: "test", Prices: prices, Exposure: "localhost", AllowAdmin: true,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func postJSON(t *testing.T, url string, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(payload)
}

func getJSON(t *testing.T, url string, dst any) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp
}

func seedRequests(t *testing.T, s *store.Store, count int) {
	t.Helper()
	now := time.Now()
	for i := 0; i < count; i++ {
		// Backdate every row: reconciliation and optimization windows are
		// exclusive at `through` with second precision, so a same-second row
		// would be racily excluded.
		if err := s.Insert(store.Request{
			Ts: now.Add(-time.Duration(i+1) * time.Minute), Provider: "anthropic",
			Model: fmt.Sprintf("model-%d", i%3), Agent: fmt.Sprintf("agent-%d", i%2),
			InTokens: 1000, OutTokens: 200, CostUSD: 0.05, LatencyMs: 40, Status: 200,
			Priced: true, PricingState: store.PricingPriced, UsageState: store.UsageExact,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReportAPIWindowedTotals(t *testing.T) {
	srv, s := newControlServer(t)
	seedRequests(t, s, 6)
	var d struct {
		Window    string  `json:"window"`
		TotalCost float64 `json:"total_cost"`
		Requests  int64   `json:"requests"`
		Models    []struct {
			Model string  `json:"model"`
			Cost  float64 `json:"cost_usd"`
		} `json:"models"`
		Agents []struct {
			Agent string `json:"agent"`
		} `json:"agents"`
	}
	if resp := getJSON(t, srv.URL+"/api/report?window=7d", &d); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if d.Requests != 6 || d.TotalCost < 0.29 || d.TotalCost > 0.31 || len(d.Models) != 3 || len(d.Agents) != 2 {
		t.Fatalf("report = %+v", d)
	}
	if d.Window != "last 7 days" {
		t.Fatalf("window label = %q", d.Window)
	}
	if resp := getJSON(t, srv.URL+"/api/report?window=99999d", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized window status = %d", resp.StatusCode)
	}
}

func TestRequestsAPINewestFirstWithoutFingerprint(t *testing.T) {
	srv, s := newControlServer(t)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		if err := s.Insert(store.Request{
			Ts: base.Add(time.Duration(i) * time.Minute), Provider: "openai",
			Model: fmt.Sprintf("m-%d", i), Status: 200, BodyHash: "secret-fingerprint",
			PricingState: store.PricingPriced, UsageState: store.UsageExact, Priced: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Get(srv.URL + "/api/requests?limit=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "secret-fingerprint") {
		t.Fatal("request fingerprint leaked into activity feed")
	}
	var d struct {
		Requests []struct {
			Model string `json:"model"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Requests) != 2 || d.Requests[0].Model != "m-2" || d.Requests[1].Model != "m-1" {
		t.Fatalf("recent requests = %+v", d.Requests)
	}
	if resp := getJSON(t, srv.URL+"/api/requests?limit=100000", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized limit status = %d", resp.StatusCode)
	}
}

func TestGuardrailsAPIReflectsConfiguredState(t *testing.T) {
	srv, s := newControlServer(t)
	for key, value := range map[string]string{
		budget.KeyDailyCapUSD:                    "10",
		budget.KeyFuseBurst:                      "5m:4",
		budget.KeyWebhookURL:                     "https://hooks.example.com/T000/B000/secretpart",
		budget.KeyAgentCapPrefix + "night-shift": "5",
	} {
		if err := s.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	var d struct {
		AllowAdmin bool `json:"allow_admin"`
		Windows    []struct {
			Name   string  `json:"name"`
			Set    bool    `json:"set"`
			CapUSD float64 `json:"cap_usd"`
		} `json:"windows"`
		AgentCaps []struct {
			Agent  string  `json:"agent"`
			CapUSD float64 `json:"cap_usd"`
		} `json:"agent_caps"`
		Webhook string  `json:"webhook"`
		WarnPct float64 `json:"warn_pct"`
		Fuse    struct {
			Rules []struct {
				Name   string  `json:"name"`
				Window string  `json:"window"`
				CapUSD float64 `json:"cap_usd"`
			} `json:"rules"`
			Cooldown string `json:"cooldown"`
		} `json:"fuse"`
	}
	if resp := getJSON(t, srv.URL+"/api/guardrails", &d); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !d.AllowAdmin || len(d.Windows) != 3 || !d.Windows[0].Set || d.Windows[0].CapUSD != 10 {
		t.Fatalf("guardrails windows = %+v", d)
	}
	if len(d.AgentCaps) != 1 || d.AgentCaps[0].Agent != "night-shift" || d.AgentCaps[0].CapUSD != 5 {
		t.Fatalf("agent caps = %+v", d.AgentCaps)
	}
	if strings.Contains(d.Webhook, "secretpart") || !strings.Contains(d.Webhook, "hooks.example.com") {
		t.Fatalf("webhook was not redacted: %q", d.Webhook)
	}
	if d.WarnPct != budget.DefaultWarnPct {
		t.Fatalf("warn pct = %v", d.WarnPct)
	}
	if len(d.Fuse.Rules) != 1 || d.Fuse.Rules[0].Name != "burst" || d.Fuse.Rules[0].Window != "5m" || d.Fuse.Rules[0].CapUSD != 4 {
		t.Fatalf("fuse rules = %+v", d.Fuse.Rules)
	}
	if d.Fuse.Cooldown != "15m" {
		t.Fatalf("cooldown = %q", d.Fuse.Cooldown)
	}
}

func TestAdminEndpointsDisabledOnReadOnlyDashboard(t *testing.T) {
	srv, _ := newServer(t) // web.Register keeps AllowAdmin false
	for _, path := range []string{"cap", "warn", "fuse", "ban", "lift", "webhook"} {
		resp, body := postJSON(t, srv.URL+"/api/admin/"+path, `{}`)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s status = %d body=%s", path, resp.StatusCode, body)
		}
	}
}

func TestAdminCapLifecycleMirrorsCLISemantics(t *testing.T) {
	srv, s := newControlServer(t)
	// A live today-override must be cleared when a cap is set, so the fresh
	// cap actually enforces.
	if err := s.SetSetting(budget.KeyOverrideDay, time.Now().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	resp, body := postJSON(t, srv.URL+"/api/admin/cap", `{"window":"daily","cap_usd":12.5}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "cap override cleared") {
		t.Fatalf("cap set status=%d body=%s", resp.StatusCode, body)
	}
	if v, _ := s.GetSetting(budget.KeyDailyCapUSD); v != "12.5" {
		t.Fatalf("stored daily cap = %q", v)
	}
	if v, _ := s.GetSetting(budget.KeyOverrideDay); v != "" {
		t.Fatalf("override survived cap set: %q", v)
	}
	var ok struct {
		Guardrails struct {
			Windows []struct {
				Name   string  `json:"name"`
				CapUSD float64 `json:"cap_usd"`
				Set    bool    `json:"set"`
			} `json:"windows"`
		} `json:"guardrails"`
	}
	if err := json.Unmarshal([]byte(body), &ok); err != nil || !ok.Guardrails.Windows[0].Set {
		t.Fatalf("admin response missing fresh guardrails: %v %s", err, body)
	}

	// Sub-cent caps are rejected exactly like the CLI.
	if resp, body := postJSON(t, srv.URL+"/api/admin/cap", `{"window":"daily","cap_usd":0.005}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("sub-cent cap status=%d body=%s", resp.StatusCode, body)
	}
	// Removal clears the setting.
	if resp, _ := postJSON(t, srv.URL+"/api/admin/cap", `{"window":"daily","cap_usd":0}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("cap clear status=%d", resp.StatusCode)
	}
	if v, _ := s.GetSetting(budget.KeyDailyCapUSD); v != "" {
		t.Fatalf("daily cap not removed: %q", v)
	}
	// Agent caps: set then remove.
	if resp, _ := postJSON(t, srv.URL+"/api/admin/cap", `{"agent":"night-shift","cap_usd":5}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("agent cap status=%d", resp.StatusCode)
	}
	if v, _ := s.GetSetting(budget.KeyAgentCapPrefix + "night-shift"); v != "5" {
		t.Fatalf("agent cap = %q", v)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/admin/cap", `{"agent":"night-shift","cap_usd":0}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("agent cap clear status=%d", resp.StatusCode)
	}
	if v, _ := s.GetSetting(budget.KeyAgentCapPrefix + "night-shift"); v != "" {
		t.Fatalf("agent cap not removed: %q", v)
	}
	// Weekly/monthly caps are refused for agents, like the CLI.
	if resp, _ := postJSON(t, srv.URL+"/api/admin/cap", `{"agent":"x","window":"weekly","cap_usd":5}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("agent weekly cap status=%d", resp.StatusCode)
	}
	// window=all removes every global cap.
	for _, w := range budget.Windows() {
		if err := s.SetSetting(w.Key, "9"); err != nil {
			t.Fatal(err)
		}
	}
	if resp, _ := postJSON(t, srv.URL+"/api/admin/cap", `{"window":"all","cap_usd":0}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("all-off status=%d", resp.StatusCode)
	}
	for _, w := range budget.Windows() {
		if v, _ := s.GetSetting(w.Key); v != "" {
			t.Fatalf("cap %s survived all-off: %q", w.Name, v)
		}
	}
}

func TestAdminBanAndLiftFlow(t *testing.T) {
	srv, s := newControlServer(t)
	if resp, _ := postJSON(t, srv.URL+"/api/admin/ban", `{}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("ban status=%d", resp.StatusCode)
	}
	if v, _ := s.GetSetting(budget.KeyBanActive); v != "1" {
		t.Fatalf("ban not stored: %q", v)
	}
	resp, body := postJSON(t, srv.URL+"/api/admin/lift", `{"today":true}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "overridden for the rest of today") {
		t.Fatalf("lift status=%d body=%s", resp.StatusCode, body)
	}
	if v, _ := s.GetSetting(budget.KeyBanActive); v != "" {
		t.Fatalf("ban survived lift: %q", v)
	}
	if v, _ := s.GetSetting(budget.KeyOverrideDay); v != time.Now().Format("2006-01-02") {
		t.Fatalf("override day = %q", v)
	}
}

func TestAdminFuseSetResetAndOff(t *testing.T) {
	srv, s := newControlServer(t)
	resp, body := postJSON(t, srv.URL+"/api/admin/fuse",
		`{"action":"set","hourly_usd":20,"burst":"5m:4","fanout":"1m:120","baseline":{"multiplier":3},"cooldown":"30m"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fuse set status=%d body=%s", resp.StatusCode, body)
	}
	for key, want := range map[string]string{
		budget.KeyFuseHourlyUSD: "20",
		budget.KeyFuseBurst:     "5m:4",
		budget.KeyFuseFanout:    "1m:120",
		budget.KeyFuseCooldown:  "30m",
	} {
		if v, _ := s.GetSetting(key); v != want {
			t.Fatalf("setting %s = %q, want %q", key, v, want)
		}
	}
	if v, _ := s.GetSetting(budget.KeyFuseBaseline); !strings.Contains(v, `"multiplier":3`) {
		t.Fatalf("baseline setting = %q", v)
	}
	// Simulate a live trip, then reset it.
	trip := fmt.Sprintf(`{"started_at":%q,"until":%q,"rule":"burst","window":"5m","limit_usd":4,"projected_usd":5}`,
		time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Add(10*time.Minute).Format(time.RFC3339))
	if err := s.SetSetting(budget.KeyFuseTrip, trip); err != nil {
		t.Fatal(err)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/admin/fuse", `{"action":"reset"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("fuse reset status=%d", resp.StatusCode)
	}
	if v, _ := s.GetSetting(budget.KeyFuseTrip); v != "" {
		t.Fatalf("trip survived reset: %q", v)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/admin/fuse", `{"action":"off"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("fuse off status=%d", resp.StatusCode)
	}
	for _, key := range []string{budget.KeyFuseHourlyUSD, budget.KeyFuseBurst, budget.KeyFuseFanout, budget.KeyFuseBaseline, budget.KeyFuseCooldown} {
		if v, _ := s.GetSetting(key); v != "" {
			t.Fatalf("fuse setting %s survived off: %q", key, v)
		}
	}
	// Invalid burst grammar is a 400, not a 500.
	if resp, _ := postJSON(t, srv.URL+"/api/admin/fuse", `{"action":"set","burst":"nonsense"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad burst status=%d", resp.StatusCode)
	}
}

func TestAdminWebhookSetAndRemove(t *testing.T) {
	srv, s := newControlServer(t)
	if resp, _ := postJSON(t, srv.URL+"/api/admin/webhook", `{"url":"ftp://bad"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad webhook scheme status=%d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/admin/webhook", `{"url":"https://hooks.example.com/x"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook set status=%d", resp.StatusCode)
	}
	if v, _ := s.GetSetting(budget.KeyWebhookURL); v != "https://hooks.example.com/x" {
		t.Fatalf("webhook = %q", v)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/admin/webhook", `{"off":true}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook off status=%d", resp.StatusCode)
	}
	if v, _ := s.GetSetting(budget.KeyWebhookURL); v != "" {
		t.Fatalf("webhook survived off: %q", v)
	}
}

func TestAdminRejectsNonJSONAndUnknownFields(t *testing.T) {
	srv, _ := newControlServer(t)
	resp, err := http.Post(srv.URL+"/api/admin/ban", "text/plain", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-JSON content type status=%d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/admin/cap", `{"window":"daily","cap_usd":5,"surprise":true}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d", resp.StatusCode)
	}
}

func TestWhatifAPIRepricesWindow(t *testing.T) {
	srv, s := newControlServer(t)
	seedRequests(t, s, 4)
	var d struct {
		ActualCost float64 `json:"actual_cost_usd"`
		Priced     int64   `json:"priced_requests"`
		Rows       []struct {
			Model string  `json:"model"`
			Cost  float64 `json:"cost_usd"`
		} `json:"rows"`
	}
	if resp := getJSON(t, srv.URL+"/api/whatif?window=7d", &d); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if d.Priced != 4 || len(d.Rows) == 0 {
		t.Fatalf("whatif = %+v", d)
	}
	for i := 1; i < len(d.Rows); i++ {
		if d.Rows[i].Cost < d.Rows[i-1].Cost {
			t.Fatalf("rows not sorted cheapest-first at %d: %+v", i, d.Rows[i-1:i+1])
		}
	}
	if resp := getJSON(t, srv.URL+"/api/whatif?window=7d&model=not-a-real-model", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown model status = %d", resp.StatusCode)
	}
}

func TestExportAPIDownloadsCSVAndJSON(t *testing.T) {
	srv, s := newControlServer(t)
	seedRequests(t, s, 2)
	resp, err := http.Get(srv.URL + "/api/export?window=7d&format=csv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("csv export status=%d disposition=%q", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}
	records, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[0][0] != "ts" {
		t.Fatalf("csv rows = %d", len(records))
	}
	respJSON, err := http.Get(srv.URL + "/api/export?window=7d&format=json")
	if err != nil {
		t.Fatal(err)
	}
	defer respJSON.Body.Close()
	raw, _ := io.ReadAll(respJSON.Body)
	if !json.Valid(bytes.TrimSpace(raw)) {
		t.Fatalf("json export invalid: %s", raw[:min(len(raw), 200)])
	}
}

func TestPricingDiagnosticsPolicyDownshiftReconcileEmptyStates(t *testing.T) {
	srv, s := newControlServer(t)
	seedRequests(t, s, 1)

	var pricingResp struct {
		Models []struct {
			Model string `json:"model"`
		} `json:"models"`
		Diagnostics struct {
			ModelCount int `json:"model_count"`
		} `json:"diagnostics"`
	}
	if resp := getJSON(t, srv.URL+"/api/pricing", &pricingResp); resp.StatusCode != http.StatusOK {
		t.Fatalf("pricing status = %d", resp.StatusCode)
	}
	if len(pricingResp.Models) == 0 || pricingResp.Diagnostics.ModelCount != len(pricingResp.Models) {
		t.Fatalf("pricing models = %d, count = %d", len(pricingResp.Models), pricingResp.Diagnostics.ModelCount)
	}
	if resp := getJSON(t, srv.URL+"/api/pricing?model=definitely-not-priced", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown pricing model status = %d", resp.StatusCode)
	}

	var diag struct {
		DBOk   bool `json:"db_ok"`
		Ledger struct {
			Requests int64 `json:"requests"`
		} `json:"ledger"`
		Pricing struct {
			Version string `json:"version"`
		} `json:"pricing"`
		AdminEnabled bool `json:"admin_enabled"`
	}
	if resp := getJSON(t, srv.URL+"/api/diagnostics", &diag); resp.StatusCode != http.StatusOK {
		t.Fatalf("diagnostics status = %d", resp.StatusCode)
	}
	if !diag.DBOk || diag.Ledger.Requests != 1 || diag.Pricing.Version == "" || !diag.AdminEnabled {
		t.Fatalf("diagnostics = %+v", diag)
	}

	var policy struct {
		Active bool  `json:"active"`
		Events []any `json:"events"`
	}
	if resp := getJSON(t, srv.URL+"/api/policy", &policy); resp.StatusCode != http.StatusOK {
		t.Fatalf("policy status = %d", resp.StatusCode)
	}
	if policy.Active {
		t.Fatalf("policy should be inactive: %+v", policy)
	}

	var downshift struct {
		Active bool `json:"active"`
	}
	if resp := getJSON(t, srv.URL+"/api/downshift", &downshift); resp.StatusCode != http.StatusOK {
		t.Fatalf("downshift status = %d", resp.StatusCode)
	}
	if downshift.Active {
		t.Fatalf("downshift should be inactive")
	}

	var reconcile struct {
		Report struct {
			LedgerEstimateMicros int64 `json:"ledger_estimate_micros"`
		} `json:"report"`
	}
	if resp := getJSON(t, srv.URL+"/api/reconcile?window=30d", &reconcile); resp.StatusCode != http.StatusOK {
		t.Fatalf("reconcile status = %d", resp.StatusCode)
	}
	if reconcile.Report.LedgerEstimateMicros <= 0 {
		t.Fatalf("reconcile estimate = %d", reconcile.Report.LedgerEstimateMicros)
	}

	var optimizeResp struct {
		Cache struct {
			SampledRows int `json:"sampled_rows"`
		} `json:"cache"`
		Allocation struct {
			Dimension string `json:"dimension"`
		} `json:"allocation"`
	}
	if resp := getJSON(t, srv.URL+"/api/optimize?window=30d", &optimizeResp); resp.StatusCode != http.StatusOK {
		t.Fatalf("optimize status = %d", resp.StatusCode)
	}
	if optimizeResp.Cache.SampledRows != 1 || optimizeResp.Allocation.Dimension != "agent" {
		t.Fatalf("optimize = %+v", optimizeResp)
	}
}

func TestDashboardShipsControlRoom(t *testing.T) {
	srv, _ := newControlServer(t)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	for _, want := range []string{
		`role="tablist"`, `data-tab="controls"`, `data-tab="report"`, `data-tab="activity"`,
		`data-tab="optimize"`, `data-tab="system"`, "/api/guardrails", "/api/admin/cap",
		"/api/admin/fuse", "/api/admin/ban", "/api/admin/lift", "/api/admin/webhook",
		"/api/report", "/api/requests", "/api/whatif", "/api/export", "/api/pricing",
		"/api/policy", "/api/reconcile", "/api/downshift", "Emergency stop",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}
