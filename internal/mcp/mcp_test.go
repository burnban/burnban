package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/approvalclient"
	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/mcp"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

type fakeApprovalRequester struct {
	request approvalclient.Request
}

func (f *fakeApprovalRequester) Request(_ context.Context, in approvalclient.Request) (approvalclient.Response, error) {
	f.request = in
	return approvalclient.Response{
		ID: "apr_mcp", ScopeType: "meter", ScopeValue: "mtr_ci", Window: in.Window,
		IncreaseUSD: in.IncreaseUSD, Status: "pending", ValidUntil: "2026-07-12T20:00:00Z",
	}, nil
}

func TestBudgetExceptionToolCanRequestButNotApprove(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	requester := &fakeApprovalRequester{}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"request_budget_exception","arguments":{"window":"daily","increase_usd":7.5,"reason":"finish bounded task","ticket":"OPS-7","expires_minutes":30}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	srv := &mcp.Server{S: s, Prices: testPrices(), Version: "test", In: strings.NewReader(in), Out: &out,
		AllowBudgetRequests: true, ApprovalRequester: requester}
	if err := srv.Run(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "request_budget_exception") || strings.Contains(lines[0], "burn_ban") {
		t.Fatalf("tool list=%s", lines[0])
	}
	if !strings.Contains(lines[1], `\"status\": \"pending\"`) || !strings.Contains(lines[1], `\"human_authorization_required\": true`) {
		t.Fatalf("receipt=%s", lines[1])
	}
	if requester.request.Window != "daily" || requester.request.IncreaseUSD != 7.5 || requester.request.ExpiresIn != 30*time.Minute {
		t.Fatalf("request=%+v", requester.request)
	}
}

func TestServerSession(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"set_daily_cap","arguments":{"usd":5}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"burn_status","arguments":{}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	srv := &mcp.Server{S: s, Prices: testPrices(), Version: "test", In: strings.NewReader(in), Out: &out, AllowBudgetAdmin: true}
	if err := srv.Run(); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d responses, want 4 (notification must not be answered):\n%s", len(lines), out.String())
	}

	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &init); err != nil {
		t.Fatal(err)
	}
	if init.Result.ProtocolVersion == "" || init.Result.ServerInfo.Name != "burnban" {
		t.Fatalf("bad initialize response: %s", lines[0])
	}

	var tl struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &tl); err != nil {
		t.Fatal(err)
	}
	if len(tl.Result.Tools) != 7 {
		t.Fatalf("tools/list returned %d tools, want 7", len(tl.Result.Tools))
	}

	// Caps store at full precision (no 'f',2 truncation that turned
	// sub-cent caps into "0.00" deny-alls).
	if v, _ := s.GetSetting(budget.KeyDailyCapUSD); v != "5" {
		t.Fatalf("cap setting = %q, want 5", v)
	}
	if !strings.Contains(lines[3], "cap_daily_usd") {
		t.Fatalf("burn_status missing cap: %s", lines[3])
	}
}

func testPrices() *pricing.Table {
	return &pricing.Table{Metadata: pricing.Metadata{
		Version: "test", EffectiveDate: "2026-07-01", VerifiedDate: "2026-07-01",
		Sources: []pricing.Source{{Provider: "test", URL: "https://example.test/pricing", VerifiedDate: "2026-07-01"}},
	}, Models: map[string]pricing.Price{"test": {InputPerMTok: 1, OutputPerMTok: 2}}}
}

func TestDefaultServerIsReadOnly(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"set_daily_cap","arguments":{"usd":5}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	srv := &mcp.Server{S: s, Prices: testPrices(), Version: "test", In: strings.NewReader(in), Out: &out}
	if err := srv.Run(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "--allow-budget-admin") {
		t.Fatalf("read-only response = %s", out.String())
	}
	if strings.Contains(lines[0], "set_daily_cap") || strings.Contains(lines[0], "burn_ban") {
		t.Fatalf("read-only tools advertised mutation: %s", lines[0])
	}
	if value, _ := s.GetSetting(budget.KeyDailyCapUSD); value != "" {
		t.Fatalf("read-only server mutated cap to %q", value)
	}
}

func TestStrictArgumentsAndAgentRemaining(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetSetting(budget.KeyAgentCapPrefix+"codex", "5"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyFuseBurst, "5m:4"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyFuseFanout, "1m:10"); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(store.Request{Ts: time.Now(), Agent: "codex", CostUSD: 1.25, Priced: true}); err != nil {
		t.Fatal(err)
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set_daily_cap","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"burn_status","arguments":{"agent":"codex","typo":true}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"burn_status","arguments":{"agent":"codex"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"set_daily_cap","arguments":{"usd":5,"usd":0}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	srv := &mcp.Server{S: s, Prices: testPrices(), Version: "test", In: strings.NewReader(in), Out: &out, AllowBudgetAdmin: true}
	if err := srv.Run(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.Contains(lines[0], "usd is required") {
		t.Fatalf("missing required usd accepted: %s", lines[0])
	}
	if !strings.Contains(lines[1], "unknown field") {
		t.Fatalf("unknown argument accepted: %s", lines[1])
	}
	if !strings.Contains(lines[2], `\"remaining_usd\": 3.75`) {
		t.Fatalf("agent remaining missing: %s", lines[2])
	}
	if !strings.Contains(lines[2], `\"velocity_fuse\"`) || !strings.Contains(lines[2], `\"window\": \"5m\"`) {
		t.Fatalf("velocity fuse missing: %s", lines[2])
	}
	if !strings.Contains(lines[2], `\"fanout\"`) || !strings.Contains(lines[2], `\"limit_requests\": 10`) {
		t.Fatalf("fanout fuse missing: %s", lines[2])
	}
	if !strings.Contains(lines[3], "duplicate JSON field") {
		t.Fatalf("duplicate mutation argument accepted: %s", lines[3])
	}
}

func TestAgentSelectorsAreBoundedBeforeSettingsLookupOrMutation(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"burn_status","arguments":{"agent":"bad\u202ename"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"set_daily_cap","arguments":{"usd":5,"agent":"` + strings.Repeat("a", 129) + `"}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	srv := &mcp.Server{S: s, Prices: testPrices(), Version: "test", In: strings.NewReader(in), Out: &out, AllowBudgetAdmin: true}
	if err := srv.Run(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "unsafe character") || !strings.Contains(lines[1], "128 characters") {
		t.Fatalf("unsafe agent selectors were not rejected: %s", out.String())
	}
	settings, err := s.GetSettings(budget.KeyAgentCapPrefix+strings.Repeat("a", 129), budget.KeyDailyCapUSD)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != 0 {
		t.Fatalf("rejected agent selector mutated settings: %+v", settings)
	}
}

func TestUnknownToolIsInBandError(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}` + "\n"
	var out bytes.Buffer
	srv := &mcp.Server{S: s, Version: "test", In: strings.NewReader(in), Out: &out}
	if err := srv.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"isError":true`) {
		t.Fatalf("expected in-band tool error, got: %s", out.String())
	}
}

func TestPricingDiagnosticsTool(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pricing_diagnostics","arguments":{}}}` + "\n"
	var out bytes.Buffer
	srv := &mcp.Server{S: s, Prices: testPrices(), Version: "test", In: strings.NewReader(in), Out: &out}
	if err := srv.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `\"version\": \"test\"`) || !strings.Contains(out.String(), "https://example.test/pricing") {
		t.Fatalf("pricing diagnostics missing provenance: %s", out.String())
	}
}
