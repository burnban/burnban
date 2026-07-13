package downshift

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

type SimulationReport struct {
	ConfigDigest          string           `json:"config_digest"`
	ConfigRevision        int64            `json:"config_revision"`
	Since                 time.Time        `json:"since"`
	Through               time.Time        `json:"through"`
	TotalRequests         int64            `json:"total_requests"`
	MatchedRequests       int64            `json:"matched_requests"`
	EligibleRequests      int64            `json:"eligible_requests"`
	IneligibleRequests    int64            `json:"ineligible_requests"`
	IndeterminateRequests int64            `json:"indeterminate_requests"`
	SourceCostUSD         float64          `json:"source_cost_usd"`
	TargetCostUSD         float64          `json:"target_cost_usd"`
	SavingsUSD            float64          `json:"savings_usd"`
	SavingsPct            float64          `json:"savings_pct"`
	Reasons               map[string]int64 `json:"reasons"`
	Notes                 []string         `json:"notes"`
}

func NewSimulation(compiled *Compiled, prices *pricing.Table, since, through time.Time) (*Simulator, error) {
	if compiled == nil || prices == nil || since.IsZero() || through.IsZero() || through.Before(since) {
		return nil, fmt.Errorf("simulation requires config, prices, and a valid time window")
	}
	return &Simulator{Compiled: compiled, Prices: prices, Report: SimulationReport{
		ConfigDigest: compiled.Digest, ConfigRevision: compiled.Config.Revision,
		Since: since.UTC(), Through: through.UTC(), Reasons: map[string]int64{},
		Notes: []string{
			"Historical replay uses observed token totals and the target price effective on each request date.",
			"Rows recorded before content-free feature receipts are indeterminate; prompts and tool schemas are never retained.",
		},
	}}, nil
}

type Simulator struct {
	Compiled *Compiled
	Prices   *pricing.Table
	Report   SimulationReport
}

func (s *Simulator) Add(row store.Request) {
	s.Report.TotalRequests++
	route, model := row.RequestedProvider, row.RequestedModel
	if route == "" {
		route = row.Provider
	}
	if model == "" {
		model = row.Model
	}
	rule := s.Compiled.Rule(route, model)
	if rule == nil {
		return
	}
	s.Report.MatchedRequests++
	if row.DownshiftAction == "downshift" {
		s.indeterminate("already-downshifted rows lack counterfactual source usage")
		return
	}
	features, err := parseFeatureReceipt(row.DownshiftFeatures)
	if err != nil {
		s.indeterminate("missing or invalid compatibility receipt")
		return
	}
	identity := Identity{Tenant: row.IdentityTenant, Principal: row.Principal, ServiceAccount: row.ServiceAccount,
		Project: row.Project, CostCenter: row.CostCenter, Confidence: row.IdentityConfidence}
	if row.Policy != nil && row.Policy.ContextJSON != "" && len(row.Policy.ContextJSON) <= 64<<10 {
		var confidence struct {
			Team    string `json:"team_confidence"`
			User    string `json:"user_confidence"`
			Project string `json:"project_confidence"`
		}
		if json.Unmarshal([]byte(row.Policy.ContextJSON), &confidence) == nil {
			identity.TeamConfidence, identity.UserConfidence, identity.ProjectConfidence = confidence.Team, confidence.User, confidence.Project
		}
	}
	if ok, reason := ScopeMatches(rule.Scope, identity); !ok {
		s.ineligible(reason)
		return
	}
	if ok, reason := Eligible(rule, features); !ok {
		s.ineligible(reason)
		return
	}
	if row.PricingState != store.PricingPriced || row.FeeUnpriced || invalidCost(row.CostUSD) {
		s.indeterminate("source historical cost is incomplete or unknown")
		return
	}
	resolved, ok := s.Prices.Resolve(rule.Target.Route, rule.Target.Model, row.InferenceGeo, row.ServiceTier, row.Ts, 0, false)
	if !ok || resolved.HasFinalCost {
		s.indeterminate("target price is unknown for the historical date and dimensions")
		return
	}
	targetCost, ok := historicalTargetCost(resolved, row)
	if !ok {
		s.indeterminate("target price does not cover a historical tier or region fee")
		return
	}
	s.Report.EligibleRequests++
	s.Report.SourceCostUSD += row.CostUSD
	s.Report.TargetCostUSD += targetCost
}

func (s *Simulator) ineligible(reason string) {
	s.Report.IneligibleRequests++
	s.Report.Reasons[reason]++
}

func (s *Simulator) indeterminate(reason string) {
	s.Report.IndeterminateRequests++
	s.Report.Reasons[reason]++
}

func (s *Simulator) Finish() SimulationReport {
	s.Report.SourceCostUSD = roundedMoney(s.Report.SourceCostUSD)
	s.Report.TargetCostUSD = roundedMoney(s.Report.TargetCostUSD)
	s.Report.SavingsUSD = roundedMoney(s.Report.SourceCostUSD - s.Report.TargetCostUSD)
	if s.Report.SourceCostUSD > 0 {
		s.Report.SavingsPct = (s.Report.SourceCostUSD - s.Report.TargetCostUSD) / s.Report.SourceCostUSD * 100
	}
	if math.IsNaN(s.Report.SavingsPct) || math.IsInf(s.Report.SavingsPct, 0) {
		s.Report.SavingsPct = 0
	}
	s.Report.SavingsPct = math.Round(s.Report.SavingsPct*100) / 100
	if s.Report.EligibleRequests == 0 {
		s.Report.Notes = append(s.Report.Notes, "No request had enough evidence to quantify impact; enforcing activation requires an explicit audited force reason.")
	}
	if s.Report.IndeterminateRequests > 0 {
		s.Report.Notes = append(s.Report.Notes, "Indeterminate rows are excluded from savings rather than assumed compatible or free.")
	}
	sort.Strings(s.Report.Notes)
	return s.Report
}

func parseFeatureReceipt(raw string) (Features, error) {
	if raw == "" || len(raw) > 4096 {
		return Features{}, fmt.Errorf("missing feature receipt")
	}
	var out Features
	if json.Unmarshal([]byte(raw), &out) != nil || out.Version != "burnban.features/v1" ||
		(out.Dialect != "openai" && out.Dialect != "anthropic" && out.Dialect != "gemini") ||
		out.ContextUpperTokens < 0 || len(out.Modalities) > 3 {
		return Features{}, fmt.Errorf("invalid feature receipt")
	}
	if (out.Dialect == "openai" && out.Operation != "chat_completions") ||
		(out.Dialect == "anthropic" && out.Operation != "messages") ||
		(out.Dialect == "gemini" && out.Operation != "generate_content") {
		return Features{}, fmt.Errorf("invalid feature receipt")
	}
	seen := map[string]bool{}
	for _, modality := range out.Modalities {
		if !knownModality(modality) || seen[modality] {
			return Features{}, fmt.Errorf("invalid feature receipt")
		}
		seen[modality] = true
	}
	return out, nil
}

func historicalTargetCost(resolved pricing.Resolution, row store.Request) (float64, bool) {
	if row.InTokens < 0 || row.OutTokens < 0 || row.CacheReadTokens < 0 || row.CacheWriteTokens < 0 || row.CacheWrite1hTokens < 0 ||
		row.InTokens > math.MaxInt64-row.CacheReadTokens || row.InTokens+row.CacheReadTokens > math.MaxInt64-row.CacheWriteTokens {
		return 0, false
	}
	if !resolved.CoversTier {
		switch strings.ToLower(strings.TrimSpace(row.ServiceTier)) {
		case "", "default", "standard", "standard_only":
		default:
			return 0, false
		}
	}
	cost := pricing.Cost(resolved.Price, row.InTokens, row.OutTokens, row.CacheReadTokens, row.CacheWriteTokens)
	oneHour := min(max(row.CacheWrite1hTokens, 0), max(row.CacheWriteTokens, 0))
	if oneHour > 0 {
		inputMultiplier := 1.0
		totalInput := row.InTokens + row.CacheReadTokens + row.CacheWriteTokens
		if resolved.Price.LongContextThreshold > 0 && totalInput > resolved.Price.LongContextThreshold && resolved.Price.LongInputMult > 0 {
			inputMultiplier = resolved.Price.LongInputMult
		}
		cost += float64(oneHour) * resolved.Price.InputPerMTok * (2 - resolved.Price.CacheWriteMult) * inputMultiplier / 1e6
	}
	if !resolved.CoversRegion {
		switch strings.ToLower(strings.TrimSpace(row.InferenceGeo)) {
		case "", "global":
		case "us":
			cost *= 1.1
		default:
			return 0, false
		}
	}
	if invalidCost(cost) {
		return 0, false
	}
	return cost, true
}

func invalidCost(value float64) bool {
	return value < 0 || value > 1e12 || math.IsNaN(value) || math.IsInf(value, 0)
}

func roundedMoney(value float64) float64 { return math.Round(value*1e9) / 1e9 }
