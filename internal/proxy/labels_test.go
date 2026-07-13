package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/policy"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

func labelTestProxy(t *testing.T, response []byte, prices *pricing.Table) (*httptest.Server, *store.Store, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	}))
	t.Cleanup(upstream.Close)
	s, err := store.Open(filepath.Join(t.TempDir(), "labels.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p, err := New(s, prices, map[string]Upstream{
		"openai": {URL: upstream.URL, Shape: "openai"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return srv, s, &hits
}

func labelTestPost(t *testing.T, base, path string, body []byte, headers http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
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
	return resp.StatusCode, string(responseBody)
}

func TestNewRejectsUnsafeUpstreamRouteNames(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "route-names.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, name := range []string{"", ".", "..", "-hidden", "_hidden", "has/slash", "has space", strings.Repeat("a", 65)} {
		t.Run(name, func(t *testing.T) {
			_, err := New(s, &pricing.Table{}, map[string]Upstream{
				name: {URL: "https://example.test", Shape: "openai"},
			})
			if err == nil {
				t.Fatalf("New accepted unsafe upstream route name %q", name)
			}
		})
	}
	if _, err := New(s, &pricing.Table{}, map[string]Upstream{
		"safe.route_name-1": {URL: "https://example.test", Shape: "openai"},
	}); err != nil {
		t.Fatalf("New rejected safe upstream route name: %v", err)
	}
}

func TestExplicitIdentityBoundariesRejectBeforeForwarding(t *testing.T) {
	response := []byte(`{"model":"known","usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	prices := &pricing.Table{Models: map[string]pricing.Price{
		"known": {InputPerMTok: 1, OutputPerMTok: 2},
	}}
	srv, s, hits := labelTestProxy(t, response, prices)
	body := []byte(`{"model":"known","max_tokens":5}`)

	// Agent lands exactly on the rune ceiling; session lands exactly on the
	// UTF-8 byte ceiling with complete four-byte code points.
	agent := strings.Repeat("a", maxExplicitIdentityRunes)
	session := strings.Repeat("\U00010000", maxExplicitIdentityBytes/4)
	status, responseBody := labelTestPost(t, srv.URL, "/openai/v1/chat/completions", body, http.Header{
		"X-Burnban-Agent":   {agent},
		"X-Burnban-Session": {session},
	})
	if status != http.StatusOK {
		t.Fatalf("boundary request status=%d body=%q", status, responseBody)
	}
	rows, err := s.Export(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 || len(rows) != 1 || rows[0].Agent != agent || rows[0].Session != session {
		t.Fatalf("boundary attribution hits=%d rows=%+v", hits.Load(), rows)
	}

	tests := []struct {
		name    string
		headers http.Header
		field   string
	}{
		{
			name: "agent over rune limit",
			headers: http.Header{"X-Burnban-Agent": {
				strings.Repeat("a", maxExplicitIdentityRunes+1),
			}},
			field: "x-burnban-agent",
		},
		{
			name: "session over byte limit without split rune",
			headers: http.Header{"X-Burnban-Session": {
				strings.Repeat("\U00010000", maxExplicitIdentityBytes/4+1),
			}},
			field: "x-burnban-session",
		},
		{
			name:    "ambiguous agent fields",
			headers: http.Header{"X-Burnban-Agent": {"first", "second"}},
			field:   "x-burnban-agent",
		},
		{
			name:    "ambiguous session fields",
			headers: http.Header{"X-Burnban-Session": {"first", "second"}},
			field:   "x-burnban-session",
		},
		{
			name:    "format control",
			headers: http.Header{"X-Burnban-Agent": {"agent\u202ename"}},
			field:   "x-burnban-agent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, responseBody := labelTestPost(t, srv.URL, "/openai/v1/chat/completions", body, tt.headers)
			if status != http.StatusBadRequest || !strings.Contains(responseBody, tt.field) {
				t.Fatalf("status=%d body=%q", status, responseBody)
			}
			if hits.Load() != 1 {
				t.Fatalf("oversized/ambiguous identity reached upstream: hits=%d", hits.Load())
			}
			rows, err := s.Export(time.Unix(0, 0))
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("rejected identity was persisted: rows=%d", len(rows))
			}
		})
	}
	if err := validateExplicitIdentity(string([]byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 identity was accepted")
	}
}

func TestProviderDerivedLabelsAreBoundedAfterFullModelPricing(t *testing.T) {
	fullModel := strings.Repeat("模型", 80) + "\u202e" + strings.Repeat("x", 40)
	serviceTier := strings.Repeat("級", 140) + "\npriority"
	inferenceGeo := strings.Repeat("域", 140) + "\ue000"
	response, err := json.Marshal(map[string]any{
		"model": fullModel, "service_tier": serviceTier,
		"inference_geo": inferenceGeo,
		"usage":         map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	prices := &pricing.Table{Models: map[string]pricing.Price{
		fullModel: {InputPerMTok: 2, OutputPerMTok: 4},
	}}
	srv, s, hits := labelTestProxy(t, response, prices)
	if err := s.SetSetting(budget.KeyDailyCapUSD, "100"); err != nil {
		t.Fatal(err)
	}
	requestBody, err := json.Marshal(map[string]any{
		"model": fullModel, "max_tokens": 5, "inference_geo": inferenceGeo,
	})
	if err != nil {
		t.Fatal(err)
	}
	userAgent := strings.Repeat("客", 140) + "\u202eclient"
	longRoute := "/openai/v1/chat/" + strings.Repeat("segment", 80)
	status, responseBody := labelTestPost(t, srv.URL, longRoute, requestBody, http.Header{
		"User-Agent": {userAgent},
	})
	if status != http.StatusOK {
		t.Fatalf("full-model priced request status=%d body=%q", status, responseBody)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits=%d", hits.Load())
	}
	rows, err := s.Export(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	row := rows[0]
	if row.PricingState != store.PricingPriced || row.CostUSD <= 0 {
		t.Fatalf("full model ID was not priced before bounding: %+v", row)
	}
	for name, value := range map[string]string{
		"agent": row.Agent, "model": row.Model, "route": row.Route,
		"service tier": row.ServiceTier, "inference geo": row.InferenceGeo,
		"provider": row.Provider,
	} {
		assertSafeBoundedLabel(t, name, value)
	}
	if row.Model == fullModel || row.Agent == userAgent || row.Route == longRoute ||
		row.ServiceTier == serviceTier || row.InferenceGeo == inferenceGeo {
		t.Fatalf("derived labels were retained without bounding: %+v", row)
	}
	if row.Model != persistedLabel(fullModel) || row.Agent != persistedLabel(userAgent) ||
		row.ServiceTier != persistedLabel(serviceTier) || row.InferenceGeo != persistedLabel(inferenceGeo) {
		t.Fatalf("persisted labels are not deterministic: %+v", row)
	}
	if persistedLabel(fullModel) == persistedLabel(fullModel+"different") {
		t.Fatal("different oversized model IDs collapsed to one display label")
	}
}

func TestPolicyMatchesCompleteModelInsteadOfTruncatedDisplayLabel(t *testing.T) {
	model := strings.Repeat("a", maxPersistedLabelBytes+32) + "-blocked-tail"
	response := []byte(`{"model":"known","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	prices := &pricing.Table{Models: map[string]pricing.Price{
		model: {InputPerMTok: 1, OutputPerMTok: 1},
	}}
	srv, s, hits := labelTestProxy(t, response, prices)
	document := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "long-model", Namespace: "long-model", Revision: 1},
		Mode:     policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "deny-tail", Match: policy.Match{
			Model: policy.AccessList{Deny: []string{"*-blocked-tail"}},
		}}},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := policy.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPolicyDocument(store.PolicyDocumentRecord{
		APIVersion: compiled.Document.APIVersion, Name: compiled.Document.Metadata.Name,
		Namespace: compiled.Document.Metadata.Namespace, Revision: compiled.Document.Metadata.Revision,
		Digest: compiled.Digest, Source: "local", DocumentJSON: string(compiled.Canonical),
	}); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"model": model, "max_tokens": 1})
	if err != nil {
		t.Fatal(err)
	}
	status, responseBody := labelTestPost(t, srv.URL, "/openai/v1/chat/completions", body, nil)
	if status != http.StatusForbidden || hits.Load() != 0 {
		t.Fatalf("long model bypassed deny rule: status=%d hits=%d body=%q", status, hits.Load(), responseBody)
	}
}

func TestMalformedOrUnboundedAdmissionMetadataIsRejected(t *testing.T) {
	response := []byte(`{"model":"known","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	prices := &pricing.Table{Models: map[string]pricing.Price{"known": {InputPerMTok: 1, OutputPerMTok: 1}}}
	srv, _, hits := labelTestProxy(t, response, prices)
	for name, body := range map[string][]byte{
		"wrong model type": []byte(`{"model":7,"max_tokens":1}`),
		"oversized model":  []byte(`{"model":"` + strings.Repeat("x", maxPolicyAdmissionLabelBytes+1) + `","max_tokens":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			status, _ := labelTestPost(t, srv.URL, "/openai/v1/chat/completions", body, nil)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400", status)
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid admission metadata reached upstream: hits=%d", hits.Load())
	}
}

func assertSafeBoundedLabel(t *testing.T, name, value string) {
	t.Helper()
	if !utf8.ValidString(value) || len(value) > maxPersistedLabelBytes ||
		utf8.RuneCountInString(value) > maxPersistedLabelRunes {
		t.Fatalf("%s label exceeds bounds: bytes=%d runes=%d valid=%t value=%q",
			name, len(value), utf8.RuneCountInString(value), utf8.ValidString(value), value)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			t.Fatalf("%s label retained unsafe rune %U: %q", name, r, value)
		}
	}
}
