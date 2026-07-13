package proxy_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/downshift"
	"github.com/burnban/burnban/internal/identity"
	"github.com/burnban/burnban/internal/policy"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/proxy"
	"github.com/burnban/burnban/internal/store"
)

func downshiftConfig(mode downshift.Mode, scoped bool) downshift.Config {
	scope := downshift.Scope{}
	if scoped {
		scope.Project = "trusted-project"
	}
	return downshift.Config{
		APIVersion: downshift.APIVersion, Revision: 1, Mode: mode,
		WarnAtPct: 70, DownshiftAtPct: 80, DownshiftOnDenial: true,
		Rules: []downshift.Rule{{
			ID: "safe-coding", Source: downshift.Endpoint{Route: "openai", Model: "source-model", Family: "coding", Dialect: "openai", ContextTokens: 200000},
			Target: downshift.Endpoint{Route: "vllm", Model: "target-model", Family: "coding", Dialect: "openai", ContextTokens: 128000},
			Scope:  scope, Capabilities: downshift.Capabilities{Tools: true, StructuredOutput: true, Modalities: []string{"text"}},
		}},
	}
}

func activateDownshift(t *testing.T, s *store.Store, config downshift.Config) *downshift.Compiled {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := downshift.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDownshiftDocument(store.DownshiftDocumentRecord{
		APIVersion: compiled.Config.APIVersion, Revision: compiled.Config.Revision,
		Digest: compiled.Digest, Mode: string(compiled.Config.Mode), DocumentJSON: string(compiled.Canonical),
		Forced: true, ForceReason: "test mapping was explicitly reviewed without historical production receipts",
	}); err != nil {
		t.Fatal(err)
	}
	return compiled
}

type downshiftFixture struct {
	server *httptest.Server
	store  *store.Store
	source *atomic.Int64
	target *atomic.Int64
}

func newDownshiftFixture(t *testing.T, targetHandler http.Handler, config downshift.Config) downshiftFixture {
	t.Helper()
	var sourceHits, targetHits atomic.Int64
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"source-model","usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	t.Cleanup(source.Close)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		targetHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(target.Close)
	s, err := store.Open(filepath.Join(t.TempDir(), "downshift.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	activateDownshift(t, s, config)
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"source-model": {InputPerMTok: 100, OutputPerMTok: 100},
		"target-model": {InputPerMTok: 1, OutputPerMTok: 1},
	}}
	p, err := proxy.New(s, prices, map[string]proxy.Upstream{
		"openai": {URL: source.URL, Shape: "openai"},
		"vllm":   {URL: target.URL, Shape: "openai"},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p.Handler())
	t.Cleanup(server.Close)
	return downshiftFixture{server: server, store: s, source: &sourceHits, target: &targetHits}
}

func postDownshift(t *testing.T, base, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/openai/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(responseBody)
}

func TestDownshiftAtBudgetThresholdUsesTargetPriceAndImmutableReceipt(t *testing.T) {
	var receivedModel, receivedAuth, receivedSpoof string
	fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedSpoof = r.Header.Get("X-Burnban-Downshift-Reason")
		var request struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		receivedModel = request.Model
		// An upstream cannot forge Burnban's routing explanation.
		w.Header().Set("X-Burnban-Downshift-Reason", "upstream-secret-url=https://user:password@example")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"target-model","usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
	if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Insert(store.Request{Ts: time.Now(), Provider: "seed", CostUSD: .85, PricingState: store.PricingPriced}); err != nil {
		t.Fatal(err)
	}
	resp, body := postDownshift(t, fixture.server.URL,
		`{"model":"source-model","max_tokens":100,"messages":[{"role":"user","content":"private prompt"}]}`,
		map[string]string{"X-Burnban-Downshift-Reason": "client-spoof"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "target-model") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if fixture.source.Load() != 0 || fixture.target.Load() != 1 || receivedModel != "target-model" || receivedAuth != "" || receivedSpoof != "" {
		t.Fatalf("source=%d target=%d model=%q auth=%q spoof=%q", fixture.source.Load(), fixture.target.Load(), receivedModel, receivedAuth, receivedSpoof)
	}
	if resp.Header.Get("X-Burnban-Downshift-Action") != "downshift" ||
		resp.Header.Get("X-Burnban-Requested-Route") != "openai" || resp.Header.Get("X-Burnban-Chosen-Route") != "vllm" ||
		resp.Header.Get("X-Burnban-Chosen-Model") != "target-model" || len(resp.Header.Values("X-Burnban-Downshift-Reason")) != 1 ||
		strings.Contains(resp.Header.Get("X-Burnban-Downshift-Reason"), "password") {
		t.Fatalf("routing headers=%v", resp.Header)
	}
	rows, err := fixture.store.Export(time.Unix(0, 0))
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	row := rows[1]
	if row.Provider != "vllm" || row.Model != "target-model" || row.RequestedProvider != "openai" ||
		row.RequestedModel != "source-model" || row.DownshiftAction != "downshift" || row.DownshiftRule != "safe-coding" ||
		row.DownshiftReason == "" || row.DownshiftDigest == "" || row.DownshiftTargetUSD <= 0 ||
		row.DownshiftTargetUSD >= row.DownshiftSourceUSD || strings.Contains(row.DownshiftFeatures, "private prompt") {
		t.Fatalf("downshift receipt=%+v", row)
	}
}

func TestCrossRouteDownshiftNeverForwardsSourceCredentialsOrQuery(t *testing.T) {
	fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Authorization", "X-Api-Key", "Cookie"} {
			if r.Header.Get(name) != "" {
				t.Errorf("source credential %s reached target", name)
			}
		}
		if r.URL.RawQuery != "" {
			t.Errorf("source query reached target: %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"target-model","usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
	if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Insert(store.Request{Ts: time.Now(), Provider: "seed", CostUSD: .85, PricingState: store.PricingPriced}); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		path      string
		headers   map[string]string
		downshift bool
	}{
		"authorization":      {headers: map[string]string{"Authorization": "Bearer source-secret"}, downshift: true},
		"api key":            {headers: map[string]string{"X-Api-Key": "source-secret"}, downshift: true},
		"cookie":             {headers: map[string]string{"Cookie": "session=source-secret"}, downshift: true},
		"source query":       {path: "?key=source-secret"},
		"unproven beta gate": {headers: map[string]string{"OpenAI-Beta": "future-semantics"}},
		"arbitrary metadata": {headers: map[string]string{"X-Custom-Tenant-Secret": "source-secret"}},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost,
				fixture.server.URL+"/openai/v1/chat/completions"+test.path,
				strings.NewReader(`{"model":"source-model","max_tokens":10}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			for key, value := range test.headers {
				req.Header.Set(key, value)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			wantAction := "none"
			if test.downshift {
				wantAction = "downshift"
			}
			if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Burnban-Downshift-Action") != wantAction {
				t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
			}
		})
	}
	if fixture.target.Load() != 3 || fixture.source.Load() != 3 {
		t.Fatalf("source=%d target=%d", fixture.source.Load(), fixture.target.Load())
	}
}

func TestDenialTriggeredCrossRouteDiscardsSourceCredential(t *testing.T) {
	fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("source credential reached denial-triggered target")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"target-model","usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
	if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "0.001"); err != nil {
		t.Fatal(err)
	}
	resp, _ := postDownshift(t, fixture.server.URL,
		`{"model":"source-model","max_tokens":10}`,
		map[string]string{"Authorization": "Bearer source-secret"})
	if resp.StatusCode != http.StatusOK || fixture.source.Load() != 0 || fixture.target.Load() != 1 ||
		resp.Header.Get("X-Burnban-Downshift-Action") != "downshift" {
		t.Fatalf("status=%d source=%d target=%d headers=%v", resp.StatusCode, fixture.source.Load(), fixture.target.Load(), resp.Header)
	}
}

func TestDownshiftPolicyEvaluatesSourceAndActualTargetWithoutDoubleCounting(t *testing.T) {
	t.Run("source deny is authoritative", func(t *testing.T) {
		fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("source-denied request reached target")
		}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
		applyPolicy(t, fixture.store, policy.Document{
			APIVersion: policy.APIVersion, Kind: policy.Kind,
			Metadata: policy.Metadata{Name: "routing", Namespace: "source-deny", Revision: 1}, Mode: policy.ModeEnforce,
			Rules: []policy.Rule{{ID: "deny-source", Match: policy.Match{Provider: policy.AccessList{Deny: []string{"openai"}}}}},
		})
		if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
			t.Fatal(err)
		}
		if err := fixture.store.Insert(store.Request{Ts: time.Now(), CostUSD: .85, PricingState: store.PricingPriced}); err != nil {
			t.Fatal(err)
		}
		resp, _ := postDownshift(t, fixture.server.URL, `{"model":"source-model","max_tokens":10}`, nil)
		if resp.StatusCode != http.StatusForbidden || fixture.source.Load() != 0 || fixture.target.Load() != 0 {
			t.Fatalf("status=%d source=%d target=%d", resp.StatusCode, fixture.source.Load(), fixture.target.Load())
		}
	})

	t.Run("target deny blocks before outbound", func(t *testing.T) {
		fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("target-policy-denied request reached target")
		}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
		applyPolicy(t, fixture.store, policy.Document{
			APIVersion: policy.APIVersion, Kind: policy.Kind,
			Metadata: policy.Metadata{Name: "routing", Namespace: "target-deny", Revision: 1}, Mode: policy.ModeEnforce,
			Rules: []policy.Rule{{ID: "deny-target", Match: policy.Match{Provider: policy.AccessList{Deny: []string{"vllm"}}}}},
		})
		if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
			t.Fatal(err)
		}
		if err := fixture.store.Insert(store.Request{Ts: time.Now(), CostUSD: .85, PricingState: store.PricingPriced}); err != nil {
			t.Fatal(err)
		}
		resp, _ := postDownshift(t, fixture.server.URL, `{"model":"source-model","max_tokens":10}`, nil)
		if resp.StatusCode != http.StatusForbidden || fixture.source.Load() != 0 || fixture.target.Load() != 0 ||
			resp.Header.Get("X-Burnban-Downshift-Action") != "none" {
			t.Fatalf("status=%d source=%d target=%d headers=%v", resp.StatusCode, fixture.source.Load(), fixture.target.Load(), resp.Header)
		}
		summary, err := fixture.store.PolicyDecisionsSince(time.Unix(0, 0))
		if err != nil || summary.Total != 2 || summary.Denied != 1 || summary.Admitted != 0 {
			t.Fatalf("policy summary=%+v err=%v", summary, err)
		}
	})

	t.Run("only target counters are committed", func(t *testing.T) {
		fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"model":"target-model","usage":{"prompt_tokens":5,"completion_tokens":1}}`)
		}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
		applyPolicy(t, fixture.store, policy.Document{
			APIVersion: policy.APIVersion, Kind: policy.Kind,
			Metadata: policy.Metadata{Name: "routing", Namespace: "target-counter", Revision: 1}, Mode: policy.ModeEnforce,
			Rules: []policy.Rule{{ID: "target-requests", Scope: policy.Scope{Provider: []string{"vllm"}},
				Limits: policy.Limits{Requests: []policy.WindowLimit{{ID: "hour", Max: 10, Window: "1h", WindowType: "rolling"}}}}},
		})
		if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
			t.Fatal(err)
		}
		if err := fixture.store.Insert(store.Request{Ts: time.Now(), CostUSD: .85, PricingState: store.PricingPriced}); err != nil {
			t.Fatal(err)
		}
		resp, _ := postDownshift(t, fixture.server.URL, `{"model":"source-model","max_tokens":10}`, nil)
		if resp.StatusCode != http.StatusOK || fixture.target.Load() != 1 {
			t.Fatalf("status=%d target=%d", resp.StatusCode, fixture.target.Load())
		}
		usage, err := fixture.store.PolicyRuleUsageSince("target-counter", "target-requests", time.Unix(0, 0))
		if err != nil || usage.Requests != 1 {
			t.Fatalf("target policy usage=%+v err=%v", usage, err)
		}
		summary, err := fixture.store.PolicyDecisionsSince(time.Unix(0, 0))
		if err != nil || summary.Total != 2 || summary.Admitted != 1 {
			t.Fatalf("policy summary=%+v err=%v", summary, err)
		}
	})
}

func TestDownshiftInsteadOfBlockRetriesOnlyCompatibleCheaperTarget(t *testing.T) {
	fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"target-model","usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
	if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "0.001"); err != nil {
		t.Fatal(err)
	}
	resp, body := postDownshift(t, fixture.server.URL,
		`{"model":"source-model","max_tokens":10,"messages":[{"role":"user","content":"x"}]}`, nil)
	if resp.StatusCode != http.StatusOK || fixture.source.Load() != 0 || fixture.target.Load() != 1 {
		t.Fatalf("status=%d source=%d target=%d body=%s", resp.StatusCode, fixture.source.Load(), fixture.target.Load(), body)
	}
	if resp.Header.Get("X-Burnban-Downshift-Trigger") != "budget_denial" ||
		!strings.Contains(resp.Header.Get("X-Burnban-Downshift-Reason"), "exceeded remaining budget") {
		t.Fatalf("headers=%v", resp.Header)
	}
}

func TestDownshiftNeverBypassesBanOrClaimsAnUnforwardedRewrite(t *testing.T) {
	fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("burn-banned request reached target")
	}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
	if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Insert(store.Request{Ts: time.Now(), CostUSD: .85, PricingState: store.PricingPriced}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.SetSetting(budget.KeyBanActive, "1"); err != nil {
		t.Fatal(err)
	}
	resp, body := postDownshift(t, fixture.server.URL, `{"model":"source-model","max_tokens":10}`, nil)
	if resp.StatusCode != http.StatusPaymentRequired || fixture.source.Load() != 0 || fixture.target.Load() != 0 ||
		resp.Header.Get("X-Burnban-Downshift-Action") != "none" ||
		!strings.Contains(resp.Header.Get("X-Burnban-Downshift-Reason"), "not forwarded") ||
		!strings.Contains(body, "burnban_banned") {
		t.Fatalf("status=%d source=%d target=%d headers=%v body=%s", resp.StatusCode, fixture.source.Load(), fixture.target.Load(), resp.Header, body)
	}
}

func TestDownshiftNeverRewritesUnknownToolsOrSelfReportedIdentityScope(t *testing.T) {
	tests := []struct {
		name       string
		config     downshift.Config
		body       string
		headers    map[string]string
		wantReason string
	}{
		{"unknown hosted tool", downshiftConfig(downshift.ModeWarnThenDownshift, false),
			`{"model":"source-model","max_tokens":10,"tools":[{"type":"web_search"}]}`, nil, "tools cannot be downshifted"},
		{"self-reported scope", downshiftConfig(downshift.ModeWarnThenDownshift, true),
			`{"model":"source-model","max_tokens":10}`, map[string]string{"X-Burnban-Project": "trusted-project"}, "requires authenticated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("ineligible request reached target")
			}), test.config)
			if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
				t.Fatal(err)
			}
			if err := fixture.store.Insert(store.Request{Ts: time.Now(), CostUSD: .85, PricingState: store.PricingPriced}); err != nil {
				t.Fatal(err)
			}
			resp, _ := postDownshift(t, fixture.server.URL, test.body, test.headers)
			if resp.StatusCode != http.StatusOK || fixture.source.Load() != 1 || fixture.target.Load() != 0 ||
				resp.Header.Get("X-Burnban-Downshift-Action") != "none" ||
				!strings.Contains(resp.Header.Get("X-Burnban-Downshift-Reason"), test.wantReason) {
				t.Fatalf("status=%d source=%d target=%d headers=%v", resp.StatusCode, fixture.source.Load(), fixture.target.Load(), resp.Header)
			}
		})
	}
}

func TestDownshiftNeverRewritesArbitraryPostOperations(t *testing.T) {
	fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("non-generation operation reached target")
	}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
	if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Insert(store.Request{Ts: time.Now(), CostUSD: .85, PricingState: store.PricingPriced}); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, fixture.server.URL+"/openai/v1/embeddings",
		strings.NewReader(`{"model":"source-model","max_tokens":10,"input":"private"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || fixture.source.Load() != 1 || fixture.target.Load() != 0 ||
		resp.Header.Get("X-Burnban-Downshift-Action") != "none" ||
		!strings.Contains(resp.Header.Get("X-Burnban-Downshift-Reason"), "only the exact /v1/chat/completions") {
		t.Fatalf("status=%d source=%d target=%d headers=%v", resp.StatusCode, fixture.source.Load(), fixture.target.Load(), resp.Header)
	}
}

func TestStreamingDownshiftPreservesSSEAndRecordsChosenRoute(t *testing.T) {
	fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(w, "data: {\"model\":\"target-model\",\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\n")
	}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
	if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Insert(store.Request{Ts: time.Now(), CostUSD: .81, PricingState: store.PricingPriced}); err != nil {
		t.Fatal(err)
	}
	resp, body := postDownshift(t, fixture.server.URL,
		`{"model":"source-model","max_tokens":10,"stream":true,"stream_options":{"include_usage":true}}`, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "one") || resp.Header.Get("X-Burnban-Downshift-Action") != "downshift" {
		t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, body)
	}
	rows, err := fixture.store.Export(time.Unix(0, 0))
	if err != nil || len(rows) != 2 || !rows[1].Streamed || rows[1].Provider != "vllm" || rows[1].Model != "target-model" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestSignedIdentityBindingSelectsScopedDownshiftBeforeBodyRewrite(t *testing.T) {
	fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Burnban-Identity") != "" {
			t.Error("signed identity leaked to target")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"target-model","usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}), downshiftConfig(downshift.ModeWarnThenDownshift, true))
	privateKey, grant := installIdentityTrust(t, fixture.store)
	if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Insert(store.Request{Ts: time.Now(), CostUSD: .85, PricingState: store.PricingPriced}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"source-model","max_tokens":10,"messages":[{"role":"user","content":"identity-bound"}]}`
	issue := func() string {
		t.Helper()
		token, _, err := identity.Issue(privateKey, grant, identity.RequestBinding{
			Audience: identity.AudienceProxy, Method: http.MethodPost, Route: "/openai/v1/chat/completions",
			QuerySHA256: identity.BodyDigest(nil), BodySHA256: identity.BodyDigest([]byte(body)),
		}, identity.Attribution{Projects: []string{"trusted-project"}}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	// A signed wildcard grant authenticates the device and principal, but the
	// asserted project remains self-reported and cannot select a scoped route.
	resp, _ := postDownshift(t, fixture.server.URL, body, map[string]string{"X-Burnban-Identity": issue()})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Burnban-Downshift-Action") != "none" ||
		fixture.source.Load() != 1 || fixture.target.Load() != 0 {
		t.Fatalf("wildcard status=%d headers=%v source=%d target=%d", resp.StatusCode, resp.Header, fixture.source.Load(), fixture.target.Load())
	}

	grant.Revision++
	grant.Attribution.Projects = []string{"trusted-project"}
	rawGrant, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.SetSetting(identity.KeyTrustGrant, string(rawGrant)); err != nil {
		t.Fatal(err)
	}
	token, _, err := identity.Issue(privateKey, grant, identity.RequestBinding{
		Audience: identity.AudienceProxy, Method: http.MethodPost, Route: "/openai/v1/chat/completions",
		QuerySHA256: identity.BodyDigest(nil), BodySHA256: identity.BodyDigest([]byte(body)),
	}, identity.Attribution{Projects: []string{"trusted-project"}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = postDownshift(t, fixture.server.URL, body, map[string]string{"X-Burnban-Identity": token})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Burnban-Downshift-Action") != "downshift" || fixture.target.Load() != 1 {
		t.Fatalf("status=%d headers=%v target=%d", resp.StatusCode, resp.Header, fixture.target.Load())
	}
	rows, err := fixture.store.Export(time.Unix(0, 0))
	if err != nil || len(rows) != 3 || rows[2].IdentityConfidence != "authenticated" || rows[2].Project != "trusted-project" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestConcurrentDownshiftAdmissionsUseTargetBoundsWithoutOvershootRace(t *testing.T) {
	fixture := newDownshiftFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"target-model","usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}), downshiftConfig(downshift.ModeWarnThenDownshift, false))
	if err := fixture.store.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Insert(store.Request{Ts: time.Now(), CostUSD: .85, PricingState: store.PricingPriced}); err != nil {
		t.Fatal(err)
	}
	const requests = 24
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(fixture.server.URL+"/openai/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"source-model","max_tokens":10}`))
			if err != nil {
				errs <- err
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Burnban-Downshift-Action") != "downshift" {
				errs <- fmt.Errorf("status=%d action=%q", resp.StatusCode, resp.Header.Get("X-Burnban-Downshift-Action"))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if fixture.source.Load() != 0 || fixture.target.Load() != requests {
		t.Fatalf("source=%d target=%d", fixture.source.Load(), fixture.target.Load())
	}
}
