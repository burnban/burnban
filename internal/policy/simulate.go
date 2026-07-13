package policy

import (
	"sort"
	"strings"
	"time"
)

// HistoricalSample is the metadata-only input to a policy replay. AdmissionKnown
// means the original pre-upstream estimate/output bound was durably recorded.
// Older request rows can still be replayed, but their result is explicitly partial.
type HistoricalSample struct {
	Ts             time.Time
	End            time.Time
	Context        Context
	AdmissionKnown bool
}

type SimulationReport struct {
	PolicyName              string                      `json:"policy_name"`
	PolicyRevision          int64                       `json:"policy_revision"`
	PolicyDigest            string                      `json:"policy_digest"`
	Requests                int64                       `json:"requests"`
	WouldAllow              int64                       `json:"would_allow"`
	WouldDeny               int64                       `json:"would_deny"`
	WouldBlock              int64                       `json:"would_block"`
	WouldWarn               int64                       `json:"would_warn"`
	WouldObserve            int64                       `json:"would_observe"`
	Indeterminate           int64                       `json:"indeterminate"`
	Confidence              string                      `json:"confidence"`
	Reasons                 map[string]int64            `json:"reasons,omitempty"`
	Affected                SimulationAffectedBreakdown `json:"affected"`
	Precedence              string                      `json:"precedence"`
	RuleInteractions        []SimulationRuleInteraction `json:"rule_interactions,omitempty"`
	RuleInteractionsLimited bool                        `json:"rule_interactions_limited,omitempty"`
	Notes                   []string                    `json:"notes"`
}

// SimulationImpact is a metadata-only breakdown for one affected selector.
// Calls are counted once at the final precedence-resolved outcome, even when
// several rules matched that call.
type SimulationImpact struct {
	Value        string `json:"value"`
	Calls        int64  `json:"calls"`
	WouldBlock   int64  `json:"would_block,omitempty"`
	WouldWarn    int64  `json:"would_warn,omitempty"`
	WouldObserve int64  `json:"would_observe,omitempty"`
}

// SimulationDimensionBreakdown is exact unless Limited is true. The bounded
// list prevents attacker-controlled labels in a large replay from producing
// an unbounded report or auxiliary map.
type SimulationDimensionBreakdown struct {
	Distinct int                `json:"distinct"`
	Values   []SimulationImpact `json:"values,omitempty"`
	Limited  bool               `json:"limited,omitempty"`
}

type SimulationAffectedBreakdown struct {
	Agents   SimulationDimensionBreakdown `json:"agents"`
	Users    SimulationDimensionBreakdown `json:"users"`
	Models   SimulationDimensionBreakdown `json:"models"`
	Projects SimulationDimensionBreakdown `json:"projects"`
}

type SimulationRuleMode struct {
	RuleID string `json:"rule_id"`
	Mode   Mode   `json:"mode"`
}

// SimulationRuleInteraction explains calls on which multiple rules produced
// violations. Rules always intersect; specificity only makes explanations
// stable and never overrides another rule.
type SimulationRuleInteraction struct {
	Rules       []SimulationRuleMode `json:"rules"`
	Calls       int64                `json:"calls"`
	Outcome     string               `json:"outcome"`
	Explanation string               `json:"explanation"`
}

const (
	maxSimulationBreakdownValues = 256
	maxSimulationInteractions    = 128
	simulationPrecedence         = "all applicable rules intersect; enforce violations block, otherwise warn violations record a warning, and observe violations only record shadow impact"
)

type simulationDimensionAccumulator struct {
	values  map[string]*SimulationImpact
	limited bool
}

func (a *simulationDimensionAccumulator) add(value, outcome string) {
	if value == "" {
		return
	}
	if a.values == nil {
		a.values = map[string]*SimulationImpact{}
	}
	impact := a.values[value]
	if impact == nil {
		if len(a.values) >= maxSimulationBreakdownValues {
			a.limited = true
			return
		}
		impact = &SimulationImpact{Value: value}
		a.values[value] = impact
	}
	impact.Calls++
	switch outcome {
	case "block":
		impact.WouldBlock++
	case "warn":
		impact.WouldWarn++
	case "observe":
		impact.WouldObserve++
	}
}

func (a simulationDimensionAccumulator) report() SimulationDimensionBreakdown {
	out := SimulationDimensionBreakdown{Distinct: len(a.values), Limited: a.limited}
	for _, impact := range a.values {
		out.Values = append(out.Values, *impact)
	}
	sort.Slice(out.Values, func(i, j int) bool { return out.Values[i].Value < out.Values[j].Value })
	return out
}

type simulationAffectedAccumulator struct {
	agents   simulationDimensionAccumulator
	users    simulationDimensionAccumulator
	models   simulationDimensionAccumulator
	projects simulationDimensionAccumulator
}

func (a *simulationAffectedAccumulator) add(context Context, outcome string) {
	a.agents.add(context.Agent, outcome)
	a.users.add(context.User, outcome)
	a.models.add(context.Model, outcome)
	a.projects.add(context.Project, outcome)
}

func (a simulationAffectedAccumulator) report() SimulationAffectedBreakdown {
	return SimulationAffectedBreakdown{
		Agents: a.agents.report(), Users: a.users.report(),
		Models: a.models.report(), Projects: a.projects.report(),
	}
}

type simulationInteractionAccumulator struct {
	values  map[string]*SimulationRuleInteraction
	limited bool
}

func (a *simulationInteractionAccumulator) add(ruleModes map[string]Mode, outcome string) {
	if len(ruleModes) < 2 {
		return
	}
	rules := make([]SimulationRuleMode, 0, len(ruleModes))
	for ruleID, mode := range ruleModes {
		rules = append(rules, SimulationRuleMode{RuleID: ruleID, Mode: mode})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })
	var key strings.Builder
	key.WriteString(outcome)
	for _, rule := range rules {
		key.WriteByte(0)
		key.WriteString(rule.RuleID)
		key.WriteByte(0)
		key.WriteString(string(rule.Mode))
	}
	if a.values == nil {
		a.values = map[string]*SimulationRuleInteraction{}
	}
	interaction := a.values[key.String()]
	if interaction == nil {
		if len(a.values) >= maxSimulationInteractions {
			a.limited = true
			return
		}
		interaction = &SimulationRuleInteraction{
			Rules: rules, Outcome: outcome,
			Explanation: simulationInteractionExplanation(outcome),
		}
		a.values[key.String()] = interaction
	}
	interaction.Calls++
}

func (a simulationInteractionAccumulator) report() []SimulationRuleInteraction {
	out := make([]SimulationRuleInteraction, 0, len(a.values))
	for _, interaction := range a.values {
		out = append(out, *interaction)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Outcome != out[j].Outcome {
			return out[i].Outcome < out[j].Outcome
		}
		for k := 0; k < len(out[i].Rules) && k < len(out[j].Rules); k++ {
			if out[i].Rules[k].RuleID != out[j].Rules[k].RuleID {
				return out[i].Rules[k].RuleID < out[j].Rules[k].RuleID
			}
		}
		return len(out[i].Rules) < len(out[j].Rules)
	})
	return out
}

func simulationInteractionExplanation(outcome string) string {
	switch outcome {
	case "block":
		return "at least one enforce rule violated; enforce wins while every applicable rule remains intersected"
	case "warn":
		return "no enforce rule violated; warn wins over observe and the call remains allowed"
	default:
		return "only observe rules violated; the call remains allowed and the impact is shadow-only"
	}
}

type simEvent struct {
	ts           time.Time
	inputTokens  simUint128
	outputTokens simUint128
	totalTokens  simUint128
	costMicroUSD simUint128
}

// simUint128 keeps replay prefixes exact. The CLI bounds a replay to one
// million rows, so even one MaxInt64 value per row fits with ample headroom.
// Exact subtraction is important after old saturated values leave a rolling
// window; int64 saturation would otherwise undercount the remaining usage.
type simUint128 struct {
	hi uint64
	lo uint64
}

func (n simUint128) addInt64(value int64) simUint128 {
	if value <= 0 {
		return n
	}
	old := n.lo
	n.lo += uint64(value)
	if n.lo < old {
		n.hi++
	}
	return n
}

func (n simUint128) sub(other simUint128) simUint128 {
	borrow := uint64(0)
	if n.lo < other.lo {
		borrow = 1
	}
	return simUint128{hi: n.hi - other.hi - borrow, lo: n.lo - other.lo}
}

func (n simUint128) greaterThanInt64(value int64) bool {
	return value < 0 || n.hi != 0 || n.lo > uint64(value)
}

func (n simUint128) clampedInt64() int64 {
	if n.hi != 0 || n.lo > uint64(1<<63-1) {
		return 1<<63 - 1
	}
	return int64(n.lo)
}

// Simulate replays requests in timestamp order. Count/token/dollar windows are exact
// for samples with v2 admission metadata. Concurrency is reconstructed from
// ledger start+latency and tied timestamps retain ledger order; the report does
// not overstate that reconstruction as an audited fact.
func Simulate(compiled *Compiled, samples []HistoricalSample) SimulationReport {
	report := SimulationReport{
		Confidence: "partial", Reasons: map[string]int64{},
		Precedence: simulationPrecedence,
		Notes: []string{
			"simulation does not forward traffic or mutate live policy counters",
			"concurrency is reconstructed from request timestamp and recorded latency",
		},
	}
	if compiled == nil {
		report.Notes = append(report.Notes, "no candidate policy was supplied")
		return report
	}
	report.PolicyName = compiled.Document.Metadata.Name
	report.PolicyRevision = compiled.Document.Metadata.Revision
	report.PolicyDigest = compiled.Digest
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].Ts.Before(samples[j].Ts) })
	history := map[string][]simEvent{}
	windowCursors := map[policyWindowKey]int{}
	inFlight := map[string][]time.Time{}
	var affected simulationAffectedAccumulator
	var interactions simulationInteractionAccumulator
	legacy := int64(0)
	for _, sample := range samples {
		report.Requests++
		if !sample.AdmissionKnown {
			legacy++
		}
		rules := applicableRules(compiled.Document, sample.Context)
		definiteDeny, warned, observed, unknown := false, false, false, false
		reasonsForRequest := map[string]struct{}{}
		violatingRuleModes := map[string]Mode{}
		for _, indexed := range rules {
			rule := indexed.Rule
			mode := ruleMode(compiled.Document.Mode, rule)
			violations := staticViolations(rule, sample.Context)
			if mode == ModeEnforce && !identityScopeAuthenticated(rule.Scope, sample.Context) {
				violations = append(violations, Violation{
					Code: "authenticated_identity_required", Dimension: "identity",
					Message: "server-authorized identity is required for every identity dimension in this scoped enforcement rule",
				})
			}
			if !sample.AdmissionKnown {
				filtered := violations[:0]
				for _, violation := range violations {
					if violation.Code == "output_bound_required" {
						unknown = true
						continue
					}
					filtered = append(filtered, violation)
				}
				violations = filtered
			}
			key := counterKey(compiled.Document.Metadata.Namespace, rule.ID)
			events := history[key]
			for _, limit := range rule.Limits.Requests {
				windowKey := makePolicyWindowKey(rule.ID, "requests", limit.ID)
				duration := compiled.windows[windowKey]
				cutoff := windowStart(sample.Ts, duration, limit.WindowType)
				first := advanceSimulationCursor(events, windowCursors[windowKey], cutoff)
				windowCursors[windowKey] = first
				current := int64(len(events) - first)
				if current+1 > limit.Max {
					violations = append(violations, Violation{Code: "request_limit", Dimension: "requests",
						LimitID: limit.ID, Current: current, Requested: 1, Maximum: limit.Max})
				}
			}
			for _, limit := range rule.Limits.Tokens {
				requested, bounded := sample.Context.EstimatedTokensForKind(limit.Kind)
				if !bounded {
					continue
				}
				windowKey := makePolicyWindowKey(rule.ID, "tokens", limit.ID)
				duration := compiled.windows[windowKey]
				cutoff := windowStart(sample.Ts, duration, limit.WindowType)
				first := advanceSimulationCursor(events, windowCursors[windowKey], cutoff)
				windowCursors[windowKey] = first
				current := simWindowPrefix(events, first, limit.Kind)
				if current.greaterThanInt64(limit.Max - requested) {
					violations = append(violations, Violation{Code: "token_limit", Dimension: tokenLimitKind(limit.Kind) + "_tokens",
						LimitID: limit.ID, Current: current.clampedInt64(), Requested: requested, Maximum: limit.Max})
				}
			}
			for _, limit := range rule.Limits.Dollars {
				requested, bounded := sample.Context.ConservativeCostMicroUSD()
				if !bounded {
					violations = append(violations, Violation{Code: "estimated_cost_unknown", Dimension: "dollars",
						LimitID: limit.ID, Maximum: limit.MaxMicroUSD})
					continue
				}
				windowKey := makePolicyWindowKey(rule.ID, "dollars", limit.ID)
				duration := compiled.windows[windowKey]
				cutoff := windowStart(sample.Ts, duration, limit.WindowType)
				first := advanceSimulationCursor(events, windowCursors[windowKey], cutoff)
				windowCursors[windowKey] = first
				current := simCostWindowPrefix(events, first)
				if current.greaterThanInt64(limit.MaxMicroUSD - requested) {
					violations = append(violations, Violation{Code: "dollar_limit", Dimension: "dollars",
						LimitID: limit.ID, Current: current.clampedInt64(), Requested: requested, Maximum: limit.MaxMicroUSD})
				}
			}
			if rule.Limits.Concurrency > 0 {
				active := inFlight[key][:0]
				for _, end := range inFlight[key] {
					if end.After(sample.Ts) {
						active = append(active, end)
					}
				}
				inFlight[key] = active
				if int64(len(active))+1 > rule.Limits.Concurrency {
					violations = append(violations, Violation{Code: "concurrency_limit", Dimension: "concurrency",
						Current: int64(len(active)), Requested: 1, Maximum: rule.Limits.Concurrency})
				}
			}
			if len(violations) == 0 {
				continue
			}
			violatingRuleModes[rule.ID] = mode
			switch mode {
			case ModeEnforce:
				definiteDeny = true
				for _, violation := range violations {
					reasonsForRequest[violation.Code] = struct{}{}
				}
			case ModeWarn:
				warned = true
			case ModeObserve:
				observed = true
			}
		}
		outcome := ""
		switch {
		case definiteDeny:
			outcome = "block"
		case warned:
			outcome = "warn"
		case observed:
			outcome = "observe"
		}
		if outcome != "" {
			affected.add(sample.Context, outcome)
			interactions.add(violatingRuleModes, outcome)
		}
		if unknown {
			report.Indeterminate++
		}
		if definiteDeny {
			report.WouldDeny++
			report.WouldBlock++
			for reason := range reasonsForRequest {
				report.Reasons[reason]++
			}
			continue
		}
		report.WouldAllow++
		if warned {
			report.WouldWarn++
		} else if observed {
			report.WouldObserve++
		}
		for _, indexed := range rules {
			key := counterKey(compiled.Document.Metadata.Namespace, indexed.Rule.ID)
			events := history[key]
			cost, _ := sample.Context.ConservativeCostMicroUSD()
			input, _ := sample.Context.EstimatedTokensForKind("input")
			output := int64(0)
			if sample.Context.OutputBoundPresent {
				output = max(sample.Context.OutputBound, 0)
			}
			total, _ := sample.Context.EstimatedTokensForKind("total")
			next := simEvent{ts: sample.Ts}
			if len(events) != 0 {
				last := events[len(events)-1]
				next.inputTokens, next.outputTokens = last.inputTokens, last.outputTokens
				next.totalTokens, next.costMicroUSD = last.totalTokens, last.costMicroUSD
			}
			next.inputTokens = next.inputTokens.addInt64(input)
			next.outputTokens = next.outputTokens.addInt64(output)
			next.totalTokens = next.totalTokens.addInt64(total)
			next.costMicroUSD = next.costMicroUSD.addInt64(cost)
			history[key] = append(events, next)
			if indexed.Rule.Limits.Concurrency > 0 && sample.End.After(sample.Ts) {
				inFlight[key] = append(inFlight[key], sample.End)
			}
		}
	}
	report.Affected = affected.report()
	report.RuleInteractions = interactions.report()
	report.RuleInteractionsLimited = interactions.limited
	if legacy != 0 {
		report.Notes = append(report.Notes,
			"legacy rows without v2 admission metadata use available input-token and priced-cost metadata as proxies; output-bound checks are indeterminate")
	}
	if report.Requests == 0 {
		report.Confidence = "missing"
	} else if legacy == 0 {
		report.Confidence = "estimated"
	}
	return report
}

func simEventPrefix(event simEvent, kind string) simUint128 {
	switch tokenLimitKind(kind) {
	case "input":
		return event.inputTokens
	case "output":
		return event.outputTokens
	default:
		return event.totalTokens
	}
}

func simWindowPrefix(events []simEvent, first int, kind string) simUint128 {
	if len(events) == 0 || first >= len(events) {
		return simUint128{}
	}
	current := simEventPrefix(events[len(events)-1], kind)
	if first > 0 {
		current = current.sub(simEventPrefix(events[first-1], kind))
	}
	return current
}

func simCostWindowPrefix(events []simEvent, first int) simUint128 {
	if len(events) == 0 || first >= len(events) {
		return simUint128{}
	}
	current := events[len(events)-1].costMicroUSD
	if first > 0 {
		current = current.sub(events[first-1].costMicroUSD)
	}
	return current
}

func advanceSimulationCursor(events []simEvent, first int, cutoff time.Time) int {
	if first < 0 {
		first = 0
	}
	for first < len(events) && events[first].ts.Before(cutoff) {
		first++
	}
	return first
}

func saturatedAdd(a, b int64) int64 {
	if a < 0 || b < 0 || a > (1<<63-1)-b {
		return 1<<63 - 1
	}
	return a + b
}
