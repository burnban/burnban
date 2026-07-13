package policy

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/burnban/burnban/internal/store"
)

type Engine struct {
	S *store.Store

	mu          sync.Mutex
	active      *Compiled
	activeID    int64
	inFlight    map[ruleCounterKey]int64
	pending     map[ruleCounterKey]map[uint64]pendingUsage
	nextPending uint64
}

func NewEngine(s *store.Store) *Engine {
	return &Engine{S: s, inFlight: map[ruleCounterKey]int64{}, pending: map[ruleCounterKey]map[uint64]pendingUsage{}}
}

// ruleCounterKey is deliberately structured. Namespace and rule identifiers
// may both contain '/', so delimiter concatenation is not injective and could
// make a reservation from a reset policy lineage consume the new lineage's
// pending or concurrency capacity.
type ruleCounterKey struct {
	namespace string
	ruleID    string
}

type pendingUsage struct {
	ts           time.Time
	requests     int64
	inputTokens  int64
	outputTokens int64
	totalTokens  int64
	costMicroUSD int64
}

type Violation struct {
	Code      string  `json:"code"`
	Dimension string  `json:"dimension,omitempty"`
	LimitID   string  `json:"limit_id,omitempty"`
	Message   string  `json:"message"`
	Current   int64   `json:"current,omitempty"`
	Requested int64   `json:"requested,omitempty"`
	Maximum   int64   `json:"maximum,omitempty"`
	USD       float64 `json:"estimated_cost_usd,omitempty"`
	MaxUSD    float64 `json:"max_cost_usd,omitempty"`
}

type RuleDecision struct {
	RuleID      string      `json:"rule_id"`
	Mode        Mode        `json:"mode"`
	Specificity int         `json:"specificity"`
	Violations  []Violation `json:"violations,omitempty"`
}

type Decision struct {
	ID               int64          `json:"-"`
	PolicyName       string         `json:"policy_name"`
	PolicyNamespace  string         `json:"policy_namespace"`
	PolicyRevision   int64          `json:"policy_revision"`
	PolicyDigest     string         `json:"policy_digest"`
	Mode             Mode           `json:"mode"`
	Outcome          string         `json:"outcome"`
	HTTPStatus       int            `json:"http_status,omitempty"`
	Confidence       string         `json:"confidence"`
	WindowAccounting string         `json:"window_accounting,omitempty"`
	Summary          string         `json:"summary"`
	Rules            []RuleDecision `json:"rules"`
}

func (d *Decision) Denied() bool { return d != nil && d.Outcome == "deny" }

type Reservation struct {
	engine          *Engine
	decision        *Decision
	record          store.PolicyDecisionRecord
	concurrencyKeys []ruleCounterKey
	counterKeys     []ruleCounterKey
	pendingID       uint64
	state           int // 0 prepared, 1 committed/in-flight, 2 released or cancelled
}

func (r *Reservation) Release() {
	if r == nil || r.engine == nil {
		return
	}
	r.engine.mu.Lock()
	defer r.engine.mu.Unlock()
	if r.state == 2 {
		return
	}
	if r.state == 0 {
		r.releasePendingLocked()
	}
	r.releaseConcurrencyLocked()
	r.state = 2
}

func (r *Reservation) Commit() error {
	if r == nil || r.engine == nil {
		return nil
	}
	r.engine.mu.Lock()
	defer r.engine.mu.Unlock()
	if r.state == 1 {
		return nil
	}
	if r.state == 2 {
		return fmt.Errorf("policy reservation is already closed")
	}
	if err := r.persistLocked(true); err != nil {
		r.releasePendingLocked()
		r.releaseConcurrencyLocked()
		r.state = 2
		return err
	}
	r.releasePendingLocked()
	r.state = 1
	return nil
}

func (r *Reservation) Cancel() error {
	if r == nil || r.engine == nil {
		return nil
	}
	r.engine.mu.Lock()
	defer r.engine.mu.Unlock()
	if r.state != 0 {
		return nil
	}
	err := r.persistLocked(false)
	r.releasePendingLocked()
	r.releaseConcurrencyLocked()
	r.state = 2
	return err
}

func (r *Reservation) persistLocked(admitted bool) error {
	r.record.Admitted = admitted
	rules := make([]store.PolicyDecisionRule, len(r.record.Rules))
	copy(rules, r.record.Rules)
	for i := range rules {
		rules[i].Accepted = admitted
	}
	r.record.Rules = rules
	id, err := r.engine.S.InsertPolicyDecision(r.record)
	if err != nil {
		return fmt.Errorf("persist policy decision: %w", err)
	}
	r.decision.ID = id
	return nil
}

func (r *Reservation) releasePendingLocked() {
	for _, key := range r.counterKeys {
		delete(r.engine.pending[key], r.pendingID)
		if len(r.engine.pending[key]) == 0 {
			delete(r.engine.pending, key)
		}
	}
	r.counterKeys = nil
}

func (r *Reservation) releaseConcurrencyLocked() {
	for _, key := range r.concurrencyKeys {
		if r.engine.inFlight[key] <= 1 {
			delete(r.engine.inFlight, key)
		} else {
			r.engine.inFlight[key]--
		}
	}
	r.concurrencyKeys = nil
}

// Admit is the one-guard convenience API. The proxy uses Prepare and commits
// only after its independent dollar guard accepts the request.
func (e *Engine) Admit(now time.Time, context Context) (*Reservation, *Decision, error) {
	reservation, decision, err := e.Prepare(now, context)
	if err != nil || decision == nil || decision.Denied() {
		return reservation, decision, err
	}
	if err := reservation.Commit(); err != nil {
		return nil, nil, err
	}
	return reservation, decision, nil
}

// Prepare intersects every applicable rule. Its mutex covers durable usage
// reads plus pending request/token and concurrency reservations, so races cannot
// observe stale capacity. The durable accepted counter is written by Commit.
func (e *Engine) Prepare(now time.Time, context Context) (*Reservation, *Decision, error) {
	if e == nil || e.S == nil {
		return nil, nil, fmt.Errorf("policy engine has no store")
	}
	if math.IsNaN(context.EstimatedCostUSD) || math.IsInf(context.EstimatedCostUSD, 0) {
		return nil, nil, fmt.Errorf("estimated request cost must be finite")
	}
	if context.EstimatedInput < 0 || context.OutputBound < 0 || context.EstimatedCostUSD < 0 {
		return nil, nil, fmt.Errorf("estimated tokens and cost must be non-negative")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refreshLocked(); err != nil {
		return nil, nil, err
	}
	if e.active == nil {
		return nil, nil, nil
	}

	compiled := e.active
	rules := applicableRules(compiled.Document, context)
	decision := &Decision{
		PolicyName: compiled.Document.Metadata.Name, PolicyNamespace: compiled.Document.Metadata.Namespace,
		PolicyRevision: compiled.Document.Metadata.Revision,
		PolicyDigest:   compiled.Digest, Mode: compiled.Document.Mode, Outcome: "allow",
		Confidence: decisionConfidence(context), Rules: make([]RuleDecision, 0, len(rules)),
	}
	for _, indexed := range rules {
		if ruleHasCounters(indexed.Rule) {
			decision.WindowAccounting = "admission_requests_and_conservative_bounds"
			break
		}
	}
	estimatedTokens := context.EstimatedTokens()
	estimatedInput := max(context.EstimatedInput, 0)
	estimatedOutput, outputBounded := context.EstimatedTokensForKind("output")
	estimatedTotal, totalBounded := context.EstimatedTokensForKind("total")
	estimatedCostMicroUSD, costBounded := context.ConservativeCostMicroUSD()
	if context.CostKnown && !costBounded {
		return nil, nil, fmt.Errorf("estimated request cost cannot be represented conservatively in micro-USD")
	}
	for _, indexed := range rules {
		rule := indexed.Rule
		ruleDecision := RuleDecision{
			RuleID: rule.ID, Mode: ruleMode(compiled.Document.Mode, rule), Specificity: indexed.Specificity,
			Violations: staticViolations(rule, context),
		}
		if ruleDecision.Mode == ModeEnforce && !identityScopeAuthenticated(rule.Scope, context) {
			ruleDecision.Violations = append(ruleDecision.Violations, Violation{
				Code: "authenticated_identity_required", Dimension: "identity",
				Message: "server-authorized identity is required for every identity dimension in this scoped enforcement rule",
			})
		}
		for _, limit := range rule.Limits.Requests {
			duration := compiled.windows[makePolicyWindowKey(rule.ID, "requests", limit.ID)]
			cutoff := windowStart(now, duration, limit.WindowType)
			usage, err := e.S.PolicyRuleUsageSince(compiled.Document.Metadata.Namespace, rule.ID, cutoff)
			if err != nil {
				return nil, nil, fmt.Errorf("load request limit %s/%s: %w", rule.ID, limit.ID, err)
			}
			pending := e.pendingSince(counterKey(compiled.Document.Metadata.Namespace, rule.ID), cutoff)
			usage.Requests = saturatedAdd(usage.Requests, pending.requests)
			if usage.Requests >= limit.Max {
				ruleDecision.Violations = append(ruleDecision.Violations, Violation{
					Code: "request_limit", Dimension: "requests", LimitID: limit.ID,
					Message: fmt.Sprintf("request limit %s permits %d per %s %s window", limit.ID, limit.Max, limit.Window, limit.WindowType),
					Current: usage.Requests, Requested: 1, Maximum: limit.Max,
				})
			}
		}
		for _, limit := range rule.Limits.Tokens {
			requested, bounded := context.EstimatedTokensForKind(limit.Kind)
			if !bounded {
				continue
			}
			duration := compiled.windows[makePolicyWindowKey(rule.ID, "tokens", limit.ID)]
			cutoff := windowStart(now, duration, limit.WindowType)
			usage, err := e.S.PolicyRuleUsageSince(compiled.Document.Metadata.Namespace, rule.ID, cutoff)
			if err != nil {
				return nil, nil, fmt.Errorf("load token limit %s/%s: %w", rule.ID, limit.ID, err)
			}
			pending := e.pendingSince(counterKey(compiled.Document.Metadata.Namespace, rule.ID), cutoff)
			current := tokenUsageForKind(usage, limit.Kind)
			current = saturatedAdd(current, pendingTokensForKind(pending, limit.Kind))
			if current > limit.Max-requested {
				ruleDecision.Violations = append(ruleDecision.Violations, Violation{
					Code: "token_limit", Dimension: tokenLimitKind(limit.Kind) + "_tokens", LimitID: limit.ID,
					Message: fmt.Sprintf("%s-token limit %s permits %d per %s %s window", tokenLimitKind(limit.Kind), limit.ID, limit.Max, limit.Window, limit.WindowType),
					Current: current, Requested: requested, Maximum: limit.Max,
				})
			}
		}
		for _, limit := range rule.Limits.Dollars {
			if !costBounded {
				ruleDecision.Violations = append(ruleDecision.Violations, Violation{
					Code: "estimated_cost_unknown", Dimension: "dollars", LimitID: limit.ID,
					Message: "request cost cannot be conservatively bounded for a scoped dollar window",
					Maximum: limit.MaxMicroUSD,
				})
				continue
			}
			duration := compiled.windows[makePolicyWindowKey(rule.ID, "dollars", limit.ID)]
			cutoff := windowStart(now, duration, limit.WindowType)
			usage, err := e.S.PolicyRuleUsageSince(compiled.Document.Metadata.Namespace, rule.ID, cutoff)
			if err != nil {
				return nil, nil, fmt.Errorf("load dollar limit %s/%s: %w", rule.ID, limit.ID, err)
			}
			pending := e.pendingSince(counterKey(compiled.Document.Metadata.Namespace, rule.ID), cutoff)
			current := saturatedAdd(usage.CostMicroUSD, pending.costMicroUSD)
			if current > limit.MaxMicroUSD-estimatedCostMicroUSD {
				ruleDecision.Violations = append(ruleDecision.Violations, Violation{
					Code: "dollar_limit", Dimension: "dollars", LimitID: limit.ID,
					Message: fmt.Sprintf("dollar limit %s permits %d micro-USD per %s %s window", limit.ID, limit.MaxMicroUSD, limit.Window, limit.WindowType),
					Current: current, Requested: estimatedCostMicroUSD, Maximum: limit.MaxMicroUSD,
				})
			}
		}
		if rule.Limits.Concurrency > 0 {
			key := counterKey(compiled.Document.Metadata.Namespace, rule.ID)
			current := e.inFlight[key]
			if current+1 > rule.Limits.Concurrency {
				ruleDecision.Violations = append(ruleDecision.Violations, Violation{
					Code: "concurrency_limit", Dimension: "concurrency",
					Message: fmt.Sprintf("concurrency limit permits %d in-flight requests", rule.Limits.Concurrency),
					Current: current, Requested: 1, Maximum: rule.Limits.Concurrency,
				})
			}
		}
		sortViolations(ruleDecision.Violations)
		decision.Rules = append(decision.Rules, ruleDecision)
	}

	enforced := enforcedViolations(decision.Rules)
	if len(enforced) != 0 {
		decision.Outcome = "deny"
		decision.HTTPStatus = statusForViolations(enforced)
	}
	decision.Summary = summarizeDecision(decision)
	contextJSON, err := json.Marshal(context)
	if err != nil {
		return nil, nil, err
	}
	explanationJSON, err := json.Marshal(decision)
	if err != nil {
		return nil, nil, err
	}
	ruleRows := make([]store.PolicyDecisionRule, 0, len(decision.Rules))
	countedRules := map[string]bool{}
	for _, indexed := range rules {
		countedRules[indexed.Rule.ID] = ruleHasCounters(indexed.Rule)
	}
	for _, rule := range decision.Rules {
		if !countedRules[rule.RuleID] {
			continue
		}
		ruleRows = append(ruleRows, store.PolicyDecisionRule{
			RuleID: rule.RuleID, Accepted: decision.Outcome == "allow", EstimatedTokens: estimatedTokens,
			EstimatedInputTokens: estimatedInput, EstimatedOutputTokens: estimatedOutput,
			EstimatedTotalTokens: estimatedTotal, EstimatedCostMicroUSD: estimatedCostMicroUSD,
		})
	}
	record := store.PolicyDecisionRecord{
		Ts: now, PolicyDigest: decision.PolicyDigest, PolicyRevision: decision.PolicyRevision,
		PolicyName: decision.PolicyName, PolicyNamespace: decision.PolicyNamespace,
		Mode: string(decision.Mode), Outcome: decision.Outcome,
		HTTPStatus: decision.HTTPStatus, Confidence: decision.Confidence,
		ContextJSON: string(contextJSON), ExplanationJSON: string(explanationJSON), Rules: ruleRows,
	}
	if decision.Denied() {
		decision.ID, err = e.S.InsertPolicyDecision(record)
		if err != nil {
			return nil, nil, fmt.Errorf("persist policy decision: %w", err)
		}
		return nil, decision, nil
	}
	e.nextPending++
	reservation := &Reservation{engine: e, decision: decision, record: record, pendingID: e.nextPending}
	for _, indexed := range rules {
		key := counterKey(compiled.Document.Metadata.Namespace, indexed.Rule.ID)
		if ruleHasCounters(indexed.Rule) {
			if e.pending[key] == nil {
				e.pending[key] = map[uint64]pendingUsage{}
			}
			e.pending[key][reservation.pendingID] = pendingUsage{
				ts: now, requests: 1, inputTokens: estimatedInput,
				outputTokens: boundedCounterValue(estimatedOutput, outputBounded),
				totalTokens:  boundedCounterValue(estimatedTotal, totalBounded),
				costMicroUSD: boundedCounterValue(estimatedCostMicroUSD, costBounded),
			}
			reservation.counterKeys = append(reservation.counterKeys, key)
		}
		if indexed.Rule.Limits.Concurrency != 0 {
			e.inFlight[key]++
			reservation.concurrencyKeys = append(reservation.concurrencyKeys, key)
		}
	}
	return reservation, decision, nil
}

func (e *Engine) pendingSince(key ruleCounterKey, cutoff time.Time) pendingUsage {
	var out pendingUsage
	for _, usage := range e.pending[key] {
		if usage.ts.Before(cutoff) {
			continue
		}
		out.requests = saturatedAdd(out.requests, usage.requests)
		out.inputTokens = saturatedAdd(out.inputTokens, usage.inputTokens)
		out.outputTokens = saturatedAdd(out.outputTokens, usage.outputTokens)
		out.totalTokens = saturatedAdd(out.totalTokens, usage.totalTokens)
		out.costMicroUSD = saturatedAdd(out.costMicroUSD, usage.costMicroUSD)
	}
	return out
}

func tokenLimitKind(kind string) string {
	if kind == "" {
		return "total"
	}
	return kind
}

func tokenUsageForKind(usage store.PolicyRuleUsage, kind string) int64 {
	switch tokenLimitKind(kind) {
	case "input":
		return usage.InputTokens
	case "output":
		return usage.OutputTokens
	default:
		return usage.TotalTokens
	}
}

func pendingTokensForKind(usage pendingUsage, kind string) int64 {
	switch tokenLimitKind(kind) {
	case "input":
		return usage.inputTokens
	case "output":
		return usage.outputTokens
	default:
		return usage.totalTokens
	}
}

func ruleHasCounters(rule Rule) bool {
	return len(rule.Limits.Requests) != 0 || len(rule.Limits.Tokens) != 0 || len(rule.Limits.Dollars) != 0
}

func boundedCounterValue(value int64, bounded bool) int64 {
	if !bounded {
		return 0
	}
	return value
}

func (e *Engine) refreshLocked() error {
	record, err := e.S.ActivePolicyDocument()
	if err != nil {
		return fmt.Errorf("load active policy: %w", err)
	}
	if record == nil {
		e.active, e.activeID = nil, 0
		return nil
	}
	if e.active != nil && e.activeID == record.ID && e.active.Digest == record.Digest {
		return nil
	}
	compiled, err := Parse([]byte(record.DocumentJSON))
	if err != nil {
		return fmt.Errorf("active policy is invalid: %w", err)
	}
	if compiled.Digest != record.Digest || compiled.Document.Metadata.Revision != record.Revision ||
		compiled.Document.Metadata.Namespace != record.Namespace {
		return fmt.Errorf("active policy metadata does not match its durable record")
	}
	e.active, e.activeID = compiled, record.ID
	return nil
}

type indexedRule struct {
	Rule        Rule
	Specificity int
}

func applicableRules(doc Document, context Context) []indexedRule {
	out := make([]indexedRule, 0, len(doc.Rules))
	for _, rule := range doc.Rules {
		applies := ruleApplies(rule, context)
		if ruleMode(doc.Mode, rule) == ModeEnforce && !identityScopeAuthenticated(rule.Scope, context) {
			applies = ruleAppliesIgnoringUntrustedIdentity(rule, context)
		}
		if applies {
			out = append(out, indexedRule{Rule: rule, Specificity: specificity(rule.Scope)})
		}
	}
	// Explanations are stable and show narrower scopes first. Evaluation still
	// intersects all rules, so this order never changes the outcome.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Specificity != out[j].Specificity {
			return out[i].Specificity > out[j].Specificity
		}
		return out[i].Rule.ID < out[j].Rule.ID
	})
	return out
}

func hasIdentityScope(scope Scope) bool {
	for dimension, selectors := range scopeSelectors(scope) {
		if len(selectors) != 0 && identityDimensionConfidence(Context{}, dimension) != "not_identity" {
			return true
		}
	}
	return false
}

func identityScopeAuthenticated(scope Scope, context Context) bool {
	if !hasIdentityScope(scope) {
		return true
	}
	for dimension, selectors := range scopeSelectors(scope) {
		if len(selectors) == 0 {
			continue
		}
		confidence := identityDimensionConfidence(context, dimension)
		if confidence != "not_identity" && confidence != "authenticated" {
			return false
		}
	}
	return true
}

// ruleAppliesIgnoringUntrustedIdentity prevents an untrusted or omitted label
// from making an enforcing scoped rule disappear. Identity dimensions that do
// have exact server authorization still participate in matching, so a trusted
// team/user/project cannot be broadened by a different untrusted dimension.
func ruleAppliesIgnoringUntrustedIdentity(rule Rule, context Context) bool {
	values := contextValues(context)
	for dimension, selectors := range scopeSelectors(rule.Scope) {
		confidence := identityDimensionConfidence(context, dimension)
		if confidence != "not_identity" && confidence != "authenticated" {
			continue
		}
		if len(selectors) != 0 && !matchesAny(selectors, values[dimension]) {
			return false
		}
	}
	return true
}

func identityDimensionConfidence(context Context, dimension string) string {
	switch dimension {
	case "organization":
		return context.OrganizationConfidence
	case "tenant":
		return context.TenantConfidence
	case "meter":
		return context.MeterConfidence
	case "device":
		return context.DeviceConfidence
	case "team":
		return context.TeamConfidence
	case "cost_center":
		return context.CostCenterConfidence
	case "principal":
		return context.PrincipalConfidence
	case "service_account":
		return context.ServiceAccountConfidence
	case "user":
		return context.UserConfidence
	case "project":
		return context.ProjectConfidence
	case "environment":
		return context.EnvironmentConfidence
	default:
		return "not_identity"
	}
}

func specificity(scope Scope) int {
	n := 0
	for _, selectors := range scopeSelectors(scope) {
		if len(selectors) != 0 {
			n++
		}
	}
	return n
}

func staticViolations(rule Rule, context Context) []Violation {
	values := contextValues(context)
	var out []Violation
	for _, dimension := range []string{
		"provider", "model", "model_class", "route", "tier", "service_tier", "geo", "inference_geo",
	} {
		access := matchAccess(rule.Match)[dimension]
		value := values[dimension]
		switch {
		case len(access.Deny) != 0 && matchesAny(access.Deny, value):
			out = append(out, Violation{Code: "deny_match", Dimension: dimension,
				Message: fmt.Sprintf("%s %q matches a deny selector", dimension, value)})
		case len(access.Allow) != 0 && !matchesAny(access.Allow, value):
			out = append(out, Violation{Code: "not_allowed", Dimension: dimension,
				Message: fmt.Sprintf("%s %q is outside the allow selectors", dimension, value)})
		}
	}
	requiresOutputBound := rule.Limits.RequireOutputBound
	for _, limit := range rule.Limits.Tokens {
		if tokenLimitKind(limit.Kind) != "input" {
			requiresOutputBound = true
			break
		}
	}
	if requiresOutputBound && !context.OutputBoundPresent {
		out = append(out, Violation{Code: "output_bound_required", Dimension: "output_tokens",
			Message: "request must provide a finite output-token bound"})
	}
	if maximum := rule.Limits.MaxEstimatedCallCostUSD; maximum != nil {
		switch {
		case !context.CostKnown:
			out = append(out, Violation{Code: "estimated_cost_unknown", Dimension: "cost",
				Message: "request cost cannot be bounded with the active price table", MaxUSD: *maximum})
		case context.EstimatedCostUSD > *maximum:
			out = append(out, Violation{Code: "max_estimated_call_cost", Dimension: "cost",
				Message: fmt.Sprintf("estimated call cost $%.6f exceeds $%.6f", context.EstimatedCostUSD, *maximum),
				USD:     context.EstimatedCostUSD, MaxUSD: *maximum})
		}
	}
	sortViolations(out)
	return out
}

func sortViolations(violations []Violation) {
	sort.SliceStable(violations, func(i, j int) bool {
		a, b := violations[i], violations[j]
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Dimension != b.Dimension {
			return a.Dimension < b.Dimension
		}
		return a.LimitID < b.LimitID
	})
}

func enforcedViolations(rules []RuleDecision) []Violation {
	var out []Violation
	for _, rule := range rules {
		if rule.Mode == ModeEnforce {
			out = append(out, rule.Violations...)
		}
	}
	return out
}

func statusForViolations(violations []Violation) int {
	status := http.StatusUnprocessableEntity
	for _, violation := range violations {
		switch violation.Code {
		case "authenticated_identity_required":
			return http.StatusUnauthorized
		case "deny_match", "not_allowed":
			return http.StatusForbidden
		case "max_estimated_call_cost", "estimated_cost_unknown", "dollar_limit":
			status = http.StatusPaymentRequired
		case "request_limit", "token_limit", "concurrency_limit":
			if status != http.StatusPaymentRequired {
				status = http.StatusTooManyRequests
			}
		}
	}
	return status
}

func summarizeDecision(decision *Decision) string {
	if decision == nil {
		return "no active policy"
	}
	matched, violations, warnings, observed := len(decision.Rules), 0, 0, 0
	for _, rule := range decision.Rules {
		violations += len(rule.Violations)
		if len(rule.Violations) == 0 {
			continue
		}
		switch rule.Mode {
		case ModeWarn:
			warnings += len(rule.Violations)
		case ModeObserve:
			observed += len(rule.Violations)
		}
	}
	if decision.Outcome == "deny" {
		return fmt.Sprintf("denied by policy %s revision %d (%d applicable rules, %d violations)",
			decision.PolicyName, decision.PolicyRevision, matched, violations)
	}
	parts := []string{fmt.Sprintf("allowed by policy %s revision %d (%d applicable rules)",
		decision.PolicyName, decision.PolicyRevision, matched)}
	if warnings != 0 {
		parts = append(parts, fmt.Sprintf("%d warning violations", warnings))
	}
	if observed != 0 {
		parts = append(parts, fmt.Sprintf("%d observed violations", observed))
	}
	return strings.Join(parts, "; ")
}

func decisionConfidence(context Context) string {
	if context.IdentityConfidence == "self_reported" || context.IdentityConfidence == "unverified" {
		return "partial"
	}
	fieldConfidencePresent := false
	for _, confidence := range []string{
		context.OrganizationConfidence, context.TenantConfidence, context.MeterConfidence, context.DeviceConfidence,
		context.TeamConfidence, context.CostCenterConfidence, context.PrincipalConfidence,
		context.ServiceAccountConfidence, context.UserConfidence, context.ProjectConfidence,
		context.EnvironmentConfidence,
	} {
		if confidence != "" {
			fieldConfidencePresent = true
			if confidence != "authenticated" {
				return "partial"
			}
		}
	}
	// Older persisted contexts only had envelope-level confidence and cannot
	// prove which individual identity dimensions the server authorized.
	if context.IdentityConfidence == "authenticated" && !fieldConfidencePresent {
		return "partial"
	}
	return "exact"
}

func windowStart(now time.Time, duration time.Duration, kind string) time.Time {
	now = now.UTC()
	if kind == "rolling" {
		return now.Add(-duration)
	}
	nanos := now.UnixNano()
	return time.Unix(0, nanos-nanos%int64(duration)).UTC()
}

func counterKey(namespace, ruleID string) ruleCounterKey {
	return ruleCounterKey{namespace: namespace, ruleID: ruleID}
}

type ActiveSummary struct {
	Active    bool      `json:"active"`
	Name      string    `json:"name,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
	Revision  int64     `json:"revision,omitempty"`
	Digest    string    `json:"digest,omitempty"`
	Mode      Mode      `json:"mode,omitempty"`
	Rules     int       `json:"rules,omitempty"`
	AppliedAt time.Time `json:"applied_at,omitempty"`
}

func LoadActiveSummary(s *store.Store) (ActiveSummary, error) {
	record, err := s.ActivePolicyDocument()
	if err != nil || record == nil {
		return ActiveSummary{}, err
	}
	compiled, err := Parse([]byte(record.DocumentJSON))
	if err != nil {
		return ActiveSummary{}, err
	}
	return ActiveSummary{
		Active: true, Name: compiled.Document.Metadata.Name, Namespace: compiled.Document.Metadata.Namespace,
		Revision: compiled.Document.Metadata.Revision,
		Digest:   compiled.Digest, Mode: compiled.Document.Mode, Rules: len(compiled.Document.Rules),
		AppliedAt: record.AppliedAt,
	}, nil
}
