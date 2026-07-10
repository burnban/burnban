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
	if u.Model != "gpt-5.6-luna" || u.In != 500 || u.Out != 80 || u.CacheRead != 400 || u.CacheWrite != 100 || !u.Found || u.Estimated {
		t.Fatalf("usage = %+v", u)
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
