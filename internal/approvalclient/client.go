// Package approvalclient implements the narrow write boundary used by the MCP
// server to request (never grant) a temporary Team budget exception.
package approvalclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxResponseBytes = 1 << 20

type Request struct {
	Window      string
	IncreaseUSD float64
	Reason      string
	Ticket      string
	ExpiresIn   time.Duration
}

type Response struct {
	ID          string  `json:"id"`
	ScopeType   string  `json:"scope_type"`
	ScopeValue  string  `json:"scope_value"`
	Window      string  `json:"window"`
	IncreaseUSD float64 `json:"increase_usd"`
	Requester   string  `json:"requester"`
	Reason      string  `json:"reason"`
	Ticket      string  `json:"ticket"`
	RequestedAt string  `json:"requested_at"`
	ValidUntil  string  `json:"valid_until"`
	BreakGlass  bool    `json:"break_glass"`
	Status      string  `json:"status"`
}

type Requester interface {
	Request(context.Context, Request) (Response, error)
}

type Client struct {
	endpoint string
	meterID  string
	token    string
	http     *http.Client
	now      func() time.Time
}

func New(baseURL, meterID, token string) (*Client, error) {
	transport := cloneDefaultTransport()
	return newClient(baseURL, meterID, token, &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	})
}

func cloneDefaultTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// NewWithHTTP is intended for a caller-owned client such as a TLS test server.
// Production callers should use New, which rejects redirects and has a timeout.
func NewWithHTTP(baseURL, meterID, token string, client *http.Client) (*Client, error) {
	if client == nil {
		return nil, errors.New("approval HTTP client is required")
	}
	return newClient(baseURL, meterID, token, client)
}

func newClient(baseURL, meterID, token string, client *http.Client) (*Client, error) {
	baseURL, meterID, token = strings.TrimSpace(baseURL), strings.TrimSpace(meterID), strings.TrimSpace(token)
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("approval base URL must be an absolute origin without credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname())) {
		return nil, errors.New("approval base URL must use HTTPS (HTTP is allowed only on loopback)")
	}
	if !safeText(meterID, 100, true) || !strings.HasPrefix(meterID, "mtr_") {
		return nil, errors.New("approval meter ID is invalid")
	}
	if !strings.HasPrefix(token, "bbt_") || len(token) < 32 || len(token) > 200 || !safeText(token, 200, true) {
		return nil, errors.New("approval meter token is invalid")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/v1/meter/approvals"
	parsed.RawPath = ""
	return &Client{endpoint: parsed.String(), meterID: meterID, token: token, http: client, now: time.Now}, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) Request(ctx context.Context, in Request) (Response, error) {
	in.Window = strings.ToLower(strings.TrimSpace(in.Window))
	in.Reason = strings.TrimSpace(in.Reason)
	in.Ticket = strings.TrimSpace(in.Ticket)
	if in.Window != "daily" && in.Window != "weekly" && in.Window != "monthly" {
		return Response{}, errors.New("window must be daily, weekly, or monthly")
	}
	if math.IsNaN(in.IncreaseUSD) || math.IsInf(in.IncreaseUSD, 0) || in.IncreaseUSD < 0.01 || in.IncreaseUSD > 1e9 {
		return Response{}, errors.New("increase_usd must be a finite amount between $0.01 and $1 billion")
	}
	if !safeText(in.Reason, 1000, true) || !safeText(in.Ticket, 200, true) {
		return Response{}, errors.New("reason and ticket are required and must contain safe text")
	}
	if in.ExpiresIn < 5*time.Minute || in.ExpiresIn > 30*24*time.Hour {
		return Response{}, errors.New("expiry must be between 5 minutes and 30 days")
	}
	now := c.now().UTC()
	payload := struct {
		ScopeType   string  `json:"scope_type"`
		ScopeValue  string  `json:"scope_value"`
		Window      string  `json:"window"`
		IncreaseUSD float64 `json:"increase_usd"`
		Reason      string  `json:"reason"`
		Ticket      string  `json:"ticket"`
		ValidUntil  string  `json:"valid_until"`
	}{"meter", c.meterID, in.Window, in.IncreaseUSD, in.Reason, in.Ticket, now.Add(in.ExpiresIn).Format(time.RFC3339)}
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, errors.New("build approval request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Burnban-Meter-ID", c.meterID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "burnban-mcp/approval-request")
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error includes the secret-bearing endpoint and must not escape.
		return Response{}, errors.New("approval service transport failed")
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return Response{}, errors.New("read approval service response")
	}
	if len(raw) > maxResponseBytes {
		return Response{}, errors.New("approval service response exceeded 1 MiB")
	}
	if resp.StatusCode != http.StatusCreated {
		return Response{}, fmt.Errorf("approval service returned HTTP %d", resp.StatusCode)
	}
	var out Response
	if err := rejectAmbiguousResponseJSON(raw); err != nil {
		return Response{}, errors.New("approval service returned ambiguous JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return Response{}, errors.New("approval service returned invalid JSON")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Response{}, errors.New("approval service returned trailing data")
	}
	if !safeText(out.ID, 100, true) || !strings.HasPrefix(out.ID, "apr_") || out.ScopeType != "meter" ||
		out.ScopeValue != c.meterID || out.Requester != "meter:"+c.meterID || out.Window != in.Window ||
		out.IncreaseUSD != in.IncreaseUSD || out.Reason != in.Reason || out.Ticket != in.Ticket ||
		out.ValidUntil != payload.ValidUntil || out.Status != "pending" || out.BreakGlass {
		return Response{}, errors.New("approval service returned a mismatched request receipt")
	}
	requestedAt, requestErr := time.Parse(time.RFC3339, out.RequestedAt)
	validUntil, validErr := time.Parse(time.RFC3339, out.ValidUntil)
	if requestErr != nil || validErr != nil || requestedAt.Before(now.Add(-5*time.Minute)) || requestedAt.After(now.Add(5*time.Minute)) ||
		validUntil.Before(requestedAt.Add(5*time.Minute)) || validUntil.After(requestedAt.Add(30*24*time.Hour)) {
		return Response{}, errors.New("approval service returned an invalid request interval")
	}
	return out, nil
}

func rejectAmbiguousResponseJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return errors.New("response must be an object")
	}
	allowed := map[string]struct{}{
		"id": {}, "scope_type": {}, "scope_value": {}, "window": {}, "increase_usd": {},
		"requester": {}, "reason": {}, "ticket": {}, "requested_at": {}, "valid_until": {},
		"break_glass": {}, "status": {},
	}
	seen := make(map[string]struct{}, len(allowed))
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("response key is not a string")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown response field")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate response field")
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return errors.New("response object is not terminated")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("response contains trailing data")
	}
	return nil
}

func safeText(value string, max int, required bool) bool {
	if value != strings.TrimSpace(value) || len(value) > max || !utf8.ValidString(value) || required && value == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			return false
		}
	}
	return true
}
