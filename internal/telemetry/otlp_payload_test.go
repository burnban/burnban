package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWarehouseEventGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/event.golden.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(fixtureEvent())
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, want) {
		t.Fatalf("warehouse event schema changed\nwant: %s\n got: %s", want, got)
	}
}

func fixtureEvent() Event {
	return Event{
		SchemaVersion: SchemaVersion, RequestID: 7,
		ObservedAt: "2026-07-12T01:02:03.000000004Z", Provider: "openai",
		Model: "gpt-test", Agent: "ci", Route: "/v1/chat/completions",
		IdentityConfidence: "authenticated", TrustedPrincipal: "svc-ci", Project: "oss",
		InputTokens: 10, OutputTokens: 4, CacheReadTokens: 2, CostUSD: .001,
		LatencyMs: 250, HTTPStatus: 200, Streamed: true,
		UsageConfidence: "exact", PricingState: "priced", CostSource: "contract",
		DecisionID: 9, Decision: "allow", DecisionAdmitted: true,
		PolicyName: "default", PolicyVersion: 3, PolicyDigest: strings.Repeat("a", 64),
	}
}

func TestOTLPJSONUsesProtocolMappingAndNeverContentFields(t *testing.T) {
	random := make([]byte, 24)
	for i := range random {
		random[i] = byte(i)
	}
	payload, err := buildTracePayload(Batch{Events: []Event{fixtureEvent()}}, "burnban", "test", bytes.NewReader(random))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(payload) {
		t.Fatalf("invalid OTLP JSON: %s", payload)
	}
	text := string(payload)
	for _, want := range []string{
		`"traceId":"000102030405060708090a0b0c0d0e0f"`,
		`"spanId":"1011121314151617"`, `"kind":3`,
		`"gen_ai.provider.name"`, `"gen_ai.usage.input_tokens"`,
		`"burnban.identity.trusted_principal"`, `"burnban.policy.version"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("OTLP payload missing %s: %s", want, text)
		}
	}
	for _, forbidden := range []string{"input.messages", "output.messages", "system_instructions", "authorization", "body_hash", "session"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Errorf("OTLP payload contains forbidden field %q: %s", forbidden, text)
		}
	}
}

func TestOTLPMetricsUseGenAIHistograms(t *testing.T) {
	payload, err := buildMetricsPayload(Batch{Events: []Event{fixtureEvent()}, DroppedRows: 2}, "burnban", "test", time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, want := range []string{"gen_ai.client.token.usage", "gen_ai.client.operation.duration", "burnban.telemetry.dropped", `"aggregationTemporality":1`} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics payload missing %q: %s", want, text)
		}
	}
}

func TestInputTokenTotalSaturates(t *testing.T) {
	if got := inputTokenTotal(Event{InputTokens: 1<<63 - 2, CacheReadTokens: 10}); got != 1<<63-1 {
		t.Fatalf("saturated token total = %d", got)
	}
}

func TestOTLPDownshiftAttributesContainEvidenceButNoFeaturePayload(t *testing.T) {
	event := fixtureEvent()
	event.Downshifted, event.DownshiftFrom = true, "source-model"
	event.RequestedProvider, event.RequestedRoute = "openai", "/v1/chat/completions"
	event.DownshiftAction, event.DownshiftRule, event.DownshiftTrigger = "downshift", "safe", "budget_threshold"
	event.DownshiftReason, event.DownshiftDigest = "selected compatible allowlisted target", strings.Repeat("b", 64)
	event.DownshiftSourceUSD, event.DownshiftTargetUSD = .2, .02
	payload, err := buildTracePayload(Batch{Events: []Event{event}}, "burnban", "test", bytes.NewReader(make([]byte, 24)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, want := range []string{"burnban.downshift.requested_provider", "burnban.downshift.rule", "burnban.downshift.reason", "burnban.downshift.target_estimated_usd"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"feature", "tool_schema", "upstream_url"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("downshift OTLP leaked %q: %s", forbidden, text)
		}
	}
}
