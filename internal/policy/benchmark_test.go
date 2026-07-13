package policy_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/policy"
)

func BenchmarkPolicyAdmissionUnmatched(b *testing.B) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "bench-unmatched", Namespace: "bench-unmatched", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "other-agent", Mode: policy.ModeObserve, Scope: policy.Scope{Agent: []string{"other"}},
			Match: policy.Match{Provider: policy.AccessList{Deny: []string{"openai"}}}}},
	}
	_, engine, _ := newEngine(b, doc)
	context := policy.Context{Provider: "openai", Agent: "codex"}
	now := time.Now().UTC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reservation, decision, err := engine.Admit(now.Add(time.Duration(i)), context)
		if err != nil || decision.Denied() {
			b.Fatalf("decision=%+v err=%v", decision, err)
		}
		reservation.Release()
	}
}

func BenchmarkPolicyAdmission256ApplicableRules(b *testing.B) {
	rules := make([]policy.Rule, 256)
	for i := range rules {
		rules[i] = policy.Rule{
			ID: fmt.Sprintf("rule-%03d", i), Mode: policy.ModeObserve,
			Match: policy.Match{Provider: policy.AccessList{Allow: []string{"openai"}}},
		}
	}
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "bench-256", Namespace: "bench-256", Revision: 1}, Mode: policy.ModeObserve,
		Rules: rules,
	}
	_, engine, _ := newEngine(b, doc)
	context := policy.Context{Provider: "openai"}
	now := time.Now().UTC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reservation, decision, err := engine.Admit(now.Add(time.Duration(i)), context)
		if err != nil || decision.Denied() {
			b.Fatalf("decision=%+v err=%v", decision, err)
		}
		reservation.Release()
	}
}
