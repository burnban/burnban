package proxy

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/downshift"
	"github.com/burnban/burnban/internal/policy"
)

type downshiftPlan struct {
	started  time.Time
	compiled *downshift.Compiled
	decision downshift.Decision
	features downshift.Features

	requestedProvider string
	requestedModel    string
	requestedRoute    string
	requestedPath     string
	requestedBody     []byte
	requestedShape    string
	requestedInfo     requestEstimate

	provider    string
	route       string
	path        string
	body        []byte
	shape       string
	requestInfo requestEstimate
}

func (p *Proxy) prepareDownshift(start time.Time, provider, route, path string, body []byte,
	requestInfo requestEstimate, attribution requestIdentityAttribution) (*downshiftPlan, error) {
	shape := p.upstreams[provider].shape
	plan := &downshiftPlan{
		started: start, features: downshift.AnalyzeAt(body, shape, path), requestedProvider: provider,
		requestedModel: requestInfo.model, requestedRoute: route, requestedPath: path,
		requestedBody: body, requestedShape: shape, requestedInfo: requestInfo,
		provider: provider, route: route, path: path, body: body,
		shape: shape, requestInfo: requestInfo,
	}
	compiled, err := p.Downshift.Active()
	if err != nil {
		return nil, err
	}
	plan.compiled = compiled
	if compiled == nil {
		return plan, nil
	}
	pct, err := routingBudgetPct(p.Store, start)
	if err != nil {
		return nil, fmt.Errorf("load budget utilization for downshift routing: %w", err)
	}
	plan.decision = p.downshiftDecision(compiled, plan, attribution, pct, false)
	if plan.decision.Action == downshift.ActionDownshift {
		if err := p.applyDownshiftTarget(plan, false); err != nil {
			plan.decision.Action = downshift.ActionNone
			plan.decision.Reason = err.Error()
		}
	} else if plan.decision.Action == downshift.ActionWarn {
		candidate := *plan
		if err := p.applyDownshiftTarget(&candidate, false); err != nil {
			plan.decision.Action = downshift.ActionNone
			plan.decision.Reason = err.Error()
		}
	}
	return plan, nil
}

func (p *Proxy) downshiftDecision(compiled *downshift.Compiled, plan *downshiftPlan,
	attribution requestIdentityAttribution, pct float64, budgetDenial bool) downshift.Decision {
	rule := compiled.Rule(plan.requestedProvider, plan.requestedModel)
	targetExists, targetDialect := false, ""
	if rule != nil {
		if target, ok := p.upstreams[rule.Target.Route]; ok {
			targetExists, targetDialect = true, target.shape
		}
	}
	return downshift.Decide(compiled, downshift.Input{
		Route: plan.requestedProvider, Model: plan.requestedModel, Dialect: p.upstreams[plan.requestedProvider].shape,
		TargetRouteExists: targetExists, TargetDialect: targetDialect,
		Identity: downshift.Identity{
			Tenant: attribution.Tenant, Principal: attribution.Principal, ServiceAccount: attribution.ServiceAccount,
			Project: attribution.Project, CostCenter: attribution.CostCenter, Confidence: attribution.Confidence,
			TeamConfidence: attribution.TeamConfidence, UserConfidence: attribution.UserConfidence,
			ProjectConfidence: attribution.ProjectConfidence,
		},
		Features: plan.features, BudgetPct: pct, BudgetDenial: budgetDenial,
	})
}

func (p *Proxy) retryDownshiftAfterDenial(plan *downshiftPlan, attribution requestIdentityAttribution) error {
	if plan.compiled == nil {
		return fmt.Errorf("no active downshift configuration")
	}
	plan.decision = p.downshiftDecision(plan.compiled, plan, attribution, plan.decision.BudgetPct, true)
	if plan.decision.Action == downshift.ActionWarn {
		candidate := *plan
		if err := p.applyDownshiftTarget(&candidate, true); err != nil {
			plan.decision.Action = downshift.ActionNone
			plan.decision.Reason = err.Error()
		}
		return fmt.Errorf("%s", plan.decision.Reason)
	}
	if plan.decision.Action != downshift.ActionDownshift {
		return fmt.Errorf("%s", plan.decision.Reason)
	}
	if err := p.applyDownshiftTarget(plan, true); err != nil {
		plan.decision.Action = downshift.ActionNone
		plan.decision.Reason = err.Error()
		return err
	}
	return nil
}

func (p *Proxy) applyDownshiftTarget(plan *downshiftPlan, sourceWasDenied bool) error {
	rule := plan.compiled.Rule(plan.requestedProvider, plan.requestedModel)
	if rule == nil {
		return fmt.Errorf("source route/model has no allowlisted equivalent target")
	}
	rewrittenPath, rewrittenBody, err := downshift.RewriteRequest(plan.requestedPath, plan.requestedBody,
		rule.Source.Dialect, rule.Target.Model)
	if err != nil {
		return fmt.Errorf("compatible target was not selected: %w", err)
	}
	if eligible, reason := downshift.Eligible(rule, downshift.AnalyzeAt(rewrittenBody, rule.Target.Dialect, rewrittenPath)); !eligible {
		return fmt.Errorf("compatible target was not selected after model rewrite: %s", reason)
	}
	targetInfo := p.estimateRequestAt(rewrittenPath, rewrittenBody, rule.Target.Route, plan.started)
	if targetInfo.parseErr != nil {
		return fmt.Errorf("compatible target was not selected: rewritten admission metadata is invalid")
	}
	if err := validatePolicyAdmissionMetadata(rule.Target.Route, rewrittenPath, targetInfo); err != nil {
		return fmt.Errorf("compatible target was not selected: %w", err)
	}
	if targetInfo.model != rule.Target.Model {
		return fmt.Errorf("compatible target was not selected: rewritten model did not match the allowlist")
	}
	if !targetInfo.admission.Priced || !targetInfo.admission.Bounded {
		return fmt.Errorf("compatible target was not selected: actual target price or bounded cost is unavailable")
	}
	if !sourceWasDenied && plan.requestInfo.admission.Priced &&
		targetInfo.admission.USD >= plan.requestInfo.admission.USD-1e-12 {
		return fmt.Errorf("compatible target was not selected: actual target cost bound is not lower than the source")
	}
	plan.provider = rule.Target.Route
	plan.route = rewrittenPath
	plan.path = rewrittenPath
	plan.body = rewrittenBody
	plan.shape = rule.Target.Dialect
	plan.requestInfo = targetInfo
	return nil
}

func routingBudgetPct(s budgetStatusStore, now time.Time) (float64, error) {
	states, err := budget.Status(s, now)
	if err != nil {
		return 0, err
	}
	maximum := -1.0
	for _, state := range states {
		if !state.Set {
			continue
		}
		pct := state.Pct()
		if math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 {
			return 0, fmt.Errorf("budget utilization is invalid")
		}
		maximum = max(maximum, pct)
	}
	return maximum, nil
}

type budgetStatusStore interface {
	GetSettings(keys ...string) (map[string]string, error)
	SpentSinceMulti([]time.Time) ([]float64, error)
}

func eligibleDownshiftDenial(denial *budget.Denial) bool {
	if denial == nil {
		return false
	}
	return denial.Type == "burnban_request_exceeds_remaining" || denial.Type == "burnban_unpriced_request"
}

func cancelUnforwardedDownshift(plan *downshiftPlan, denial *budget.Denial) {
	if plan == nil || plan.decision.Action != downshift.ActionDownshift || denial == nil {
		return
	}
	plan.decision.Action = downshift.ActionNone
	plan.decision.Reason = "compatible target was not forwarded because final budget admission denied the request (" + denial.Type + ")"
	plan.provider, plan.route, plan.path, plan.body, plan.shape, plan.requestInfo =
		plan.requestedProvider, plan.requestedRoute, plan.requestedPath, plan.requestedBody,
		plan.requestedShape, plan.requestedInfo
}

func cancelSelectedDownshift(plan *downshiftPlan, reason string) {
	if plan == nil || plan.decision.Action == downshift.ActionNone {
		return
	}
	plan.decision.Action = downshift.ActionNone
	plan.decision.Reason = reason
	plan.provider, plan.route, plan.path, plan.body, plan.shape, plan.requestInfo =
		plan.requestedProvider, plan.requestedRoute, plan.requestedPath, plan.requestedBody,
		plan.requestedShape, plan.requestedInfo
}

// A cross-route rewrite crosses a provider credential and metadata boundary.
// The target gets only a minimal, explicitly non-secret wire-header set. Any
// other caller header or source query makes the candidate unusable; falling
// back to the original route keeps its credential attached only to its owner.
func validateCrossRouteBoundary(r *http.Request, plan *downshiftPlan) error {
	if r == nil || plan == nil || plan.decision.TargetRoute == "" || plan.decision.TargetRoute == plan.requestedProvider {
		return nil
	}
	if r.URL != nil && r.URL.RawQuery != "" {
		return fmt.Errorf("compatible cross-route target was not selected because source query parameters cannot cross provider boundaries")
	}
	for name := range r.Header {
		if crossRouteHeaderAllowed(name) || crossRouteHeaderDiscardable(name) || locallyConsumedHeader(name) {
			continue
		}
		return fmt.Errorf("compatible cross-route target was not selected because request header %q is not safe to forward across providers", http.CanonicalHeaderKey(name))
	}
	return nil
}

func crossRouteHeaderDiscardable(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Cookie", "Cookie2", "Set-Cookie", "Proxy-Authorization",
		"X-Api-Key", "Api-Key", "X-Goog-Api-Key", "X-Amz-Security-Token",
		"X-Auth-Token", "X-Access-Token", "Cf-Access-Client-Id", "Cf-Access-Client-Secret":
		return true
	default:
		return false
	}
}

func crossRouteHeaderAllowed(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Accept", "Content-Type", "User-Agent", "Anthropic-Version":
		return true
	default:
		return false
	}
}

func locallyConsumedHeader(name string) bool {
	canonical := http.CanonicalHeaderKey(name)
	for _, hop := range hopHeaders {
		if canonical == http.CanonicalHeaderKey(hop) {
			return true
		}
	}
	if strings.HasPrefix(canonical, "X-Burnban-") {
		return true
	}
	switch canonical {
	case "Connection", "Content-Length", "Host":
		return true
	default:
		return false
	}
}

func retainCrossRouteHeaders(header http.Header) {
	for name := range header {
		if !crossRouteHeaderAllowed(name) {
			header.Del(name)
		}
	}
}

func policyContextForRequest(attribution requestIdentityAttribution, agent, session, modelClass, provider, route string,
	requestInfo requestEstimate,
) policy.Context {
	costMicroUSD := int64(0)
	if requestInfo.admission.Priced && requestInfo.admission.Bounded {
		costMicroUSD, _ = policy.MicroUSDFromUSD(requestInfo.admission.USD)
	}
	return policy.Context{
		Organization: attribution.Organization, Tenant: attribution.Tenant, Meter: attribution.Meter,
		Device: attribution.Device, Team: attribution.CostCenter, CostCenter: attribution.CostCenter,
		Principal: attribution.Principal, ServiceAccount: attribution.ServiceAccount,
		User: attribution.policyUser(), Project: attribution.Project, Environment: attribution.Environment,
		Agent: persistedLabel(agent), Session: persistedLabel(session), Provider: policyAdmissionLabel(provider),
		Model: policyAdmissionLabel(requestInfo.model), ModelClass: persistedLabel(modelClass), Route: policyAdmissionLabel(route),
		Tier: policyAdmissionLabel(requestInfo.serviceTier), ServiceTier: policyAdmissionLabel(requestInfo.serviceTier),
		Geo: policyAdmissionLabel(requestInfo.inferenceGeo), InferenceGeo: policyAdmissionLabel(requestInfo.inferenceGeo),
		EstimatedInput: requestInfo.inputUpper, OutputBound: requestInfo.outputBound,
		OutputBoundPresent: requestInfo.outputBoundPresent, EstimatedCostUSD: requestInfo.admission.USD,
		EstimatedCostMicroUSD: costMicroUSD, CostKnown: requestInfo.admission.Priced && requestInfo.admission.Bounded,
		IdentityConfidence:       attribution.Confidence,
		OrganizationConfidence:   attribution.OrganizationConfidence,
		TenantConfidence:         attribution.TenantConfidence,
		MeterConfidence:          attribution.MeterConfidence,
		DeviceConfidence:         attribution.DeviceConfidence,
		TeamConfidence:           attribution.TeamConfidence,
		CostCenterConfidence:     attribution.CostCenterConfidence,
		PrincipalConfidence:      attribution.PrincipalConfidence,
		ServiceAccountConfidence: attribution.ServiceAccountConfidence,
		UserConfidence:           attribution.UserConfidence,
		ProjectConfidence:        attribution.ProjectConfidence,
		EnvironmentConfidence:    attribution.EnvironmentConfidence,
	}
}

// prepareTargetPolicy closes the source's two-phase reservation before doing
// a complete evaluation for the actual target context. Thus source denies
// remain authoritative, while only the forwarded target consumes counters.
func (p *Proxy) prepareTargetPolicy(now time.Time, source *policy.Reservation, plan *downshiftPlan,
	attribution requestIdentityAttribution, agent, session, modelClass string,
) (*policy.Reservation, *policy.Decision, error) {
	if source != nil {
		if err := source.Cancel(); err != nil {
			return nil, nil, fmt.Errorf("cancel source policy reservation: %w", err)
		}
	}
	reservation, decision, err := p.Policy.Prepare(now,
		policyContextForRequest(attribution, agent, session, modelClass, plan.provider, plan.route, plan.requestInfo))
	if err != nil {
		return nil, nil, fmt.Errorf("evaluate downshift target policy: %w", err)
	}
	return reservation, decision, nil
}

func setDownshiftHeaders(header http.Header, plan *downshiftPlan) {
	if plan == nil || plan.compiled == nil || plan.decision.RuleID == "" {
		return
	}
	header.Set("X-Burnban-Downshift-Action", string(plan.decision.Action))
	header.Set("X-Burnban-Downshift-Rule", plan.decision.RuleID)
	header.Set("X-Burnban-Downshift-Trigger", string(plan.decision.Trigger))
	header.Set("X-Burnban-Downshift-Reason", plan.decision.Reason)
	header.Set("X-Burnban-Downshift-Config", plan.compiled.Digest)
	header.Set("X-Burnban-Requested-Route", plan.requestedProvider)
	header.Set("X-Burnban-Requested-Model", plan.requestedModel)
	header.Set("X-Burnban-Chosen-Route", plan.provider)
	header.Set("X-Burnban-Chosen-Model", plan.requestInfo.model)
}

func stripUpstreamDownshiftHeaders(header http.Header) {
	for _, name := range []string{
		"X-Burnban-Downshift-Action", "X-Burnban-Downshift-Rule", "X-Burnban-Downshift-Trigger",
		"X-Burnban-Downshift-Reason", "X-Burnban-Downshift-Config", "X-Burnban-Requested-Route",
		"X-Burnban-Requested-Model", "X-Burnban-Chosen-Route", "X-Burnban-Chosen-Model",
	} {
		header.Del(name)
	}
}
