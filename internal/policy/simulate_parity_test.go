package policy_test

import (
	"math"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/policy"
)

func TestSimulationWindowCountersMatchLiveAdmission(t *testing.T) {
	fixedBoundary := time.Date(2026, 7, 13, 10, 59, 59, 0, time.UTC)
	rollingBoundary := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	cost := func(microUSD int64) policy.Context {
		return policy.Context{Provider: "openai", CostKnown: true, EstimatedCostMicroUSD: microUSD}
	}
	dollarSaturationSamples := make([]policy.HistoricalSample, 0, 12)
	for i := 0; i < 11; i++ {
		dollarSaturationSamples = append(dollarSaturationSamples, policy.HistoricalSample{
			Ts: rollingBoundary.Add(time.Duration(i) * 2 * time.Minute), AdmissionKnown: true,
			Context: cost(1_000_000_000_000_000_000),
		})
	}
	dollarSaturationSamples = append(dollarSaturationSamples, policy.HistoricalSample{
		Ts: rollingBoundary.Add(20*time.Minute + time.Second), AdmissionKnown: true,
		Context: cost(1_000_000_000_000_000_000),
	})
	tests := []struct {
		name        string
		ruleMode    policy.Mode
		limits      policy.Limits
		samples     []policy.HistoricalSample
		wantAllow   int64
		wantDeny    int64
		wantWarn    int64
		wantReasons map[string]int64
	}{
		{
			name: "input_without_output_bound",
			limits: policy.Limits{Tokens: []policy.WindowLimit{
				{ID: "input", Kind: "input", Max: 5, Window: "1h", WindowType: "rolling"},
			}},
			samples: []policy.HistoricalSample{{Ts: rollingBoundary, AdmissionKnown: true,
				Context: policy.Context{Provider: "openai", EstimatedInput: 6}}},
			wantDeny: 1, wantReasons: map[string]int64{"token_limit": 1},
		},
		{
			name: "input_ignores_large_output",
			limits: policy.Limits{Tokens: []policy.WindowLimit{
				{ID: "input", Kind: "input", Max: 5, Window: "1h", WindowType: "rolling"},
			}},
			samples: []policy.HistoricalSample{{Ts: rollingBoundary, AdmissionKnown: true,
				Context: policy.Context{Provider: "openai", EstimatedInput: 5, OutputBound: math.MaxInt64,
					OutputBoundPresent: true}}},
			wantAllow: 1, wantReasons: map[string]int64{},
		},
		{
			name: "output_ignores_large_input",
			limits: policy.Limits{Tokens: []policy.WindowLimit{
				{ID: "output", Kind: "output", Max: 5, Window: "1h", WindowType: "rolling"},
			}},
			samples: []policy.HistoricalSample{{Ts: rollingBoundary, AdmissionKnown: true,
				Context: policy.Context{Provider: "openai", EstimatedInput: math.MaxInt64, OutputBound: 5,
					OutputBoundPresent: true}}},
			wantAllow: 1, wantReasons: map[string]int64{},
		},
		{
			name: "total_exact_max_then_one_more",
			limits: policy.Limits{Tokens: []policy.WindowLimit{
				{ID: "total", Kind: "total", Max: math.MaxInt64, Window: "1h", WindowType: "rolling"},
			}},
			samples: []policy.HistoricalSample{
				{Ts: rollingBoundary, AdmissionKnown: true, Context: policy.Context{Provider: "openai",
					EstimatedInput: math.MaxInt64 - 1, OutputBound: 1, OutputBoundPresent: true}},
				{Ts: rollingBoundary.Add(time.Second), AdmissionKnown: true, Context: policy.Context{Provider: "openai",
					EstimatedInput: 1, OutputBoundPresent: true}},
			},
			wantAllow: 1, wantDeny: 1, wantReasons: map[string]int64{"token_limit": 1},
		},
		{
			name:     "unbounded_total_does_not_charge_future_window",
			ruleMode: policy.ModeWarn,
			limits: policy.Limits{Tokens: []policy.WindowLimit{
				{ID: "total", Kind: "total", Max: 5, Window: "1h", WindowType: "rolling"},
			}},
			samples: []policy.HistoricalSample{
				{Ts: rollingBoundary, AdmissionKnown: true,
					Context: policy.Context{Provider: "openai", EstimatedInput: 5}},
				{Ts: rollingBoundary.Add(time.Second), AdmissionKnown: true, Context: policy.Context{Provider: "openai",
					EstimatedInput: 5, OutputBoundPresent: true}},
			},
			wantAllow: 2, wantWarn: 1, wantReasons: map[string]int64{},
		},
		{
			name: "token_saturation_then_expiry",
			limits: policy.Limits{Tokens: []policy.WindowLimit{
				{ID: "input", Kind: "input", Max: math.MaxInt64, Window: "1m", WindowType: "rolling"},
			}},
			samples: []policy.HistoricalSample{
				{Ts: rollingBoundary, AdmissionKnown: true,
					Context: policy.Context{Provider: "openai", EstimatedInput: math.MaxInt64}},
				{Ts: rollingBoundary.Add(2 * time.Minute), AdmissionKnown: true,
					Context: policy.Context{Provider: "openai", EstimatedInput: 1}},
				{Ts: rollingBoundary.Add(2*time.Minute + time.Second), AdmissionKnown: true,
					Context: policy.Context{Provider: "openai", EstimatedInput: math.MaxInt64}},
			},
			wantAllow: 2, wantDeny: 1, wantReasons: map[string]int64{"token_limit": 1},
		},
		{
			name: "fixed_dollars_reset_at_boundary",
			limits: policy.Limits{Dollars: []policy.DollarLimit{
				{ID: "spend", MaxMicroUSD: 100, Window: "1h", WindowType: "fixed"},
			}},
			samples: []policy.HistoricalSample{
				{Ts: fixedBoundary, AdmissionKnown: true, Context: cost(100)},
				{Ts: fixedBoundary.Add(time.Second), AdmissionKnown: true, Context: cost(100)},
			},
			wantAllow: 2, wantReasons: map[string]int64{},
		},
		{
			name: "rolling_dollars_cross_fixed_boundary",
			limits: policy.Limits{Dollars: []policy.DollarLimit{
				{ID: "spend", MaxMicroUSD: 100, Window: "1h", WindowType: "rolling"},
			}},
			samples: []policy.HistoricalSample{
				{Ts: fixedBoundary, AdmissionKnown: true, Context: cost(100)},
				{Ts: fixedBoundary.Add(time.Second), AdmissionKnown: true, Context: cost(100)},
			},
			wantAllow: 1, wantDeny: 1, wantReasons: map[string]int64{"dollar_limit": 1},
		},
		{
			name: "rolling_dollars_include_exact_cutoff",
			limits: policy.Limits{Dollars: []policy.DollarLimit{
				{ID: "spend", MaxMicroUSD: 100, Window: "1h", WindowType: "rolling"},
			}},
			samples: []policy.HistoricalSample{
				{Ts: rollingBoundary, AdmissionKnown: true, Context: cost(100)},
				{Ts: rollingBoundary.Add(time.Hour), AdmissionKnown: true, Context: cost(100)},
				{Ts: rollingBoundary.Add(time.Hour + time.Nanosecond), AdmissionKnown: true, Context: cost(100)},
			},
			wantAllow: 2, wantDeny: 1, wantReasons: map[string]int64{"dollar_limit": 1},
		},
		{
			name: "unknown_cost_fails_closed",
			limits: policy.Limits{Dollars: []policy.DollarLimit{
				{ID: "spend", MaxMicroUSD: 100, Window: "1h", WindowType: "rolling"},
			}},
			samples: []policy.HistoricalSample{{Ts: rollingBoundary, AdmissionKnown: true,
				Context: policy.Context{Provider: "custom"}}},
			wantDeny: 1, wantReasons: map[string]int64{"estimated_cost_unknown": 1},
		},
		{
			name: "dollar_saturation_then_expiry",
			limits: policy.Limits{Dollars: []policy.DollarLimit{
				{ID: "spend", MaxMicroUSD: 1_000_000_000_000_000_000, Window: "1m", WindowType: "rolling"},
			}},
			samples:   dollarSaturationSamples,
			wantAllow: 11, wantDeny: 1, wantReasons: map[string]int64{"dollar_limit": 1},
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := policy.Document{
				APIVersion: policy.APIVersion,
				Kind:       policy.Kind,
				Metadata: policy.Metadata{
					Name: "simulation-parity", Namespace: "simulation-parity-" + string(rune('a'+i)), Revision: 1,
				},
				Mode:  policy.ModeEnforce,
				Rules: []policy.Rule{{ID: "bounded", Mode: test.ruleMode, Limits: test.limits}},
			}
			_, engine, compiled := newEngine(t, doc)
			liveSamples := append([]policy.HistoricalSample(nil), test.samples...)
			sort.SliceStable(liveSamples, func(i, j int) bool { return liveSamples[i].Ts.Before(liveSamples[j].Ts) })
			liveAllow, liveDeny, liveWarn := int64(0), int64(0), int64(0)
			liveReasons := map[string]int64{}
			for _, sample := range liveSamples {
				reservation, decision, err := engine.Admit(sample.Ts, sample.Context)
				if err != nil {
					t.Fatalf("live admission: %v", err)
				}
				if decision == nil {
					t.Fatal("live engine returned no decision")
				}
				if decision.Denied() {
					liveDeny++
					requestReasons := map[string]struct{}{}
					for _, rule := range decision.Rules {
						if rule.Mode != policy.ModeEnforce {
							continue
						}
						for _, violation := range rule.Violations {
							requestReasons[violation.Code] = struct{}{}
						}
					}
					for reason := range requestReasons {
						liveReasons[reason]++
					}
					continue
				}
				liveAllow++
				for _, rule := range decision.Rules {
					if rule.Mode == policy.ModeWarn && len(rule.Violations) != 0 {
						liveWarn++
						break
					}
				}
				reservation.Release()
			}
			if liveAllow != test.wantAllow || liveDeny != test.wantDeny || liveWarn != test.wantWarn ||
				!reflect.DeepEqual(liveReasons, test.wantReasons) {
				t.Fatalf("live allow=%d deny=%d warn=%d reasons=%v, want allow=%d deny=%d warn=%d reasons=%v",
					liveAllow, liveDeny, liveWarn, liveReasons, test.wantAllow, test.wantDeny, test.wantWarn, test.wantReasons)
			}

			report := policy.Simulate(compiled, append([]policy.HistoricalSample(nil), test.samples...))
			if report.WouldAllow != liveAllow || report.WouldDeny != liveDeny || report.WouldWarn != liveWarn ||
				!reflect.DeepEqual(report.Reasons, liveReasons) {
				t.Fatalf("simulation=%+v, live allow=%d deny=%d warn=%d reasons=%v",
					report, liveAllow, liveDeny, liveWarn, liveReasons)
			}
		})
	}
}

func TestPolicyWindowKeysDoNotCollideAcrossRuleAndLimitLabels(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion,
		Kind:       policy.Kind,
		Metadata:   policy.Metadata{Name: "collision", Namespace: "collision", Revision: 1},
		Mode:       policy.ModeEnforce,
		Rules: []policy.Rule{
			{ID: "a", Limits: policy.Limits{Tokens: []policy.WindowLimit{
				{ID: "b:tokens:c", Kind: "input", Max: 5, Window: "1h", WindowType: "rolling"},
			}}},
			{ID: "a:tokens:b", Limits: policy.Limits{Tokens: []policy.WindowLimit{
				{ID: "c", Kind: "input", Max: 100, Window: "1s", WindowType: "rolling"},
			}}},
		},
	}
	_, engine, compiled := newEngine(t, doc)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	firstContext := policy.Context{Provider: "openai", EstimatedInput: 5}
	first, decision, err := engine.Admit(now, firstContext)
	if err != nil || decision.Denied() {
		t.Fatalf("first decision=%+v err=%v", decision, err)
	}
	first.Release()
	secondContext := policy.Context{Provider: "openai", EstimatedInput: 1}
	_, decision, err = engine.Admit(now.Add(2*time.Second), secondContext)
	if err != nil || !decision.Denied() || !contains(violationCodes(decision), "token_limit") {
		t.Fatalf("colliding window key weakened the one-hour rule: decision=%+v err=%v", decision, err)
	}
	report := policy.Simulate(compiled, []policy.HistoricalSample{
		{Ts: now, Context: firstContext, AdmissionKnown: true},
		{Ts: now.Add(2 * time.Second), Context: secondContext, AdmissionKnown: true},
	})
	if report.WouldAllow != 1 || report.WouldDeny != 1 || report.Reasons["token_limit"] != 1 {
		t.Fatalf("simulation window labels collided: %+v", report)
	}
}
