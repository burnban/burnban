package telemetry

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

type Batch struct {
	Events      []Event
	DroppedRows int64
	// Signals is zero for the normal all-signal export. The worker sets an
	// explicit mask when one signal has already reached a terminal OTLP
	// response for the current ledger range and only the lagging signal should
	// be retried.
	Signals SignalMask
}

type SignalMask uint8

const (
	SignalTraces SignalMask = 1 << iota
	SignalMetrics
	signalAll = SignalTraces | SignalMetrics
)

func (b Batch) exports(signal SignalMask) bool {
	mask := b.Signals
	if mask == 0 {
		mask = signalAll
	}
	return mask&signal != 0
}

type otlpValue struct {
	StringValue string   `json:"stringValue,omitempty"`
	IntValue    string   `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

type otlpAttribute struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

func stringAttr(key, value string) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpValue{StringValue: value}}
}

func intAttr(key string, value int64) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpValue{IntValue: fmt.Sprint(value)}}
}

func doubleAttr(key string, value float64) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpValue{DoubleValue: &value}}
}

func boolAttr(key string, value bool) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpValue{BoolValue: &value}}
}

func resourceAttributes(serviceName, serviceVersion string) []otlpAttribute {
	attrs := []otlpAttribute{
		stringAttr("service.name", serviceName),
		stringAttr("telemetry.sdk.name", "burnban"),
		stringAttr("telemetry.sdk.language", "go"),
		stringAttr("burnban.schema.version", SchemaVersion),
	}
	if serviceVersion != "" {
		attrs = append(attrs, stringAttr("service.version", serviceVersion))
	}
	return attrs
}

func buildTracePayload(batch Batch, serviceName, serviceVersion string, random io.Reader) ([]byte, error) {
	spans := make([]map[string]any, 0, len(batch.Events))
	for _, event := range batch.Events {
		ids := make([]byte, 24)
		if _, err := io.ReadFull(random, ids); err != nil {
			return nil, fmt.Errorf("generate OTLP identifiers: %w", err)
		}
		start := parseEventTime(event.ObservedAt)
		end := start.Add(time.Duration(max(event.LatencyMs, 0)) * time.Millisecond)
		span := map[string]any{
			"traceId":           hex.EncodeToString(ids[:16]),
			"spanId":            hex.EncodeToString(ids[16:]),
			"name":              spanName(event),
			"kind":              3, // SPAN_KIND_CLIENT
			"startTimeUnixNano": fmt.Sprint(start.UnixNano()),
			"endTimeUnixNano":   fmt.Sprint(end.UnixNano()),
			"attributes":        eventAttributes(event),
		}
		if event.HTTPStatus >= 400 || (event.HTTPStatus == 0 && event.Incomplete) {
			span["status"] = map[string]any{"code": 2} // STATUS_CODE_ERROR
		}
		spans = append(spans, span)
	}
	payload := map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": resourceAttributes(serviceName, serviceVersion)},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "github.com/burnban/burnban/internal/telemetry", "version": serviceVersion},
				"spans": spans,
			}},
		}},
	}
	return json.Marshal(payload)
}

func eventAttributes(event Event) []otlpAttribute {
	attrs := []otlpAttribute{
		stringAttr("gen_ai.operation.name", operationName(event)),
		stringAttr("gen_ai.provider.name", semanticProvider(event.Provider)),
		intAttr("burnban.request.id", event.RequestID),
		intAttr("gen_ai.usage.input_tokens", inputTokenTotal(event)),
		intAttr("gen_ai.usage.output_tokens", event.OutputTokens),
		intAttr("burnban.usage.cache_read_tokens", event.CacheReadTokens),
		intAttr("burnban.usage.cache_write_tokens", event.CacheWriteTokens),
		doubleAttr("burnban.cost.usd", event.CostUSD),
		intAttr("http.response.status_code", int64(event.HTTPStatus)),
		intAttr("burnban.latency.ms", event.LatencyMs),
		stringAttr("burnban.usage.confidence", event.UsageConfidence),
		stringAttr("burnban.pricing.state", event.PricingState),
		stringAttr("burnban.identity.confidence", event.IdentityConfidence),
		boolAttr("gen_ai.request.stream", event.Streamed),
		boolAttr("burnban.enforcement_gap", event.EnforcementGap),
		boolAttr("burnban.downshifted", event.Downshifted),
		intAttr("burnban.retry.count", event.RetryCount),
	}
	optional := []struct{ key, value string }{
		{"gen_ai.request.model", event.Model},
		{"burnban.agent", event.Agent},
		{"burnban.route", event.Route},
		{"burnban.service_tier", event.ServiceTier},
		{"burnban.inference_geo", event.Geo},
		{"burnban.identity.principal", event.Principal},
		{"burnban.identity.trusted_principal", event.TrustedPrincipal},
		{"burnban.identity.service_account", event.ServiceAccount},
		{"burnban.identity.tenant", event.IdentityTenant},
		{"burnban.identity.project", event.Project},
		{"burnban.identity.cost_center", event.CostCenter},
		{"burnban.identity.device_id", event.IdentityDeviceID},
		{"burnban.cost.source", event.CostSource},
		{"burnban.cost.source_ref", event.CostSourceRef},
		{"burnban.cost.effective_from", event.CostEffectiveFrom},
		{"burnban.cost.valid_through", event.CostValidThrough},
		{"burnban.cost.confidence", event.CostConfidence},
		{"burnban.policy.decision", event.Decision},
		{"burnban.policy.name", event.PolicyName},
		{"burnban.policy.namespace", event.PolicyNamespace},
		{"burnban.policy.digest", event.PolicyDigest},
		{"burnban.policy.mode", event.PolicyMode},
		{"burnban.policy.confidence", event.PolicyConfidence},
		{"burnban.downshift.from_model", event.DownshiftFrom},
		{"burnban.downshift.requested_provider", event.RequestedProvider},
		{"burnban.downshift.requested_route", event.RequestedRoute},
		{"burnban.downshift.action", event.DownshiftAction},
		{"burnban.downshift.rule", event.DownshiftRule},
		{"burnban.downshift.trigger", event.DownshiftTrigger},
		{"burnban.downshift.reason", event.DownshiftReason},
		{"burnban.downshift.config_digest", event.DownshiftDigest},
	}
	for _, item := range optional {
		if item.value != "" {
			attrs = append(attrs, stringAttr(item.key, item.value))
		}
	}
	if event.DecisionID != 0 {
		attrs = append(attrs, intAttr("burnban.policy.decision_id", event.DecisionID))
	}
	if event.PolicyVersion != 0 {
		attrs = append(attrs, intAttr("burnban.policy.version", event.PolicyVersion))
	}
	if event.Decision != "" {
		attrs = append(attrs, boolAttr("burnban.policy.admitted", event.DecisionAdmitted))
	}
	if event.DownshiftAction != "" {
		attrs = append(attrs,
			doubleAttr("burnban.downshift.source_estimated_usd", event.DownshiftSourceUSD),
			doubleAttr("burnban.downshift.target_estimated_usd", event.DownshiftTargetUSD))
	}
	return attrs
}

var tokenBounds = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
var durationBounds = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}

func buildMetricsPayload(batch Batch, serviceName, serviceVersion string, now time.Time) ([]byte, error) {
	inputPoints := make([]any, 0, len(batch.Events))
	outputPoints := make([]any, 0, len(batch.Events))
	durationPoints := make([]any, 0, len(batch.Events))
	for _, event := range batch.Events {
		timestamp := parseEventTime(event.ObservedAt)
		inputPoints = append(inputPoints, histogramPoint(float64(inputTokenTotal(event)), tokenBounds, metricAttributes(event, "input"), timestamp))
		outputPoints = append(outputPoints, histogramPoint(float64(event.OutputTokens), tokenBounds, metricAttributes(event, "output"), timestamp))
		durationPoints = append(durationPoints, histogramPoint(float64(event.LatencyMs)/1000, durationBounds, metricAttributes(event, ""), timestamp))
	}
	metrics := []any{
		map[string]any{
			"name": "gen_ai.client.token.usage", "description": "Number of input and output tokens used.", "unit": "{token}",
			"histogram": map[string]any{"aggregationTemporality": 1, "dataPoints": append(inputPoints, outputPoints...)},
		},
		map[string]any{
			"name": "gen_ai.client.operation.duration", "description": "GenAI operation duration.", "unit": "s",
			"histogram": map[string]any{"aggregationTemporality": 1, "dataPoints": durationPoints},
		},
	}
	if batch.DroppedRows > 0 {
		metrics = append(metrics, map[string]any{
			"name": "burnban.telemetry.dropped", "description": "Ledger rows dropped because the configured telemetry backlog bound was exceeded.", "unit": "{request}",
			"gauge": map[string]any{"dataPoints": []any{map[string]any{
				"timeUnixNano": fmt.Sprint(now.UnixNano()), "asInt": fmt.Sprint(batch.DroppedRows),
			}}},
		})
	}
	payload := map[string]any{
		"resourceMetrics": []any{map[string]any{
			"resource": map[string]any{"attributes": resourceAttributes(serviceName, serviceVersion)},
			"scopeMetrics": []any{map[string]any{
				"scope":   map[string]any{"name": "github.com/burnban/burnban/internal/telemetry", "version": serviceVersion},
				"metrics": metrics,
			}},
		}},
	}
	return json.Marshal(payload)
}

func metricAttributes(event Event, tokenType string) []otlpAttribute {
	attrs := []otlpAttribute{
		stringAttr("gen_ai.operation.name", operationName(event)),
		stringAttr("gen_ai.provider.name", semanticProvider(event.Provider)),
	}
	if tokenType != "" {
		attrs = append(attrs, stringAttr("gen_ai.token.type", tokenType))
	}
	if event.Model != "" {
		attrs = append(attrs, stringAttr("gen_ai.request.model", event.Model))
	}
	return attrs
}

func histogramPoint(value float64, bounds []float64, attrs []otlpAttribute, timestamp time.Time) map[string]any {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		value = 0
	}
	buckets := make([]string, len(bounds)+1)
	for i := range buckets {
		buckets[i] = "0"
	}
	index := len(bounds)
	for i, bound := range bounds {
		if value <= bound {
			index = i
			break
		}
	}
	buckets[index] = "1"
	return map[string]any{
		"attributes": attrs, "timeUnixNano": fmt.Sprint(timestamp.UnixNano()),
		"count": "1", "sum": value, "bucketCounts": buckets,
		"explicitBounds": bounds, "min": value, "max": value,
	}
}

func parseEventTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.UnixNano() < 0 {
		return time.Unix(0, 0).UTC()
	}
	return parsed.UTC()
}

func inputTokenTotal(event Event) int64 {
	total := event.InputTokens
	for _, value := range []int64{event.CacheReadTokens, event.CacheWriteTokens} {
		if value > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += value
	}
	return total
}

func spanName(event Event) string {
	name := operationName(event)
	if event.Model != "" {
		name += " " + event.Model
	}
	return name
}

func operationName(event Event) string {
	route := strings.ToLower(event.Route)
	switch {
	case strings.Contains(route, "embed"):
		return "embeddings"
	case strings.Contains(route, "generatecontent") || event.Provider == "gemini":
		return "generate_content"
	case strings.Contains(route, "/completions") && !strings.Contains(route, "/chat/"):
		return "text_completion"
	default:
		return "chat"
	}
}

func semanticProvider(provider string) string {
	switch strings.ToLower(provider) {
	case "gemini":
		return "gcp.gemini"
	case "xai":
		return "x_ai"
	case "mistral":
		return "mistral_ai"
	default:
		return strings.ToLower(provider)
	}
}
