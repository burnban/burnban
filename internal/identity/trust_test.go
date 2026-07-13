package identity

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestLoadTrustGrantIsCanonicalFreshAndLedgerBound(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "trust.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	pub, _, keyID, _ := GenerateKey()
	grant := TrustGrant{
		ProtocolVersion: 1, Revision: 1, TenantKind: "personal", TenantID: "usr_test", DeviceID: "dev_test",
		KeyID: keyID, PublicKey: EncodePublicKey(pub), Enabled: true,
		NotBefore: now.Add(-time.Hour).Format(time.RFC3339), OnlineAt: now.Format(time.RFC3339),
		ValidUntil:  now.Add(15 * time.Minute).Format(time.RFC3339),
		Attribution: Attribution{Principal: "person@example.test", Projects: []string{"*"}},
	}
	raw, _ := json.Marshal(grant)
	for key, value := range map[string]string{
		KeyTrustGrant: string(raw), KeyTrustSource: "burnban-personal:dev_test", KeyPolicySource: "burnban-personal:dev_test",
	} {
		if err := s.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := LoadTrustGrant(s, now)
	if err != nil || loaded.KeyID != keyID {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if err := s.SetSetting(KeyTrustSource, "burnban-personal:dev_other"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustGrant(s, now); !errors.Is(err, ErrTrustUnavailable) {
		t.Fatalf("source mismatch error=%v", err)
	}
	if err := s.SetSetting(KeyTrustSource, "burnban-personal:dev_test"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(KeyTrustGrant, " "+string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustGrant(s, now); !errors.Is(err, ErrTrustUnavailable) {
		t.Fatalf("noncanonical error=%v", err)
	}
}
