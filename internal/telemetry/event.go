// Package telemetry exports prompt-free Burnban ledger metadata to OTLP/HTTP
// collectors and portable warehouse batches. It deliberately has no hook on
// the provider request path.
package telemetry

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/burnban/burnban/internal/store"
)

const SchemaVersion = "burnban.telemetry.v1"

// Event is the shared, content-free contract for OTLP and warehouse exports.
// It intentionally has no prompt, response, request/response header, URL,
// query, provider credential, session, or request fingerprint field.
type Event struct {
	SchemaVersion string `json:"schema_version"`
	RequestID     int64  `json:"request_id"`
	ObservedAt    string `json:"observed_at"`

	Provider    string `json:"provider"`
	Model       string `json:"model,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Route       string `json:"route,omitempty"`
	ServiceTier string `json:"service_tier,omitempty"`
	Geo         string `json:"geo,omitempty"`

	Principal          string `json:"principal,omitempty"`
	TrustedPrincipal   string `json:"trusted_principal,omitempty"`
	ServiceAccount     string `json:"service_account,omitempty"`
	IdentityTenant     string `json:"identity_tenant,omitempty"`
	Project            string `json:"project,omitempty"`
	CostCenter         string `json:"cost_center,omitempty"`
	IdentityDeviceID   string `json:"identity_device_id,omitempty"`
	IdentityConfidence string `json:"identity_confidence"`

	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	CacheReadTokens    int64   `json:"cache_read_tokens"`
	CacheWriteTokens   int64   `json:"cache_write_tokens"`
	CacheWrite1hTokens int64   `json:"cache_write_1h_tokens"`
	CostUSD            float64 `json:"cost_usd"`
	LatencyMs          int64   `json:"latency_ms"`
	HTTPStatus         int     `json:"http_status"`
	Streamed           bool    `json:"streamed"`

	UsageConfidence   string `json:"usage_confidence"`
	PricingState      string `json:"pricing_state"`
	CostSource        string `json:"cost_source,omitempty"`
	CostSourceRef     string `json:"cost_source_ref,omitempty"`
	CostEffectiveFrom string `json:"cost_effective_from,omitempty"`
	CostValidThrough  string `json:"cost_valid_through,omitempty"`
	CostConfidence    string `json:"cost_confidence,omitempty"`
	Incomplete        bool   `json:"incomplete"`
	EnforcementGap    bool   `json:"enforcement_gap"`

	DecisionID       int64  `json:"decision_id,omitempty"`
	Decision         string `json:"decision,omitempty"`
	DecisionAdmitted bool   `json:"decision_admitted,omitempty"`
	PolicyName       string `json:"policy_name,omitempty"`
	PolicyNamespace  string `json:"policy_namespace,omitempty"`
	PolicyVersion    int64  `json:"policy_version,omitempty"`
	PolicyDigest     string `json:"policy_digest,omitempty"`
	PolicyMode       string `json:"policy_mode,omitempty"`
	PolicyConfidence string `json:"policy_confidence,omitempty"`

	RetryCount         int64   `json:"retry_count"`
	Downshifted        bool    `json:"downshifted"`
	DownshiftFrom      string  `json:"downshift_from,omitempty"`
	RequestedProvider  string  `json:"requested_provider,omitempty"`
	RequestedRoute     string  `json:"requested_route,omitempty"`
	DownshiftAction    string  `json:"downshift_action,omitempty"`
	DownshiftRule      string  `json:"downshift_rule,omitempty"`
	DownshiftTrigger   string  `json:"downshift_trigger,omitempty"`
	DownshiftReason    string  `json:"downshift_reason,omitempty"`
	DownshiftDigest    string  `json:"downshift_config_digest,omitempty"`
	DownshiftSourceUSD float64 `json:"downshift_source_estimated_usd,omitempty"`
	DownshiftTargetUSD float64 `json:"downshift_target_estimated_usd,omitempty"`
}

// FromRow projects only the explicit allowlist above. Even if the ledger grows
// content-bearing fields in the future, they cannot leak through reflection or
// embedding here.
func FromRow(row store.TelemetryRow) Event {
	r := row.Request
	e := Event{
		SchemaVersion: SchemaVersion, RequestID: row.ID,
		ObservedAt: r.Ts.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		Provider:   safeLabel(r.Provider), Model: safeLabel(r.Model), Agent: safeLabel(r.Agent),
		Route: safeLabel(r.Route), ServiceTier: safeLabel(r.ServiceTier), Geo: safeLabel(r.InferenceGeo),
		InputTokens: nonNegative(r.InTokens), OutputTokens: nonNegative(r.OutTokens),
		CacheReadTokens: nonNegative(r.CacheReadTokens), CacheWriteTokens: nonNegative(r.CacheWriteTokens),
		CacheWrite1hTokens: nonNegative(r.CacheWrite1hTokens), CostUSD: safeCost(r.CostUSD),
		LatencyMs: nonNegative(r.LatencyMs), HTTPStatus: safeHTTPStatus(r.Status), Streamed: r.Streamed,
		UsageConfidence: safeLabel(string(r.UsageState)), PricingState: safeLabel(string(r.PricingState)),
		CostSource: safeLabel(string(r.CostSource)), CostSourceRef: safeSourceRef(r.CostSourceRef),
		CostEffectiveFrom: safeDate(r.CostEffectiveFrom), CostValidThrough: safeDate(r.CostValidThrough),
		CostConfidence: safeLabel(string(r.CostConfidence)),
		Incomplete:     r.Incomplete, EnforcementGap: r.EnforcementUnsafe,
		IdentityConfidence: "unverified",
	}
	if r.DownshiftAction == "warn" || r.DownshiftAction == "downshift" {
		e.DownshiftAction = safeLabel(r.DownshiftAction)
		e.Downshifted = r.DownshiftAction == "downshift"
		e.DownshiftFrom = safeLabel(r.RequestedModel)
		e.RequestedProvider = safeLabel(r.RequestedProvider)
		e.RequestedRoute = safeLabel(r.RequestedRoute)
		e.DownshiftRule = safeLabel(r.DownshiftRule)
		e.DownshiftTrigger = safeLabel(r.DownshiftTrigger)
		e.DownshiftReason = safeLabel(r.DownshiftReason)
		e.DownshiftDigest = safeDigest(r.DownshiftDigest)
		e.DownshiftSourceUSD = safeCost(r.DownshiftSourceUSD)
		e.DownshiftTargetUSD = safeCost(r.DownshiftTargetUSD)
	}

	// These fields are populated by signed-identity aware ledgers. Older rows
	// remain explicitly unverified instead of being silently promoted.
	e.Principal = safeLabel(r.Principal)
	e.ServiceAccount = safeLabel(r.ServiceAccount)
	e.IdentityTenant = safeLabel(r.IdentityTenant)
	e.Project = safeLabel(r.Project)
	e.CostCenter = safeLabel(r.CostCenter)
	e.IdentityDeviceID = safeLabel(r.IdentityDevice)
	if confidence := safeIdentityConfidence(r.IdentityConfidence); confidence != "" {
		e.IdentityConfidence = confidence
	}
	if e.IdentityConfidence == "authenticated" {
		if e.Principal != "" {
			e.TrustedPrincipal = e.Principal
		} else {
			e.TrustedPrincipal = e.ServiceAccount
		}
	}

	if r.Policy != nil {
		e.DecisionID = r.Policy.DecisionID
		e.Decision = safeLabel(r.Policy.Outcome)
		e.DecisionAdmitted = r.Policy.Admitted
		e.PolicyName = safeLabel(r.Policy.Name)
		e.PolicyNamespace = safeLabel(r.Policy.Namespace)
		e.PolicyVersion = r.Policy.Revision
		e.PolicyDigest = safeDigest(r.Policy.Digest)
		e.PolicyMode = safeLabel(r.Policy.Mode)
		e.PolicyConfidence = safeLabel(r.Policy.Confidence)
		// Compatibility for policy decisions recorded before signed identity
		// columns existed. Self-reported values are exported only with their
		// explicit confidence and never copied into trusted_principal.
		if e.Principal == "" && len(r.Policy.ContextJSON) <= 64<<10 {
			var context struct {
				User               string `json:"user"`
				Project            string `json:"project"`
				Team               string `json:"team"`
				IdentityConfidence string `json:"identity_confidence"`
			}
			if json.Unmarshal([]byte(r.Policy.ContextJSON), &context) == nil {
				e.Principal = safeLabel(context.User)
				e.Project = firstNonEmpty(e.Project, safeLabel(context.Project))
				e.CostCenter = firstNonEmpty(e.CostCenter, safeLabel(context.Team))
				if e.IdentityConfidence == "unverified" {
					e.IdentityConfidence = firstNonEmpty(safeIdentityConfidence(context.IdentityConfidence), "unverified")
				}
			}
		}
	}
	return e
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func safeCost(v float64) float64 {
	if v < 0 || v != v || v > 1e12 {
		return 0
	}
	return v
}

func safeHTTPStatus(status int) int {
	if status < 0 || status > 999 {
		return 0
	}
	return status
}

func safeSourceRef(value string) string {
	value = safeLabel(value)
	if !strings.Contains(value, "://") {
		return value
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	// Price sources can be URLs. Preserve only the public origin: userinfo,
	// signed paths, queries, and fragments are unnecessary for attribution and
	// could contain credentials.
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}

func safeIdentityConfidence(value string) string {
	switch value {
	case "authenticated", "self_reported", "unverified":
		return value
	default:
		return ""
	}
}

func safeDigest(value string) string {
	value = strings.ToLower(value)
	if len(value) != 64 {
		return ""
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return ""
		}
	}
	return value
}

func safeDate(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return ""
	}
	return parsed.Format("2006-01-02")
}

func safeLabel(value string) string {
	if !utf8.ValidString(value) {
		return ""
	}
	var out strings.Builder
	out.Grow(min(len(value), 256))
	for _, r := range value {
		if out.Len()+utf8.RuneLen(r) > 256 {
			break
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			continue
		}
		out.WriteRune(r)
	}
	return strings.TrimSpace(out.String())
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
