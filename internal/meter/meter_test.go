package meter

import "testing"

func TestParseOpenAICacheReadsAndWrites(t *testing.T) {
	chat := ParseOpenAIJSON([]byte(`{"model":"gpt-5.6-sol","usage":{"prompt_tokens":2000,"completion_tokens":300,"prompt_tokens_details":{"cached_tokens":700,"cache_write_tokens":500}}}`))
	if chat.Model != "gpt-5.6-sol" || chat.In != 800 || chat.Out != 300 || chat.CacheRead != 700 || chat.CacheWrite != 500 || !chat.Found {
		t.Fatalf("chat usage = %+v", chat)
	}

	responses := ParseOpenAIJSON([]byte(`{"model":"gpt-5.6-terra","usage":{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":300,"cache_write_tokens":250}}}`))
	if responses.Model != "gpt-5.6-terra" || responses.In != 450 || responses.Out != 200 || responses.CacheRead != 300 || responses.CacheWrite != 250 || !responses.Found {
		t.Fatalf("responses usage = %+v", responses)
	}
}

func TestOpenAIResponsesSSE(t *testing.T) {
	tr := &OpenAISSE{}
	tr.Feed([]byte(`data: {"type":"response.created","response":{"model":"gpt-5.6-luna"}}`))
	tr.Feed([]byte(`data: {"type":"response.output_text.delta","delta":"eight characters"}`))
	tr.Feed([]byte(`data: {"type":"response.completed","response":{"model":"gpt-5.6-luna","usage":{"input_tokens":1000,"output_tokens":80,"input_tokens_details":{"cached_tokens":400,"cache_write_tokens":100}}}}`))
	u := tr.Usage()
	if u.Model != "gpt-5.6-luna" || u.In != 500 || u.Out != 80 || u.CacheRead != 400 || u.CacheWrite != 100 || !u.Found || u.Estimated || !u.Exact || u.Incomplete {
		t.Fatalf("usage = %+v", u)
	}
}

func TestOpenAIStreamEstimatesToolAndReasoningDeltas(t *testing.T) {
	tr := &OpenAISSE{}
	tr.Feed([]byte(`data: {"model":"gpt-5.6-luna","choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"city\":\"Vancouver\"}"}}]}}]}`))
	tr.Feed([]byte(`data: {"choices":[{"delta":{"reasoning_content":"check the forecast"}}]}`))
	u := tr.Usage()
	if !u.Found || u.Out == 0 || !u.Estimated || !u.Incomplete || u.Exact {
		t.Fatalf("tool/reasoning usage = %+v", u)
	}
}

func TestAnthropicTruncatedStreamIsPartial(t *testing.T) {
	tr := &AnthropicSSE{}
	tr.Feed([]byte(`data: {"type":"message_start","message":{"model":"claude-test","usage":{"input_tokens":100,"output_tokens":1}}}`))
	tr.Feed([]byte(`data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Vancouver\"}"}}`))
	u := tr.Usage()
	if u.Model != "claude-test" || u.In != 100 || u.Out == 0 || !u.Estimated || !u.Incomplete || u.Exact {
		t.Fatalf("truncated anthropic usage = %+v", u)
	}
}

func TestAnthropicNestedCacheTierGeoAndServerTools(t *testing.T) {
	u := ParseAnthropicJSON([]byte(`{
		"model":"claude-test","service_tier":"priority","inference_geo":"us-west",
		"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":10,
			"cache_creation_input_tokens":100,
			"cache_creation":{"ephemeral_5m_input_tokens":60,"ephemeral_1h_input_tokens":40},
			"server_tool_use":{"web_search_requests":2,"web_fetch_requests":1}}}`))
	if u.Model != "claude-test" || u.In != 100 || u.Out != 20 || u.CacheRead != 10 ||
		u.CacheWrite != 100 || u.CacheWrite1h != 40 || u.ServiceTier != "priority" ||
		u.InferenceGeo != "us-west" || u.ServerToolCalls != 3 || !u.FeeUnknown ||
		!u.Found || !u.Exact || u.Incomplete {
		t.Fatalf("anthropic metadata usage = %+v", u)
	}
}

func TestAnthropicInferenceGeoFeeSafety(t *testing.T) {
	us := ParseAnthropicJSON([]byte(`{"model":"claude-test","inference_geo":"us","usage":{"input_tokens":100,"output_tokens":20}}`))
	if us.FeeUnknown || us.InferenceGeo != "us" {
		t.Fatalf("known US geo should be priced: %+v", us)
	}
	unknown := ParseAnthropicJSON([]byte(`{"model":"claude-test","inference_geo":"future-region","usage":{"input_tokens":100,"output_tokens":20}}`))
	if !unknown.FeeUnknown {
		t.Fatalf("unknown inference geo was treated as priced: %+v", unknown)
	}
}

func TestAnthropicSSEPreservesUsageMetadata(t *testing.T) {
	tr := &AnthropicSSE{}
	tr.Feed([]byte(`data: {"type":"message_start","message":{"model":"claude-test","service_tier":"standard","inference_geo":"global","usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":30},"server_tool_use":{"web_search_requests":1}}}}`))
	tr.Feed([]byte(`data: {"type":"message_delta","usage":{"output_tokens":25,"server_tool_use":{"web_search_requests":2}}}`))
	tr.Feed([]byte(`data: {"type":"message_stop"}`))
	u := tr.Usage()
	if u.Model != "claude-test" || u.In != 100 || u.Out != 25 || u.CacheWrite != 50 ||
		u.CacheWrite1h != 30 || u.ServiceTier != "standard" || u.InferenceGeo != "global" ||
		u.ServerToolCalls != 2 || !u.FeeUnknown || !u.Exact || u.Estimated || u.Incomplete {
		t.Fatalf("anthropic stream metadata usage = %+v", u)
	}
}

func TestGeminiFunctionCallWithoutUsageIsEstimated(t *testing.T) {
	tr := &GeminiSSE{}
	tr.Feed([]byte(`data: {"modelVersion":"gemini-test","candidates":[{"content":{"parts":[{"functionCall":{"name":"weather","args":{"city":"Vancouver"}}}]}}]}`))
	u := tr.Usage()
	if u.Model != "gemini-test" || u.Out == 0 || !u.Estimated || !u.Incomplete || u.Exact {
		t.Fatalf("gemini function usage = %+v", u)
	}
}

func TestGeminiCachedSubsetCannotMakeInputNegative(t *testing.T) {
	u := ParseGeminiJSON([]byte(`{"modelVersion":"gemini-test","usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":20}}`))
	if u.In != 0 || u.CacheRead != 20 || !u.Exact {
		t.Fatalf("gemini normalized usage = %+v", u)
	}
}

func TestOpenAIUsageClampsInvalidSubsets(t *testing.T) {
	u := ParseOpenAIJSON([]byte(`{"model":"gpt-5.6-sol","usage":{"input_tokens":10,"output_tokens":-4,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":30}}}`))
	if u.In != 0 || u.Out != 0 || u.CacheRead != 20 || u.CacheWrite != 30 || !u.Found {
		t.Fatalf("invalid counts were not safely normalized: %+v", u)
	}
}

func TestGeminiSSEMergesFrames(t *testing.T) {
	tr := &GeminiSSE{}
	tr.Feed([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":300,"thoughtsTokenCount":200,"cachedContentTokenCount":400},"modelVersion":"gemini-3-pro"}`))
	// A trailing frame that repeats only the prompt count must not wipe
	// the output and cache counts an earlier frame reported.
	tr.Feed([]byte(`data: {"usageMetadata":{"promptTokenCount":1000}}`))
	u := tr.Usage()
	if u.Model != "gemini-3-pro" || u.In != 600 || u.Out != 500 || u.CacheRead != 400 || !u.Found || u.Estimated {
		t.Fatalf("usage = %+v", u)
	}
}

func TestGeminiSSEEstimatesTruncatedStream(t *testing.T) {
	tr := &GeminiSSE{}
	// Stream dies before any usage frame: model and ~800 chars of text seen.
	tr.Feed([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"` + longText(800) + `"}]}}],"modelVersion":"gemini-3-pro"}`))
	u := tr.Usage()
	if !u.Found || u.Model != "gemini-3-pro" {
		t.Fatalf("truncated stream lost the model: %+v", u)
	}
	if u.Out != 200 || !u.Estimated {
		t.Fatalf("want flagged chars/4 estimate (200), got %+v", u)
	}
}

func TestParseGeminiJSONArray(t *testing.T) {
	body := `[{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":10},"modelVersion":"gemini-3-pro"},` +
		`{"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":300,"thoughtsTokenCount":200,"cachedContentTokenCount":400}}]`
	u := ParseGeminiJSON([]byte(body))
	if u.Model != "gemini-3-pro" || u.In != 600 || u.Out != 500 || u.CacheRead != 400 || !u.Found {
		t.Fatalf("usage = %+v", u)
	}
}

func longText(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
