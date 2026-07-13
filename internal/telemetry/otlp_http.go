package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	maxOTLPPayload  = 8 << 20
	maxOTLPResponse = 64 << 10
)

type HTTPConfig struct {
	Endpoint            string
	AuthorizationEnv    string
	AllowPrivateNetwork bool
	ServiceName         string
	ServiceVersion      string
	RequestTimeout      time.Duration
	MaxAttempts         int
	BaseBackoff         time.Duration

	// Test seams remain package-private in effect: production callers leave
	// these nil and receive the hardened transport, cryptographic IDs, and real
	// timers below.
	client *http.Client
	random io.Reader
	sleep  func(context.Context, time.Duration) error
}

type HTTPExporter struct {
	tracesURL        string
	metricsURL       string
	sinkID           string
	authorizationEnv string
	serviceName      string
	serviceVersion   string
	requestTimeout   time.Duration
	maxAttempts      int
	baseBackoff      time.Duration
	client           *http.Client
	random           io.Reader
	sleep            func(context.Context, time.Duration) error
}

// PartialRejectError means an OTLP receiver populated partialSuccess. The OTLP
// specification forbids retrying that request; the worker records those rows
// on its separate dropped cursor rather than calling them delivered.
type PartialRejectError struct {
	Signal string
}

func (e *PartialRejectError) Error() string {
	return "OTLP collector partially rejected " + e.Signal + "; request will not be retried"
}

type permanentExportError struct{ message string }

func (e *permanentExportError) Error() string { return e.message }

func NewHTTPExporter(config HTTPConfig) (*HTTPExporter, error) {
	endpoint, err := parseEndpoint(config.Endpoint, config.AllowPrivateNetwork)
	if err != nil {
		return nil, err
	}
	if config.AuthorizationEnv == "" {
		config.AuthorizationEnv = "BURNBAN_OTLP_AUTHORIZATION"
	}
	if !validEnvName(config.AuthorizationEnv) {
		return nil, fmt.Errorf("OTLP authorization env name must contain only A-Z, 0-9, and underscore and must not start with a digit")
	}
	if config.ServiceName == "" {
		config.ServiceName = "burnban"
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 10 * time.Second
	}
	if config.RequestTimeout < time.Second || config.RequestTimeout > time.Minute {
		return nil, fmt.Errorf("OTLP request timeout must be between 1s and 1m")
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 5
	}
	if config.MaxAttempts < 1 || config.MaxAttempts > 10 {
		return nil, fmt.Errorf("OTLP max attempts must be between 1 and 10")
	}
	if config.BaseBackoff == 0 {
		config.BaseBackoff = 200 * time.Millisecond
	}
	if config.BaseBackoff < 10*time.Millisecond || config.BaseBackoff > 10*time.Second {
		return nil, fmt.Errorf("OTLP base backoff must be between 10ms and 10s")
	}
	sinkDigest := sha256.Sum256([]byte(strings.Join([]string{
		endpoint.sinkID, SchemaVersion, config.ServiceName, config.AuthorizationEnv,
	}, "\x00")))
	endpoint.sinkID = hex.EncodeToString(sinkDigest[:16])
	client := config.client
	if client == nil {
		client = secureHTTPClient(endpoint)
	}
	random := config.random
	if random == nil {
		random = rand.Reader
	}
	sleep := config.sleep
	if sleep == nil {
		sleep = sleepContext
	}
	return &HTTPExporter{
		tracesURL: endpoint.tracesURL, metricsURL: endpoint.metricsURL,
		sinkID: endpoint.sinkID, authorizationEnv: config.AuthorizationEnv,
		serviceName: config.ServiceName, serviceVersion: config.ServiceVersion,
		requestTimeout: config.RequestTimeout, maxAttempts: config.MaxAttempts,
		baseBackoff: config.BaseBackoff, client: client, random: random, sleep: sleep,
	}, nil
}

func (e *HTTPExporter) SinkID() string { return e.sinkID }

func (e *HTTPExporter) Export(ctx context.Context, batch Batch) error {
	if len(batch.Events) == 0 {
		return nil
	}
	traces, err := buildTracePayload(batch, e.serviceName, e.serviceVersion, e.random)
	if err != nil {
		return err
	}
	metrics, err := buildMetricsPayload(batch, e.serviceName, e.serviceVersion, time.Now().UTC())
	if err != nil {
		return err
	}
	if len(traces) > maxOTLPPayload || len(metrics) > maxOTLPPayload {
		return &permanentExportError{message: "OTLP batch exceeds the 8 MiB payload bound; lower --otlp-batch"}
	}
	if err := e.postWithRetry(ctx, e.tracesURL, "traces", traces); err != nil {
		return err
	}
	return e.postWithRetry(ctx, e.metricsURL, "metrics", metrics)
}

func (e *HTTPExporter) postWithRetry(ctx context.Context, endpoint, signal string, body []byte) error {
	var last error
	var delay time.Duration
	for attempt := 0; attempt < e.maxAttempts; attempt++ {
		if delay > 0 {
			if err := e.sleep(ctx, delay); err != nil {
				return err
			}
		}
		retryAfter, err := e.post(ctx, endpoint, signal, body)
		if err == nil {
			return nil
		}
		last = err
		var permanent *permanentExportError
		var partial *PartialRejectError
		if errors.As(err, &permanent) || errors.As(err, &partial) {
			return err
		}
		delay = retryDelay(e.baseBackoff, attempt+1, retryAfter)
	}
	return last
}

func (e *HTTPExporter) post(ctx context.Context, endpoint, signal string, body []byte) (string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, e.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", &permanentExportError{message: "build OTLP request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if value, present := os.LookupEnv(e.authorizationEnv); present && value != "" {
		if err := validateAuthorization(value); err != nil {
			return "", err
		}
		req.Header.Set("Authorization", value)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		if attemptCtx.Err() != nil {
			return "", fmt.Errorf("OTLP %s request timed out or was canceled", signal)
		}
		return "", fmt.Errorf("OTLP %s connection failed", signal)
	}
	defer resp.Body.Close()
	response, readErr := io.ReadAll(io.LimitReader(resp.Body, maxOTLPResponse+1))
	if readErr != nil {
		return "", fmt.Errorf("read OTLP %s response: %w", signal, readErr)
	}
	if len(response) > maxOTLPResponse {
		return "", &permanentExportError{message: "OTLP collector response exceeds 64 KiB"}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if len(bytes.TrimSpace(response)) > 0 && !json.Valid(response) {
			return "", &permanentExportError{message: "OTLP collector returned malformed JSON on success"}
		}
		if partialResponse(response) {
			return "", &PartialRejectError{Signal: signal}
		}
		return "", nil
	}
	if retryableStatus(resp.StatusCode) {
		return resp.Header.Get("Retry-After"), fmt.Errorf("OTLP collector returned retryable HTTP %d for %s", resp.StatusCode, signal)
	}
	return "", &permanentExportError{message: fmt.Sprintf("OTLP collector returned non-retryable HTTP %d for %s", resp.StatusCode, signal)}
}

func partialResponse(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	var response struct {
		PartialSuccess json.RawMessage `json:"partialSuccess"`
	}
	return json.Unmarshal(body, &response) == nil && len(bytes.TrimSpace(response.PartialSuccess)) > 0 && string(bytes.TrimSpace(response.PartialSuccess)) != "null"
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func validateAuthorization(value string) error {
	if len(value) > 8192 {
		return &permanentExportError{message: "OTLP authorization value exceeds 8 KiB"}
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return &permanentExportError{message: "OTLP authorization value must contain only visible ASCII and spaces"}
		}
	}
	return nil
}

func validEnvName(value string) bool {
	if value == "" || value[0] >= '0' && value[0] <= '9' {
		return false
	}
	for _, r := range value {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

type parsedEndpoint struct {
	tracesURL, metricsURL string
	sinkID                string
	hostname              string
	port                  string
	allowPrivate          bool
	allowLoopback         bool
}

func parseEndpoint(raw string, allowPrivate bool) (parsedEndpoint, error) {
	if strings.TrimSpace(raw) == "" {
		return parsedEndpoint{}, fmt.Errorf("OTLP endpoint must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return parsedEndpoint{}, fmt.Errorf("OTLP endpoint must be an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return parsedEndpoint{}, fmt.Errorf("OTLP endpoint must not contain credentials, query parameters, or a fragment")
	}
	if u.RawPath != "" || strings.Contains(u.Path, "\\") || strings.Contains(u.Path, "//") || path.Clean(u.Path) != u.Path && u.Path != "" && u.Path != "/" {
		return parsedEndpoint{}, fmt.Errorf("OTLP endpoint path must be canonical and unescaped")
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" || strings.Contains(hostname, "%") {
		return parsedEndpoint{}, fmt.Errorf("OTLP endpoint host is invalid")
	}
	allowLoopback := hostname == "localhost"
	if ip := net.ParseIP(hostname); ip != nil {
		allowLoopback = ip.IsLoopback()
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowLoopback {
			return parsedEndpoint{}, fmt.Errorf("remote OTLP endpoints must use https; plaintext is allowed only on loopback")
		}
	default:
		return parsedEndpoint{}, fmt.Errorf("OTLP endpoint scheme must be https, or http on loopback")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return parsedEndpoint{}, fmt.Errorf("OTLP endpoint port is invalid")
	}
	basePath := strings.TrimSuffix(u.Path, "/")
	for _, suffix := range []string{"/v1/traces", "/v1/metrics"} {
		basePath = strings.TrimSuffix(basePath, suffix)
	}
	traces := *u
	metrics := *u
	traces.Path = basePath + "/v1/traces"
	metrics.Path = basePath + "/v1/metrics"
	canonicalHost := hostname
	if strings.Contains(canonicalHost, ":") {
		canonicalHost = "[" + canonicalHost + "]"
	}
	canonical := u.Scheme + "://" + net.JoinHostPort(strings.Trim(canonicalHost, "[]"), port) + basePath + "\x00" + SchemaVersion
	digest := sha256.Sum256([]byte(canonical))
	return parsedEndpoint{
		tracesURL: traces.String(), metricsURL: metrics.String(), sinkID: hex.EncodeToString(digest[:16]),
		hostname: hostname, port: port, allowPrivate: allowPrivate, allowLoopback: allowLoopback,
	}, nil
}

func secureHTTPClient(endpoint parsedEndpoint) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	resolver := net.DefaultResolver
	transport := &http.Transport{
		Proxy: nil, ForceAttemptHTTP2: true, MaxIdleConns: 2, MaxIdleConnsPerHost: 2,
		IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(strings.Trim(host, "[]"), endpoint.hostname) || port != endpoint.port {
			return nil, fmt.Errorf("OTLP transport refused an unexpected destination")
		}
		addresses, err := resolver.LookupIPAddr(ctx, endpoint.hostname)
		if err != nil {
			return nil, fmt.Errorf("resolve OTLP collector: %w", err)
		}
		var last error
		for _, address := range addresses {
			if !allowedCollectorIP(address.IP, endpoint.allowPrivate, endpoint.allowLoopback) {
				last = fmt.Errorf("OTLP collector resolved to a disallowed network range")
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		if last == nil {
			last = fmt.Errorf("OTLP collector resolved to no addresses")
		}
		return nil, last
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func allowedCollectorIP(ip net.IP, allowPrivate, allowLoopback bool) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() {
		return false
	}
	if allowLoopback {
		// An endpoint explicitly named localhost or a loopback literal is a
		// loopback-only promise, not permission for a poisoned hostname mapping
		// to escape to a public or private address.
		return address.IsLoopback()
	}
	if address.IsLoopback() {
		return false
	}
	if address.IsPrivate() {
		return allowPrivate
	}
	// The standard library treats deprecated site-local, translation, and
	// other unallocated IPv6 spaces as global unicast. Public IPv6 collector
	// destinations are currently allocated only from 2000::/3; fail closed on
	// everything outside that range rather than treating future or special-use
	// space as public by default.
	if address.Is6() && !publicIPv6CollectorPrefix.Contains(address) {
		return false
	}
	for _, prefix := range nonPublicCollectorPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return address.IsGlobalUnicast()
}

var publicIPv6CollectorPrefix = netip.MustParsePrefix("2000::/3")

// net.IP.IsGlobalUnicast deliberately includes a number of special-use
// address blocks. They are not public collector destinations and some (for
// example shared CGNAT and benchmarking ranges) can route inside a provider
// network, so accepting them would reopen the DNS-rebinding/SSRF boundary.
// --otlp-allow-private-network remains intentionally limited to the standard
// RFC 1918 and ULA ranges reported by netip.Addr.IsPrivate.
var nonPublicCollectorPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
}

func retryDelay(base time.Duration, attempt int, retryAfter string) time.Duration {
	if retryAfter != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
			return min(time.Duration(seconds)*time.Second, 30*time.Second)
		}
		if when, err := http.ParseTime(retryAfter); err == nil {
			return min(max(time.Until(when), 0), 30*time.Second)
		}
	}
	delay := base
	for i := 1; i < attempt && delay < 30*time.Second; i++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	var jitter [1]byte
	if _, err := rand.Read(jitter[:]); err == nil {
		// 75%-125% jitter avoids synchronized retry storms while preserving a
		// hard upper bound.
		delay = delay * time.Duration(192+int(jitter[0])/2) / 256
	}
	return delay
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
