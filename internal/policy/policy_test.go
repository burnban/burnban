package policy_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/policy"
	"github.com/burnban/burnban/internal/store"
)

func TestParseStrictVersionedSchema(t *testing.T) {
	valid := `{
		"apiVersion":"burnban.dev/v2","kind":"PolicySet",
		"metadata":{"name":"local","namespace":"local","revision":1},"mode":"enforce",
		"rules":[{"id":"guard","mode":"warn","scope":{"agent":["codex*"]},
		"match":{"provider":{"allow":["openai"]}},
		"limits":{"requests":[{"id":"rpm","max":3,"window":"1m","window_type":"rolling"}]}}]}`
	compiled, err := policy.Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Digest == "" || compiled.Document.Metadata.Revision != 1 || compiled.Document.Mode != policy.ModeEnforce {
		t.Fatalf("compiled = %+v", compiled)
	}
	for name, mutate := range map[string]func(string) string{
		"duplicate": func(s string) string {
			return strings.Replace(s, `"kind":"PolicySet"`, `"kind":"PolicySet","kind":"PolicySet"`, 1)
		},
		"unknown":    func(s string) string { return strings.Replace(s, `"revision":1`, `"revision":1,"owner":"x"`, 1) },
		"version":    func(s string) string { return strings.Replace(s, policy.APIVersion, "burnban.dev/v1", 1) },
		"duration":   func(s string) string { return strings.Replace(s, `"window":"1m"`, `"window":"forever"`, 1) },
		"trailing":   func(s string) string { return s + `{}` },
		"case alias": func(s string) string { return strings.Replace(s, `"name":"local"`, `"Name":"local"`, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := policy.Parse([]byte(mutate(valid))); err == nil {
				t.Fatalf("mutated document was accepted")
			}
		})
	}
}

func TestParseBoundsWindowLimitWork(t *testing.T) {
	limits := func(n int) []policy.WindowLimit {
		out := make([]policy.WindowLimit, n)
		for i := range out {
			out[i] = policy.WindowLimit{ID: fmt.Sprintf("limit-%d", i), Max: 1, Window: "1m", WindowType: "rolling"}
		}
		return out
	}
	base := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "bounded", Namespace: "bounded", Revision: 1}, Mode: policy.ModeEnforce,
	}
	tooManyInRule := base
	tooManyInRule.Rules = []policy.Rule{{ID: "one", Limits: policy.Limits{Requests: limits(65)}}}
	if _, err := policy.Parse(mustJSON(t, tooManyInRule)); err == nil || !strings.Contains(err.Error(), "maximum of 64") {
		t.Fatalf("per-rule window bound error=%v", err)
	}

	tooManyInDocument := base
	for i := 0; i < 5; i++ {
		tooManyInDocument.Rules = append(tooManyInDocument.Rules, policy.Rule{
			ID: fmt.Sprintf("rule-%d", i), Limits: policy.Limits{Requests: limits(64)},
		})
	}
	if _, err := policy.Parse(mustJSON(t, tooManyInDocument)); err == nil || !strings.Contains(err.Error(), "256 total") {
		t.Fatalf("document window bound error=%v", err)
	}
}

func TestCancelledTwoPhaseReservationDoesNotConsumeWindows(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "two-phase", Namespace: "two-phase", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "one", Limits: policy.Limits{Requests: []policy.WindowLimit{
			{ID: "one-hour", Max: 1, Window: "1h", WindowType: "rolling"},
		}}}},
	}
	s, engine, _ := newEngine(t, doc)
	now := time.Now().UTC()
	prepared, decision, err := engine.Prepare(now, policy.Context{Provider: "openai"})
	if err != nil || decision.Denied() {
		t.Fatalf("prepare=%+v err=%v", decision, err)
	}
	if err := prepared.Cancel(); err != nil {
		t.Fatal(err)
	}
	usage, err := s.PolicyRuleUsageSince("two-phase", "one", now.Add(-time.Hour))
	if err != nil || usage.Requests != 0 {
		t.Fatalf("cancelled usage=%+v err=%v", usage, err)
	}
	committed, allowed, err := engine.Admit(now.Add(time.Second), policy.Context{Provider: "openai"})
	if err != nil || allowed.Denied() {
		t.Fatalf("allowed=%+v err=%v", allowed, err)
	}
	committed.Release()
	_, denied, err := engine.Admit(now.Add(2*time.Second), policy.Context{Provider: "openai"})
	if err != nil || !denied.Denied() || !contains(violationCodes(denied), "request_limit") {
		t.Fatalf("denied=%+v err=%v", denied, err)
	}
}

func TestPolicyNamespaceChangeRequiresExplicitLineageReset(t *testing.T) {
	base := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "first", Namespace: "namespace-one", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "global", Limits: policy.Limits{Requests: []policy.WindowLimit{
			{ID: "one", Max: 1, Window: "1h", WindowType: "rolling"},
		}}}},
	}
	s, engine, _ := newEngine(t, base)
	now := time.Now().UTC()
	reservation, first, err := engine.Admit(now, policy.Context{Provider: "openai"})
	if err != nil || first.Denied() {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	reservation.Release()
	second := base
	second.Metadata = policy.Metadata{Name: "second", Namespace: "namespace-two", Revision: 2}
	compiled := compile(t, second)
	if err := s.ApplyPolicyDocument(store.PolicyDocumentRecord{
		APIVersion: compiled.Document.APIVersion, Name: compiled.Document.Metadata.Name,
		Namespace: compiled.Document.Metadata.Namespace, Revision: compiled.Document.Metadata.Revision,
		Digest: compiled.Digest, DocumentJSON: string(compiled.Canonical),
	}); err == nil {
		t.Fatal("namespace change silently reset durable counters")
	}
}

func TestNamespaceResetCannotInheritCollidingReservationKey(t *testing.T) {
	oldDocument := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "old", Namespace: "tenant", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "group/rule", Limits: policy.Limits{
			Requests:    []policy.WindowLimit{{ID: "one", Max: 1, Window: "1h", WindowType: "rolling"}},
			Concurrency: 1,
		}}},
	}
	s, engine, oldCompiled := newEngine(t, oldDocument)
	now := time.Now().UTC()
	oldReservation, oldDecision, err := engine.Prepare(now, policy.Context{Provider: "openai"})
	if err != nil || oldDecision.Denied() {
		t.Fatalf("old reservation decision=%+v err=%v", oldDecision, err)
	}
	defer oldReservation.Release()

	if err := s.ResetPolicyLineage(store.PolicyLineageReset{
		Actor: "policy-test", Reason: "exercise an explicitly authorized namespace replacement",
		ExpectedDigest: oldCompiled.Digest, ExpectedSource: "local", At: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	newDocument := oldDocument
	newDocument.Metadata = policy.Metadata{Name: "new", Namespace: "tenant/group", Revision: 1}
	newDocument.Rules[0].ID = "rule"
	newCompiled := compile(t, newDocument)
	if err := s.ApplyPolicyDocument(store.PolicyDocumentRecord{
		AppliedAt: now.Add(2 * time.Second), APIVersion: newCompiled.Document.APIVersion,
		Name: newCompiled.Document.Metadata.Name, Namespace: newCompiled.Document.Metadata.Namespace,
		Revision: newCompiled.Document.Metadata.Revision, Digest: newCompiled.Digest,
		DocumentJSON: string(newCompiled.Canonical),
	}); err != nil {
		t.Fatal(err)
	}

	// "tenant" + "/" + "group/rule" and
	// "tenant/group" + "/" + "rule" used to alias. The old pending and
	// concurrency reservations must remain isolated from the new lineage.
	newReservation, decision, err := engine.Prepare(now.Add(3*time.Second), policy.Context{Provider: "openai"})
	if err != nil || decision.Denied() {
		t.Fatalf("new lineage inherited colliding reservation: decision=%+v err=%v", decision, err)
	}
	newReservation.Release()
}

func TestUntrustedIdentityScopesCannotEnforce(t *testing.T) {
	for _, dimension := range []string{"team", "user", "project", "agent", "session", "model_class"} {
		t.Run(dimension, func(t *testing.T) {
			scope := policy.Scope{}
			switch dimension {
			case "team":
				scope.Team = []string{"platform"}
			case "user":
				scope.User = []string{"alice"}
			case "project":
				scope.Project = []string{"secret"}
			case "agent":
				scope.Agent = []string{"codex"}
			case "session":
				scope.Session = []string{"run-1"}
			case "model_class":
				scope.ModelClass = []string{"frontier"}
			}
			doc := policy.Document{
				APIVersion: policy.APIVersion, Kind: policy.Kind,
				Metadata: policy.Metadata{Name: "identity", Namespace: "identity", Revision: 1}, Mode: policy.ModeEnforce,
				Rules: []policy.Rule{{ID: "identity-rule", Scope: scope,
					Match: policy.Match{Provider: policy.AccessList{Deny: []string{"openai"}}}}},
			}
			raw, _ := json.Marshal(doc)
			if dimension == "agent" || dimension == "session" || dimension == "model_class" {
				if _, err := policy.Parse(raw); err == nil || !strings.Contains(err.Error(), "not authenticated") {
					t.Fatalf("enforcing %s scope error=%v", dimension, err)
				}
				doc.Rules[0].Mode = policy.ModeWarn
				if _, err := policy.Parse(mustJSON(t, doc)); err != nil {
					t.Fatalf("warn-only %s scope: %v", dimension, err)
				}
				return
			}
			_, engine, _ := newEngine(t, doc)
			_, denied, err := engine.Admit(time.Now().UTC(), policy.Context{Provider: "openai", Team: "platform", User: "alice", Project: "secret", IdentityConfidence: "self_reported"})
			if err != nil || !denied.Denied() || !contains(violationCodes(denied), "authenticated_identity_required") {
				t.Fatalf("untrusted %s decision=%+v err=%v", dimension, denied, err)
			}
			reservation, authenticated, err := engine.Admit(time.Now().UTC().Add(time.Second), policy.Context{
				Provider: "anthropic", Team: "platform", User: "alice", Project: "secret", IdentityConfidence: "authenticated",
				TeamConfidence: "authenticated", UserConfidence: "authenticated", ProjectConfidence: "authenticated",
			})
			if err != nil || authenticated.Denied() {
				t.Fatalf("authenticated %s decision=%+v err=%v", dimension, authenticated, err)
			}
			reservation.Release()
		})
	}
}

func TestCooperativeModelClassMatchCannotEnforce(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "model-class", Namespace: "model-class", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "class", Match: policy.Match{
			ModelClass: &policy.AccessList{Deny: []string{"unknown"}},
		}}},
	}
	if _, err := policy.Parse(mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("enforcing model-class match error=%v", err)
	}
	doc.Rules[0].Mode = policy.ModeWarn
	if _, err := policy.Parse(mustJSON(t, doc)); err != nil {
		t.Fatalf("warn model-class match: %v", err)
	}
}

func TestSeparatePrincipalAndServiceAccountTrust(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "identity-types", Namespace: "identity-types", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "ci", Scope: policy.Scope{ServiceAccount: []string{"ci-bot"}},
			Match: policy.Match{Provider: policy.AccessList{Deny: []string{"openai"}}}}},
	}
	_, engine, _ := newEngine(t, doc)
	_, denied, err := engine.Admit(time.Now().UTC(), policy.Context{
		Provider: "anthropic", ServiceAccount: "ci-bot", ServiceAccountConfidence: "self_reported",
	})
	if err != nil || !denied.Denied() || !contains(violationCodes(denied), "authenticated_identity_required") {
		t.Fatalf("untrusted service account=%+v err=%v", denied, err)
	}
	reservation, principal, err := engine.Admit(time.Now().UTC().Add(time.Second), policy.Context{
		Provider: "openai", Principal: "alice@example.com", PrincipalConfidence: "authenticated",
		ServiceAccountConfidence: "authenticated",
	})
	if err != nil || principal.Denied() || len(principal.Rules) != 0 {
		t.Fatalf("authenticated principal was treated as service account: %+v err=%v", principal, err)
	}
	reservation.Release()
}

func TestProjectScopeRequiresExactFieldTrustWithoutDiscardingTrustedTeam(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "provenance", Namespace: "provenance", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "team-project", Scope: policy.Scope{Team: []string{"platform"}, Project: []string{"secret"}},
			Match: policy.Match{Provider: policy.AccessList{Deny: []string{"openai"}}}}},
	}
	_, engine, compiled := newEngine(t, doc)
	now := time.Now().UTC()
	_, denied, err := engine.Admit(now, policy.Context{
		Provider: "anthropic", Team: "platform", Project: "other", IdentityConfidence: "authenticated",
		TeamConfidence: "authenticated", UserConfidence: "authenticated", ProjectConfidence: "self_reported",
	})
	if err != nil || !denied.Denied() || !contains(violationCodes(denied), "authenticated_identity_required") {
		t.Fatalf("wildcard project bypassed enforcement: decision=%+v err=%v", denied, err)
	}

	// The untrusted project dimension is ignored fail-closed, but an exact
	// trusted team mismatch still keeps this rule out of scope.
	reservation, allowed, err := engine.Admit(now.Add(time.Second), policy.Context{
		Provider: "openai", Team: "another-team", Project: "secret", IdentityConfidence: "authenticated",
		TeamConfidence: "authenticated", UserConfidence: "authenticated", ProjectConfidence: "self_reported",
	})
	if err != nil || allowed.Denied() {
		t.Fatalf("trusted team scope was broadened: decision=%+v err=%v", allowed, err)
	}
	reservation.Release()

	report := policy.Simulate(compiled, []policy.HistoricalSample{{
		Ts: now, AdmissionKnown: true, Context: policy.Context{
			Provider: "anthropic", Team: "platform", Project: "other", IdentityConfidence: "authenticated",
			TeamConfidence: "authenticated", UserConfidence: "authenticated", ProjectConfidence: "self_reported",
		},
	}})
	if report.WouldDeny != 1 || report.Reasons["authenticated_identity_required"] != 1 {
		t.Fatalf("simulation did not preserve identity denial: %+v", report)
	}
}

func TestStrictestApplicableRuleWinsWithDeterministicExplanation(t *testing.T) {
	maximum := 0.50
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "strict", Namespace: "strict", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{
			{ID: "global", Match: policy.Match{Provider: policy.AccessList{Allow: []string{"openai"}}},
				Limits: policy.Limits{MaxEstimatedCallCostUSD: &maximum}},
			{ID: "preview-models", Scope: policy.Scope{Model: []string{"*-preview"}},
				Match: policy.Match{Model: policy.AccessList{Deny: []string{"*-preview"}}}},
		},
	}
	s, engine, compiled := newEngine(t, doc)
	context := policy.Context{
		Agent: "codex", Provider: "anthropic", Model: "claude-preview",
		EstimatedCostUSD: 1, CostKnown: true, OutputBoundPresent: true, OutputBound: 100,
	}
	_, first, err := engine.Admit(time.Now().UTC(), context)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Denied() || first.HTTPStatus != 403 || len(first.Rules) != 2 {
		t.Fatalf("decision = %+v", first)
	}
	if first.Rules[0].RuleID != "preview-models" || first.Rules[1].RuleID != "global" {
		t.Fatalf("rule order = %+v", first.Rules)
	}
	if got := violationCodes(first); !reflect.DeepEqual(got, []string{"deny_match", "max_estimated_call_cost", "not_allowed"}) {
		t.Fatalf("violations = %v", got)
	}
	_, second, err := engine.Admit(time.Now().UTC().Add(time.Millisecond), context)
	if err != nil {
		t.Fatal(err)
	}
	first.ID, second.ID = 0, 0
	if !reflect.DeepEqual(first, second) {
		firstJSON, _ := json.Marshal(first)
		secondJSON, _ := json.Marshal(second)
		t.Fatalf("nondeterministic decisions:\n%s\n%s", firstJSON, secondJSON)
	}
	summary, err := s.PolicyDecisionsSince(time.Unix(0, 0))
	if err != nil || summary.Denied != 2 || summary.Allowed != 0 {
		t.Fatalf("durable summary=%+v err=%v digest=%s", summary, err, compiled.Digest)
	}
}

func TestObserveAndWarnRecordViolationsWithoutBlocking(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "shadow", Namespace: "shadow", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{
			{ID: "observe", Mode: policy.ModeObserve, Match: policy.Match{Geo: policy.AccessList{Allow: []string{"us"}}}},
			{ID: "warn", Mode: policy.ModeWarn, Match: policy.Match{Model: policy.AccessList{Deny: []string{"expensive"}}},
				Limits: policy.Limits{Requests: []policy.WindowLimit{{ID: "audit", Max: 1000, Window: "1h", WindowType: "rolling"}}}},
		},
	}
	s, engine, _ := newEngine(t, doc)
	reservation, decision, err := engine.Admit(time.Now().UTC(), policy.Context{Provider: "openai", Model: "expensive", Geo: "eu"})
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release()
	if decision.Denied() || decision.Outcome != "allow" || len(decision.Rules) != 2 {
		t.Fatalf("decision = %+v", decision)
	}
	for _, rule := range decision.Rules {
		if len(rule.Violations) != 1 {
			t.Fatalf("rule = %+v", rule)
		}
	}
	usage, err := s.PolicyRuleUsageSince("shadow", "warn", time.Unix(0, 0))
	if err != nil || usage.Requests != 1 {
		t.Fatalf("warn usage=%+v err=%v", usage, err)
	}
}

func TestRollingFixedTokenRequestAndConcurrencyLimits(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "limits", Namespace: "limits", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "all", Limits: policy.Limits{
			Requests:    []policy.WindowLimit{{ID: "rolling", Max: 2, Window: "1m", WindowType: "rolling"}},
			Tokens:      []policy.WindowLimit{{ID: "fixed", Max: 100, Window: "1h", WindowType: "fixed"}},
			Concurrency: 2,
		}}},
	}
	_, engine, _ := newEngine(t, doc)
	now := time.Date(2026, 7, 12, 12, 10, 0, 0, time.UTC)
	context := policy.Context{Provider: "openai", EstimatedInput: 20, OutputBound: 30, OutputBoundPresent: true}
	firstReservation, first, err := engine.Admit(now, context)
	if err != nil || first.Denied() {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	secondReservation, second, err := engine.Admit(now.Add(time.Second), context)
	if err != nil || second.Denied() {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	_, third, err := engine.Admit(now.Add(2*time.Second), context)
	if err != nil || !third.Denied() {
		t.Fatalf("third=%+v err=%v", third, err)
	}
	if got := violationCodes(third); !reflect.DeepEqual(got, []string{"concurrency_limit", "request_limit", "token_limit"}) {
		t.Fatalf("third violations=%v", got)
	}
	firstReservation.Release()
	secondReservation.Release()
	_, withinFixed, err := engine.Admit(now.Add(2*time.Minute), context)
	if err != nil || !withinFixed.Denied() || !contains(violationCodes(withinFixed), "token_limit") {
		t.Fatalf("within fixed=%+v err=%v", withinFixed, err)
	}
	reservation, nextWindow, err := engine.Admit(now.Add(time.Hour), context)
	if err != nil || nextWindow.Denied() {
		t.Fatalf("next window=%+v err=%v", nextWindow, err)
	}
	reservation.Release()
}

func TestConcurrencyReservationIsAtomicUnderRace(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "race", Namespace: "race", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "three", Limits: policy.Limits{Concurrency: 3}}},
	}
	_, engine, _ := newEngine(t, doc)
	const workers = 48
	start := make(chan struct{})
	release := make(chan struct{})
	results := make(chan bool, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reservation, decision, err := engine.Admit(time.Now().UTC(), policy.Context{Provider: "openai"})
			allowed := err == nil && !decision.Denied()
			results <- allowed
			if allowed {
				<-release
				reservation.Release()
			}
		}()
	}
	close(start)
	allowed := 0
	for i := 0; i < workers; i++ {
		if <-results {
			allowed++
		}
	}
	close(release)
	wg.Wait()
	if allowed != 3 {
		t.Fatalf("allowed=%d, want exactly 3", allowed)
	}
}

func TestPolicyRenameDoesNotResetStableRuleConcurrency(t *testing.T) {
	first := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "before-rename", Namespace: "stable-policy", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "stable-concurrency", Limits: policy.Limits{Concurrency: 1}}},
	}
	s, engine, _ := newEngine(t, first)
	held, decision, err := engine.Admit(time.Now().UTC(), policy.Context{Provider: "openai"})
	if err != nil || decision.Denied() {
		t.Fatalf("first=%+v err=%v", decision, err)
	}
	second := first
	second.Metadata = policy.Metadata{Name: "after-rename", Namespace: "stable-policy", Revision: 2}
	compiled := compile(t, second)
	if err := s.ApplyPolicyDocument(store.PolicyDocumentRecord{
		APIVersion: compiled.Document.APIVersion, Name: compiled.Document.Metadata.Name,
		Namespace: compiled.Document.Metadata.Namespace,
		Revision:  compiled.Document.Metadata.Revision, Digest: compiled.Digest,
		DocumentJSON: string(compiled.Canonical),
	}); err != nil {
		t.Fatal(err)
	}
	_, denied, err := engine.Admit(time.Now().UTC().Add(time.Second), policy.Context{Provider: "openai"})
	if err != nil || !denied.Denied() || !contains(violationCodes(denied), "concurrency_limit") {
		t.Fatalf("renamed decision=%+v err=%v", denied, err)
	}
	held.Release()
	allowed, afterRelease, err := engine.Admit(time.Now().UTC().Add(2*time.Second), policy.Context{Provider: "openai"})
	if err != nil || afterRelease.Denied() {
		t.Fatalf("after release=%+v err=%v", afterRelease, err)
	}
	allowed.Release()
}

func TestOutputBoundAndUnknownCostFailClosedOnlyInEnforceMode(t *testing.T) {
	maximum := 0.10
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "bounds", Namespace: "bounds", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "bound", Limits: policy.Limits{
			Tokens:                  []policy.WindowLimit{{ID: "tpm", Max: 1000, Window: "1m", WindowType: "rolling"}},
			MaxEstimatedCallCostUSD: &maximum,
		}}},
	}
	_, engine, _ := newEngine(t, doc)
	_, decision, err := engine.Admit(time.Now().UTC(), policy.Context{Provider: "custom", EstimatedInput: 1})
	if err != nil || !decision.Denied() {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if got := violationCodes(decision); !reflect.DeepEqual(got, []string{"estimated_cost_unknown", "output_bound_required"}) {
		t.Fatalf("violations=%v", got)
	}
}

func TestIndependentTokenKindsAndDollarWindows(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "counter-kinds", Namespace: "counter-kinds", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "bounded", Limits: policy.Limits{
			Tokens: []policy.WindowLimit{
				{ID: "input", Kind: "input", Max: 10, Window: "1h", WindowType: "rolling"},
				{ID: "output", Kind: "output", Max: 20, Window: "1h", WindowType: "fixed"},
				{ID: "total", Kind: "total", Max: 30, Window: "1h", WindowType: "rolling"},
			},
			Dollars: []policy.DollarLimit{{ID: "spend", MaxMicroUSD: 150, Window: "1h", WindowType: "rolling"}},
		}}},
	}
	s, engine, _ := newEngine(t, doc)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	firstReservation, first, err := engine.Admit(now, policy.Context{
		Provider: "openai", EstimatedInput: 4, OutputBound: 6, OutputBoundPresent: true,
		EstimatedCostUSD: 0.0001, CostKnown: true,
	})
	if err != nil || first.Denied() || first.WindowAccounting != "admission_requests_and_conservative_bounds" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	firstReservation.Release()
	usage, err := s.PolicyRuleUsageSince("counter-kinds", "bounded", now.Add(-time.Hour))
	if err != nil || usage.Requests != 1 || usage.InputTokens != 4 || usage.OutputTokens != 6 ||
		usage.TotalTokens != 10 || usage.CostMicroUSD != 100 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	_, second, err := engine.Admit(now.Add(time.Second), policy.Context{
		Provider: "openai", EstimatedInput: 1, OutputBound: 1, OutputBoundPresent: true,
		EstimatedCostUSD: 0.00006, CostKnown: true,
	})
	if err != nil || !second.Denied() || second.HTTPStatus != 402 || !contains(violationCodes(second), "dollar_limit") {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestInputOnlyTokenWindowDoesNotRequireOutputBound(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "input-only", Namespace: "input-only", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "input", Limits: policy.Limits{Tokens: []policy.WindowLimit{
			{ID: "input", Kind: "input", Max: 10, Window: "1m", WindowType: "rolling"},
		}}}},
	}
	_, engine, _ := newEngine(t, doc)
	reservation, decision, err := engine.Admit(time.Now().UTC(), policy.Context{Provider: "local", EstimatedInput: 5})
	if err != nil || decision.Denied() || contains(violationCodes(decision), "output_bound_required") {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	reservation.Release()
}

func TestDollarWindowUnknownCostFailsClosedAndCancellationDoesNotCharge(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "dollars", Namespace: "dollars", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "spend", Limits: policy.Limits{Dollars: []policy.DollarLimit{
			{ID: "minute", MaxMicroUSD: 100, Window: "1m", WindowType: "rolling"},
		}}}},
	}
	s, engine, _ := newEngine(t, doc)
	now := time.Now().UTC()
	_, unknown, err := engine.Admit(now, policy.Context{Provider: "custom"})
	if err != nil || !unknown.Denied() || unknown.HTTPStatus != 402 || !contains(violationCodes(unknown), "estimated_cost_unknown") {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	prepared, allowed, err := engine.Prepare(now.Add(time.Second), policy.Context{
		Provider: "openai", EstimatedCostUSD: 0.0001, CostKnown: true,
	})
	if err != nil || allowed.Denied() {
		t.Fatalf("prepared=%+v err=%v", allowed, err)
	}
	if err := prepared.Cancel(); err != nil {
		t.Fatal(err)
	}
	usage, err := s.PolicyRuleUsageSince("dollars", "spend", now.Add(-time.Minute))
	if err != nil || usage.CostMicroUSD != 0 || usage.Requests != 0 {
		t.Fatalf("cancelled usage=%+v err=%v", usage, err)
	}
}

func TestConservativeBoundsAreDurablyChargedAtAdmission(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "conservative", Namespace: "conservative", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "total", Limits: policy.Limits{Tokens: []policy.WindowLimit{
			{ID: "total", Kind: "total", Max: 100, Window: "1h", WindowType: "rolling"},
		}}}},
	}
	_, engine, _ := newEngine(t, doc)
	now := time.Now().UTC()
	reservation, first, err := engine.Admit(now, policy.Context{
		Provider: "openai", EstimatedInput: 1, OutputBound: 99, OutputBoundPresent: true,
	})
	if err != nil || first.Denied() || first.WindowAccounting != "admission_requests_and_conservative_bounds" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	// Release ends concurrency only. Policy windows intentionally keep the
	// admitted conservative bound; they do not masquerade as post-response
	// actual token settlement.
	reservation.Release()
	_, second, err := engine.Admit(now.Add(time.Second), policy.Context{
		Provider: "openai", EstimatedInput: 1, OutputBound: 1, OutputBoundPresent: true,
	})
	if err != nil || !second.Denied() || !contains(violationCodes(second), "token_limit") {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestDollarReservationIsAtomicUnderRace(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "dollar-race", Namespace: "dollar-race", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "spend", Limits: policy.Limits{Dollars: []policy.DollarLimit{
			{ID: "burst", MaxMicroUSD: 100, Window: "1m", WindowType: "rolling"},
		}}}},
	}
	_, engine, _ := newEngine(t, doc)
	const workers = 32
	start := make(chan struct{})
	results := make(chan bool, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reservation, decision, err := engine.Admit(time.Now().UTC(), policy.Context{
				Provider: "openai", EstimatedCostUSD: 0.00006, CostKnown: true,
			})
			allowed := err == nil && !decision.Denied()
			results <- allowed
			if allowed {
				reservation.Release()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	allowed := 0
	for value := range results {
		if value {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("allowed=%d, want exactly 1", allowed)
	}
}

func TestSimulationMarksLegacyOutputBoundChecksIndeterminate(t *testing.T) {
	doc := policy.Document{
		APIVersion: policy.APIVersion, Kind: policy.Kind,
		Metadata: policy.Metadata{Name: "sim", Namespace: "sim", Revision: 1}, Mode: policy.ModeEnforce,
		Rules: []policy.Rule{{ID: "bounded", Limits: policy.Limits{RequireOutputBound: true}}},
	}
	compiled := compile(t, doc)
	now := time.Now().UTC()
	report := policy.Simulate(compiled, []policy.HistoricalSample{{
		Ts: now, End: now.Add(time.Second), Context: policy.Context{Provider: "openai"}, AdmissionKnown: false,
	}})
	if report.Requests != 1 || report.WouldDeny != 0 || report.WouldAllow != 1 || report.Indeterminate != 1 || report.Confidence != "partial" {
		t.Fatalf("report=%+v", report)
	}
}

func newEngine(t testing.TB, doc policy.Document) (*store.Store, *policy.Engine, *policy.Compiled) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	compiled := compile(t, doc)
	if err := s.ApplyPolicyDocument(store.PolicyDocumentRecord{
		AppliedAt: time.Now().UTC(), APIVersion: compiled.Document.APIVersion,
		Name: compiled.Document.Metadata.Name, Namespace: compiled.Document.Metadata.Namespace,
		Revision: compiled.Document.Metadata.Revision,
		Digest:   compiled.Digest, DocumentJSON: string(compiled.Canonical),
	}); err != nil {
		t.Fatal(err)
	}
	return s, policy.NewEngine(s), compiled
}

func compile(t testing.TB, doc policy.Document) *policy.Compiled {
	t.Helper()
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := policy.Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func violationCodes(decision *policy.Decision) []string {
	var out []string
	for _, rule := range decision.Rules {
		for _, violation := range rule.Violations {
			out = append(out, violation.Code)
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
