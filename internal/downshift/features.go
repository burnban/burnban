package downshift

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
)

// Features is a content-free compatibility summary. It is safe to retain in
// the ledger: it contains no prompt, schema, tool name, URL, or credential.
type Features struct {
	Version            string   `json:"version"`
	Dialect            string   `json:"dialect"`
	Operation          string   `json:"operation,omitempty"`
	ContextUpperTokens int64    `json:"context_upper_tokens"`
	OutputBoundPresent bool     `json:"output_bound_present"`
	UsesTools          bool     `json:"uses_tools"`
	StructuredOutput   bool     `json:"structured_output"`
	Modalities         []string `json:"modalities"`
	Compatible         bool     `json:"compatible"`
	Reason             string   `json:"reason,omitempty"`
}

func Analyze(body []byte, dialect string) Features {
	features := Features{Version: "burnban.features/v1", Dialect: dialect, Compatible: true}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return incompatible(features, "request body is not a JSON object")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &root); err != nil {
		return incompatible(features, "request body is not valid JSON")
	}
	output, present, ok := outputBound(root, dialect)
	if !ok {
		return incompatible(features, "output-token bound is malformed")
	}
	features.OutputBoundPresent = present
	if !present {
		return incompatible(features, "request has no explicit output-token bound")
	}
	if int64(len(body)) > math.MaxInt64-output {
		return incompatible(features, "context bound overflows")
	}
	// One request byte per token is a deliberately conservative tokenizer-
	// independent upper bound, matching budget admission.
	features.ContextUpperTokens = int64(len(body)) + output

	modalities := map[string]bool{"text": true}
	var err error
	switch dialect {
	case "openai":
		features.UsesTools, err = analyzeOpenAITools(root["tools"])
		if err == nil {
			features.StructuredOutput, err = analyzeOpenAIStructured(root["response_format"])
		}
		if err == nil {
			err = analyzeOpenAIModalities(root, modalities)
		}
	case "anthropic":
		err = rejectAnthropicThinking(root)
		if err == nil {
			features.UsesTools, err = analyzeAnthropicTools(root["tools"])
		}
		if err == nil {
			features.StructuredOutput, err = analyzeAnthropicStructured(root)
		}
		if err == nil {
			err = analyzeAnthropicModalities(root["messages"], modalities)
		}
	case "gemini":
		features.UsesTools, err = analyzeGeminiTools(root["tools"])
		if err == nil {
			features.StructuredOutput, err = analyzeGeminiStructured(root["generationConfig"])
		}
		if err == nil {
			err = analyzeGeminiModalities(root["contents"], modalities)
		}
	default:
		return incompatible(features, "unsupported request dialect")
	}
	if err != nil {
		return incompatible(features, err.Error())
	}
	features.Modalities = sortedKeys(modalities)
	return features
}

// AnalyzeAt adds an exact wire-operation gate. Version 1 downshift supports
// only text-generation operations whose request compatibility is fully
// classified below; embeddings, image generation, batches, files, arbitrary
// custom POST routes, and the OpenAI Responses API remain pass-through.
func AnalyzeAt(body []byte, dialect, path string) Features {
	features := Analyze(body, dialect)
	if !features.Compatible {
		return features
	}
	switch dialect {
	case "openai":
		if path != "/v1/chat/completions" {
			return incompatible(features, "OpenAI downshift supports only the exact /v1/chat/completions operation")
		}
		features.Operation = "chat_completions"
	case "anthropic":
		if path != "/v1/messages" {
			return incompatible(features, "Anthropic downshift supports only the exact /v1/messages operation")
		}
		features.Operation = "messages"
	case "gemini":
		if !supportedGeminiOperation(path) {
			return incompatible(features, "Gemini downshift supports only generateContent and streamGenerateContent operations")
		}
		features.Operation = "generate_content"
	default:
		return incompatible(features, "unsupported request dialect")
	}
	return features
}

func supportedGeminiOperation(path string) bool {
	var rest string
	for _, prefix := range []string{"/v1/models/", "/v1beta/models/"} {
		if strings.HasPrefix(path, prefix) {
			rest = strings.TrimPrefix(path, prefix)
			break
		}
	}
	if rest == "" || strings.Contains(rest, "/") {
		return false
	}
	model, operation, ok := strings.Cut(rest, ":")
	return ok && model != "" && !strings.Contains(operation, ":") &&
		(operation == "generateContent" || operation == "streamGenerateContent")
}

func outputBound(root map[string]json.RawMessage, dialect string) (int64, bool, bool) {
	keys := []string{"max_tokens", "max_output_tokens", "max_completion_tokens"}
	var maximum int64
	present := false
	for _, key := range keys {
		if raw, exists := root[key]; exists {
			value, ok := positiveInt64(raw)
			if !ok {
				return 0, false, false
			}
			maximum, present = max(maximum, value), true
		}
	}
	if dialect == "gemini" {
		if raw, exists := root["generationConfig"]; exists {
			var config map[string]json.RawMessage
			if json.Unmarshal(raw, &config) != nil {
				return 0, false, false
			}
			if rawBound, exists := config["maxOutputTokens"]; exists {
				value, ok := positiveInt64(rawBound)
				if !ok {
					return 0, false, false
				}
				maximum, present = max(maximum, value), true
			}
		}
	}
	return maximum, present, true
}

func positiveInt64(raw json.RawMessage) (int64, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value json.Number
	if dec.Decode(&value) != nil {
		return 0, false
	}
	parsed, err := value.Int64()
	return parsed, err == nil && parsed > 0
}

func analyzeOpenAITools(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, nil
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return false, fmt.Errorf("tools must be an array of objects")
	}
	for _, tool := range tools {
		var kind string
		if json.Unmarshal(tool["type"], &kind) != nil || kind != "function" {
			return false, fmt.Errorf("provider-hosted or unknown tools cannot be downshifted")
		}
		var function map[string]json.RawMessage
		if json.Unmarshal(tool["function"], &function) != nil {
			return false, fmt.Errorf("function tool descriptor is malformed")
		}
		var name string
		if json.Unmarshal(function["name"], &name) != nil || strings.TrimSpace(name) == "" {
			return false, fmt.Errorf("function tool name is malformed")
		}
		if parameters, ok := function["parameters"]; ok && !jsonObject(parameters) {
			return false, fmt.Errorf("function tool parameters are not a JSON schema object")
		}
	}
	return len(tools) != 0, nil
}

func analyzeAnthropicTools(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, nil
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return false, fmt.Errorf("tools must be an array of objects")
	}
	for _, tool := range tools {
		var name string
		if json.Unmarshal(tool["name"], &name) != nil || strings.TrimSpace(name) == "" || !jsonObject(tool["input_schema"]) {
			return false, fmt.Errorf("anthropic tool schema is malformed")
		}
		if _, exists := tool["type"]; exists {
			return false, fmt.Errorf("provider-hosted or unknown tools cannot be downshifted")
		}
	}
	return len(tools) != 0, nil
}

func rejectAnthropicThinking(root map[string]json.RawMessage) error {
	for _, key := range []string{"thinking", "redacted_thinking"} {
		if raw, exists := root[key]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("anthropic %s requests cannot be downshifted", key)
		}
	}
	return nil
}

func analyzeGeminiTools(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, nil
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return false, fmt.Errorf("tools must be an array of objects")
	}
	uses := false
	for _, tool := range tools {
		if len(tool) != 1 {
			return false, fmt.Errorf("gemini tool descriptor is ambiguous")
		}
		rawFunctions, exists := tool["functionDeclarations"]
		if !exists {
			return false, fmt.Errorf("provider-hosted Gemini tools cannot be downshifted")
		}
		var functions []map[string]json.RawMessage
		if json.Unmarshal(rawFunctions, &functions) != nil || len(functions) == 0 {
			return false, fmt.Errorf("gemini function declarations are malformed")
		}
		for _, function := range functions {
			var name string
			if json.Unmarshal(function["name"], &name) != nil || strings.TrimSpace(name) == "" {
				return false, fmt.Errorf("gemini function name is malformed")
			}
			if parameters, ok := function["parameters"]; ok && !jsonObject(parameters) {
				return false, fmt.Errorf("gemini function parameters are not a JSON schema object")
			}
		}
		uses = true
	}
	return uses, nil
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(trimmed, &object) == nil
}

func analyzeOpenAIStructured(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, nil
	}
	var format map[string]json.RawMessage
	if json.Unmarshal(raw, &format) != nil {
		return false, fmt.Errorf("response_format is malformed")
	}
	var kind string
	if json.Unmarshal(format["type"], &kind) != nil {
		return false, fmt.Errorf("response_format.type is malformed")
	}
	switch kind {
	case "text":
		return false, nil
	case "json_object":
		return true, nil
	case "json_schema":
		if !jsonObject(format["json_schema"]) {
			return false, fmt.Errorf("response_format JSON schema is malformed")
		}
		return true, nil
	default:
		return false, fmt.Errorf("unknown response_format cannot be downshifted")
	}
}

func analyzeAnthropicStructured(root map[string]json.RawMessage) (bool, error) {
	for _, key := range []string{"response_format", "output_config"} {
		if raw, exists := root[key]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if !jsonObject(raw) {
				return false, fmt.Errorf("%s is malformed", key)
			}
			return true, nil
		}
	}
	return false, nil
}

func analyzeGeminiStructured(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var config map[string]json.RawMessage
	if json.Unmarshal(raw, &config) != nil {
		return false, fmt.Errorf("generationConfig is malformed")
	}
	if schema, exists := config["responseSchema"]; exists {
		if !jsonObject(schema) {
			return false, fmt.Errorf("gemini responseSchema is malformed")
		}
		return true, nil
	}
	if schema, exists := config["responseJsonSchema"]; exists {
		if !jsonObject(schema) {
			return false, fmt.Errorf("gemini responseJsonSchema is malformed")
		}
		return true, nil
	}
	if rawMime, exists := config["responseMimeType"]; exists {
		var mime string
		if json.Unmarshal(rawMime, &mime) != nil {
			return false, fmt.Errorf("gemini responseMimeType is malformed")
		}
		if mime != "" && mime != "text/plain" {
			return true, nil
		}
	}
	return false, nil
}

func analyzeOpenAIModalities(root map[string]json.RawMessage, modalities map[string]bool) error {
	if raw, exists := root["modalities"]; exists {
		var requested []string
		if json.Unmarshal(raw, &requested) != nil {
			return fmt.Errorf("modalities is malformed")
		}
		for _, modality := range requested {
			if !knownModality(modality) {
				return fmt.Errorf("unknown modality cannot be downshifted")
			}
			modalities[modality] = true
		}
	}
	var messages []struct {
		Content json.RawMessage `json:"content"`
	}
	if raw := root["messages"]; len(raw) != 0 && json.Unmarshal(raw, &messages) != nil {
		return fmt.Errorf("messages is malformed")
	}
	for _, message := range messages {
		trimmed := bytes.TrimSpace(message.Content)
		if len(trimmed) == 0 || trimmed[0] == '"' || bytes.Equal(trimmed, []byte("null")) {
			continue
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(trimmed, &parts) != nil {
			return fmt.Errorf("message content parts are malformed")
		}
		for _, part := range parts {
			var kind string
			if json.Unmarshal(part["type"], &kind) != nil {
				return fmt.Errorf("message content part type is missing")
			}
			switch kind {
			case "text", "input_text", "output_text":
			case "image_url", "input_image":
				modalities["image"] = true
			case "input_audio", "audio":
				modalities["audio"] = true
			default:
				return fmt.Errorf("unknown message modality cannot be downshifted")
			}
		}
	}
	return nil
}

func analyzeAnthropicModalities(raw json.RawMessage, modalities map[string]bool) error {
	if len(raw) == 0 {
		return nil
	}
	var messages []struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &messages) != nil {
		return fmt.Errorf("messages is malformed")
	}
	for _, message := range messages {
		if err := analyzeAnthropicContent(message.Content, modalities, true); err != nil {
			return err
		}
	}
	return nil
}

func analyzeAnthropicContent(raw json.RawMessage, modalities map[string]bool, allowToolResult bool) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '"' || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(trimmed, &parts) != nil {
		return fmt.Errorf("anthropic content blocks are malformed")
	}
	for _, part := range parts {
		if signature, exists := part["signature"]; exists && !bytes.Equal(bytes.TrimSpace(signature), []byte("null")) {
			return fmt.Errorf("anthropic signed thinking content cannot be downshifted")
		}
		var kind string
		if json.Unmarshal(part["type"], &kind) != nil {
			return fmt.Errorf("anthropic content block type is missing")
		}
		switch kind {
		case "text", "tool_use":
		case "tool_result":
			if !allowToolResult {
				return fmt.Errorf("nested Anthropic tool results cannot be downshifted")
			}
			if content, exists := part["content"]; exists {
				if err := analyzeAnthropicContent(content, modalities, false); err != nil {
					return err
				}
			}
		case "thinking", "redacted_thinking":
			return fmt.Errorf("anthropic thinking content cannot be downshifted")
		case "image":
			modalities["image"] = true
		default:
			return fmt.Errorf("unknown Anthropic content modality cannot be downshifted")
		}
	}
	return nil
}

func analyzeGeminiModalities(raw json.RawMessage, modalities map[string]bool) error {
	if len(raw) == 0 {
		return nil
	}
	var contents []struct {
		Parts []map[string]json.RawMessage `json:"parts"`
	}
	if json.Unmarshal(raw, &contents) != nil {
		return fmt.Errorf("gemini contents are malformed")
	}
	for _, content := range contents {
		for _, part := range content.Parts {
			switch {
			case part["text"] != nil, part["functionCall"] != nil, part["functionResponse"] != nil:
			case part["inlineData"] != nil, part["fileData"] != nil:
				rawData := part["inlineData"]
				if rawData == nil {
					rawData = part["fileData"]
				}
				var data struct {
					MimeType string `json:"mimeType"`
				}
				if json.Unmarshal(rawData, &data) != nil || data.MimeType == "" {
					return fmt.Errorf("gemini media part lacks a known MIME type")
				}
				switch {
				case strings.HasPrefix(data.MimeType, "image/"):
					modalities["image"] = true
				case strings.HasPrefix(data.MimeType, "audio/"):
					modalities["audio"] = true
				default:
					return fmt.Errorf("unsupported Gemini media modality cannot be downshifted")
				}
			default:
				return fmt.Errorf("unknown Gemini content part cannot be downshifted")
			}
		}
	}
	return nil
}

func knownModality(value string) bool { return value == "text" || value == "image" || value == "audio" }

func incompatible(features Features, reason string) Features {
	features.Compatible = false
	features.Reason = reason
	if features.Modalities == nil {
		features.Modalities = []string{}
	}
	return features
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func Eligible(rule *Rule, features Features) (bool, string) {
	if rule == nil {
		return false, "no allowlisted equivalent target"
	}
	if !features.Compatible {
		return false, features.Reason
	}
	if features.Dialect != rule.Source.Dialect || features.Dialect != rule.Target.Dialect {
		return false, "request and target dialect differ"
	}
	if !features.OutputBoundPresent {
		return false, "request has no explicit output-token bound"
	}
	if features.ContextUpperTokens > rule.Source.ContextTokens {
		return false, "request exceeds declared source context window"
	}
	if features.ContextUpperTokens > rule.Target.ContextTokens {
		return false, "request exceeds target context window"
	}
	if features.UsesTools && !rule.Capabilities.Tools {
		return false, "target is not allowlisted for function tools"
	}
	if features.StructuredOutput && !rule.Capabilities.StructuredOutput {
		return false, "target is not allowlisted for structured output"
	}
	allowed := map[string]bool{}
	for _, modality := range rule.Capabilities.Modalities {
		allowed[modality] = true
	}
	for _, modality := range features.Modalities {
		if !allowed[modality] {
			return false, "target is not allowlisted for " + modality + " modality"
		}
	}
	return true, "compatibility checks passed"
}

// RewriteRequest changes only the model selector. JSON is re-encoded but no
// prompt, tool, modality, or output field is added, removed, or translated.
func RewriteRequest(path string, body []byte, dialect, targetModel string) (string, []byte, error) {
	switch dialect {
	case "openai", "anthropic":
		var root map[string]json.RawMessage
		if json.Unmarshal(body, &root) != nil {
			return "", nil, fmt.Errorf("cannot rewrite non-object JSON request")
		}
		if _, exists := root["model"]; !exists {
			return "", nil, fmt.Errorf("request has no explicit model field")
		}
		encodedModel, _ := json.Marshal(targetModel)
		root["model"] = encodedModel
		rewritten, err := json.Marshal(root)
		if err != nil {
			return "", nil, err
		}
		return path, rewritten, nil
	case "gemini":
		const marker = "/models/"
		start := strings.Index(path, marker)
		if start < 0 {
			return "", nil, fmt.Errorf("gemini request path has no model selector")
		}
		start += len(marker)
		end := len(path)
		if index := strings.IndexAny(path[start:], ":/"); index >= 0 {
			end = start + index
		}
		if end == start {
			return "", nil, fmt.Errorf("gemini request path has an empty model selector")
		}
		return path[:start] + url.PathEscape(targetModel) + path[end:], body, nil
	default:
		return "", nil, fmt.Errorf("unsupported dialect %q", dialect)
	}
}

func FeatureJSON(features Features) string {
	encoded, err := json.Marshal(features)
	if err != nil || len(encoded) > 4096 {
		return ""
	}
	return string(encoded)
}
