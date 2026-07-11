package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/proxy"
	"github.com/burnban/burnban/internal/store"
)

// Route names become ServeMux patterns; anything that could panic the mux
// or register a wildcard must die at flag parsing, not at startup.
func TestUpstreamFlagRejectsUnsafeNames(t *testing.T) {
	for _, bad := range []string{
		"x{y=http://localhost:1", // unclosed brace: mux panics
		"{x}=http://localhost:1", // wildcard: swallows arbitrary paths
		"a/b=http://localhost:1", // slash: nested route
		"a b=http://localhost:1", // space
		"api=http://localhost:1", // reserved for the dashboard
		"metrics=http://localhost:1",
		"health=http://localhost:1",
		"=http://localhost:1",
		"noequals",
		"groq=api.groq.com", // missing scheme
	} {
		u := upstreamFlags{}
		if err := u.Set(bad); err == nil {
			t.Errorf("Set(%q) accepted an unsafe upstream", bad)
		}
	}
}

func TestServeLifecycleHealthAndGracefulStop(t *testing.T) {
	t.Setenv("BURNBAN_TOKEN", "")
	dbPath := filepath.Join(t.TempDir(), "lifecycle.db")
	errCh := make(chan error, 1)
	go func() {
		errCh <- cmdServeWithOptions([]string{"--db", dbPath, "--port", "0"}, false, false)
	}()

	var state serverState
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		state, err = readServerState(serverStatePath(dbPath))
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if state.ControlURL == "" {
		t.Fatal("server did not publish lifecycle state")
	}
	controlURL, err := url.Parse(state.ControlURL)
	if err != nil || controlURL.Scheme != "http" || !isLoopbackHost(controlURL.Hostname()) || controlURL.Port() == "" {
		t.Fatalf("private control URL = %q, want an HTTP loopback origin with an ephemeral port", state.ControlURL)
	}
	if state.ControlURL == state.URL {
		t.Fatalf("control listener must be isolated from public listener: %+v", state)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if resp, err := controlRequest(ctx, state, http.MethodPost); err == nil {
			resp.Body.Close()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	resp, err := controlRequest(ctx, state, http.MethodGet)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Health struct {
			OK    bool   `json:"ok"`
			State string `json:"state"`
		} `json:"health"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !status.Health.OK || status.Health.State != "healthy" {
		t.Fatalf("control status=%d payload=%+v", resp.StatusCode, status)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	resp, err = controlRequest(ctx, state, http.MethodPost)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop status=%d, want 202", resp.StatusCode)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve returned after graceful stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop within 5 seconds")
	}
	if _, err := os.Stat(serverStatePath(dbPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("server state remained after stop: %v", err)
	}
}

func TestTLSServeKeepsLifecycleControlOnPlainLoopback(t *testing.T) {
	certPath, keyPath := writeTestCertificate(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	dbPath := filepath.Join(t.TempDir(), "tls-lifecycle.db")
	errCh := make(chan error, 1)
	go func() {
		errCh <- cmdServeWithOptions([]string{
			"--db", dbPath, "--port", "0", "--tls-cert", certPath, "--tls-key", keyPath,
		}, false, false)
	}()
	state := waitForServerState(t, dbPath)
	if !strings.HasPrefix(state.URL, "https://") || !strings.HasPrefix(state.ControlURL, "http://127.0.0.1:") {
		t.Fatalf("TLS lifecycle state = %+v", state)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	resp, err := controlRequest(ctx, state, http.MethodGet)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS server control status = %d", resp.StatusCode)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	resp, err = controlRequest(ctx, state, http.MethodPost)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("TLS serve returned after stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TLS server did not stop through its plain loopback control listener")
	}
}

func TestLoadServerCertificateValidation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	validCert, validKey := writeTestCertificate(t, now.Add(-time.Hour), now.Add(time.Hour), []string{"burnban.example"}, nil)
	pair, err := loadServerCertificate(validCert, validKey, "burnban.example", now)
	if err != nil || pair.Leaf == nil {
		t.Fatalf("valid certificate: pair leaf=%v err=%v", pair.Leaf, err)
	}
	if _, err := loadServerCertificate(validCert, validKey, "other.example", now); err == nil || !strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("hostname mismatch error = %v", err)
	}
	expiredCert, expiredKey := writeTestCertificate(t, now.Add(-2*time.Hour), now.Add(-time.Hour), []string{"burnban.example"}, nil)
	if _, err := loadServerCertificate(expiredCert, expiredKey, "burnban.example", now); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired certificate error = %v", err)
	}
	futureCert, futureKey := writeTestCertificate(t, now.Add(time.Hour), now.Add(2*time.Hour), []string{"burnban.example"}, nil)
	if _, err := loadServerCertificate(futureCert, futureKey, "burnban.example", now); err == nil || !strings.Contains(err.Error(), "not valid until") {
		t.Fatalf("future certificate error = %v", err)
	}
	badCert := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badCert, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadServerCertificate(badCert, validKey, "", now); err == nil || !strings.Contains(err.Error(), "load TLS") {
		t.Fatalf("malformed certificate error = %v", err)
	}
}

func TestPublicURLExposureFailsClosedBeforeListening(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "must-not-open.db")
	t.Setenv("BURNBAN_TOKEN", "")
	if err := cmdServeWithOptions([]string{"--db", dbPath, "--port", "0", "--public-url", "https://burnban.example"}, false, false); err == nil || !strings.Contains(err.Error(), "without BURNBAN_TOKEN") {
		t.Fatalf("external public URL without token error = %v", err)
	}
	t.Setenv("BURNBAN_TOKEN", "short")
	if err := cmdServeWithOptions([]string{"--db", dbPath, "--port", "0", "--public-url", "https://burnban.example"}, false, false); err == nil || !strings.Contains(err.Error(), "at least 16") {
		t.Fatalf("external public URL with weak token error = %v", err)
	}
	t.Setenv("BURNBAN_TOKEN", "a-secure-team-token-value")
	if err := cmdServeWithOptions([]string{"--db", dbPath, "--port", "0", "--public-url", "http://burnban.example"}, false, false); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("external plaintext public URL error = %v", err)
	}
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation should fail before opening the ledger; stat error=%v", err)
	}
}

func TestValidateBurnbanToken(t *testing.T) {
	tests := []struct {
		name, token string
		strong      bool
		wantErr     bool
	}{
		{name: "local short visible token", token: "local", wantErr: false},
		{name: "network short token", token: "short", strong: true, wantErr: true},
		{name: "spaces", token: "0123456789 abcdef", wantErr: true},
		{name: "control", token: "0123456789\nabcdef", wantErr: true},
		{name: "unicode", token: "0123456789abcdef☃", wantErr: true},
		{name: "low diversity", token: strings.Repeat("a", 32), strong: true, wantErr: true},
		{name: "generated hex", token: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", strong: true},
		{name: "base64url", token: "wC0a-Safe_Random.Token=123456", strong: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBurnbanToken(tt.token, tt.strong)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateBurnbanToken error=%v, wantErr=%t", err, tt.wantErr)
			}
		})
	}
}

func writeTestCertificate(t *testing.T, notBefore, notAfter time.Time, dnsNames []string, ips []net.IP) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Burnban test"},
		NotBefore: notBefore, NotAfter: notAfter, DNSNames: dnsNames, IPAddresses: ips,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestGracefulStopWaitsForInFlightProxyRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-5.6-luna","usage":{"prompt_tokens":20,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "drain.db")
	errCh := make(chan error, 1)
	go func() {
		errCh <- cmdServeWithOptions([]string{
			"--db", dbPath, "--port", "0", "--upstream", "slow=" + upstream.URL,
		}, false, false)
	}()
	state := waitForServerState(t, dbPath)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			select {
			case <-release:
			default:
				close(release)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if resp, err := controlRequest(ctx, state, http.MethodPost); err == nil {
				resp.Body.Close()
			}
		}
	})

	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.Post(state.URL+"/slow/v1/chat/completions", "application/json",
			bytes.NewBufferString(`{"model":"gpt-5.6-luna","max_tokens":10}`))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("proxy status %d", resp.StatusCode)
			}
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy request did not reach upstream")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	resp, err := controlRequest(ctx, state, http.MethodPost)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case err := <-errCh:
		t.Fatalf("serve returned before in-flight request drained: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if err := <-requestDone; err != nil {
		t.Fatalf("in-flight client failed during graceful stop: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve returned after draining request: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not exit after in-flight request drained")
	}
	stopped = true
}

func TestPruneRefusesWhileLedgerIsServed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "live-prune.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(store.Request{
		Ts: time.Now().Add(-48 * time.Hour), Provider: "openai", Model: "old",
		CostUSD: 1, PricingState: store.PricingPriced,
	}); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- cmdServeWithOptions([]string{"--db", dbPath, "--port", "0"}, false, false)
	}()
	state := waitForServerState(t, dbPath)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if resp, err := controlRequest(ctx, state, http.MethodPost); err == nil {
				resp.Body.Close()
			}
		}
	})

	err = cmdPrune([]string{"--db", dbPath, "--older-than", "24h", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "refusing to prune while") {
		t.Fatalf("live prune error = %v", err)
	}
	verify, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := verify.Summarize(time.Unix(0, 0))
	verify.Close()
	if err != nil || summary.Requests != 1 {
		t.Fatalf("live prune changed ledger: summary=%+v err=%v", summary, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	resp, err := controlRequest(ctx, state, http.MethodPost)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	stopped = true
	if err := cmdPrune([]string{"--db", dbPath, "--older-than", "24h", "--yes"}); err != nil {
		t.Fatalf("stopped prune failed: %v", err)
	}
	verify, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	summary, err = verify.Summarize(time.Unix(0, 0))
	if err != nil || summary.Requests != 0 {
		t.Fatalf("stopped prune did not remove row: summary=%+v err=%v", summary, err)
	}
}

func waitForServerState(t *testing.T, dbPath string) serverState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, err := readServerState(serverStatePath(dbPath))
		if err == nil {
			return state
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("server did not publish lifecycle state")
	return serverState{}
}

func TestUpstreamFlagShapes(t *testing.T) {
	u := upstreamFlags{}
	if err := u.Set("groq=https://api.groq.com/openai"); err != nil {
		t.Fatal(err)
	}
	if got := u["groq"]; got.URL != "https://api.groq.com/openai" || got.Shape != "" {
		t.Fatalf("groq = %+v, want unspecified shape (meters OpenAI by default)", got)
	}
	if err := u.Set("corp=anthropic:https://llm.corp.internal"); err != nil {
		t.Fatal(err)
	}
	if got := u["corp"]; got.URL != "https://llm.corp.internal" || got.Shape != "anthropic" {
		t.Fatalf("corp = %+v", got)
	}
	// "https" is not a shape: a bare url with a scheme must parse as a url.
	if err := u.Set("mistral=https://api.mistral.ai"); err != nil {
		t.Fatal(err)
	}
	if got := u["mistral"]; got.URL != "https://api.mistral.ai" {
		t.Fatalf("mistral = %+v", got)
	}
	if s := u.String(); !strings.Contains(s, "corp=") || !strings.Contains(s, "groq=") {
		t.Fatalf("String() = %q", s)
	}
}

func TestDemoProviderRoutesNeverReachProxyFallback(t *testing.T) {
	var fallbackHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits++
		w.WriteHeader(http.StatusNoContent)
	})
	registerDemoProviderBlocks(mux, map[string]proxy.Upstream{
		"openai": {URL: "https://example.invalid", Shape: "openai"},
	})
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.6-luna"}`))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict || fallbackHits != 0 || !strings.Contains(rr.Body.String(), "demo_network_disabled") {
		t.Fatalf("status=%d fallback=%d body=%q", rr.Code, fallbackHits, rr.Body.String())
	}
}

func TestLoopbackAndURLRedaction(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "127.42.0.9", "::1", "[::1]"} {
		if !isLoopbackHost(host) {
			t.Errorf("%q should be loopback", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "192.168.1.4", "example.com", ""} {
		if isLoopbackHost(host) {
			t.Errorf("%q should not be loopback", host)
		}
	}
	got := redactURL("https://user:password@example.com/v1?api_key=secret")
	if strings.Contains(got, "user") || strings.Contains(got, "password") || strings.Contains(got, "secret") {
		t.Fatalf("redactURL leaked credentials: %q", got)
	}
}
