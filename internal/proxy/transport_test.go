package proxy

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/burnban/burnban/internal/pricing"
)

func TestCloneDefaultTransportPreservesSafeDefaults(t *testing.T) {
	transport := cloneDefaultTransport()
	if transport == nil {
		t.Fatal("cloneDefaultTransport returned nil")
	}
	if original, ok := http.DefaultTransport.(*http.Transport); ok && transport == original {
		t.Fatal("proxy mutated the process-wide default transport")
	}
	if transport.Proxy == nil || transport.TLSHandshakeTimeout <= 0 ||
		transport.IdleConnTimeout <= 0 || transport.ExpectContinueTimeout <= 0 ||
		!transport.ForceAttemptHTTP2 {
		t.Fatalf("transport defaults were not preserved: %+v", transport)
	}
}

func TestWebhookTransportErrorsDoNotLeakEndpointSecrets(t *testing.T) {
	raw := &url.Error{
		Op:  "Post",
		URL: "https://user:password@hooks.example/private/path?token=secret-token",
		Err: errors.New("dial failed"),
	}
	got := safeWebhookError(raw).Error()
	if strings.Contains(got, "password") || strings.Contains(got, "private/path") ||
		strings.Contains(got, "secret-token") || !strings.Contains(got, "dial failed") {
		t.Fatalf("sanitized webhook error = %q", got)
	}
}

func TestStripHopHeadersIncludesConnectionExtensions(t *testing.T) {
	header := http.Header{
		"Connection":      {"keep-alive, X-Private-Hop", "X-Second-Hop"},
		"Keep-Alive":      {"timeout=5"},
		"X-Private-Hop":   {"secret"},
		"X-Second-Hop":    {"also-secret"},
		"X-End-To-End":    {"keep"},
		"Content-Type":    {"application/json"},
		"X-Burnban-Agent": {"local"},
	}
	stripHopHeaders(header)
	for _, name := range []string{"Connection", "Keep-Alive", "X-Private-Hop", "X-Second-Hop", "X-Burnban-Agent"} {
		if got := header.Get(name); got != "" {
			t.Errorf("%s survived with %q", name, got)
		}
	}
	if header.Get("X-End-To-End") != "keep" || header.Get("Content-Type") != "application/json" {
		t.Fatalf("end-to-end headers were removed: %v", header)
	}
}

func TestEstimateRequestTreatsProviderToolFeesAsUnbounded(t *testing.T) {
	p := &Proxy{Prices: &pricing.Table{Models: map[string]pricing.Price{
		"known": {InputPerMTok: 5, OutputPerMTok: 25, CacheWriteMult: 1.25},
	}}}
	for _, test := range []struct {
		name    string
		body    string
		bounded bool
	}{
		{name: "plain bounded request", body: `{"model":"known","max_tokens":100}`, bounded: true},
		{name: "client function", body: `{"model":"known","max_tokens":100,"tools":[{"name":"weather","input_schema":{"type":"object"}}]}`, bounded: true},
		{name: "OpenAI function", body: `{"model":"known","max_tokens":100,"tools":[{"type":"function","function":{"name":"weather"}}]}`, bounded: true},
		{name: "Anthropic web search", body: `{"model":"known","max_tokens":100,"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}]}`, bounded: false},
		{name: "OpenAI code interpreter", body: `{"model":"known","max_tokens":100,"tools":[{"type":"code_interpreter"}]}`, bounded: false},
		{name: "Gemini search", body: `{"model":"known","generationConfig":{"maxOutputTokens":100},"tools":[{"googleSearch":{}}]}`, bounded: false},
		{name: "US inference", body: `{"model":"known","max_tokens":100,"inference_geo":"us"}`, bounded: true},
		{name: "unknown inference geo", body: `{"model":"known","max_tokens":100,"inference_geo":"future-region"}`, bounded: false},
		{name: "priority tier", body: `{"model":"known","max_tokens":100,"service_tier":"priority"}`, bounded: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			estimate := p.estimateRequest("/v1/messages", []byte(test.body)).admission
			if estimate.Bounded != test.bounded || !estimate.Priced {
				t.Fatalf("estimate = %+v, want bounded=%v priced=true", estimate, test.bounded)
			}
		})
	}
	usBody := []byte(`{"model":"known","max_tokens":100,"inference_geo":"us"}`)
	us := p.estimateRequest("/v1/messages", usBody).admission
	want := float64(100*25+len(usBody)*5*2) * 1.1 / 1e6
	if diff := us.USD - want; diff < -1e-12 || diff > 1e-12 {
		t.Fatalf("US admission bound = %.12f, want %.12f", us.USD, want)
	}
}
