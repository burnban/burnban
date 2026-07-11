package proxy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/proxy"
	"github.com/burnban/burnban/internal/store"
)

const anthropicJSON = `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-4-7-20260301","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":2000}}`

const anthropicSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_01","model":"claude-opus-4-7-20260301","usage":{"input_tokens":300,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func newProxy(t *testing.T, upstream http.Handler) (*httptest.Server, *store.Store) {
	t.Helper()
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"claude-opus-4-7": {InputPerMTok: 5, OutputPerMTok: 25, CacheReadMult: 0.1, CacheWriteMult: 1.25},
	}}
	return newProxyFor(t, "anthropic", upstream, prices)
}

func post(t *testing.T, base string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(base+"/anthropic/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func summarize(t *testing.T, s *store.Store) *store.Summary {
	t.Helper()
	sum, err := s.Summarize(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestPassthroughAndMetering(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream saw path %s, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	resp, body := post(t, srv.URL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if body != anthropicJSON {
		t.Fatalf("proxy altered the response body:\n%s", body)
	}
	sum := summarize(t, s)
	if sum.Requests != 1 || sum.In != 1000 || sum.Out != 500 || sum.CacheRead != 2000 {
		t.Fatalf("summary = %+v", sum)
	}
	if want := 0.0185; math.Abs(sum.Cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", sum.Cost, want)
	}
}

func TestHealthIdentifiesBurnbanService(t *testing.T) {
	srv, _ := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, anthropicJSON)
	}))
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var health proxy.HealthSnapshot
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || health.Service != "burnban" || !health.OK || !health.PersistenceOK {
		t.Fatalf("health status=%d snapshot=%+v", resp.StatusCode, health)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["last_failure"]; present {
		t.Fatalf("clean health emitted last_failure: %s", body)
	}
}

func TestAnthropicCacheTierAndToolFeesArePreservedAndFailClosedUnderCap(t *testing.T) {
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"claude-opus-4-7": {InputPerMTok: 5, OutputPerMTok: 25, CacheWriteMult: 1.25},
	}}
	providerBody := `{"model":"claude-opus-4-7","service_tier":"priority","inference_geo":"us",` +
		`"usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":100,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":60,"ephemeral_1h_input_tokens":40},` +
		`"server_tool_use":{"web_search_requests":2}}}`
	srv, s := newProxyFor(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, providerBody)
	}), prices)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	call := func() (*http.Response, string) {
		resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(
			`{"model":"claude-opus-4-7","max_tokens":500,"service_tier":"priority","inference_geo":"us","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}
	if resp, body := call(); resp.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d body=%s", resp.StatusCode, body)
	}
	rows, err := s.Export(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.CacheWriteTokens != 100 || row.CacheWrite1hTokens != 40 || row.ServiceTier != "priority" ||
		row.InferenceGeo != "us" || row.ServerToolCalls != 2 || !row.FeeUnpriced ||
		!row.EnforcementUnsafe {
		t.Fatalf("stored anthropic metadata = %+v", row)
	}
	// Ordinary cache writes are 1.25x input; the nested one-hour subset is
	// corrected to Anthropic's 2x rate.
	wantCost := (1000*5.0 + 500*25.0 + 100*5.0*1.25 + 40*5.0*(2-1.25)) * 1.1 / 1e6
	if math.Abs(row.CostUSD-wantCost) > 1e-12 {
		t.Fatalf("cost = %.12f, want %.12f", row.CostUSD, wantCost)
	}
	if resp, body := call(); resp.StatusCode != http.StatusPaymentRequired ||
		!strings.Contains(body, "burnban_metering_gap") {
		t.Fatalf("second status=%d body=%s", resp.StatusCode, body)
	}
}

func TestBurnbanHeadersStayLocal(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"x-burnban-token", "x-burnban-agent", "x-burnban-session"} {
			if got := r.Header.Get(name); got != "" {
				t.Errorf("upstream received %s=%q", name, got)
			}
		}
		if got := r.Header.Get("X-Private-Hop"); got != "" {
			t.Errorf("upstream received Connection-nominated header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude-opus-4-7"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-burnban-token", "gateway-secret")
	req.Header.Set("x-burnban-agent", "payments-agent")
	req.Header.Set("x-burnban-session", "private-session")
	req.Header.Set("Connection", "keep-alive, X-Private-Hop")
	req.Header.Set("X-Private-Hop", "local-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := summarize(t, s).Agents[0].Agent; got != "payments-agent" {
		t.Fatalf("local attribution = %q", got)
	}
}

func TestUpstreamConnectionHeadersStayHopLocal(t *testing.T) {
	srv, _ := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "X-Upstream-Hop")
		w.Header().Set("X-Upstream-Hop", "provider-secret")
		w.Header().Set("X-End-To-End", "keep")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	resp, _ := post(t, srv.URL)
	if resp.Header.Get("Connection") != "" || resp.Header.Get("X-Upstream-Hop") != "" {
		t.Fatalf("upstream hop headers leaked: %v", resp.Header)
	}
	if got := resp.Header.Get("X-End-To-End"); got != "keep" {
		t.Fatalf("end-to-end response header = %q", got)
	}
}

func TestUpstreamQueryIsPreserved(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("api-version"); got != "2026-01-01" {
			t.Errorf("upstream api-version = %q", got)
		}
		if got := r.URL.Query().Get("stream"); got != "true" {
			t.Errorf("request query = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	t.Cleanup(up.Close)
	s, err := store.Open(filepath.Join(t.TempDir(), "query.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p, err := proxy.New(s, &pricing.Table{Models: map[string]pricing.Price{}}, map[string]proxy.Upstream{
		"anthropic": {URL: up.URL + "?api-version=2026-01-01", Shape: "anthropic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	outer := httptest.NewServer(p.Handler())
	t.Cleanup(outer.Close)
	resp, err := http.Post(outer.URL+"/anthropic/v1/messages?stream=true", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestUpstreamRedirectIsRelayedNotFollowed(t *testing.T) {
	redirected := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected++
	}))
	t.Cleanup(target.Close)
	srv, _ := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/credential-sink", http.StatusTemporaryRedirect)
	}))
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", "provider-secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || redirected != 0 {
		t.Fatalf("status=%d redirected=%d", resp.StatusCode, redirected)
	}
}

func TestStreamMetering(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, line := range strings.SplitAfter(anthropicSSE, "\n") {
			io.WriteString(w, line)
			f.Flush()
		}
	}))
	resp, body := post(t, srv.URL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatal("stream reached the client truncated")
	}
	sum := summarize(t, s)
	if sum.In != 300 || sum.Out != 42 || sum.CacheRead != 100 || sum.CacheWrite != 50 {
		t.Fatalf("summary = %+v", sum)
	}
	want := (300*5.0 + 42*25.0 + 100*5.0*0.1 + 50*5.0*1.25) / 1e6
	if math.Abs(sum.Cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", sum.Cost, want)
	}
}

func TestCancelledAnthropicStreamPersistsPartialUsage(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"model":"claude-opus-4-7","usage":{"input_tokens":100,"output_tokens":1}}}`+"\n\n")
		io.WriteString(w, "event: content_block_delta\n"+
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial streamed answer from provider"}}`+"\n\n")
		flusher.Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-7","max_tokens":100,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	observed := false
	for i := 0; i < 12; i++ {
		line, readErr := reader.ReadString('\n')
		if strings.Contains(line, "partial streamed answer") {
			observed = true
			break
		}
		if readErr != nil {
			break
		}
	}
	if !observed {
		cancel()
		resp.Body.Close()
		t.Fatal("client did not observe streamed output before cancellation")
	}
	cancel()
	resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	var rows []store.Request
	for time.Now().Before(deadline) {
		rows, err = s.Export(time.Unix(0, 0))
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("cancelled stream receipts = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.UsageState != store.UsagePartial || !row.Incomplete || !row.Estimated ||
		row.InTokens == 0 || row.OutTokens == 0 || row.PricingState != store.PricingPriced ||
		row.CostUSD <= 0 {
		t.Fatalf("cancelled stream receipt = %+v", row)
	}
}

func TestDailyCapBlocks(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, anthropicJSON)
	}))
	if err := s.Insert(store.Request{Ts: time.Now(), Provider: "anthropic", CostUSD: 0.02, Priced: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyDailyCapUSD, "0.01"); err != nil {
		t.Fatal(err)
	}
	resp, body := post(t, srv.URL)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
	if !strings.Contains(body, "burnban_cap_reached") {
		t.Fatalf("body = %s", body)
	}
}

func TestAgentCapBlocksOnlyThatAgent(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	if err := s.Insert(store.Request{Ts: time.Now(), Provider: "anthropic", Agent: "alpha", CostUSD: 0.02, Priced: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyAgentCapPrefix+"alpha", "0.01"); err != nil {
		t.Fatal(err)
	}

	postAs := func(agent string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
			strings.NewReader(`{"model":"claude-opus-4-7","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("x-burnban-agent", agent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}

	resp, body := postAs("alpha")
	if resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(body, "burnban_agent_cap_reached") {
		t.Fatalf("alpha: status = %d, body = %s", resp.StatusCode, body)
	}
	resp, _ = postAs("beta")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("beta should pass: status = %d", resp.StatusCode)
	}
}

func TestBanBlocksAndLifts(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	if err := s.SetSetting(budget.KeyBanActive, "1"); err != nil {
		t.Fatal(err)
	}
	resp, body := post(t, srv.URL)
	if resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(body, "burnban_banned") {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if err := s.DeleteSetting(budget.KeyBanActive); err != nil {
		t.Fatal(err)
	}
	resp, _ = post(t, srv.URL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after lift, status = %d", resp.StatusCode)
	}
}

const geminiJSON = `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":300,"thoughtsTokenCount":200,"cachedContentTokenCount":400,"totalTokenCount":1500},"modelVersion":"gemini-3-pro"}`

const geminiSSE = `data: {"candidates":[{"content":{"parts":[{"text":"partial"}]}}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":10},"modelVersion":"gemini-3-pro"}` + "\n\n" +
	`data: {"candidates":[{"content":{"parts":[{"text":"done"}]}}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":300,"thoughtsTokenCount":200,"cachedContentTokenCount":400}}` + "\n\n"

// newProxyFor builds a proxy with one named upstream and the given price
// table. Built-in provider names get their native metering shape, exactly
// as DefaultUpstreams wires them; anything else meters as OpenAI-shaped.
func newProxyFor(t *testing.T, name string, upstream http.Handler, prices *pricing.Table) (*httptest.Server, *store.Store) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	shape := "openai"
	if name == "anthropic" || name == "gemini" {
		shape = name
	}
	p, err := proxy.New(s, prices, map[string]proxy.Upstream{name: {URL: up.URL, Shape: shape}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return srv, s
}

func TestGeminiMetering(t *testing.T) {
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"gemini-3-pro": {InputPerMTok: 2, OutputPerMTok: 12, CacheReadMult: 0.1},
	}}
	srv, s := newProxyFor(t, "gemini", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, geminiJSON)
	}), prices)

	resp, err := http.Post(srv.URL+"/gemini/v1beta/models/gemini-3-pro:generateContent",
		"application/json", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	sum := summarize(t, s)
	// In = prompt(1000) - cached(400); Out = candidates(300) + thoughts(200).
	if sum.In != 600 || sum.Out != 500 || sum.CacheRead != 400 {
		t.Fatalf("summary = %+v", sum)
	}
	want := (600*2.0 + 500*12.0 + 400*2.0*0.1) / 1e6
	if math.Abs(sum.Cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", sum.Cost, want)
	}
	if len(sum.Models) != 1 || sum.Models[0].Model != "gemini-3-pro" {
		t.Fatalf("models = %+v", sum.Models)
	}
}

func TestGeminiStreamMetering(t *testing.T) {
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"gemini-3-pro": {InputPerMTok: 2, OutputPerMTok: 12, CacheReadMult: 0.1},
	}}
	srv, s := newProxyFor(t, "gemini", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, line := range strings.SplitAfter(geminiSSE, "\n") {
			io.WriteString(w, line)
			f.Flush()
		}
	}), prices)

	resp, err := http.Post(srv.URL+"/gemini/v1beta/models/gemini-3-pro:streamGenerateContent?alt=sse",
		"application/json", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "done") {
		t.Fatal("stream reached the client truncated")
	}
	sum := summarize(t, s)
	// The final cumulative frame wins; model carries over from frame one.
	if sum.In != 600 || sum.Out != 500 || sum.CacheRead != 400 {
		t.Fatalf("summary = %+v", sum)
	}
	if len(sum.Models) != 1 || sum.Models[0].Model != "gemini-3-pro" {
		t.Fatalf("models = %+v", sum.Models)
	}
}

// A custom upstream (serve --upstream groq=…) gets routed by name and
// metered with OpenAI-shaped parsing — the compat-provider default.
func TestCustomUpstreamOpenAIShaped(t *testing.T) {
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"kimi-k3": {InputPerMTok: 1, OutputPerMTok: 3, CacheReadMult: 0.1},
	}}
	srv, s := newProxyFor(t, "groq", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/chat/completions" {
			t.Errorf("upstream saw path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"cmpl","model":"kimi-k3","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":800,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":300}}}`)
	}), prices)

	resp, err := http.Post(srv.URL+"/groq/openai/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"kimi-k3","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	sum := summarize(t, s)
	if sum.In != 500 || sum.Out != 200 || sum.CacheRead != 300 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestWeeklyCapBlocksE2E(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, anthropicJSON)
	}))
	// Spend earlier in the week (2 days back, clamped inside Monday) plus
	// today crosses the weekly cap even though today alone would not.
	early := time.Now().AddDate(0, 0, -2)
	if ws := budget.WeekStart(time.Now()); early.Before(ws) {
		early = ws.Add(time.Hour)
	}
	if err := s.Insert(store.Request{Ts: early, Provider: "anthropic", CostUSD: 0.008, Priced: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(store.Request{Ts: time.Now(), Provider: "anthropic", CostUSD: 0.004, Priced: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyWeeklyCapUSD, "0.01"); err != nil {
		t.Fatal(err)
	}
	resp, body := post(t, srv.URL)
	if resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(body, "weekly") {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

// Gemini's REST streaming default (no alt=sse) is a JSON array of chunks
// with Content-Type application/json — it must still be metered.
func TestGeminiJSONArrayStream(t *testing.T) {
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"gemini-3-pro": {InputPerMTok: 2, OutputPerMTok: 12, CacheReadMult: 0.1},
	}}
	arrayBody := `[{"candidates":[{"content":{"parts":[{"text":"par"}]}}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":10},"modelVersion":"gemini-3-pro"},` +
		`{"candidates":[{"content":{"parts":[{"text":"tial"}]}}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":300,"thoughtsTokenCount":200,"cachedContentTokenCount":400}}]`
	srv, s := newProxyFor(t, "gemini", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, arrayBody)
	}), prices)

	resp, err := http.Post(srv.URL+"/gemini/v1beta/models/gemini-3-pro:streamGenerateContent",
		"application/json", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != arrayBody {
		t.Fatal("proxy altered the array body")
	}
	sum := summarize(t, s)
	if sum.In != 600 || sum.Out != 500 || sum.CacheRead != 400 {
		t.Fatalf("summary = %+v", sum)
	}
	if len(sum.Models) != 1 || sum.Models[0].Model != "gemini-3-pro" {
		t.Fatalf("models = %+v", sum.Models)
	}
}

// Two different windows tripping on the same day are two alerts, and a
// weekly cap that stays tripped alerts once per week, not once per day.
func TestDistinctWindowAlertsSameDay(t *testing.T) {
	hits := make(chan string, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits <- string(b)
	}))
	t.Cleanup(hook.Close)

	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	if err := s.SetSetting(budget.KeyWebhookURL, hook.URL); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(store.Request{Ts: time.Now(), Provider: "anthropic", CostUSD: 5, Priced: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyDailyCapUSD, "4"); err != nil {
		t.Fatal(err)
	}

	wait := func(wantSub string) string {
		t.Helper()
		select {
		case msg := <-hits:
			if !strings.Contains(msg, wantSub) {
				t.Fatalf("webhook = %s, want %q", msg, wantSub)
			}
			return msg
		case <-time.After(3 * time.Second):
			t.Fatalf("no webhook containing %q", wantSub)
			return ""
		}
	}

	if resp, _ := post(t, srv.URL); resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
	wait("daily burn cap")

	// Daily denial repeats: no second alert for the same window instance.
	post(t, srv.URL)
	select {
	case msg := <-hits:
		t.Fatalf("daily window alerted twice: %s", msg)
	case <-time.After(300 * time.Millisecond):
	}

	// Raise the daily cap out of the way; now the weekly cap trips — a
	// different window, so its alert must not be swallowed by today's.
	if err := s.SetSetting(budget.KeyDailyCapUSD, "1000"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyWeeklyCapUSD, "5"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := post(t, srv.URL); resp.StatusCode != http.StatusPaymentRequired {
		t.Fatal("weekly cap should deny")
	}
	wait("weekly burn cap")
}

func TestOversizeBodyRefused(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("oversize body must never reach the upstream")
	}))
	huge := strings.Repeat("x", 32<<20+1)
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(huge))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	_ = s
}

func TestOversizeResponsePassesThroughInFull(t *testing.T) {
	want := strings.Repeat("x", (32<<20)+17)
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, want)
	}))
	resp, body := post(t, srv.URL)
	if resp.StatusCode != http.StatusOK || body != want {
		t.Fatalf("response status=%d bytes=%d, want %d", resp.StatusCode, len(body), len(want))
	}
	if sum := summarize(t, s); sum.Requests != 1 || sum.Unpriced != 0 || sum.Incomplete != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestWarnWebhookFiresOnce(t *testing.T) {
	hits := make(chan string, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits <- string(b)
	}))
	t.Cleanup(hook.Close)

	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	if err := s.SetSetting(budget.KeyWebhookURL, hook.URL); err != nil {
		t.Fatal(err)
	}
	// $9 already burned against a $10 cap: past the default 80% threshold,
	// under the cap, so requests still pass but the warning must fire.
	if err := s.Insert(store.Request{Ts: time.Now(), Provider: "anthropic", CostUSD: 9, Priced: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}

	if resp, _ := post(t, srv.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want request to pass under the cap", resp.StatusCode)
	}
	select {
	case msg := <-hits:
		if !strings.Contains(msg, "daily cap") || !strings.Contains(msg, "⚠️") {
			t.Fatalf("webhook payload = %s", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("warning webhook never fired")
	}

	// Same window: a second request must not warn again.
	if resp, _ := post(t, srv.URL); resp.StatusCode != http.StatusOK {
		t.Fatal("second request should still pass")
	}
	select {
	case msg := <-hits:
		t.Fatalf("warned twice in one window: %s", msg)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWarnWebhookRetriesNon2xxBeforeMarkingDelivered(t *testing.T) {
	var attempts atomic.Int64
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if attempt < 3 {
			http.Error(w, "try again", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)

	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	if err := s.SetSetting(budget.KeyWebhookURL, hook.URL); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(store.Request{Ts: time.Now(), Provider: "anthropic", CostUSD: 9, Priced: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := post(t, srv.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(3 * time.Second)
	for attempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("webhook attempts = %d, want 3", got)
	}
	// The successful third attempt marks this window only after delivery, so
	// another request must not enqueue a fourth call.
	if resp, _ := post(t, srv.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d", resp.StatusCode)
	}
	time.Sleep(300 * time.Millisecond)
	if got := attempts.Load(); got != 3 {
		t.Fatalf("delivered webhook retried again: attempts=%d", got)
	}
}

func TestDuplicateWasteReceipt(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	post(t, srv.URL)
	post(t, srv.URL)
	sum := summarize(t, s)
	if sum.DupGroups != 1 {
		t.Fatalf("dup groups = %d, want 1", sum.DupGroups)
	}
	if want := 0.0185; math.Abs(sum.DupWastedUSD-want) > 1e-9 {
		t.Fatalf("wasted = %v, want %v", sum.DupWastedUSD, want)
	}
}

func TestFailedResponseIsUnmeteredNotUnknownPrice(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
	}))
	resp, _ := post(t, srv.URL)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	sum := summarize(t, s)
	if sum.Unmetered != 1 || sum.UnknownPricing != 0 || sum.Unpriced != 0 {
		t.Fatalf("failed response classification = %+v", sum)
	}
}

func TestFailedResponseWithProviderUsageIsStillMetered(t *testing.T) {
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"model":"claude-opus-4-7","usage":{"input_tokens":100,"output_tokens":20}}`)
	}))
	resp, _ := post(t, srv.URL)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	rows, err := s.Export(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].UsageState != store.UsageExact ||
		rows[0].PricingState != store.PricingPriced || rows[0].CostUSD <= 0 ||
		rows[0].EnforcementUnsafe {
		t.Fatalf("failed response usage receipt = %+v", rows)
	}
}

func TestUnknownRequestModelFailsClosedUnderCap(t *testing.T) {
	var hits atomic.Int64
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, anthropicJSON)
	}))
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
		strings.NewReader(`{"model":"future-model","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(string(body), "burnban_unpriced_request") || hits.Load() != 0 {
		t.Fatalf("status=%d hits=%d body=%s", resp.StatusCode, hits.Load(), body)
	}
}

func TestUnknownResponsePriceCreatesPersistentEnforcementGap(t *testing.T) {
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"known-model": {InputPerMTok: 1, OutputPerMTok: 2},
	}}
	srv, s := newProxyFor(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"provider-switched-model","usage":{"input_tokens":100,"output_tokens":20}}`)
	}), prices)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "10"); err != nil {
		t.Fatal(err)
	}
	call := func() (*http.Response, string) {
		resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
			strings.NewReader(`{"model":"known-model","max_tokens":100,"messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}
	if resp, body := call(); resp.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d body=%s", resp.StatusCode, body)
	}
	if sum := summarize(t, s); sum.UnknownPricing != 1 || sum.EnforcementGaps != 1 {
		t.Fatalf("gap summary=%+v", sum)
	}
	if resp, body := call(); resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(body, "burnban_metering_gap") {
		t.Fatalf("second status=%d body=%s", resp.StatusCode, body)
	}
}

func TestUpstreamTransportFailureCreatesPersistentEnforcementGap(t *testing.T) {
	var hits atomic.Int64
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"known-model": {InputPerMTok: 1, OutputPerMTok: 2},
	}}
	srv, s := newProxyFor(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	}), prices)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	call := func() (*http.Response, string) {
		resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
			strings.NewReader(`{"model":"known-model","max_tokens":100,"messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}
	if resp, body := call(); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("first status=%d body=%s", resp.StatusCode, body)
	}
	rows, err := s.Export(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].UsageState != store.UsageMissing ||
		rows[0].PricingState != store.PricingUnmetered || !rows[0].Incomplete ||
		!rows[0].EnforcementUnsafe {
		t.Fatalf("transport-failure receipt = %+v", rows)
	}
	if resp, body := call(); resp.StatusCode != http.StatusPaymentRequired ||
		!strings.Contains(body, "burnban_metering_gap") || hits.Load() != 1 {
		t.Fatalf("second status=%d hits=%d body=%s", resp.StatusCode, hits.Load(), body)
	}
}

func TestUnboundedConcurrentRequestsAreExclusivelyAdmitted(t *testing.T) {
	var hits atomic.Int64
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	if err := s.SetSetting(budget.KeyDailyCapUSD, "1"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	statuses := make(chan int, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
				strings.NewReader(`{"model":"claude-opus-4-7","messages":[]}`))
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if hits.Load() != 1 || counts[http.StatusOK] != 1 || counts[http.StatusPaymentRequired] != 9 {
		t.Fatalf("hits=%d statuses=%v", hits.Load(), counts)
	}
}

func TestPersistenceFailureLatchesProxyFailClosed(t *testing.T) {
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"claude-opus-4-7": {InputPerMTok: 5, OutputPerMTok: 25, CacheReadMult: 0.1, CacheWriteMult: 1.25},
	}}
	s, err := store.Open(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = s.Close() // admission succeeded; force the post-response insert to fail
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	defer upstream.Close()
	p, err := proxy.New(s, prices, map[string]proxy.Upstream{"anthropic": {URL: upstream.URL, Shape: "anthropic"}})
	if err != nil {
		t.Fatal(err)
	}
	p.Logf = func(string, ...any) {}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, _ := post(t, srv.URL)
	if resp.StatusCode != http.StatusOK || p.Health().OK {
		t.Fatalf("first status=%d health=%+v", resp.StatusCode, p.Health())
	}
	healthResp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	var health proxy.HealthSnapshot
	_ = json.NewDecoder(healthResp.Body).Decode(&health)
	healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusServiceUnavailable || health.OK || health.PersistenceOK {
		t.Fatalf("health status=%d body=%+v", healthResp.StatusCode, health)
	}
	resp, _ = post(t, srv.URL)
	if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 {
		t.Fatalf("second status=%d upstream hits=%d", resp.StatusCode, hits.Load())
	}
}

func TestAdmissionPersistenceFailureLatchesBeforeUpstream(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "preflight-health.db"))
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, anthropicJSON)
	}))
	defer upstream.Close()
	p, err := proxy.New(s, &pricing.Table{Models: map[string]pricing.Price{
		"claude-opus-4-7": {InputPerMTok: 5, OutputPerMTok: 25},
	}}, map[string]proxy.Upstream{"anthropic": {URL: upstream.URL, Shape: "anthropic"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()
	resp, body := post(t, srv.URL)
	if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 0 || p.Health().OK ||
		!strings.Contains(body, "fail-closed") {
		t.Fatalf("status=%d hits=%d health=%+v body=%s", resp.StatusCode, hits.Load(), p.Health(), body)
	}
}

func TestDuplicateFingerprintIncludesRouteAndProvider(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicJSON)
	}))
	defer up.Close()
	s, err := store.Open(filepath.Join(t.TempDir(), "fingerprints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"claude-opus-4-7": {InputPerMTok: 5, OutputPerMTok: 25, CacheReadMult: 0.1, CacheWriteMult: 1.25},
	}}
	p, err := proxy.New(s, prices, map[string]proxy.Upstream{
		"anthropic": {URL: up.URL, Shape: "anthropic"},
		"mirror":    {URL: up.URL, Shape: "anthropic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()
	body := `{"model":"claude-opus-4-7","messages":[]}`
	for _, route := range []string{"/anthropic/v1/messages", "/mirror/v1/messages"} {
		resp, err := http.Post(srv.URL+route, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if sum := summarize(t, s); sum.DupGroups != 0 {
		t.Fatalf("cross-provider calls grouped as duplicates: %+v", sum)
	}
	resp, _ := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if sum := summarize(t, s); sum.DupGroups != 1 {
		t.Fatalf("same-route duplicate not found: %+v", sum)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-burnban-session", "independent-session")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if sum := summarize(t, s); sum.DupGroups != 1 {
		t.Fatalf("cross-session call grouped as a duplicate: %+v", sum)
	}
}
