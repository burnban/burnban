package meter

import "testing"

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
