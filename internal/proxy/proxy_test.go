package proxy_test

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/pricing"
	"github.com/syft8/burnban/internal/proxy"
	"github.com/syft8/burnban/internal/store"
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
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	prices := &pricing.Table{Models: map[string]pricing.Price{
		"claude-opus-4-7": {InputPerMTok: 5, OutputPerMTok: 25, CacheReadMult: 0.1, CacheWriteMult: 1.25},
	}}
	p, err := proxy.New(s, prices, map[string]string{"anthropic": up.URL})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return srv, s
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
