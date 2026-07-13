package proxy_test

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/identity"
	"github.com/burnban/burnban/internal/policy"
	"github.com/burnban/burnban/internal/store"
)

const identityRequestBody = `{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`

func installIdentityTrust(t *testing.T, s *store.Store) (ed25519.PrivateKey, identity.TrustGrant) {
	t.Helper()
	publicKey, privateKey, keyID, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	grant := identity.TrustGrant{
		ProtocolVersion: 1, Revision: 1, TenantKind: "personal", TenantID: "usr_test", DeviceID: "dev_test",
		KeyID: keyID, PublicKey: identity.EncodePublicKey(publicKey), Enabled: true,
		NotBefore: now.Add(-time.Hour).Format(time.RFC3339), OnlineAt: now.Format(time.RFC3339),
		ValidUntil:  now.Add(15 * time.Minute).Format(time.RFC3339),
		Attribution: identity.Attribution{Principal: "alice@example.test", Projects: []string{"*"}, Environment: "production"},
	}
	raw, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		identity.KeyTrustGrant: string(raw), identity.KeyTrustSource: "burnban-personal:dev_test",
		identity.KeyPolicySource: "burnban-personal:dev_test",
	} {
		if err := s.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	return privateKey, grant
}

func issueIdentity(t *testing.T, privateKey ed25519.PrivateKey, grant identity.TrustGrant, rawQuery, body string) string {
	t.Helper()
	token, _, err := identity.Issue(privateKey, grant, identity.RequestBinding{
		Audience: identity.AudienceProxy, Method: http.MethodPost, Route: "/anthropic/v1/messages",
		QuerySHA256: identity.BodyDigest([]byte(rawQuery)), BodySHA256: identity.BodyDigest([]byte(body)),
	}, identity.Attribution{Projects: []string{"project-a"}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func identityPost(t *testing.T, base, path, body, token string, headers http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Burnban-Identity", token)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(raw)
}

func TestSignedIdentityEnforcesScopeStripsHeaderAndPersistsProvenance(t *testing.T) {
	var hits atomic.Int64
	var leaked atomic.Bool
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Burnban-Identity") != "" || r.Header.Get("X-Burnban-User") != "" {
			leaked.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicJSON)
	}))
	privateKey, grant := installIdentityTrust(t, s)
	applyPolicy(t, s, policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "identity", Namespace: "identity", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "alice-only", Scope: policy.Scope{
			Organization: []string{"usr_test"}, Tenant: []string{"personal:usr_test"}, Meter: []string{"dev_test"},
			Device: []string{"dev_test"}, Principal: []string{"alice@example.test"}, User: []string{"alice@example.test"},
			Environment: []string{"production"},
		},
			Match: policy.Match{Provider: policy.AccessList{Allow: []string{"anthropic"}}}}},
	})
	unsigned, _ := identityPost(t, srv.URL, "/anthropic/v1/messages", identityRequestBody, "", http.Header{
		"X-Burnban-User": {"alice@example.test"},
	})
	if unsigned.StatusCode != http.StatusUnauthorized || hits.Load() != 0 {
		t.Fatalf("unsigned status=%d hits=%d", unsigned.StatusCode, hits.Load())
	}
	token := issueIdentity(t, privateKey, grant, "", identityRequestBody)
	signed, body := identityPost(t, srv.URL, "/anthropic/v1/messages", identityRequestBody, token, nil)
	if signed.StatusCode != http.StatusOK || body != anthropicJSON || hits.Load() != 1 || leaked.Load() {
		t.Fatalf("signed status=%d hits=%d leaked=%t body=%s", signed.StatusCode, hits.Load(), leaked.Load(), body)
	}
	rows, err := s.Export(time.Unix(0, 0))
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	row := rows[0]
	if row.IdentityConfidence != "authenticated" || row.IdentityTenant != "personal:usr_test" ||
		row.IdentityDevice != "dev_test" || row.Principal != "alice@example.test" || row.Project != "project-a" {
		t.Fatalf("identity row=%+v", row)
	}
	if row.Policy == nil || !strings.Contains(row.Policy.ContextJSON, `"identity_confidence":"authenticated"`) ||
		!strings.Contains(row.Policy.ContextJSON, `"organization":"usr_test"`) ||
		!strings.Contains(row.Policy.ContextJSON, `"tenant":"personal:usr_test"`) ||
		!strings.Contains(row.Policy.ContextJSON, `"environment":"production"`) ||
		!strings.Contains(row.Policy.ContextJSON, `"principal_confidence":"authenticated"`) ||
		!strings.Contains(row.Policy.ContextJSON, `"environment_confidence":"authenticated"`) ||
		!strings.Contains(row.Policy.ContextJSON, `"user_confidence":"authenticated"`) ||
		!strings.Contains(row.Policy.ContextJSON, `"project_confidence":"self_reported"`) {
		t.Fatalf("policy metadata=%+v", row.Policy)
	}
}

func TestWildcardProjectCannotAuthenticateProjectScope(t *testing.T) {
	var hits atomic.Int64
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicJSON)
	}))
	privateKey, grant := installIdentityTrust(t, s)
	applyPolicy(t, s, policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "project", Namespace: "project", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "project-a", Scope: policy.Scope{Project: []string{"project-a"}},
			Match: policy.Match{Provider: policy.AccessList{Allow: []string{"anthropic"}}}}},
	})

	asserted := issueIdentity(t, privateKey, grant, "", identityRequestBody)
	resp, body := identityPost(t, srv.URL, "/anthropic/v1/messages", identityRequestBody, asserted, nil)
	if resp.StatusCode != http.StatusUnauthorized || hits.Load() != 0 || !strings.Contains(body, "authenticated_identity_required") {
		t.Fatalf("wildcard project status=%d hits=%d body=%s", resp.StatusCode, hits.Load(), body)
	}

	// Once the server grant names the exact project, the same field becomes an
	// authenticated scope value and the request is admitted.
	grant.Revision++
	grant.Attribution.Projects = []string{"project-a"}
	raw, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(identity.KeyTrustGrant, string(raw)); err != nil {
		t.Fatal(err)
	}
	exact := issueIdentity(t, privateKey, grant, "", identityRequestBody)
	resp, body = identityPost(t, srv.URL, "/anthropic/v1/messages", identityRequestBody, exact, nil)
	if resp.StatusCode != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("exact project status=%d hits=%d body=%s", resp.StatusCode, hits.Load(), body)
	}
}

func TestSignedIdentityRejectsReplayBindingTamperAndUnsignedOverrides(t *testing.T) {
	var hits atomic.Int64
	srv, s := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicJSON)
	}))
	privateKey, grant := installIdentityTrust(t, s)

	replay := issueIdentity(t, privateKey, grant, "", identityRequestBody)
	if resp, _ := identityPost(t, srv.URL, "/anthropic/v1/messages", identityRequestBody, replay, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("first replay status=%d", resp.StatusCode)
	}
	if resp, _ := identityPost(t, srv.URL, "/anthropic/v1/messages", identityRequestBody, replay, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status=%d", resp.StatusCode)
	}

	queryBound := issueIdentity(t, privateKey, grant, "", identityRequestBody)
	if resp, _ := identityPost(t, srv.URL, "/anthropic/v1/messages?changed=1", identityRequestBody, queryBound, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query tamper status=%d", resp.StatusCode)
	}
	// A failed binding check must not burn the nonce before the exact request.
	if resp, _ := identityPost(t, srv.URL, "/anthropic/v1/messages", identityRequestBody, queryBound, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("exact retry status=%d", resp.StatusCode)
	}

	bodyBound := issueIdentity(t, privateKey, grant, "", identityRequestBody)
	if resp, _ := identityPost(t, srv.URL, "/anthropic/v1/messages", `{"model":"claude-opus-4-7","messages":[]}`, bodyBound, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("body tamper status=%d", resp.StatusCode)
	}
	routeBound := issueIdentity(t, privateKey, grant, "", identityRequestBody)
	if resp, _ := identityPost(t, srv.URL, "/anthropic/v1/other", identityRequestBody, routeBound, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("route tamper status=%d", resp.StatusCode)
	}

	override := issueIdentity(t, privateKey, grant, "", identityRequestBody)
	if resp, _ := identityPost(t, srv.URL, "/anthropic/v1/messages", identityRequestBody, override, http.Header{
		"X-Burnban-User": {"mallory@example.test"},
	}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("override status=%d", resp.StatusCode)
	}
	if resp, _ := identityPost(t, srv.URL, "/anthropic/v1/messages", identityRequestBody, override, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("override exact retry status=%d", resp.StatusCode)
	}
	if hits.Load() != 3 {
		t.Fatalf("upstream hits=%d, want 3", hits.Load())
	}
	rows, err := s.Export(time.Unix(0, 0))
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
}
