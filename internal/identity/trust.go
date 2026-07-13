package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/store"
)

const (
	KeyTrustGrant   = "_identity_trust_grant_v1"
	KeyTrustSource  = "_identity_trust_source_v1"
	KeyPolicySource = "external_policy_source"
)

var ErrTrustUnavailable = errors.New("burnban identity trust is unavailable")

// LoadTrustGrant loads the grant written by an opt-in Teams/Personal sync
// client. Canonical JSON and source binding are checked before request-time
// signature verification; missing or corrupt enrolled state never degrades to
// authenticated attribution.
func LoadTrustGrant(s *store.Store, now time.Time) (TrustGrant, error) {
	settings, err := s.GetSettings(KeyTrustGrant, KeyTrustSource, KeyPolicySource)
	if err != nil {
		return TrustGrant{}, err
	}
	raw, source, policySource := settings[KeyTrustGrant], settings[KeyTrustSource], settings[KeyPolicySource]
	if raw == "" || source == "" || policySource == "" {
		return TrustGrant{}, ErrTrustUnavailable
	}
	if source != policySource {
		return TrustGrant{}, fmt.Errorf("%w: trust source does not own this ledger", ErrTrustUnavailable)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var grant TrustGrant
	if err := decoder.Decode(&grant); err != nil {
		return TrustGrant{}, fmt.Errorf("%w: %v", ErrTrustUnavailable, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return TrustGrant{}, fmt.Errorf("%w: trailing trust JSON", ErrTrustUnavailable)
	}
	canonical, err := json.Marshal(grant)
	if err != nil || !bytes.Equal(canonical, []byte(raw)) {
		return TrustGrant{}, fmt.Errorf("%w: trust grant is not canonical", ErrTrustUnavailable)
	}
	expectedSource := "burnban-" + grant.TenantKind + ":" + grant.DeviceID
	if grant.TenantKind == "teams" {
		expectedSource = "burnban-teams:" + grant.DeviceID
	}
	if source != expectedSource {
		return TrustGrant{}, fmt.Errorf("%w: trust device does not match ledger owner", ErrTrustUnavailable)
	}
	if err := ValidateTrustGrant(grant, now.UTC()); err != nil {
		return TrustGrant{}, fmt.Errorf("%w: %v", ErrTrustUnavailable, err)
	}
	return grant, nil
}
