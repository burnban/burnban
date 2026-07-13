package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestFromRowIsExplicitlyContentFreeAndTrustAware(t *testing.T) {
	now := time.Date(2026, 7, 12, 9, 8, 7, 6, time.UTC)
	row := store.TelemetryRow{ID: 42, Request: store.Request{
		Ts: now, Provider: "openai", Model: "gpt-test", Agent: "agent-a",
		Session: "private-session", BodyHash: "private-body-fingerprint",
		Principal: "alice@example.test", Project: "red", CostCenter: "eng",
		IdentityDevice: "device-1", IdentityConfidence: "authenticated",
		InTokens: 10, OutTokens: 20, CostUSD: .0123, Status: 200,
		UsageState: store.UsageExact, PricingState: store.PricingPriced,
		CostSource: store.CostContract, CostSourceRef: "https://user:secret@prices.example/private-token?key=secret#fragment",
		CostConfidence: store.ConfidenceContract,
	}}
	event := FromRow(row)
	if event.TrustedPrincipal != "alice@example.test" || event.IdentityConfidence != "authenticated" {
		t.Fatalf("trusted identity projection = %+v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-session", "private-body-fingerprint"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("content-free event leaked %q: %s", secret, encoded)
		}
	}
	if event.CostSourceRef != "https://prices.example" || strings.Contains(string(encoded), "private-token") {
		t.Fatalf("price source reference was not reduced to a safe origin: %+v", event)
	}
}

func TestLegacyPolicyAttributionNeverBecomesTrusted(t *testing.T) {
	row := store.TelemetryRow{ID: 1, Request: store.Request{
		Ts: time.Unix(1, 0).UTC(), Provider: "anthropic",
		Policy: &store.PolicyMetadata{
			DecisionID: 3, ContextJSON: `{"user":"self-claimed","project":"p","team":"c","identity_confidence":"self_reported"}`,
		},
	}}
	event := FromRow(row)
	if event.Principal != "self-claimed" || event.IdentityConfidence != "self_reported" || event.TrustedPrincipal != "" {
		t.Fatalf("legacy attribution trust projection = %+v", event)
	}
}

func TestFromRowBoundsAndSanitizesUntrustedLedgerLabels(t *testing.T) {
	event := FromRow(store.TelemetryRow{ID: 1, Request: store.Request{
		Ts: time.Unix(1, 0).UTC(), Provider: strings.Repeat("p", 1000) + "\nsecret",
		CostUSD: 1e300, InTokens: -1, LatencyMs: -1, Status: 1000, IdentityConfidence: "trusted-ish",
	}})
	if len(event.Provider) > 256 || strings.ContainsAny(event.Provider, "\r\n") {
		t.Fatalf("unsafe provider = %q", event.Provider)
	}
	if event.CostUSD != 0 || event.InputTokens != 0 || event.LatencyMs != 0 || event.HTTPStatus != 0 || event.IdentityConfidence != "unverified" {
		t.Fatalf("unsafe numeric/confidence projection = %+v", event)
	}
}

func TestFromRowExportsContentFreeDownshiftReceipt(t *testing.T) {
	event := FromRow(store.TelemetryRow{ID: 9, Request: store.Request{
		Ts: time.Unix(1, 0).UTC(), Provider: "vllm", Model: "target", Route: "/v1/chat/completions",
		RequestedProvider: "openai", RequestedModel: "source", RequestedRoute: "/v1/chat/completions",
		DownshiftAction: "downshift", DownshiftRule: "safe", DownshiftTrigger: "budget_threshold",
		DownshiftReason: "selected compatible allowlisted target", DownshiftDigest: strings.Repeat("a", 64),
		DownshiftSourceUSD: .2, DownshiftTargetUSD: .02,
		DownshiftFeatures: `{"private_prompt":"must never be projected"}`,
	}})
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if !event.Downshifted || event.DownshiftFrom != "source" || event.RequestedProvider != "openai" ||
		event.DownshiftAction != "downshift" || event.DownshiftTargetUSD != .02 ||
		strings.Contains(string(encoded), "private_prompt") {
		t.Fatalf("event=%+v JSON=%s", event, encoded)
	}
}
