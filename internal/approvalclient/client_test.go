package approvalclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientCreatesOnlyPendingOwnMeterRequest(t *testing.T) {
	fixed := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	meterID := "mtr_0123456789abcdef"
	token := "bbt_" + strings.Repeat("x", 43)
	hook := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/base/api/v1/meter/approvals" ||
			r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Burnban-Meter-ID") != meterID {
			t.Errorf("request method=%s path=%s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		defer r.Body.Close()
		var body struct {
			ScopeType   string  `json:"scope_type"`
			ScopeValue  string  `json:"scope_value"`
			Window      string  `json:"window"`
			IncreaseUSD float64 `json:"increase_usd"`
			Reason      string  `json:"reason"`
			Ticket      string  `json:"ticket"`
			ValidUntil  string  `json:"valid_until"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.ScopeType != "meter" || body.ScopeValue != meterID || body.ValidUntil != fixed.Add(45*time.Minute).Format(time.RFC3339) {
			t.Errorf("body=%+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Response{
			ID: "apr_receipt", ScopeType: body.ScopeType, ScopeValue: body.ScopeValue, Window: body.Window,
			IncreaseUSD: body.IncreaseUSD, Requester: "meter:" + meterID, Reason: body.Reason, Ticket: body.Ticket,
			RequestedAt: fixed.Format(time.RFC3339), ValidUntil: body.ValidUntil, Status: "pending",
		})
	}))
	defer hook.Close()
	client, err := NewWithHTTP(hook.URL+"/base", meterID, token, hook.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.now = func() time.Time { return fixed }
	out, err := client.Request(context.Background(), Request{
		Window: "daily", IncreaseUSD: 12.5, Reason: "bounded deployment", Ticket: "OPS-4", ExpiresIn: 45 * time.Minute,
	})
	if err != nil || out.ID != "apr_receipt" || out.Status != "pending" {
		t.Fatalf("receipt=%+v err=%v", out, err)
	}
}

func TestClientRejectsRedirectMismatchedReceiptAndUnsafeInput(t *testing.T) {
	meterID := "mtr_0123456789abcdef"
	token := "bbt_" + strings.Repeat("x", 43)
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	client, err := New(redirect.URL, meterID, token)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Request(context.Background(), Request{
		Window: "daily", IncreaseUSD: 1, Reason: "reason", Ticket: "T-1", ExpiresIn: time.Hour,
	})
	if err == nil || redirected {
		t.Fatalf("redirect error=%v followed=%t", err, redirected)
	}

	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Response{
			ID: "apr_bad", ScopeType: "organization", Window: "daily", IncreaseUSD: 1,
			Requester: "meter:" + meterID, Reason: "reason", Ticket: "T-1",
			RequestedAt: time.Now().UTC().Format(time.RFC3339), ValidUntil: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), Status: "approved",
		})
	}))
	defer bad.Close()
	client, err = NewWithHTTP(bad.URL, meterID, token, bad.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(context.Background(), Request{Window: "daily", IncreaseUSD: 1, Reason: "reason", Ticket: "T-1", ExpiresIn: time.Hour}); err == nil {
		t.Fatal("mismatched approved receipt was accepted")
	}
	for _, in := range []Request{
		{Window: "yearly", IncreaseUSD: 1, Reason: "reason", Ticket: "T", ExpiresIn: time.Hour},
		{Window: "daily", IncreaseUSD: 0, Reason: "reason", Ticket: "T", ExpiresIn: time.Hour},
		{Window: "daily", IncreaseUSD: 1, Reason: "bad\nreason", Ticket: "T", ExpiresIn: time.Hour},
		{Window: "daily", IncreaseUSD: 1, Reason: "reason", Ticket: "T", ExpiresIn: time.Minute},
	} {
		if _, err := client.Request(context.Background(), in); err == nil {
			t.Fatalf("unsafe input accepted: %+v", in)
		}
	}
}

type leakingTransport struct{ secret string }

func (t leakingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport leaked " + t.secret)
}

func TestTransportErrorsRedactEndpointAndToken(t *testing.T) {
	meterID := "mtr_0123456789abcdef"
	token := "bbt_" + strings.Repeat("s", 43)
	endpointSecret := "private-webhook-like-path"
	client, err := NewWithHTTP("https://teams.example/"+endpointSecret, meterID, token,
		&http.Client{Transport: leakingTransport{secret: token + endpointSecret}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Request(context.Background(), Request{Window: "daily", IncreaseUSD: 1, Reason: "reason", Ticket: "T-1", ExpiresIn: time.Hour})
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), endpointSecret) {
		t.Fatalf("unsafe error=%v", err)
	}
}

func TestNewHandlesCustomizedDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = leakingTransport{secret: "custom-default"}
	t.Cleanup(func() { http.DefaultTransport = original })
	client, err := New("https://approvals.example.test", "mtr_test", "bbt_01234567890123456789012345678901")
	if err != nil || client == nil {
		t.Fatalf("custom default transport caused construction failure: client=%v err=%v", client, err)
	}
}

func TestResponseRejectsDuplicateOrUnknownFields(t *testing.T) {
	for name, suffix := range map[string]string{
		"duplicate": `,"status":"approved"`,
		"unknown":   `,"credential":"secret"`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{"id":"apr_test","scope_type":"meter","scope_value":"mtr_test","window":"daily","increase_usd":1,"requester":"meter:mtr_test","reason":"reason","ticket":"T-1","requested_at":"2026-07-13T10:00:00Z","valid_until":"2026-07-13T11:00:00Z","break_glass":false,"status":"pending"%s}`, suffix)
			}))
			defer server.Close()
			client, err := NewWithHTTP(server.URL, "mtr_test", "bbt_01234567890123456789012345678901", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			client.now = func() time.Time { return time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC) }
			_, err = client.Request(context.Background(), Request{
				Window: "daily", IncreaseUSD: 1, Reason: "reason", Ticket: "T-1", ExpiresIn: time.Hour,
			})
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "approved") {
				t.Fatalf("ambiguous receipt error=%v", err)
			}
		})
	}
}
