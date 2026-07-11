// Package meter extracts token usage from provider responses — JSON bodies
// and SSE streams — and normalizes it across providers so pricing can treat
// them uniformly.
package meter

import (
	"bytes"
	"encoding/json"
)

// Usage is normalized: In and Out are full-price tokens, CacheRead tokens
// were billed at the provider's cached-input discount, and CacheWrite
// tokens at its cache-write premium. Estimated marks
// output counts derived from character heuristics rather than a usage
// frame, so reports never present a guess as a measurement.
type Usage struct {
	Model           string
	In              int64
	Out             int64
	CacheRead       int64
	CacheWrite      int64
	CacheWrite1h    int64
	ServiceTier     string
	InferenceGeo    string
	ServerToolCalls int64
	FeeUnknown      bool
	Estimated       bool
	Exact           bool
	Incomplete      bool
	Found           bool
}

type anthropicUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheCreation       int64 `json:"cache_creation_input_tokens"`
	CacheRead           int64 `json:"cache_read_input_tokens"`
	CacheCreationDetail struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	ServiceTier   string           `json:"service_tier"`
	InferenceGeo  string           `json:"inference_geo"`
	ServerToolUse map[string]int64 `json:"server_tool_use"`
}

func applyAnthropicUsage(dst *Usage, u anthropicUsage) {
	dst.In = max(dst.In, u.InputTokens, 0)
	dst.Out = max(dst.Out, u.OutputTokens, 0)
	dst.CacheRead = max(dst.CacheRead, u.CacheRead, 0)
	dst.CacheWrite = max(dst.CacheWrite, u.CacheCreation, 0)
	detailTotal := max(u.CacheCreationDetail.Ephemeral5m, 0) + max(u.CacheCreationDetail.Ephemeral1h, 0)
	if detailTotal > dst.CacheWrite {
		dst.CacheWrite = detailTotal
	}
	dst.CacheWrite1h = max(dst.CacheWrite1h, min(max(u.CacheCreationDetail.Ephemeral1h, 0), dst.CacheWrite))
	if u.ServiceTier != "" {
		dst.ServiceTier = u.ServiceTier
	}
	if u.InferenceGeo != "" {
		dst.InferenceGeo = u.InferenceGeo
	}
	var toolCalls int64
	for _, count := range u.ServerToolUse {
		toolCalls += max(count, 0)
	}
	dst.ServerToolCalls = max(dst.ServerToolCalls, toolCalls)
	dst.FeeUnknown = dst.FeeUnknown || dst.ServerToolCalls > 0 ||
		tierFeeUnknown(dst.ServiceTier) || geoFeeUnknown(dst.InferenceGeo)
}

func anthropicUsageFound(u anthropicUsage) bool {
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheCreation != 0 || u.CacheRead != 0 ||
		u.CacheCreationDetail.Ephemeral5m != 0 || u.CacheCreationDetail.Ephemeral1h != 0 ||
		u.ServiceTier != "" || u.InferenceGeo != "" || len(u.ServerToolUse) > 0
}

func tierFeeUnknown(tier string) bool {
	switch string(bytes.ToLower(bytes.TrimSpace([]byte(tier)))) {
	case "", "default", "standard", "standard_only":
		return false
	default:
		return true
	}
}

func geoFeeUnknown(geo string) bool {
	switch string(bytes.ToLower(bytes.TrimSpace([]byte(geo)))) {
	case "", "global", "us":
		return false
	default:
		return true
	}
}

// ParseAnthropicJSON reads usage from a non-streamed Messages API response.
// Anthropic's input_tokens already exclude cache reads and writes.
func ParseAnthropicJSON(body []byte) Usage {
	var v struct {
		Model        string         `json:"model"`
		ServiceTier  string         `json:"service_tier"`
		InferenceGeo string         `json:"inference_geo"`
		Usage        anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(body, &v) != nil || !anthropicUsageFound(v.Usage) {
		return Usage{}
	}
	u := Usage{Model: v.Model, ServiceTier: v.ServiceTier, InferenceGeo: v.InferenceGeo, Found: true, Exact: true}
	applyAnthropicUsage(&u, v.Usage)
	if u.ServiceTier == "" {
		u.ServiceTier = v.ServiceTier
	}
	if u.InferenceGeo == "" {
		u.InferenceGeo = v.InferenceGeo
	}
	u.FeeUnknown = u.FeeUnknown || tierFeeUnknown(u.ServiceTier) || geoFeeUnknown(u.InferenceGeo)
	return u
}

type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

// ParseOpenAIJSON reads usage from Chat Completions (prompt_tokens) or
// Responses API (input_tokens) bodies. OpenAI's prompt count includes the
// cached and cache-write subsets, so both are subtracted from full-price In.
func ParseOpenAIJSON(body []byte) Usage {
	var v struct {
		Model       string      `json:"model"`
		ServiceTier string      `json:"service_tier"`
		Usage       openAIUsage `json:"usage"`
	}
	if json.Unmarshal(body, &v) != nil {
		return Usage{}
	}
	in, out, cached, writes, ok := normalizeOpenAIUsage(v.Usage)
	if !ok {
		return Usage{}
	}
	return Usage{Model: v.Model, In: in, Out: out, CacheRead: cached, CacheWrite: writes,
		ServiceTier: v.ServiceTier, FeeUnknown: tierFeeUnknown(v.ServiceTier), Found: true, Exact: true}
}

func normalizeOpenAIUsage(u openAIUsage) (in, out, cached, writes int64, ok bool) {
	if u.PromptTokens != 0 || u.CompletionTokens != 0 ||
		u.PromptTokensDetails.CachedTokens != 0 || u.PromptTokensDetails.CacheWriteTokens != 0 {
		in, out = u.PromptTokens, u.CompletionTokens
		cached = u.PromptTokensDetails.CachedTokens
		writes = u.PromptTokensDetails.CacheWriteTokens
	} else {
		in, out = u.InputTokens, u.OutputTokens
		cached = u.InputTokensDetails.CachedTokens
		writes = u.InputTokensDetails.CacheWriteTokens
	}
	if in == 0 && out == 0 && cached == 0 && writes == 0 {
		return 0, 0, 0, 0, false
	}
	in, out = max(in, 0), max(out, 0)
	cached, writes = max(cached, 0), max(writes, 0)
	// Provider totals include both subsets. Clamp inconsistent upstream data
	// rather than allowing a negative full-price count to reduce spend.
	in = max(in-cached-writes, 0)
	return in, out, cached, writes, true
}

type geminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
}

type geminiBody struct {
	ModelVersion string       `json:"modelVersion"`
	Usage        *geminiUsage `json:"usageMetadata"`
	Candidates   []struct {
		Content struct {
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// ParseGeminiJSON reads usageMetadata from a generateContent response.
// streamGenerateContent WITHOUT alt=sse returns a JSON array of chunks
// (the REST default), so arrays are accepted too and merged the same way
// the SSE tracker merges frames.
func ParseGeminiJSON(body []byte) Usage {
	body = bytes.TrimSpace(body)
	var t GeminiSSE
	if len(body) > 0 && body[0] == '[' {
		var chunks []geminiBody
		if json.Unmarshal(body, &chunks) != nil {
			return Usage{}
		}
		for i := range chunks {
			t.merge(&chunks[i])
		}
		return t.Usage()
	}
	var v geminiBody
	if json.Unmarshal(body, &v) != nil {
		return Usage{}
	}
	t.merge(&v)
	return t.Usage()
}

// Tracker consumes SSE lines from a streamed response and reports usage.
type Tracker interface {
	Feed(line []byte)
	Usage() Usage
}

// AnthropicSSE tracks a Messages API stream: message_start carries the
// model plus input and cache counts, message_delta carries the cumulative
// output count.
type AnthropicSSE struct {
	u        Usage
	chars    int64
	sawStart bool
	sawDelta bool
	sawStop  bool
}

func (t *AnthropicSSE) Feed(line []byte) {
	data, ok := sseData(line)
	if !ok {
		return
	}
	var v struct {
		Type    string `json:"type"`
		Message *struct {
			Model        string         `json:"model"`
			ServiceTier  string         `json:"service_tier"`
			InferenceGeo string         `json:"inference_geo"`
			Usage        anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
		Delta *struct {
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &v) != nil {
		return
	}
	switch v.Type {
	case "message_start":
		if v.Message != nil {
			t.u.Model = v.Message.Model
			t.u.ServiceTier = v.Message.ServiceTier
			t.u.InferenceGeo = v.Message.InferenceGeo
			applyAnthropicUsage(&t.u, v.Message.Usage)
			t.u.Found = true
			t.sawStart = true
		}
	case "message_delta":
		if v.Usage != nil {
			applyAnthropicUsage(&t.u, *v.Usage)
			t.sawDelta = true
		}
	case "content_block_delta":
		if v.Delta != nil {
			deltaChars := int64(len(v.Delta.Text) + len(v.Delta.Thinking) + len(v.Delta.PartialJSON))
			t.chars += deltaChars
			if deltaChars > 0 {
				t.u.Found = true
			}
		}
	case "message_stop":
		t.sawStop = true
	}
}

func (t *AnthropicSSE) Usage() Usage {
	u := t.u
	u.Exact = t.sawStart && t.sawDelta && t.sawStop
	if !u.Exact && u.Found {
		u.Out = max(u.Out, (t.chars+3)/4)
		u.Estimated = true
		u.Incomplete = true
	}
	return u
}

// OpenAISSE tracks Chat Completions and Responses API streams. An exact
// final usage frame wins; otherwise output is estimated from streamed
// characters and flagged.
type OpenAISSE struct {
	u     Usage
	chars int64
	exact bool
}

func (t *OpenAISSE) Feed(line []byte) {
	data, ok := sseData(line)
	if !ok || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var v struct {
		Model       string       `json:"model"`
		ServiceTier string       `json:"service_tier"`
		Usage       *openAIUsage `json:"usage"`
		Delta       string       `json:"delta"`
		Response    *struct {
			Model       string       `json:"model"`
			ServiceTier string       `json:"service_tier"`
			Usage       *openAIUsage `json:"usage"`
		} `json:"response"`
		Choices []struct {
			Delta struct {
				Content          json.RawMessage `json:"content"`
				Reasoning        string          `json:"reasoning"`
				ReasoningContent string          `json:"reasoning_content"`
				Refusal          string          `json:"refusal"`
				FunctionCall     *struct {
					Arguments string `json:"arguments"`
				} `json:"function_call"`
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &v) != nil {
		return
	}
	if v.Model != "" {
		t.u.Model = v.Model
		t.u.Found = true
	}
	if v.ServiceTier != "" {
		t.u.ServiceTier = v.ServiceTier
		t.u.FeeUnknown = t.u.FeeUnknown || tierFeeUnknown(v.ServiceTier)
	}
	if v.Response != nil && v.Response.Model != "" {
		t.u.Model = v.Response.Model
		t.u.Found = true
	}
	if v.Response != nil && v.Response.ServiceTier != "" {
		t.u.ServiceTier = v.Response.ServiceTier
		t.u.FeeUnknown = t.u.FeeUnknown || tierFeeUnknown(v.Response.ServiceTier)
	}
	charsBefore := t.chars
	t.chars += int64(len(v.Delta))
	for _, c := range v.Choices {
		t.chars += rawStringLen(c.Delta.Content)
		t.chars += int64(len(c.Delta.Reasoning) + len(c.Delta.ReasoningContent) + len(c.Delta.Refusal))
		if c.Delta.FunctionCall != nil {
			t.chars += int64(len(c.Delta.FunctionCall.Arguments))
		}
		for _, call := range c.Delta.ToolCalls {
			t.chars += int64(len(call.Function.Arguments))
		}
	}
	if t.chars > charsBefore {
		t.u.Found = true
	}
	if v.Usage != nil {
		t.setExact(*v.Usage)
	}
	if v.Response != nil && v.Response.Usage != nil {
		t.setExact(*v.Response.Usage)
	}
}

func (t *OpenAISSE) setExact(raw openAIUsage) {
	in, out, cached, writes, ok := normalizeOpenAIUsage(raw)
	if !ok {
		return
	}
	t.u.In, t.u.Out = in, out
	t.u.CacheRead, t.u.CacheWrite = cached, writes
	t.u.Estimated = false
	t.u.Found = true
	t.u.Exact = true
	t.u.Incomplete = false
	t.exact = true
}

func (t *OpenAISSE) Usage() Usage {
	u := t.u
	if !t.exact && u.Found {
		// No usage frame: estimate at ~4 chars/token and say so.
		u.Out = max(u.Out, (t.chars+3)/4)
		u.Estimated = true
		u.Incomplete = true
		u.Exact = false
	}
	return u
}

func rawStringLen(raw json.RawMessage) int64 {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return 0
	}
	return int64(len(value))
}

// GeminiSSE tracks streamGenerateContent?alt=sse. Chunks carry cumulative
// usageMetadata as the response grows, but not every chunk repeats every
// field, so each count keeps its running maximum — a trailing prompt-only
// frame must never wipe the output counts an earlier frame reported. When
// the stream dies before any usage frame, output is estimated from the
// streamed text and flagged, matching the OpenAI tracker's behavior.
type GeminiSSE struct {
	model                                string
	found                                bool
	prompt, candidates, thoughts, cached int64
	chars                                int64
	usageSeen                            bool
}

func (t *GeminiSSE) Feed(line []byte) {
	data, ok := sseData(line)
	if !ok {
		return
	}
	var v geminiBody
	if json.Unmarshal(data, &v) != nil {
		return
	}
	t.merge(&v)
}

func (t *GeminiSSE) merge(v *geminiBody) {
	if v.ModelVersion != "" {
		t.model = v.ModelVersion
		t.found = true
	}
	for _, c := range v.Candidates {
		for _, p := range c.Content.Parts {
			t.chars += int64(len(p.Text))
			if p.Text != "" {
				t.found = true
			}
			if p.FunctionCall != nil {
				t.chars += int64(len(p.FunctionCall.Name) + len(p.FunctionCall.Args))
				t.found = true
			}
		}
	}
	if u := v.Usage; u != nil {
		t.usageSeen = true
		if u.PromptTokenCount > 0 || u.CandidatesTokenCount > 0 {
			t.found = true
		}
		t.prompt = max(t.prompt, u.PromptTokenCount)
		t.candidates = max(t.candidates, u.CandidatesTokenCount)
		t.thoughts = max(t.thoughts, u.ThoughtsTokenCount)
		t.cached = max(t.cached, u.CachedContentTokenCount)
	}
}

// Usage composes the tracked maxima. Gemini's promptTokenCount includes
// the cached subset, and thinking tokens are billed as output, so both
// are normalized here.
func (t *GeminiSSE) Usage() Usage {
	u := Usage{
		Model:     t.model,
		In:        max(t.prompt-t.cached, 0),
		Out:       t.candidates + t.thoughts,
		CacheRead: t.cached,
		Found:     t.found,
		Exact:     t.usageSeen,
	}
	if !t.usageSeen && u.Found {
		u.Out = max(u.Out, (t.chars+3)/4)
		u.Estimated = true
		u.Incomplete = true
	}
	return u
}

func sseData(line []byte) ([]byte, bool) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	return bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), true
}
