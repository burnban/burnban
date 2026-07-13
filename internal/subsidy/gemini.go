package subsidy

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

// ScanGemini reads Gemini CLI's automatically saved project chat records.
// Gemini may append the same message more than once as token or tool metadata
// arrives, so only the latest record for each message ID is emitted.
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
				return
			}
			if value.SessionID != "" {
				sessionID = value.SessionID
			}
			if value.ProjectHash != "" {
				projectHash = value.ProjectHash
			}
			if value.Type != "gemini" || value.ID == "" || value.Tokens == nil {
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
			ts, err := time.Parse(time.RFC3339Nano, value.Timestamp)
			if err != nil || ts.Before(since) {
				continue
			}
			tokens := value.Tokens
			prompt := max(tokens.Input, 0)
			cached := min(max(tokens.Cached, 0), prompt)
			in := prompt - cached + max(tokens.Tool, 0)
			out := max(tokens.Output, 0) + max(tokens.Thoughts, 0)
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
			contributed = true
			emit(Event{
				ID: eventID, Provider: "gemini-cli", Model: model, Time: ts, Calls: 1,
				In: in, Out: out, CacheRead: cached,
				Confidence: sourceadapter.ConfidenceExact,
			})
		}
		if contributed {
			sessions++
		}
		return nil
	})
	return ScanResult{Sessions: sessions, Stats: scanner.stats}, err
}

func hasPathComponent(path, component string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == component {
			return true
		}
	}
	return false
}
