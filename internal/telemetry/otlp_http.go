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

// PartialRejectError means an OTLP receiver reported a non-zero rejected item
// count. OTLP forbids retrying that signal request. A populated partialSuccess
// with a zero count is instead a full success (and may carry a warning).
type PartialRejectError struct {
	Signal           string
	RejectedItems    int64
	CollectorMessage string
}

func (e *PartialRejectError) Error() string {
	return fmt.Sprintf("OTLP collector rejected %d items for %s; request will not be retried", e.RejectedItems, e.Signal)
}

type permanentExportError struct{ message string }

func (e *permanentExportError) Error() string { return e.message }

// SignalExportResult separates request terminality from the aggregate error.
// The worker durably advances only the signal whose request reached a terminal
// OTLP outcome, so a trace failure cannot suppress or cause a retry of an
// independently completed metrics request (and vice versa).
type SignalExportResult struct {
	Attempted     bool
	Terminal      bool
	Failed        bool
	RejectedItems int64
	Warning       string
}

type ExportResult struct {
	Traces  SignalExportResult
	Metrics SignalExportResult
}

type responseOutcome struct {
	RejectedItems int64
	Message       string
}

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

func (e *HTTPExporter) Export(ctx context.Context, batch Batch) (ExportResult, error) {
	var result ExportResult
	if len(batch.Events) == 0 {
		return result, nil
	}
	var exportErrors []error
	if batch.exports(SignalTraces) {
		result.Traces.Attempted = true
		traces, err := buildTracePayload(batch, e.serviceName, e.serviceVersion, e.random)
		if err != nil {
			result.Traces.Failed = true
			exportErrors = append(exportErrors, err)
		} else if len(traces) > maxOTLPPayload {
			result.Traces.Terminal, result.Traces.Failed = true, true
			exportErrors = append(exportErrors, &permanentExportError{message: "OTLP traces batch exceeds the 8 MiB payload bound; lower --otlp-batch"})
		} else {
			result.Traces, err = e.exportSignal(ctx, e.tracesURL, "traces", traces)
			if result.Traces.RejectedItems > int64(len(batch.Events)) {
				result.Traces.RejectedItems = 0
				err = &permanentExportError{message: "OTLP collector rejected more spans than the request contained"}
			}
			if err != nil {
				exportErrors = append(exportErrors, err)
			}
		}
	}
	if batch.exports(SignalMetrics) {
		result.Metrics.Attempted = true
		metrics, err := buildMetricsPayload(batch, e.serviceName, e.serviceVersion, time.Now().UTC())
		if err != nil {
			result.Metrics.Failed = true
			exportErrors = append(exportErrors, err)
		} else if len(metrics) > maxOTLPPayload {
			result.Metrics.Terminal, result.Metrics.Failed = true, true
			exportErrors = append(exportErrors, &permanentExportError{message: "OTLP metrics batch exceeds the 8 MiB payload bound; lower --otlp-batch"})
		} else {
			result.Metrics, err = e.exportSignal(ctx, e.metricsURL, "metrics", metrics)
			metricItems := int64(len(batch.Events)) * 3
			if batch.DroppedRows > 0 {
				metricItems++
			}
			if result.Metrics.RejectedItems > metricItems {
				result.Metrics.RejectedItems = 0
				err = &permanentExportError{message: "OTLP collector rejected more data points than the request contained"}
			}
			if err != nil {
				exportErrors = append(exportErrors, err)
			}
		}
	}
	return result, errors.Join(exportErrors...)
}

func (e *HTTPExporter) exportSignal(ctx context.Context, endpoint, signal string, body []byte) (SignalExportResult, error) {
	result := SignalExportResult{Attempted: true}
	outcome, err := e.postWithRetry(ctx, endpoint, signal, body)
	if err == nil {
		result.Terminal = true
		result.Warning = outcome.Message
		return result, nil
	}
	result.Failed = true
	var permanent *permanentExportError
	var partial *PartialRejectError
	if errors.As(err, &permanent) || errors.As(err, &partial) {
		result.Terminal = true
	}
	if partial != nil {
		result.RejectedItems = partial.RejectedItems
		result.Warning = partial.CollectorMessage
	}
	return result, err
}

func (e *HTTPExporter) postWithRetry(ctx context.Context, endpoint, signal string, body []byte) (responseOutcome, error) {
	var last error
	var lastOutcome responseOutcome
	var delay time.Duration
	for attempt := 0; attempt < e.maxAttempts; attempt++ {
		if delay > 0 {
			if err := e.sleep(ctx, delay); err != nil {
				return responseOutcome{}, err
			}
		}
		retryAfter, outcome, err := e.post(ctx, endpoint, signal, body)
		if err == nil {
			return outcome, nil
		}
		last, lastOutcome = err, outcome
		var permanent *permanentExportError
		var partial *PartialRejectError
		if errors.As(err, &permanent) || errors.As(err, &partial) {
			return outcome, err
		}
		delay = retryDelay(e.baseBackoff, attempt+1, retryAfter)
	}
	return lastOutcome, last
}

func (e *HTTPExporter) post(ctx context.Context, endpoint, signal string, body []byte) (string, responseOutcome, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, e.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", responseOutcome{}, &permanentExportError{message: "build OTLP request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if value, present := os.LookupEnv(e.authorizationEnv); present && value != "" {
		if err := validateAuthorization(value); err != nil {
			return "", responseOutcome{}, err
		}
		req.Header.Set("Authorization", value)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		if attemptCtx.Err() != nil {
			return "", responseOutcome{}, fmt.Errorf("OTLP %s request timed out or was canceled", signal)
		}
		return "", responseOutcome{}, fmt.Errorf("OTLP %s connection failed", signal)
	}
	defer resp.Body.Close()
	response, readErr := io.ReadAll(io.LimitReader(resp.Body, maxOTLPResponse+1))
	if readErr != nil {
		return "", responseOutcome{}, fmt.Errorf("read OTLP %s response: %w", signal, readErr)
	}
	if len(response) > maxOTLPResponse {
		return "", responseOutcome{}, &permanentExportError{message: "OTLP collector response exceeds 64 KiB"}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if !json.Valid(response) {
			return "", responseOutcome{}, &permanentExportError{message: "OTLP collector returned malformed JSON on success"}
		}
		outcome, err := parseSuccessResponse(response, signal)
		if err != nil {
			return "", outcome, err
		}
		return "", outcome, nil
	}
	if retryableStatus(resp.StatusCode) {
		return resp.Header.Get("Retry-After"), responseOutcome{}, fmt.Errorf("OTLP collector returned retryable HTTP %d for %s", resp.StatusCode, signal)
	}
	return "", responseOutcome{}, &permanentExportError{message: fmt.Sprintf("OTLP collector returned non-retryable HTTP %d for %s", resp.StatusCode, signal)}
}

func parseSuccessResponse(body []byte, signal string) (responseOutcome, error) {
	var response struct {
		PartialSuccess json.RawMessage `json:"partialSuccess"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return responseOutcome{}, &permanentExportError{message: "OTLP collector returned malformed JSON on success"}
	}
	raw := bytes.TrimSpace(response.PartialSuccess)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return responseOutcome{}, nil
	}
	var partial map[string]json.RawMessage
	if err := json.Unmarshal(raw, &partial); err != nil || partial == nil {
		return responseOutcome{}, &permanentExportError{message: "OTLP collector returned malformed partialSuccess JSON"}
	}
	var rejectedField string
	switch signal {
	case "traces":
		rejectedField = "rejectedSpans"
	case "metrics":
		rejectedField = "rejectedDataPoints"
	default:
		return responseOutcome{}, &permanentExportError{message: "unsupported OTLP signal response"}
	}
	for field := range partial {
		if field != rejectedField && field != "errorMessage" {
			return responseOutcome{}, &permanentExportError{message: "OTLP collector returned an unknown or cross-signal partialSuccess field"}
		}
	}
	rejectedRaw := partial[rejectedField]
	rejected, err := parseProtoInt64(rejectedRaw)
	if err != nil || rejected < 0 {
		return responseOutcome{}, &permanentExportError{message: "OTLP collector returned an invalid rejected item count"}
	}
	var errorMessage string
	if messageRaw, present := partial["errorMessage"]; present {
		if err := json.Unmarshal(messageRaw, &errorMessage); err != nil {
			return responseOutcome{}, &permanentExportError{message: "OTLP collector returned an invalid partialSuccess error message"}
		}
	}
	message := safeLabel(strings.TrimSpace(errorMessage))
	if len(message) > 256 {
		message = message[:256]
	}
	outcome := responseOutcome{RejectedItems: rejected, Message: message}
	if rejected == 0 {
		// The official signal protos define an empty partialSuccess as
		// equivalent to it being absent. A zero count plus a message is a
		// warning on a fully accepted request, not a rejection.
		return outcome, nil
	}
	return outcome, &PartialRejectError{Signal: signal, RejectedItems: rejected, CollectorMessage: message}
}

func parseProtoInt64(raw json.RawMessage) (int64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, nil
	}
	var encoded string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return 0, err
		}
		return strconv.ParseInt(encoded, 10, 64)
	}
	return strconv.ParseInt(string(raw), 10, 64)
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
