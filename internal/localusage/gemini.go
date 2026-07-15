package localusage

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/burnban/burnban/sourceadapter"
)

type geminiTokens struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
	Thoughts int64 `json:"thoughts"`
	Tool     int64 `json:"tool"`
	Total    int64 `json:"total"`
}

type geminiLine struct {
	SessionID   string        `json:"sessionId"`
	ProjectHash string        `json:"projectHash"`
	ID          string        `json:"id"`
	Timestamp   string        `json:"timestamp"`
	Type        string        `json:"type"`
	Model       string        `json:"model"`
	Tokens      *geminiTokens `json:"tokens"`
}

const geminiMalformedWarning = "one or more Gemini CLI usage records were malformed"

// ScanGemini reads Gemini CLI's automatically saved project chat records.
// Gemini may append the same message more than once as token or tool metadata
// arrives, so only the latest record for each message ID is emitted.
//
// Compatibility was checked 2026-07-12 against google-gemini/gemini-cli
// commit f354eebaf43b25bacb176007e449bb9a638fd101. In
// packages/core/src/services/chatRecordingTypes.ts, TokensSummary maps input,
// output, cached, thoughts, tool, and total to the corresponding GenAI usage
// fields; chatRecordingService.ts appends those records under project chats/.
func ScanGemini(dir string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanGemini(dir, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

func scanGemini(dir string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error) {
	sessions := 0
	scanner := newFileScanner(limits)
	err := scanner.walkJSONLMatching(dir, since, func(path string) bool {
		return hasPathComponent(path, "chats")
	}, func(path string) error {
		var sessionID, projectHash string
		latest := map[string]geminiLine{}
		var order []string
		ferr := scanner.eachLine(path, func(line []byte) {
			// Session metadata and Gemini messages are the only records relevant
			// to metering. Avoid decoding user/tool records whenever possible.
			if !bytes.Contains(line, []byte(`"sessionId"`)) &&
				(!bytes.Contains(line, []byte(`"gemini"`)) || !bytes.Contains(line, []byte(`"tokens"`))) {
				return
			}
			var value geminiLine
			if json.Unmarshal(line, &value) != nil {
				scanner.stats.Warn(geminiMalformedWarning)
				return
			}
			if value.SessionID != "" {
				sessionID = value.SessionID
			}
			if value.ProjectHash != "" {
				projectHash = value.ProjectHash
			}
			if value.Type != "gemini" || value.Tokens == nil {
				return
			}
			if value.ID == "" || !validGeminiTokens(value.Tokens) {
				scanner.stats.Warn(geminiMalformedWarning)
				return
			}
			// Validate before replacing an earlier append for the same message.
			// A truncated or corrupt trailing update must not erase valid usage.
			if _, err := time.Parse(time.RFC3339Nano, value.Timestamp); err != nil {
				scanner.stats.Warn(geminiMalformedWarning)
				return
			}
			if _, exists := latest[value.ID]; !exists {
				order = append(order, value.ID)
			}
			latest[value.ID] = value
		})
		if ferr != nil {
			return ferr
		}

		contributed := false
		for _, id := range order {
			value := latest[id]
			ts, _ := time.Parse(time.RFC3339Nano, value.Timestamp)
			if ts.Before(since) {
				continue
			}
			tokens := value.Tokens
			prompt := tokens.Input
			cached := tokens.Cached
			in := prompt - cached + tokens.Tool
			out := tokens.Output + tokens.Thoughts
			if in+out+cached == 0 {
				continue
			}
			model := value.Model
			if model == "" {
				model = "unknown"
			}
			eventID := strings.Join([]string{projectHash, sessionID, id}, "/")
			if projectHash == "" && sessionID == "" {
				eventID = filepath.Base(path) + "/" + id
			}
			event := Event{
				ID: eventID, Provider: "gemini-cli", Model: model, Time: ts, Calls: 1,
				In: in, Out: out, CacheRead: cached,
				Confidence: sourceadapter.ConfidenceExact,
			}
			if err := event.Validate(); err != nil {
				scanner.stats.Warn(geminiMalformedWarning)
				continue
			}
			contributed = true
			emit(event)
		}
		if contributed {
			sessions++
		}
		return nil
	})
	return ScanResult{Sessions: sessions, Stats: scanner.stats}, err
}

func validGeminiTokens(tokens *geminiTokens) bool {
	if tokens == nil || tokens.Cached > tokens.Input {
		return false
	}
	values := []int64{tokens.Input, tokens.Output, tokens.Cached, tokens.Thoughts, tokens.Tool, tokens.Total}
	for _, value := range values {
		if value < 0 || value > sourceadapter.MaxEventTokens {
			return false
		}
	}
	expected := int64(0)
	for _, value := range []int64{tokens.Input, tokens.Output, tokens.Thoughts, tokens.Tool} {
		var ok bool
		expected, ok = checkedAddUsage(expected, value)
		if !ok || expected > sourceadapter.MaxEventTokens {
			return false
		}
	}
	return tokens.Total == expected
}

func hasPathComponent(path, component string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == component {
			return true
		}
	}
	return false
}
