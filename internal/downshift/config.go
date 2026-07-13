// Package downshift implements explicit, compatibility-gated model routing.
// It never translates provider dialects or manages credentials: a rule may
// only replace an exact source route/model with an exact, operator-allowlisted
// target whose wire contract is declared equivalent.
package downshift

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const APIVersion = "burnban.downshift/v1"

type Mode string

const (
	ModeObserve           Mode = "observe"
	ModeWarnThenDownshift Mode = "warn_then_downshift"
)

type Config struct {
	APIVersion        string  `json:"api_version"`
	Revision          int64   `json:"revision"`
	Mode              Mode    `json:"mode"`
	WarnAtPct         float64 `json:"warn_at_pct"`
	DownshiftAtPct    float64 `json:"downshift_at_pct"`
	DownshiftOnDenial bool    `json:"downshift_on_denial"`
	Rules             []Rule  `json:"rules"`
}

type Rule struct {
	ID           string       `json:"id"`
	Source       Endpoint     `json:"source"`
	Target       Endpoint     `json:"target"`
	Scope        Scope        `json:"scope,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
}

type Endpoint struct {
	Route         string `json:"route"`
	Model         string `json:"model"`
	Family        string `json:"family"`
	Dialect       string `json:"dialect"`
	ContextTokens int64  `json:"context_tokens"`
}

// Scope fields are exact matches. Any non-empty scope requires an
// authenticated, device-bound identity; self-reported headers cannot select a
// cheaper target intended for another principal or project.
type Scope struct {
	Tenant         string `json:"tenant,omitempty"`
	Principal      string `json:"principal,omitempty"`
	ServiceAccount string `json:"service_account,omitempty"`
	Project        string `json:"project,omitempty"`
	CostCenter     string `json:"cost_center,omitempty"`
}

type Capabilities struct {
	Tools            bool     `json:"tools"`
	StructuredOutput bool     `json:"structured_output"`
	Modalities       []string `json:"modalities"`
}

type Compiled struct {
	Config    Config
	Canonical []byte
	Digest    string
	bySource  map[string]*Rule
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func Parse(raw []byte) (*Compiled, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, fmt.Errorf("downshift config must be between 1 byte and 1 MiB")
	}
	if err := validateStrictJSON(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode downshift config: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	digestBytes := sha256.Sum256(canonical)
	compiled := &Compiled{
		Config: cfg, Canonical: canonical, Digest: hex.EncodeToString(digestBytes[:]),
		bySource: make(map[string]*Rule, len(cfg.Rules)),
	}
	for i := range compiled.Config.Rules {
		rule := &compiled.Config.Rules[i]
		compiled.bySource[sourceKey(rule.Source.Route, rule.Source.Model)] = rule
	}
	return compiled, nil
}

func validateConfig(cfg *Config) error {
	if cfg.APIVersion != APIVersion {
		return fmt.Errorf("api_version must be %q", APIVersion)
	}
	if cfg.Revision < 1 {
		return fmt.Errorf("revision must be at least 1")
	}
	if cfg.Mode != ModeObserve && cfg.Mode != ModeWarnThenDownshift {
		return fmt.Errorf("mode must be %q or %q", ModeObserve, ModeWarnThenDownshift)
	}
	if !finitePct(cfg.WarnAtPct) || !finitePct(cfg.DownshiftAtPct) {
		return fmt.Errorf("warn_at_pct and downshift_at_pct must be finite percentages in [1,100]")
	}
	if cfg.WarnAtPct > cfg.DownshiftAtPct {
		return fmt.Errorf("warn_at_pct must not exceed downshift_at_pct")
	}
	if len(cfg.Rules) == 0 || len(cfg.Rules) > 1024 {
		return fmt.Errorf("rules must contain between 1 and 1024 entries")
	}
	ids := map[string]bool{}
	sources := map[string]bool{}
	for i := range cfg.Rules {
		rule := &cfg.Rules[i]
		if !identifier.MatchString(rule.ID) {
			return fmt.Errorf("rules[%d].id must match %s", i, identifier)
		}
		if ids[rule.ID] {
			return fmt.Errorf("duplicate rule id %q", rule.ID)
		}
		ids[rule.ID] = true
		if err := validateEndpoint("source", rule.Source); err != nil {
			return fmt.Errorf("rule %q: %w", rule.ID, err)
		}
		if err := validateEndpoint("target", rule.Target); err != nil {
			return fmt.Errorf("rule %q: %w", rule.ID, err)
		}
		if rule.Source.Family != rule.Target.Family {
			return fmt.Errorf("rule %q: source and target family must match exactly", rule.ID)
		}
		if rule.Source.Dialect != rule.Target.Dialect {
			return fmt.Errorf("rule %q: dialect translation is not supported", rule.ID)
		}
		if rule.Source.Route == rule.Target.Route && rule.Source.Model == rule.Target.Model {
			return fmt.Errorf("rule %q: source and target are identical", rule.ID)
		}
		key := sourceKey(rule.Source.Route, rule.Source.Model)
		if sources[key] {
			return fmt.Errorf("ambiguous duplicate source route/model %q/%q", rule.Source.Route, rule.Source.Model)
		}
		sources[key] = true
		if err := validateScope(rule.Scope); err != nil {
			return fmt.Errorf("rule %q: %w", rule.ID, err)
		}
		modalities, err := normalizeModalities(rule.Capabilities.Modalities)
		if err != nil {
			return fmt.Errorf("rule %q: %w", rule.ID, err)
		}
		rule.Capabilities.Modalities = modalities
	}
	return nil
}

func finitePct(value float64) bool {
	return value >= 1 && value <= 100 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateEndpoint(kind string, endpoint Endpoint) error {
	if !identifier.MatchString(endpoint.Route) {
		return fmt.Errorf("%s.route must match %s", kind, identifier)
	}
	if err := safeConfigLabel(kind+".model", endpoint.Model, 256); err != nil {
		return err
	}
	if endpoint.Dialect == "gemini" && strings.ContainsAny(endpoint.Model, "/:?#") {
		return fmt.Errorf("%s.model must be one canonical Gemini path segment", kind)
	}
	if !identifier.MatchString(endpoint.Family) {
		return fmt.Errorf("%s.family must match %s", kind, identifier)
	}
	switch endpoint.Dialect {
	case "openai", "anthropic", "gemini":
	default:
		return fmt.Errorf("%s.dialect must be openai, anthropic, or gemini", kind)
	}
	if endpoint.ContextTokens < 1 || endpoint.ContextTokens > 100_000_000 {
		return fmt.Errorf("%s.context_tokens must be between 1 and 100000000", kind)
	}
	return nil
}

func validateScope(scope Scope) error {
	for name, value := range map[string]string{
		"scope.tenant": scope.Tenant, "scope.principal": scope.Principal,
		"scope.service_account": scope.ServiceAccount, "scope.project": scope.Project,
		"scope.cost_center": scope.CostCenter,
	} {
		if value != "" {
			if err := safeConfigLabel(name, value, 256); err != nil {
				return err
			}
		}
	}
	if scope.Principal != "" && scope.ServiceAccount != "" {
		return fmt.Errorf("scope cannot require both principal and service_account")
	}
	return nil
}

func safeConfigLabel(name, value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be a non-empty, valid, trimmed UTF-8 value of at most %d bytes", name, maxBytes)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control) {
			return fmt.Errorf("%s contains a forbidden control character", name)
		}
	}
	return nil
}

func normalizeModalities(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 3 {
		return nil, fmt.Errorf("capabilities.modalities must contain 1 to 3 values")
	}
	seen := map[string]bool{}
	for _, value := range values {
		switch value {
		case "text", "image", "audio":
		default:
			return nil, fmt.Errorf("unsupported modality %q", value)
		}
		if seen[value] {
			return nil, fmt.Errorf("duplicate modality %q", value)
		}
		seen[value] = true
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func (c *Compiled) Rule(route, model string) *Rule {
	if c == nil {
		return nil
	}
	return c.bySource[sourceKey(route, model)]
}

func sourceKey(route, model string) string { return route + "\x00" + model }

func ScopeMatches(scope Scope, identity Identity) (bool, string) {
	if scope == (Scope{}) {
		return true, ""
	}
	checks := []struct{ name, want, got, confidence string }{
		{"tenant", scope.Tenant, identity.Tenant, identity.Confidence},
		{"principal", scope.Principal, identity.Principal, identity.UserConfidence},
		{"service_account", scope.ServiceAccount, identity.ServiceAccount, identity.UserConfidence},
		{"project", scope.Project, identity.Project, identity.ProjectConfidence},
		{"cost_center", scope.CostCenter, identity.CostCenter, identity.TeamConfidence},
	}
	for _, check := range checks {
		if check.want == "" {
			continue
		}
		if check.confidence != "authenticated" {
			return false, "scoped rule requires authenticated " + check.name
		}
		if check.want != check.got {
			return false, "authenticated identity does not match rule " + check.name
		}
	}
	return true, ""
}

type Identity struct {
	Tenant, Principal, ServiceAccount, Project, CostCenter, Confidence string
	TeamConfidence, UserConfidence, ProjectConfidence                  string
}

func validateStrictJSON(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := scanStrictJSON(dec, 0); err != nil {
		return err
	}
	return ensureJSONEOF(dec)
}

func scanStrictJSON(dec *json.Decoder, depth int) error {
	if depth > 128 {
		return fmt.Errorf("downshift config nesting exceeds 128 levels")
	}
	token, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid downshift JSON: %w", err)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		folded := map[string]string{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return fmt.Errorf("invalid downshift JSON: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("downshift JSON object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("downshift config contains duplicate field %q", key)
			}
			lower := strings.ToLower(key)
			if previous, exists := folded[lower]; exists && previous != key {
				return fmt.Errorf("downshift config contains case-ambiguous fields %q and %q", previous, key)
			}
			seen[key], folded[lower] = true, key
			if err := scanStrictJSON(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	case '[':
		for dec.More() {
			if err := scanStrictJSON(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	default:
		return fmt.Errorf("invalid JSON delimiter %q", delim)
	}
	if err != nil {
		return fmt.Errorf("invalid downshift JSON: %w", err)
	}
	return nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("downshift config contains multiple JSON values")
		}
		return fmt.Errorf("invalid trailing downshift JSON: %w", err)
	}
	return nil
}
