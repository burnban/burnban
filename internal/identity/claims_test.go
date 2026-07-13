package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func testClaim(t *testing.T) (ed25519.PrivateKey, Claims, TrustGrant, RequestBinding, time.Time) {
	t.Helper()
	pub, privateKey, keyID, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	binding := RequestBinding{Audience: AudienceProxy, Method: "POST", Route: "/openai/v1/chat/completions", BodySHA256: BodyDigest([]byte(`{"model":"gpt"}`))}
	claims, err := NewClaims(now, "personal", "usr_test", "dev_test", keyID, binding)
	if err != nil {
		t.Fatal(err)
	}
	claims.Principal = "person@example.test"
	claims.Project = "project-a"
	claims.Environment = "production"
	grant := TrustGrant{
		ProtocolVersion: ProtocolVersion, Revision: 4, TenantKind: "personal", TenantID: "usr_test",
		DeviceID: "dev_test", KeyID: keyID, PublicKey: EncodePublicKey(pub), Enabled: true,
		NotBefore: now.Add(-time.Minute).Format(time.RFC3339), ValidUntil: now.Add(DefaultTrustTTL).Format(time.RFC3339),
		OnlineAt: now.Format(time.RFC3339), Attribution: Attribution{Principal: claims.Principal, Projects: []string{"project-a"}, Environment: claims.Environment},
	}
	return privateKey, claims, grant, binding, now
}

func TestClaimRoundTripAndReplay(t *testing.T) {
	privateKey, claims, grant, binding, now := testClaim(t)
	token, err := Sign(privateKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	consume := func(kid, jti string, _ time.Time) error {
		key := kid + ":" + jti
		if seen[key] {
			return ErrReplay
		}
		seen[key] = true
		return nil
	}
	verified, err := Verify(token, grant, binding, now, consume)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.PrincipalTrusted || !verified.ProjectTrusted || !verified.EnvironmentTrusted ||
		verified.ServiceAccountTrusted || verified.Claims.JTI != claims.JTI {
		t.Fatalf("verified=%+v", verified)
	}
	if _, err := Verify(token, grant, binding, now, consume); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay error=%v", err)
	}
}

func TestWildcardProjectIsSignedButNotServerAuthorized(t *testing.T) {
	privateKey, claims, grant, binding, now := testClaim(t)
	grant.Attribution.Projects = []string{"*"}
	token, err := Sign(privateKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(token, grant, binding, now, func(string, string, time.Time) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !verified.PrincipalTrusted || verified.ProjectTrusted {
		t.Fatalf("wildcard project provenance=%+v", verified)
	}

	// An explicit project alongside the wildcard upgrades only that exact
	// value to server-authorized provenance.
	grant.Attribution.Projects = []string{"*", claims.Project}
	verified, err = Verify(token, grant, binding, now, func(string, string, time.Time) error { return nil })
	if err != nil || !verified.ProjectTrusted {
		t.Fatalf("exact project provenance=%+v err=%v", verified, err)
	}
}

func TestClaimRejectsTamperingAndBindingChanges(t *testing.T) {
	privateKey, claims, grant, binding, now := testClaim(t)
	token, err := Sign(privateKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	consume := func(string, string, time.Time) error { return nil }
	for name, mutate := range map[string]func() (string, TrustGrant, RequestBinding, time.Time){
		"body": func() (string, TrustGrant, RequestBinding, time.Time) {
			b := binding
			b.BodySHA256 = BodyDigest([]byte("other"))
			return token, grant, b, now
		},
		"route": func() (string, TrustGrant, RequestBinding, time.Time) {
			b := binding
			b.Route += "/other"
			return token, grant, b, now
		},
		"method": func() (string, TrustGrant, RequestBinding, time.Time) {
			b := binding
			b.Method = "GET"
			return token, grant, b, now
		},
		"audience": func() (string, TrustGrant, RequestBinding, time.Time) {
			b := binding
			b.Audience = "another-service"
			return token, grant, b, now
		},
		"query": func() (string, TrustGrant, RequestBinding, time.Time) {
			b := binding
			b.QuerySHA256 = BodyDigest([]byte("changed=1"))
			return token, grant, b, now
		},
		"expired claim": func() (string, TrustGrant, RequestBinding, time.Time) {
			return token, grant, binding, now.Add(MaxClaimTTL + MaxClockSkew + time.Second)
		},
		"expired grant": func() (string, TrustGrant, RequestBinding, time.Time) {
			return token, grant, binding, now.Add(DefaultTrustTTL)
		},
		"revoked grant": func() (string, TrustGrant, RequestBinding, time.Time) {
			g := grant
			g.Enabled = false
			return token, g, binding, now
		},
		"unauthorized principal": func() (string, TrustGrant, RequestBinding, time.Time) {
			g := grant
			g.Attribution.Principal = "attacker@example.test"
			return token, g, binding, now
		},
		"unauthorized environment": func() (string, TrustGrant, RequestBinding, time.Time) {
			g := grant
			g.Attribution.Environment = "staging"
			return token, g, binding, now
		},
		"different key": func() (string, TrustGrant, RequestBinding, time.Time) {
			pub, _, keyID, keyErr := GenerateKey()
			if keyErr != nil {
				t.Fatal(keyErr)
			}
			g := grant
			g.KeyID, g.PublicKey = keyID, EncodePublicKey(pub)
			return token, g, binding, now
		},
		"signature": func() (string, TrustGrant, RequestBinding, time.Time) {
			parts := strings.Split(token, ".")
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sig[0] ^= 1
			parts[2] = base64.RawURLEncoding.EncodeToString(sig)
			return strings.Join(parts, "."), grant, binding, now
		},
	} {
		t.Run(name, func(t *testing.T) {
			testToken, testGrant, testBinding, testNow := mutate()
			if _, err := Verify(testToken, testGrant, testBinding, testNow, consume); err == nil {
				t.Fatal("tampered claim accepted")
			}
		})
	}
}

func TestClaimRejectsNonCanonicalAndDuplicateJSON(t *testing.T) {
	privateKey, claims, grant, binding, now := testClaim(t)
	payload, err := json.MarshalIndent(claims, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, signingInput(payload))
	token := TokenPrefix + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if _, err := Verify(token, grant, binding, now, func(string, string, time.Time) error { return nil }); !errors.Is(err, ErrMalformed) {
		t.Fatalf("noncanonical error=%v", err)
	}

	canonical, _ := json.Marshal(claims)
	duplicate := append([]byte(`{"v":1,`), canonical[1:]...)
	signature = ed25519.Sign(privateKey, signingInput(duplicate))
	token = TokenPrefix + "." + base64.RawURLEncoding.EncodeToString(duplicate) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if _, err := Verify(token, grant, binding, now, func(string, string, time.Time) error { return nil }); !errors.Is(err, ErrMalformed) {
		t.Fatalf("duplicate error=%v", err)
	}

	unknown := append(append([]byte(nil), canonical[:len(canonical)-1]...), []byte(`,"unknown":true}`)...)
	signature = ed25519.Sign(privateKey, signingInput(unknown))
	token = TokenPrefix + "." + base64.RawURLEncoding.EncodeToString(unknown) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if _, err := Verify(token, grant, binding, now, func(string, string, time.Time) error { return nil }); !errors.Is(err, ErrMalformed) {
		t.Fatalf("unknown-field error=%v", err)
	}
}

func TestReplayConsumerMustBeAtomic(t *testing.T) {
	privateKey, claims, grant, binding, now := testClaim(t)
	token, err := Sign(privateKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := false
	consume := func(string, string, time.Time) error {
		mu.Lock()
		defer mu.Unlock()
		if seen {
			return ErrReplay
		}
		seen = true
		return nil
	}
	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	accepted := 0
	var acceptedMu sync.Mutex
	for range workers {
		go func() {
			defer wg.Done()
			if _, err := Verify(token, grant, binding, now, consume); err == nil {
				acceptedMu.Lock()
				accepted++
				acceptedMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("accepted=%d, want 1", accepted)
	}
}

func TestPrivateKeyEncodingChecksPublicHalf(t *testing.T) {
	_, privateKey, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	encoded := EncodePrivateKey(privateKey)
	decoded, err := DecodePrivateKey(encoded)
	if err != nil || !decoded.Equal(privateKey) {
		t.Fatalf("decode err=%v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(encoded)
	raw[len(raw)-1] ^= 1
	if _, err := DecodePrivateKey(base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("corrupt private key accepted")
	}
}
