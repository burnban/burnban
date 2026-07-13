// Package policy implements Burnban's local, deterministic policy engine.
// Policies operate only on bounded request metadata; prompts, responses, and
// provider credentials are neither inspected beyond metering nor persisted.
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	APIVersion = "burnban.dev/v2"
	Kind       = "PolicySet"

	// Window limits each require a durable usage lookup during admission. Keep
	// both the per-rule fanout and the document-wide request-path work bounded,
	// independently of the encoded 1 MiB document limit.
	maxWindowLimitsPerKind = 64
	maxWindowLimitsPerDoc  = 256
)

type Mode string

const (
	ModeObserve Mode = "observe"
	ModeWarn    Mode = "warn"
	ModeEnforce Mode = "enforce"
)

type Document struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Mode       Mode     `json:"mode"`
	Rules      []Rule   `json:"rules"`
}

type Metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Revision  int64  `json:"revision"`
}

type Rule struct {
	ID     string `json:"id"`
	Mode   Mode   `json:"mode,omitempty"`
	Scope  Scope  `json:"scope,omitempty"`
	Match  Match  `json:"match,omitempty"`
	Limits Limits `json:"limits,omitempty"`
}

// Scope is an AND across dimensions and an OR within each selector list. An
// empty dimension is a wildcard. This gives one schema for organization/team,
// user, project, agent, and request-path hierarchy without implicit precedence.
type Scope struct {
	Organization   []string `json:"organization,omitempty"`
	Tenant         []string `json:"tenant,omitempty"`
	Meter          []string `json:"meter,omitempty"`
	Device         []string `json:"device,omitempty"`
	Team           []string `json:"team,omitempty"`
	CostCenter     []string `json:"cost_center,omitempty"`
	Principal      []string `json:"principal,omitempty"`
	ServiceAccount []string `json:"service_account,omitempty"`
	User           []string `json:"user,omitempty"`
	Project        []string `json:"project,omitempty"`
	Environment    []string `json:"environment,omitempty"`
	Agent          []string `json:"agent,omitempty"`
	Session        []string `json:"session,omitempty"`
	Provider       []string `json:"provider,omitempty"`
	Model          []string `json:"model,omitempty"`
	ModelClass     []string `json:"model_class,omitempty"`
	Route          []string `json:"route,omitempty"`
	Tier           []string `json:"tier,omitempty"`
	ServiceTier    []string `json:"service_tier,omitempty"`
	Geo            []string `json:"geo,omitempty"`
	InferenceGeo   []string `json:"inference_geo,omitempty"`
}

type Match struct {
	Provider     AccessList  `json:"provider,omitempty"`
	Model        AccessList  `json:"model,omitempty"`
	Route        AccessList  `json:"route,omitempty"`
	Tier         AccessList  `json:"tier,omitempty"`
	Geo          AccessList  `json:"geo,omitempty"`
	ModelClass   *AccessList `json:"model_class,omitempty"`
	ServiceTier  *AccessList `json:"service_tier,omitempty"`
	InferenceGeo *AccessList `json:"inference_geo,omitempty"`
}

type AccessList struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type Limits struct {
	Requests                []WindowLimit `json:"requests,omitempty"`
	Tokens                  []WindowLimit `json:"tokens,omitempty"`
	Dollars                 []DollarLimit `json:"dollars,omitempty"`
	Concurrency             int64         `json:"concurrency,omitempty"`
	MaxEstimatedCallCostUSD *float64      `json:"max_estimated_call_cost_usd,omitempty"`
	RequireOutputBound      bool          `json:"require_output_bound,omitempty"`
}

type WindowLimit struct {
	ID         string `json:"id"`
	Max        int64  `json:"max"`
	Window     string `json:"window"`
	WindowType string `json:"window_type"`
	Kind       string `json:"kind,omitempty"`
}

// DollarLimit uses integer micro-USD so canonical documents, pending
// reservations, and durable counters never depend on floating-point sums.
type DollarLimit struct {
	ID          string `json:"id"`
	MaxMicroUSD int64  `json:"max_microusd"`
	Window      string `json:"window"`
	WindowType  string `json:"window_type"`
}

type Context struct {
	Organization             string  `json:"organization,omitempty"`
	Tenant                   string  `json:"tenant,omitempty"`
	Meter                    string  `json:"meter,omitempty"`
	Device                   string  `json:"device,omitempty"`
	Team                     string  `json:"team,omitempty"`
	CostCenter               string  `json:"cost_center,omitempty"`
	Principal                string  `json:"principal,omitempty"`
	ServiceAccount           string  `json:"service_account,omitempty"`
	User                     string  `json:"user,omitempty"`
	Project                  string  `json:"project,omitempty"`
	Environment              string  `json:"environment,omitempty"`
	Agent                    string  `json:"agent,omitempty"`
	Session                  string  `json:"session,omitempty"`
	Provider                 string  `json:"provider"`
	Model                    string  `json:"model,omitempty"`
	ModelClass               string  `json:"model_class,omitempty"`
	Route                    string  `json:"route,omitempty"`
	Tier                     string  `json:"tier,omitempty"`
	ServiceTier              string  `json:"service_tier,omitempty"`
	Geo                      string  `json:"geo,omitempty"`
	InferenceGeo             string  `json:"inference_geo,omitempty"`
	EstimatedInput           int64   `json:"estimated_input_tokens"`
	OutputBound              int64   `json:"output_token_bound,omitempty"`
	OutputBoundPresent       bool    `json:"output_bound_present"`
	EstimatedCostUSD         float64 `json:"estimated_cost_usd,omitempty"`
	EstimatedCostMicroUSD    int64   `json:"estimated_cost_microusd,omitempty"`
	CostKnown                bool    `json:"cost_known"`
	IdentityConfidence       string  `json:"identity_confidence,omitempty"`
	OrganizationConfidence   string  `json:"organization_confidence,omitempty"`
	TenantConfidence         string  `json:"tenant_confidence,omitempty"`
	MeterConfidence          string  `json:"meter_confidence,omitempty"`
	DeviceConfidence         string  `json:"device_confidence,omitempty"`
	TeamConfidence           string  `json:"team_confidence,omitempty"`
	CostCenterConfidence     string  `json:"cost_center_confidence,omitempty"`
	PrincipalConfidence      string  `json:"principal_confidence,omitempty"`
	ServiceAccountConfidence string  `json:"service_account_confidence,omitempty"`
	UserConfidence           string  `json:"user_confidence,omitempty"`
	ProjectConfidence        string  `json:"project_confidence,omitempty"`
	EnvironmentConfidence    string  `json:"environment_confidence,omitempty"`
}

func (c Context) EstimatedTokens() int64 {
	input, output := max(c.EstimatedInput, 0), int64(0)
	if c.OutputBoundPresent {
		output = max(c.OutputBound, 0)
	}
	if input > (1<<63-1)-output {
		return 1<<63 - 1
	}
	return input + output
}

func (c Context) EstimatedTokensForKind(kind string) (int64, bool) {
	switch kind {
	case "input":
		return max(c.EstimatedInput, 0), true
	case "", "total":
		if !c.OutputBoundPresent {
			return 0, false
		}
		return c.EstimatedTokens(), true
	case "output":
		if !c.OutputBoundPresent {
			return 0, false
		}
		return max(c.OutputBound, 0), true
	default:
		return 0, false
	}
}

// MicroUSDFromUSD rounds upward so a binary floating-point conversion can
// never reserve less than the conservative dollar estimate.
func MicroUSDFromUSD(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(math.MaxInt64)/1_000_000 {
		return 0, false
	}
	scaled := math.Ceil(value * 1_000_000)
	if scaled < 0 || scaled > float64(math.MaxInt64) {
		return 0, false
	}
	return int64(scaled), true
}

func (c Context) ConservativeCostMicroUSD() (int64, bool) {
	if !c.CostKnown || c.EstimatedCostMicroUSD < 0 {
		return 0, false
	}
	minimum, ok := MicroUSDFromUSD(c.EstimatedCostUSD)
	if !ok {
		return 0, false
	}
	if c.EstimatedCostMicroUSD == 0 {
		return minimum, true
	}
	if c.EstimatedCostMicroUSD < minimum {
		return 0, false
	}
	return c.EstimatedCostMicroUSD, true
}

type Compiled struct {
	Document  Document
	Canonical []byte
	Digest    string
	windows   map[policyWindowKey]time.Duration
}

// policyWindowKey is deliberately structured rather than delimiter-encoded.
// Rule and limit labels may contain punctuation such as ':', so concatenating
// them would let two distinct limits alias the same compiled window.
type policyWindowKey struct {
	ruleID    string
	dimension string
	limitID   string
}

func makePolicyWindowKey(ruleID, dimension, limitID string) policyWindowKey {
	return policyWindowKey{ruleID: ruleID, dimension: dimension, limitID: limitID}
}

// Parse rejects duplicate/unknown fields and trailing JSON, validates all
// selectors and limits, and returns canonical content with a stable digest.
func Parse(data []byte) (*Compiled, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("policy document is empty")
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("policy document exceeds 1 MiB")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, err
	}
	var doc Document
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode policy: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("policy contains multiple JSON values")
		}
		return nil, fmt.Errorf("policy has trailing data: %w", err)
	}
	if doc.Mode == "" {
		doc.Mode = ModeEnforce
	}
	windows, err := validate(&doc)
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	digestBytes := sha256.Sum256(canonical)
	return &Compiled{
		Document: doc, Canonical: canonical, Digest: hex.EncodeToString(digestBytes[:]), windows: windows,
	}, nil
}

func validate(doc *Document) (map[policyWindowKey]time.Duration, error) {
	if doc.APIVersion != APIVersion {
		return nil, fmt.Errorf("apiVersion must be %q", APIVersion)
	}
	if doc.Kind != Kind {
		return nil, fmt.Errorf("kind must be %q", Kind)
	}
	if err := validateLabel("metadata.name", doc.Metadata.Name); err != nil {
		return nil, err
	}
	if err := validateLabel("metadata.namespace", doc.Metadata.Namespace); err != nil {
		return nil, err
	}
	if doc.Metadata.Revision < 1 {
		return nil, fmt.Errorf("metadata.revision must be at least 1")
	}
	if !validMode(doc.Mode) {
		return nil, fmt.Errorf("mode must be observe, warn, or enforce")
	}
	if len(doc.Rules) == 0 {
		return nil, fmt.Errorf("rules must contain at least one rule")
	}
	if len(doc.Rules) > 256 {
		return nil, fmt.Errorf("rules exceeds the maximum of 256")
	}
	windows := map[policyWindowKey]time.Duration{}
	ids := map[string]struct{}{}
	totalWindowLimits := 0
	for i := range doc.Rules {
		rule := &doc.Rules[i]
		path := fmt.Sprintf("rules[%d]", i)
		if err := validateLabel(path+".id", rule.ID); err != nil {
			return nil, err
		}
		if _, exists := ids[rule.ID]; exists {
			return nil, fmt.Errorf("duplicate rule id %q", rule.ID)
		}
		ids[rule.ID] = struct{}{}
		if rule.Mode != "" && !validMode(rule.Mode) {
			return nil, fmt.Errorf("%s.mode must be observe, warn, or enforce", path)
		}
		effectiveMode := ruleMode(doc.Mode, *rule)
		if effectiveMode == ModeEnforce {
			for dimension, selectors := range map[string][]string{
				"agent": rule.Scope.Agent, "session": rule.Scope.Session, "model_class": rule.Scope.ModelClass,
			} {
				if len(selectors) != 0 {
					return nil, fmt.Errorf("%s cannot enforce %s scope because that cooperative label is not authenticated; use warn or observe", path, dimension)
				}
			}
			if access := rule.Match.ModelClass; access != nil && (len(access.Allow) != 0 || len(access.Deny) != 0) {
				return nil, fmt.Errorf("%s cannot enforce model_class match because that cooperative label is not authenticated; use warn or observe", path)
			}
		}
		for name, selectors := range scopeSelectors(rule.Scope) {
			if err := validateSelectors(path+".scope."+name, selectors); err != nil {
				return nil, err
			}
		}
		for name, access := range matchAccess(rule.Match) {
			if err := validateSelectors(path+".match."+name+".allow", access.Allow); err != nil {
				return nil, err
			}
			if err := validateSelectors(path+".match."+name+".deny", access.Deny); err != nil {
				return nil, err
			}
		}
		if rule.Limits.Concurrency < 0 {
			return nil, fmt.Errorf("%s.limits.concurrency must be >= 0", path)
		}
		if cost := rule.Limits.MaxEstimatedCallCostUSD; cost != nil && (*cost < 0 || *cost > 1e12) {
			return nil, fmt.Errorf("%s.limits.max_estimated_call_cost_usd must be between 0 and 1e12", path)
		}
		limitIDs := map[string]struct{}{}
		limitKinds := []struct {
			name   string
			limits []WindowLimit
		}{{"requests", rule.Limits.Requests}, {"tokens", rule.Limits.Tokens}}
		for _, group := range limitKinds {
			kind, limits := group.name, group.limits
			if len(limits) > maxWindowLimitsPerKind {
				return nil, fmt.Errorf("%s.limits.%s exceeds the maximum of %d", path, kind, maxWindowLimitsPerKind)
			}
			totalWindowLimits += len(limits)
			if totalWindowLimits > maxWindowLimitsPerDoc {
				return nil, fmt.Errorf("policy exceeds the maximum of %d total request/token/dollar window limits", maxWindowLimitsPerDoc)
			}
			for j, limit := range limits {
				limitPath := fmt.Sprintf("%s.limits.%s[%d]", path, kind, j)
				if err := validateLabel(limitPath+".id", limit.ID); err != nil {
					return nil, err
				}
				key := kind + ":" + limit.ID
				if _, exists := limitIDs[key]; exists {
					return nil, fmt.Errorf("duplicate %s limit id %q in rule %q", kind, limit.ID, rule.ID)
				}
				limitIDs[key] = struct{}{}
				if limit.Max < 1 {
					return nil, fmt.Errorf("%s.max must be at least 1", limitPath)
				}
				if kind == "requests" && limit.Kind != "" {
					return nil, fmt.Errorf("%s.kind is valid only for token limits", limitPath)
				}
				if kind == "tokens" {
					if limit.Kind != "" && limit.Kind != "input" && limit.Kind != "output" && limit.Kind != "total" {
						return nil, fmt.Errorf("%s.kind must be input, output, or total", limitPath)
					}
				}
				if limit.WindowType != "rolling" && limit.WindowType != "fixed" {
					return nil, fmt.Errorf("%s.window_type must be rolling or fixed", limitPath)
				}
				duration, err := time.ParseDuration(limit.Window)
				if err != nil || duration < time.Second || duration > 366*24*time.Hour {
					return nil, fmt.Errorf("%s.window must be a duration from 1s through 8784h", limitPath)
				}
				windows[makePolicyWindowKey(rule.ID, kind, limit.ID)] = duration
			}
		}
		if len(rule.Limits.Dollars) > maxWindowLimitsPerKind {
			return nil, fmt.Errorf("%s.limits.dollars exceeds the maximum of %d", path, maxWindowLimitsPerKind)
		}
		totalWindowLimits += len(rule.Limits.Dollars)
		if totalWindowLimits > maxWindowLimitsPerDoc {
			return nil, fmt.Errorf("policy exceeds the maximum of %d total request/token/dollar window limits", maxWindowLimitsPerDoc)
		}
		for j, limit := range rule.Limits.Dollars {
			limitPath := fmt.Sprintf("%s.limits.dollars[%d]", path, j)
			if err := validateLabel(limitPath+".id", limit.ID); err != nil {
				return nil, err
			}
			key := "dollars:" + limit.ID
			if _, exists := limitIDs[key]; exists {
				return nil, fmt.Errorf("duplicate dollar limit id %q in rule %q", limit.ID, rule.ID)
			}
			limitIDs[key] = struct{}{}
			if limit.MaxMicroUSD < 1 || limit.MaxMicroUSD > 1_000_000_000_000_000_000 {
				return nil, fmt.Errorf("%s.max_microusd must be between 1 and 1000000000000000000", limitPath)
			}
			if limit.WindowType != "rolling" && limit.WindowType != "fixed" {
				return nil, fmt.Errorf("%s.window_type must be rolling or fixed", limitPath)
			}
			duration, err := time.ParseDuration(limit.Window)
			if err != nil || duration < time.Second || duration > 366*24*time.Hour {
				return nil, fmt.Errorf("%s.window must be a duration from 1s through 8784h", limitPath)
			}
			windows[makePolicyWindowKey(rule.ID, "dollars", limit.ID)] = duration
		}
		if !ruleHasEffect(*rule) {
			return nil, fmt.Errorf("%s has no match restriction or limit", path)
		}
	}
	return windows, nil
}

func validMode(mode Mode) bool { return mode == ModeObserve || mode == ModeWarn || mode == ModeEnforce }

func validateLabel(path, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be blank", path)
	}
	if len(value) > 256 || utf8.RuneCountInString(value) > 128 || !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8 within 128 characters and 256 bytes", path)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			return fmt.Errorf("%s contains an unsafe character", path)
		}
	}
	return nil
}

func validateSelectors(path string, selectors []string) error {
	if len(selectors) > 64 {
		return fmt.Errorf("%s exceeds the maximum of 64 selectors", path)
	}
	seen := map[string]struct{}{}
	for _, selector := range selectors {
		// The empty selector intentionally matches an omitted optional wire
		// dimension such as default tier or global/unspecified geo.
		if selector != "" {
			if err := validateLabel(path, selector); err != nil {
				return err
			}
		}
		if len(selector) > 512 {
			return fmt.Errorf("%s selector exceeds 512 bytes", path)
		}
		if _, exists := seen[selector]; exists {
			return fmt.Errorf("%s contains duplicate selector %q", path, selector)
		}
		seen[selector] = struct{}{}
	}
	return nil
}

func ruleHasEffect(rule Rule) bool {
	for _, access := range matchAccess(rule.Match) {
		if len(access.Allow) != 0 || len(access.Deny) != 0 {
			return true
		}
	}
	return len(rule.Limits.Requests) != 0 || len(rule.Limits.Tokens) != 0 || len(rule.Limits.Dollars) != 0 ||
		rule.Limits.Concurrency != 0 || rule.Limits.MaxEstimatedCallCostUSD != nil ||
		rule.Limits.RequireOutputBound
}

func scopeSelectors(scope Scope) map[string][]string {
	return map[string][]string{
		"organization": scope.Organization, "tenant": scope.Tenant, "meter": scope.Meter, "device": scope.Device,
		"team": scope.Team, "cost_center": scope.CostCenter, "principal": scope.Principal,
		"service_account": scope.ServiceAccount, "user": scope.User, "project": scope.Project,
		"environment": scope.Environment, "agent": scope.Agent, "session": scope.Session,
		"provider": scope.Provider, "model": scope.Model, "model_class": scope.ModelClass, "route": scope.Route,
		"tier": scope.Tier, "service_tier": scope.ServiceTier, "geo": scope.Geo, "inference_geo": scope.InferenceGeo,
	}
}

func matchAccess(match Match) map[string]AccessList {
	out := map[string]AccessList{
		"provider": match.Provider, "model": match.Model, "route": match.Route,
		"tier": match.Tier, "geo": match.Geo,
	}
	for name, access := range map[string]*AccessList{
		"model_class": match.ModelClass, "service_tier": match.ServiceTier, "inference_geo": match.InferenceGeo,
	} {
		if access != nil {
			out[name] = *access
		}
	}
	return out
}

func contextValues(context Context) map[string]string {
	return map[string]string{
		"organization": context.Organization, "tenant": context.Tenant, "meter": context.Meter, "device": context.Device,
		"team": context.Team, "cost_center": context.CostCenter, "principal": context.Principal,
		"service_account": context.ServiceAccount, "user": context.User, "project": context.Project,
		"environment": context.Environment, "agent": context.Agent, "session": context.Session,
		"provider": context.Provider, "model": context.Model, "model_class": context.ModelClass, "route": context.Route,
		"tier": context.Tier, "service_tier": context.ServiceTier, "geo": context.Geo, "inference_geo": context.InferenceGeo,
	}
}

func ruleMode(doc Mode, rule Rule) Mode {
	if rule.Mode != "" {
		return rule.Mode
	}
	return doc
}

func ruleApplies(rule Rule, context Context) bool {
	values := contextValues(context)
	for dimension, selectors := range scopeSelectors(rule.Scope) {
		if len(selectors) != 0 && !matchesAny(selectors, values[dimension]) {
			return false
		}
	}
	return true
}

func matchesAny(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if wildcardMatch(pattern, value) {
			return true
		}
	}
	return false
}

// wildcardMatch supports '*' (any sequence) and '?' (one Unicode code point)
// without regex compilation or path-separator semantics.
func wildcardMatch(pattern, value string) bool {
	patternRunes, valueRunes := []rune(pattern), []rune(value)
	pi, vi, star, retry := 0, 0, -1, 0
	for vi < len(valueRunes) {
		if pi < len(patternRunes) && (patternRunes[pi] == '?' || patternRunes[pi] == valueRunes[vi]) {
			pi++
			vi++
			continue
		}
		if pi < len(patternRunes) && patternRunes[pi] == '*' {
			star, retry = pi, vi
			pi++
			continue
		}
		if star >= 0 {
			pi = star + 1
			retry++
			vi = retry
			continue
		}
		return false
	}
	for pi < len(patternRunes) && patternRunes[pi] == '*' {
		pi++
	}
	return pi == len(patternRunes)
}

func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(dec, "$"); err != nil {
		return fmt.Errorf("invalid policy JSON: %w", err)
	}
	return nil
}

func scanJSONValue(dec *json.Decoder, path string) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		seenFolded := map[string]string{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s object key is not a string", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate field %q at %s", key, path)
			}
			folded := asciiFold(key)
			if previous, exists := seenFolded[folded]; exists && previous != key {
				return fmt.Errorf("case-ambiguous fields %q and %q at %s", previous, key, path)
			}
			if canonical, securityKey := canonicalPolicyJSONKeys[folded]; securityKey && canonical != key {
				return fmt.Errorf("non-canonical policy field %q at %s; use %q", key, path, canonical)
			}
			seen[key] = struct{}{}
			seenFolded[folded] = key
			if err := scanJSONValue(dec, path+"."+key); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for i := 0; dec.More(); i++ {
			if err := scanJSONValue(dec, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
}

func asciiFold(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out.WriteByte(c)
	}
	return out.String()
}

var canonicalPolicyJSONKeys = func() map[string]string {
	out := map[string]string{}
	for _, key := range []string{
		"apiVersion", "kind", "metadata", "name", "namespace", "revision", "mode", "rules",
		"id", "scope", "match", "limits", "organization", "tenant", "meter", "device", "team",
		"cost_center", "principal", "service_account", "user", "project", "environment", "agent", "session",
		"provider", "model", "model_class", "route", "tier", "service_tier", "geo", "inference_geo",
		"allow", "deny", "requests", "tokens", "dollars", "concurrency", "max_estimated_call_cost_usd",
		"require_output_bound", "max", "max_microusd", "window", "window_type",
	} {
		out[asciiFold(key)] = key
	}
	return out
}()
