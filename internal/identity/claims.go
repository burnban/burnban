// Package identity defines Burnban's device-bound request identity protocol.
//
// A claim proves that an enrolled device signed a particular HTTP request. It
// does not prove that the operating-system account holding the private key is
// uncompromised. Server-issued TrustGrant values separately describe which
// attribution values the enrollment authorized.
package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ProtocolVersion = 1
	TokenPrefix     = "bbic1"
	AudienceProxy   = "burnban-proxy/v1"
	MaxClaimTTL     = 2 * time.Minute
	MaxClockSkew    = 15 * time.Second
	DefaultTrustTTL = 15 * time.Minute
)

var (
	ErrMalformed        = errors.New("malformed Burnban identity claim")
	ErrInvalidSignature = errors.New("invalid Burnban identity signature")
	ErrExpired          = errors.New("burnban identity claim expired")
	ErrNotYetValid      = errors.New("burnban identity claim is not yet valid")
	ErrBinding          = errors.New("burnban identity request binding mismatch")
	ErrReplay           = errors.New("burnban identity claim was already used")
	ErrUntrustedKey     = errors.New("burnban identity key is not trusted")
)

// Claims is serialized as a fixed-order JSON struct. Sign and Verify both
// require that exact representation, preventing alternate JSON spellings,
// duplicate keys, or unknown fields from being accepted as equivalent.
type Claims struct {
	Version        int    `json:"v"`
	TenantKind     string `json:"tenant_kind"`
	TenantID       string `json:"tenant_id"`
	DeviceID       string `json:"device_id"`
	KeyID          string `json:"kid"`
	Audience       string `json:"aud"`
	IssuedAt       int64  `json:"iat"`
	ExpiresAt      int64  `json:"exp"`
	JTI            string `json:"jti"`
	Method         string `json:"method"`
	Route          string `json:"route"`
	QuerySHA256    string `json:"query_sha256"`
	BodySHA256     string `json:"body_sha256"`
	Principal      string `json:"principal,omitempty"`
	ServiceAccount string `json:"service_account,omitempty"`
	Project        string `json:"project,omitempty"`
	CostCenter     string `json:"cost_center,omitempty"`
	Environment    string `json:"environment,omitempty"`
}

// Attribution is the server-authorized identity envelope for a device. Empty
// values are not authorized. Projects is an exact allow-list. "*" permits a
// device-signed project assertion for attribution, but does not make the
// asserted project server-authorized for policy enforcement.
type Attribution struct {
	Principal      string   `json:"principal,omitempty"`
	ServiceAccount string   `json:"service_account,omitempty"`
	Projects       []string `json:"projects,omitempty"`
	CostCenter     string   `json:"cost_center,omitempty"`
	Environment    string   `json:"environment,omitempty"`
}

// TrustGrant is delivered over the already authenticated sync channel and
// cached beside the local meter. ValidUntil bounds offline revocation latency.
// The server never receives or stores the corresponding private key.
type TrustGrant struct {
	ProtocolVersion int         `json:"protocol_version"`
	Revision        int64       `json:"revision"`
	TenantKind      string      `json:"tenant_kind"`
	TenantID        string      `json:"tenant_id"`
	DeviceID        string      `json:"device_id"`
	KeyID           string      `json:"key_id"`
	PublicKey       string      `json:"public_key"`
	Enabled         bool        `json:"enabled"`
	NotBefore       string      `json:"not_before"`
	ValidUntil      string      `json:"valid_until"`
	OnlineAt        string      `json:"online_at"`
	Attribution     Attribution `json:"attribution"`
}

// RequestBinding is supplied by the verifier from the actual HTTP request.
type RequestBinding struct {
	Audience    string
	Method      string
	Route       string
	QuerySHA256 string
	BodySHA256  string
}

// Verified describes both cryptographic validity and field-level provenance.
// ProjectTrusted is false when a signed project was merely asserted by a
// device whose grant did not authorize that exact value.
type Verified struct {
	Claims                Claims
	PrincipalTrusted      bool
	ServiceAccountTrusted bool
	ProjectTrusted        bool
	CostCenterTrusted     bool
	EnvironmentTrusted    bool
}

// ReplayConsumer must atomically record kid+jti until expires. A duplicate
// must return ErrReplay. Verification fails closed when no consumer is given.
type ReplayConsumer func(kid, jti string, expires time.Time) error

// GenerateKey creates a device-local Ed25519 key and its deterministic ID.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, string, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, "", err
	}
	return publicKey, privateKey, KeyID(publicKey), nil
}

func KeyID(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return "ed25519_" + base64.RawURLEncoding.EncodeToString(digest[:16])
}

func EncodePublicKey(publicKey ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(publicKey)
}

func DecodePublicKey(value string) (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid Ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

func EncodePrivateKey(privateKey ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(privateKey)
}

func DecodePrivateKey(value string) (ed25519.PrivateKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid Ed25519 private key")
	}
	privateKey := ed25519.PrivateKey(raw)
	publicKey := ed25519.NewKeyFromSeed(privateKey[:ed25519.SeedSize]).Public().(ed25519.PublicKey)
	if subtle.ConstantTimeCompare(privateKey[32:], publicKey) != 1 {
		return nil, fmt.Errorf("invalid Ed25519 private key")
	}
	return privateKey, nil
}

// NewClaims constructs a request-bound short-lived claim. Identity fields are
// filled by the caller, then Sign applies validation again.
func NewClaims(now time.Time, tenantKind, tenantID, deviceID, keyID string, binding RequestBinding) (Claims, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return Claims{}, err
	}
	now = now.UTC().Truncate(time.Second)
	claims := Claims{
		Version: ProtocolVersion, TenantKind: tenantKind, TenantID: tenantID,
		DeviceID: deviceID, KeyID: keyID, Audience: binding.Audience,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(MaxClaimTTL).Unix(),
		JTI: base64.RawURLEncoding.EncodeToString(jti), Method: binding.Method,
		Route: binding.Route, QuerySHA256: digestOrEmpty(binding.QuerySHA256), BodySHA256: binding.BodySHA256,
	}
	if err := validateClaims(claims, false); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func BodyDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func Sign(privateKey ed25519.PrivateKey, claims Claims) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("invalid Ed25519 private key")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if claims.KeyID != KeyID(publicKey) {
		return "", fmt.Errorf("claim key ID does not match private key")
	}
	if err := validateClaims(claims, true); err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signature := ed25519.Sign(privateKey, signingInput(payload))
	return TokenPrefix + "." + base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(signature), nil
}

// Issue creates and signs a request-bound claim using only attribution allowed
// by the cached server grant. Empty principal/service/cost-center values select
// the grant defaults; project remains explicit because it is request context.
func Issue(privateKey ed25519.PrivateKey, grant TrustGrant, binding RequestBinding, requested Attribution, now time.Time) (string, Claims, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", Claims{}, fmt.Errorf("invalid Ed25519 private key")
	}
	if len(requested.Projects) > 1 {
		return "", Claims{}, fmt.Errorf("a request claim can contain at most one project")
	}
	if err := ValidateTrustGrant(grant, now); err != nil {
		return "", Claims{}, err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if KeyID(publicKey) != grant.KeyID || subtle.ConstantTimeCompare(publicKey, mustPublicKey(grant.PublicKey)) != 1 {
		return "", Claims{}, ErrUntrustedKey
	}
	claims, err := NewClaims(now, grant.TenantKind, grant.TenantID, grant.DeviceID, grant.KeyID, binding)
	if err != nil {
		return "", Claims{}, err
	}
	claims.Principal = requested.Principal
	claims.ServiceAccount = requested.ServiceAccount
	if claims.Principal == "" && claims.ServiceAccount == "" {
		claims.Principal, claims.ServiceAccount = grant.Attribution.Principal, grant.Attribution.ServiceAccount
	}
	claims.Project = firstProject(requested.Projects)
	claims.CostCenter = requested.CostCenter
	if claims.CostCenter == "" {
		claims.CostCenter = grant.Attribution.CostCenter
	}
	claims.Environment = requested.Environment
	if claims.Environment == "" {
		claims.Environment = grant.Attribution.Environment
	}
	if _, err := authorizeAttribution(claims, grant.Attribution); err != nil {
		return "", Claims{}, err
	}
	token, err := Sign(privateKey, claims)
	return token, claims, err
}

func mustPublicKey(value string) ed25519.PublicKey {
	publicKey, _ := DecodePublicKey(value)
	return publicKey
}

func firstProject(projects []string) string {
	if len(projects) == 0 {
		return ""
	}
	return projects[0]
}

// Verify checks canonical encoding, signature, grant freshness, request
// binding, attribution authorization, and one-time use in that order.
func Verify(token string, grant TrustGrant, binding RequestBinding, now time.Time, consume ReplayConsumer) (Verified, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != TokenPrefix || parts[1] == "" || parts[2] == "" {
		return Verified{}, ErrMalformed
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(payload) > 4096 {
		return Verified{}, ErrMalformed
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Verified{}, ErrMalformed
	}
	var claims Claims
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil {
		return Verified{}, ErrMalformed
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Verified{}, ErrMalformed
	}
	canonical, err := json.Marshal(claims)
	if err != nil || !bytes.Equal(canonical, payload) {
		return Verified{}, ErrMalformed
	}
	if err := validateClaims(claims, true); err != nil {
		return Verified{}, errors.Join(ErrMalformed, err)
	}
	publicKey, err := validateGrant(grant, now)
	if err != nil {
		return Verified{}, err
	}
	if claims.TenantKind != grant.TenantKind || claims.TenantID != grant.TenantID ||
		claims.DeviceID != grant.DeviceID || claims.KeyID != grant.KeyID {
		return Verified{}, ErrUntrustedKey
	}
	if !ed25519.Verify(publicKey, signingInput(payload), signature) {
		return Verified{}, ErrInvalidSignature
	}
	nowUnix := now.UTC().Unix()
	if claims.IssuedAt > nowUnix+int64(MaxClockSkew/time.Second) {
		return Verified{}, ErrNotYetValid
	}
	if claims.ExpiresAt <= nowUnix-int64(MaxClockSkew/time.Second) {
		return Verified{}, ErrExpired
	}
	if binding.Audience == "" {
		binding.Audience = AudienceProxy
	}
	if claims.Audience != binding.Audience || claims.Method != binding.Method || claims.Route != binding.Route ||
		claims.QuerySHA256 != digestOrEmpty(binding.QuerySHA256) ||
		claims.BodySHA256 != binding.BodySHA256 {
		return Verified{}, ErrBinding
	}
	verified, err := authorizeAttribution(claims, grant.Attribution)
	if err != nil {
		return Verified{}, err
	}
	if consume == nil {
		return Verified{}, fmt.Errorf("%w: replay consumer is required", ErrReplay)
	}
	if err := consume(claims.KeyID, claims.JTI, time.Unix(claims.ExpiresAt, 0).UTC()); err != nil {
		if errors.Is(err, ErrReplay) {
			return Verified{}, ErrReplay
		}
		return Verified{}, fmt.Errorf("record Burnban identity nonce: %w", err)
	}
	verified.Claims = claims
	return verified, nil
}

func signingInput(payload []byte) []byte {
	return append([]byte("burnban.identity.v1\x00"), payload...)
}

func validateClaims(claims Claims, requireIdentity bool) error {
	if claims.Version != ProtocolVersion {
		return fmt.Errorf("unsupported claim version")
	}
	if claims.TenantKind != "personal" && claims.TenantKind != "teams" {
		return fmt.Errorf("invalid tenant kind")
	}
	for name, value := range map[string]string{
		"tenant ID": claims.TenantID, "device ID": claims.DeviceID, "key ID": claims.KeyID,
	} {
		if !validIdentifier(value, 128) {
			return fmt.Errorf("invalid %s", name)
		}
	}
	if claims.Audience != AudienceProxy || claims.Method != http.MethodPost ||
		claims.Route == "" || len(claims.Route) > 1024 || claims.Route[0] != '/' || strings.ContainsAny(claims.Route, "?#\r\n\x00") {
		return fmt.Errorf("invalid request binding")
	}
	if !validDigest(claims.BodySHA256) || !validDigest(claims.QuerySHA256) {
		return fmt.Errorf("invalid request digest")
	}
	jti, err := base64.RawURLEncoding.Strict().DecodeString(claims.JTI)
	if err != nil || len(jti) < 16 || len(jti) > 32 {
		return fmt.Errorf("invalid claim nonce")
	}
	if claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt ||
		time.Duration(claims.ExpiresAt-claims.IssuedAt)*time.Second > MaxClaimTTL {
		return fmt.Errorf("invalid claim lifetime")
	}
	for name, value := range map[string]string{
		"principal": claims.Principal, "service account": claims.ServiceAccount,
		"project": claims.Project, "cost center": claims.CostCenter, "environment": claims.Environment,
	} {
		if value != "" && !validLabel(value) {
			return fmt.Errorf("invalid %s", name)
		}
	}
	if requireIdentity && claims.Principal == "" && claims.ServiceAccount == "" {
		return fmt.Errorf("principal or service account is required")
	}
	if claims.Principal != "" && claims.ServiceAccount != "" {
		return fmt.Errorf("principal and service account are mutually exclusive")
	}
	return nil
}

func digestOrEmpty(value string) string {
	if value != "" {
		return value
	}
	return BodyDigest(nil)
}

func validDigest(value string) bool {
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == sha256.Size && value == strings.ToLower(value)
}

func validateGrant(grant TrustGrant, now time.Time) (ed25519.PublicKey, error) {
	if grant.ProtocolVersion != ProtocolVersion || grant.Revision < 1 || !grant.Enabled ||
		(grant.TenantKind != "personal" && grant.TenantKind != "teams") ||
		!validIdentifier(grant.TenantID, 128) || !validIdentifier(grant.DeviceID, 128) ||
		!validIdentifier(grant.KeyID, 128) {
		return nil, ErrUntrustedKey
	}
	publicKey, err := DecodePublicKey(grant.PublicKey)
	if err != nil || KeyID(publicKey) != grant.KeyID {
		return nil, ErrUntrustedKey
	}
	notBefore, err := time.Parse(time.RFC3339, grant.NotBefore)
	if err != nil {
		return nil, ErrUntrustedKey
	}
	validUntil, err := time.Parse(time.RFC3339, grant.ValidUntil)
	onlineAt, onlineErr := time.Parse(time.RFC3339, grant.OnlineAt)
	if err != nil || onlineErr != nil || !validUntil.After(notBefore) || onlineAt.Before(notBefore) ||
		!validUntil.After(onlineAt) || validUntil.Sub(onlineAt) > 24*time.Hour {
		return nil, ErrUntrustedKey
	}
	if onlineAt.After(now.Add(10 * time.Minute)) {
		return nil, ErrNotYetValid
	}
	if now.Before(notBefore.Add(-MaxClockSkew)) {
		return nil, ErrNotYetValid
	}
	if !now.Before(validUntil) {
		return nil, ErrExpired
	}
	return publicKey, nil
}

// ValidateTrustGrant validates a grant before it is persisted by a sync
// client. Verification repeats these checks at request time.
func ValidateTrustGrant(grant TrustGrant, now time.Time) error {
	_, err := validateGrant(grant, now)
	if err != nil {
		return err
	}
	for name, value := range map[string]string{
		"principal": grant.Attribution.Principal, "service account": grant.Attribution.ServiceAccount,
		"cost center": grant.Attribution.CostCenter, "environment": grant.Attribution.Environment,
	} {
		if value != "" && !validLabel(value) {
			return fmt.Errorf("invalid grant %s", name)
		}
	}
	if grant.Attribution.Principal != "" && grant.Attribution.ServiceAccount != "" {
		return fmt.Errorf("grant principal and service account are mutually exclusive")
	}
	if grant.Attribution.Principal == "" && grant.Attribution.ServiceAccount == "" {
		return fmt.Errorf("grant principal or service account is required")
	}
	if len(grant.Attribution.Projects) > 256 {
		return fmt.Errorf("too many grant projects")
	}
	seen := make(map[string]struct{}, len(grant.Attribution.Projects))
	for _, project := range grant.Attribution.Projects {
		if project != "*" && !validLabel(project) {
			return fmt.Errorf("invalid grant project")
		}
		if _, exists := seen[project]; exists {
			return fmt.Errorf("duplicate grant project")
		}
		seen[project] = struct{}{}
	}
	return nil
}

func authorizeAttribution(claims Claims, allowed Attribution) (Verified, error) {
	result := Verified{}
	if claims.Principal != "" {
		if claims.Principal != allowed.Principal {
			return Verified{}, fmt.Errorf("%w: principal is not authorized", ErrUntrustedKey)
		}
		result.PrincipalTrusted = true
	}
	if claims.ServiceAccount != "" {
		if claims.ServiceAccount != allowed.ServiceAccount {
			return Verified{}, fmt.Errorf("%w: service account is not authorized", ErrUntrustedKey)
		}
		result.ServiceAccountTrusted = true
	}
	if claims.CostCenter != "" {
		if claims.CostCenter != allowed.CostCenter {
			return Verified{}, fmt.Errorf("%w: cost center is not authorized", ErrUntrustedKey)
		}
		result.CostCenterTrusted = true
	}
	if claims.Environment != "" {
		if claims.Environment != allowed.Environment {
			return Verified{}, fmt.Errorf("%w: environment is not authorized", ErrUntrustedKey)
		}
		result.EnvironmentTrusted = true
	}
	if claims.Project != "" {
		projectAllowed := false
		for _, project := range allowed.Projects {
			if project == "*" {
				projectAllowed = true
			}
			if project == claims.Project {
				projectAllowed = true
				result.ProjectTrusted = true
			}
		}
		if !projectAllowed {
			return Verified{}, fmt.Errorf("%w: project is not authorized", ErrUntrustedKey)
		}
	}
	return result, nil
}

func validIdentifier(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') &&
			r != '_' && r != '-' && r != '.' && r != ':' && r != '@' {
			return false
		}
	}
	return true
}

func validLabel(value string) bool {
	if value == "" || len(value) > 256 || utf8.RuneCountInString(value) > 128 || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			return false
		}
	}
	return true
}
