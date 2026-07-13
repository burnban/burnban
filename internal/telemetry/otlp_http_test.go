package telemetry

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPExporterRetriesFakeCollectorAndReadsAuthFromEnv(t *testing.T) {
	const secret = "Bearer test-secret-never-log"
	t.Setenv("TEST_OTLP_AUTH", secret)
	var traces, metrics atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != secret || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("collector headers = %+v", r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) == 0 {
			t.Errorf("collector body len=%d err=%v", len(body), err)
		}
		switch r.URL.Path {
		case "/v1/traces":
			if traces.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "do-not-reflect-this", http.StatusServiceUnavailable)
				return
			}
		case "/v1/metrics":
			metrics.Add(1)
		default:
			t.Errorf("unexpected collector path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer collector.Close()

	exporter, err := NewHTTPExporter(HTTPConfig{
		Endpoint: collector.URL, AuthorizationEnv: "TEST_OTLP_AUTH",
		MaxAttempts: 3, BaseBackoff: 10 * time.Millisecond,
		sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Export(context.Background(), Batch{Events: []Event{fixtureEvent()}}); err != nil {
		t.Fatal(err)
	}
	if traces.Load() != 2 || metrics.Load() != 1 {
		t.Fatalf("collector calls traces=%d metrics=%d", traces.Load(), metrics.Load())
	}
}

func TestHTTPExporterDoesNotFollowRedirectWithAuthorization(t *testing.T) {
	const secret = "Bearer redirect-secret"
	t.Setenv("TEST_OTLP_AUTH", secret)
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHits.Add(1)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != secret {
			t.Error("authorization missing at configured collector")
		}
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	exporter, err := NewHTTPExporter(HTTPConfig{
		Endpoint: redirector.URL, AuthorizationEnv: "TEST_OTLP_AUTH",
		MaxAttempts: 1, BaseBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = exporter.Export(context.Background(), Batch{Events: []Event{fixtureEvent()}})
	if err == nil || !strings.Contains(err.Error(), "non-retryable HTTP 307") {
		t.Fatalf("redirect error = %v", err)
	}
	if targetHits.Load() != 0 {
		t.Fatalf("redirect target received %d requests", targetHits.Load())
	}
}

func TestHTTPExporterPartialSuccessIsExplicitNonRetryableDrop(t *testing.T) {
	var calls atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":"1","errorMessage":"synthetic"}}`))
	}))
	defer collector.Close()
	exporter, err := NewHTTPExporter(HTTPConfig{Endpoint: collector.URL, MaxAttempts: 5})
	if err != nil {
		t.Fatal(err)
	}
	err = exporter.Export(context.Background(), Batch{Events: []Event{fixtureEvent()}})
	var partial *PartialRejectError
	if !errors.As(err, &partial) || calls.Load() != 1 {
		t.Fatalf("partial error=%v calls=%d", err, calls.Load())
	}
}

func TestHTTPExporterRejectsMalformedSuccessWithoutRetry(t *testing.T) {
	var calls atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html>not otlp json</html>`))
	}))
	defer collector.Close()
	exporter, err := NewHTTPExporter(HTTPConfig{Endpoint: collector.URL, MaxAttempts: 5})
	if err != nil {
		t.Fatal(err)
	}
	err = exporter.Export(context.Background(), Batch{Events: []Event{fixtureEvent()}})
	var permanent *permanentExportError
	if !errors.As(err, &permanent) || calls.Load() != 1 {
		t.Fatalf("malformed-success error=%v calls=%d", err, calls.Load())
	}
}

func TestAuthorizationValidationNeverReflectsSecret(t *testing.T) {
	secret := "Bearer secret\nsmuggled"
	t.Setenv("TEST_OTLP_AUTH", secret)
	collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid authorization must fail before network I/O")
	}))
	defer collector.Close()
	exporter, err := NewHTTPExporter(HTTPConfig{Endpoint: collector.URL, AuthorizationEnv: "TEST_OTLP_AUTH", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = exporter.Export(context.Background(), Batch{Events: []Event{fixtureEvent()}})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "smuggled") {
		t.Fatalf("authorization error leaked value: %v", err)
	}
}

func TestEndpointValidationAndSinkBinding(t *testing.T) {
	valid := []string{
		"https://collector.example", "https://collector.example/otel",
		"http://127.0.0.1:4318", "http://localhost:4318/v1/traces",
		"https://[::1]:4318/v1/metrics",
	}
	for _, endpoint := range valid {
		if _, err := parseEndpoint(endpoint, false); err != nil {
			t.Errorf("valid endpoint %q: %v", endpoint, err)
		}
	}
	invalid := []string{
		"", "collector.example", "ftp://collector.example", "http://collector.example",
		"https://user:pass@collector.example", "https://collector.example?token=secret",
		"https://collector.example/#fragment", "https://collector.example/a//b",
		"https://collector.example/%2e%2e/admin",
	}
	for _, endpoint := range invalid {
		if _, err := parseEndpoint(endpoint, false); err == nil {
			t.Errorf("unsafe endpoint %q was accepted", endpoint)
		}
	}
	a, err := NewHTTPExporter(HTTPConfig{Endpoint: "https://collector.example", ServiceName: "burnban-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewHTTPExporter(HTTPConfig{Endpoint: "https://collector.example", ServiceName: "burnban-b"})
	if err != nil {
		t.Fatal(err)
	}
	if a.SinkID() == b.SinkID() {
		t.Fatal("sink checkpoint identity did not bind service configuration")
	}
	upgraded, err := NewHTTPExporter(HTTPConfig{Endpoint: "https://collector.example", ServiceName: "burnban-a", ServiceVersion: "9.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if a.SinkID() != upgraded.SinkID() {
		t.Fatal("binary version change incorrectly reset the durable sink cursor")
	}
}

func TestOnlyOTLPSpecifiedHTTPStatusesRetry(t *testing.T) {
	for _, status := range []int{429, 502, 503, 504} {
		if !retryableStatus(status) {
			t.Errorf("OTLP retryable status %d was not retried", status)
		}
	}
	for _, status := range []int{400, 401, 408, 500, 505} {
		if retryableStatus(status) {
			t.Errorf("OTLP non-retryable status %d was retried", status)
		}
	}
}

func TestCollectorIPPolicyBlocksSSRFAndRebindingRanges(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "0.1.2.3", "100.64.0.1", "169.254.169.254", "192.0.2.1",
		"192.31.196.1", "192.52.193.1", "192.175.48.1", "198.18.0.1",
		"198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1",
		"fe80::1", "fec0::1", "64:ff9b::1", "100::1", "2001:1::1",
		"2001:2::1", "2001:db8::1", "2002:c000:0201::1", "3fff::1", "5f00::1",
	} {
		if allowedCollectorIP(net.ParseIP(raw), true, false) {
			t.Errorf("always-unsafe collector IP %s was allowed", raw)
		}
	}
	if allowedCollectorIP(net.ParseIP("127.0.0.1"), false, false) || !allowedCollectorIP(net.ParseIP("127.0.0.1"), false, true) {
		t.Fatal("loopback collector policy mismatch")
	}
	if allowedCollectorIP(net.ParseIP("1.1.1.1"), false, true) {
		t.Fatal("localhost endpoint was allowed to rebind to public unicast")
	}
	if allowedCollectorIP(net.ParseIP("10.0.0.1"), false, false) || !allowedCollectorIP(net.ParseIP("10.0.0.1"), true, false) {
		t.Fatal("private collector opt-in policy mismatch")
	}
	if allowedCollectorIP(net.ParseIP("fd00::1"), false, false) || !allowedCollectorIP(net.ParseIP("fd00::1"), true, false) {
		t.Fatal("ULA collector opt-in policy mismatch")
	}
	if !allowedCollectorIP(net.ParseIP("1.1.1.1"), false, false) {
		t.Fatal("public unicast collector was blocked")
	}
	if !allowedCollectorIP(net.ParseIP("2606:4700:4700::1111"), false, false) {
		t.Fatal("public IPv6 unicast collector was blocked")
	}
}
