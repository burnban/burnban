package subsidy

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

type openClawLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role      string `json:"role"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Timestamp int64  `json:"timestamp"`
		Usage     struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cacheRead"`
			CacheWrite int64 `json:"cacheWrite"`
			Cost       struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"usage"`
	} `json:"message"`
}

// ScanOpenClaw reads normalized usage from assistant entries in every agent's
// append-only session transcript. Session metadata contains only snapshots;
// the JSONL response entries are the authoritative per-call values.
func ScanOpenClaw(dir string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanOpenClaw(dir, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

func scanOpenClaw(dir string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error) {
	sessions := 0
	scanner := newFileScanner(limits)
	err := scanner.walkJSONL(dir, since, func(path string) error {
		clean := filepath.ToSlash(path)
		if !strings.Contains(clean, "/sessions/") || strings.HasSuffix(clean, ".trajectory.jsonl") {
			return nil
		}
		contributed := false
		err := scanner.eachLine(path, func(line []byte) {
			if !bytes.Contains(line, []byte(`"usage"`)) || !bytes.Contains(line, []byte(`"assistant"`)) {
				return
			}
			var value openClawLine
			if json.Unmarshal(line, &value) != nil || value.Type != "message" || value.Message.Role != "assistant" {
				return
			}
			usage := value.Message.Usage
			if usage.Input+usage.Output+usage.CacheRead+usage.CacheWrite == 0 {
				return
			}
			ts, err := time.Parse(time.RFC3339Nano, value.Timestamp)
			if err != nil && value.Message.Timestamp > 0 {
				ts = time.UnixMilli(value.Message.Timestamp)
				err = nil
			}
			if err != nil || ts.Before(since) {
				return
			}
			model := value.Message.Model
			if value.Message.Provider != "" && !strings.HasPrefix(model, value.Message.Provider+"/") {
				model = value.Message.Provider + "/" + model
			}
			contributed = true
			emit(Event{
				Provider: "openclaw", Model: model, Time: ts, Calls: 1,
				In: usage.Input, Out: usage.Output, CacheRead: usage.CacheRead,
				CacheWrite5m: usage.CacheWrite,
				CostUSD:      usage.Cost.Total, CostKnown: usage.Cost.Total > 0,
			})
		})
		if contributed {
			sessions++
		}
		return err
	})
	return ScanResult{Sessions: sessions, Stats: scanner.stats}, err
}
