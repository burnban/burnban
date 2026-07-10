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
// tokens at its cache-write premium (Anthropic only). Estimated marks
// output counts derived from character heuristics rather than a usage
// frame, so reports never present a guess as a measurement.
type Usage struct {
	Model      string
	In         int64
	Out        int64
	CacheRead  int64
	CacheWrite int64
	Estimated  bool
	Found      bool
}

type anthropicUsage struct {
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
}

// ParseAnthropicJSON reads usage from a non-streamed Messages API response.
// Anthropic's input_tokens already exclude cache reads and writes.
func ParseAnthropicJSON(body []byte) Usage {
	var v struct {
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(body, &v) != nil || v.Usage == (anthropicUsage{}) {
		return Usage{}
	}
	return Usage{
		Model:      v.Model,
		In:         v.Usage.InputTokens,
		Out:        v.Usage.OutputTokens,
		CacheRead:  v.Usage.CacheRead,
		CacheWrite: v.Usage.CacheCreation,
		Found:      true,
	}
}

type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// ParseOpenAIJSON reads usage from Chat Completions (prompt_tokens) or
// Responses API (input_tokens) bodies. OpenAI's prompt count includes the
// cached subset, so cached tokens are subtracted out of the full-price In.
func ParseOpenAIJSON(body []byte) Usage {
	var v struct {
		Model string      `json:"model"`
		Usage openAIUsage `json:"usage"`
	}
	if json.Unmarshal(body, &v) != nil {
		return Usage{}
	}
	in, out, cached := v.Usage.PromptTokens, v.Usage.CompletionTokens, v.Usage.PromptTokensDetails.CachedTokens
	if in == 0 && v.Usage.InputTokens > 0 {
		in, out, cached = v.Usage.InputTokens, v.Usage.OutputTokens, v.Usage.InputTokensDetails.CachedTokens
	}
	if in == 0 && out == 0 {
		return Usage{}
	}
	return Usage{Model: v.Model, In: in - cached, Out: out, CacheRead: cached, Found: true}
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
	u Usage
}

func (t *AnthropicSSE) Feed(line []byte) {
	data, ok := sseData(line)
	if !ok {
		return
	}
	var v struct {
		Type    string `json:"type"`
		Message *struct {
			Model string         `json:"model"`
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(data, &v) != nil {
		return
	}
	switch v.Type {
	case "message_start":
		if v.Message != nil {
			t.u.Model = v.Message.Model
			t.u.In = v.Message.Usage.InputTokens
			t.u.CacheRead = v.Message.Usage.CacheRead
			t.u.CacheWrite = v.Message.Usage.CacheCreation
			t.u.Found = true
		}
	case "message_delta":
		if v.Usage != nil && v.Usage.OutputTokens > 0 {
			t.u.Out = v.Usage.OutputTokens
		}
	}
}

func (t *AnthropicSSE) Usage() Usage { return t.u }

// OpenAISSE tracks a Chat Completions stream. When the client requested
// stream_options.include_usage the final frame carries exact counts;
// otherwise output is estimated from streamed characters and flagged.
type OpenAISSE struct {
	u     Usage
	chars int64
}

func (t *OpenAISSE) Feed(line []byte) {
	data, ok := sseData(line)
	if !ok || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var v struct {
		Model   string       `json:"model"`
		Usage   *openAIUsage `json:"usage"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
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
	for _, c := range v.Choices {
		t.chars += int64(len(c.Delta.Content))
	}
	if v.Usage != nil && (v.Usage.PromptTokens > 0 || v.Usage.CompletionTokens > 0) {
		cached := v.Usage.PromptTokensDetails.CachedTokens
		t.u.In = v.Usage.PromptTokens - cached
		t.u.Out = v.Usage.CompletionTokens
		t.u.CacheRead = cached
		t.u.Estimated = false
		t.u.Found = true
	}
}

func (t *OpenAISSE) Usage() Usage {
	u := t.u
	if u.Out == 0 && t.chars > 0 {
		// No usage frame: estimate at ~4 chars/token and say so.
		u.Out = t.chars / 4
		u.Estimated = true
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
