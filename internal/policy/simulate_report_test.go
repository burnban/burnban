package policy_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/policy"
)

func TestSimulationReportsAffectedBreakdownAndRulePrecedence(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "report", Namespace: "report", Revision: 1},
		Mode:     policy.ModeObserve,
		Rules: []policy.Rule{
			{ID: "enforcing-provider", Mode: policy.ModeEnforce,
				Match: policy.Match{Provider: policy.AccessList{Deny: []string{"openai"}}}},
			{ID: "warning-model", Mode: policy.ModeWarn,
				Match: policy.Match{Model: policy.AccessList{Deny: []string{"gpt-*"}}}},
			{ID: "observed-route", Mode: policy.ModeObserve,
				Match: policy.Match{Route: policy.AccessList{Deny: []string{"/shadow"}}}},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := policy.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	context := func(agent, user, model, project, provider, route string) policy.Context {
		return policy.Context{
			Agent: agent, User: user, Model: model, Project: project,
			Provider: provider, Route: route, OutputBoundPresent: true, CostKnown: true,
		}
	}
	now := time.Now().UTC()
	report := policy.Simulate(compiled, []policy.HistoricalSample{
		{Ts: now, AdmissionKnown: true, Context: context("agent-a", "user-a", "gpt-5", "p1", "openai", "/v1")},
		{Ts: now.Add(time.Second), AdmissionKnown: true, Context: context("agent-a", "user-a", "gpt-5", "p1", "anthropic", "/v1")},
		{Ts: now.Add(2 * time.Second), AdmissionKnown: true, Context: context("agent-b", "user-b", "claude", "p2", "openai", "/v1")},
		{Ts: now.Add(3 * time.Second), AdmissionKnown: true, Context: context("agent-c", "user-c", "claude", "p3", "anthropic", "/shadow")},
	})
	if report.Requests != 4 || report.WouldBlock != 2 || report.WouldDeny != 2 ||
		report.WouldAllow != 2 || report.WouldWarn != 1 || report.WouldObserve != 1 {
		t.Fatalf("unexpected outcomes: %+v", report)
	}
	if report.Affected.Agents.Distinct != 3 || report.Affected.Users.Distinct != 3 ||
		report.Affected.Models.Distinct != 2 || report.Affected.Projects.Distinct != 3 {
		t.Fatalf("unexpected affected breakdown: %+v", report.Affected)
	}
	gpt := simulationImpact(t, report.Affected.Models.Values, "gpt-5")
	if gpt.Calls != 2 || gpt.WouldBlock != 1 || gpt.WouldWarn != 1 {
		t.Fatalf("gpt impact = %+v", gpt)
	}
	if !strings.Contains(report.Precedence, "intersect") || !strings.Contains(report.Precedence, "enforce") {
		t.Fatalf("precedence explanation = %q", report.Precedence)
	}
	if len(report.RuleInteractions) != 1 || report.RuleInteractions[0].Outcome != "block" ||
		report.RuleInteractions[0].Calls != 1 || len(report.RuleInteractions[0].Rules) != 2 ||
		report.RuleInteractions[0].Rules[0].RuleID != "enforcing-provider" ||
		report.RuleInteractions[0].Rules[1].RuleID != "warning-model" {
		t.Fatalf("rule interactions = %+v", report.RuleInteractions)
	}
}

func TestSimulationAffectedBreakdownIsBounded(t *testing.T) {
	raw := []byte(`{"apiVersion":"burnban.dev/v2","kind":"PolicySet","metadata":{"name":"bounded","namespace":"bounded","revision":1},"mode":"enforce","rules":[{"id":"deny","match":{"provider":{"deny":["openai"]}}}]}`)
	compiled, err := policy.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]policy.HistoricalSample, 257)
	for i := range samples {
		samples[i] = policy.HistoricalSample{
			Ts: time.Unix(int64(i), 0), AdmissionKnown: true,
			Context: policy.Context{Provider: "openai", Agent: fmt.Sprintf("agent-%03d", i), OutputBoundPresent: true},
		}
	}
	report := policy.Simulate(compiled, samples)
	if report.WouldBlock != 257 || report.Affected.Agents.Distinct != 256 ||
		!report.Affected.Agents.Limited || len(report.Affected.Agents.Values) != 256 {
		t.Fatalf("bounded report = %+v", report.Affected.Agents)
	}
}

func simulationImpact(t *testing.T, impacts []policy.SimulationImpact, value string) policy.SimulationImpact {
	t.Helper()
	for _, impact := range impacts {
		if impact.Value == value {
			return impact
		}
	}
	t.Fatalf("missing impact for %q: %+v", value, impacts)
	return policy.SimulationImpact{}
}
