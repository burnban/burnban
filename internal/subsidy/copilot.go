package subsidy

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/burnban/burnban/sourceadapter"
)

const (
	copilotMalformedWarning    = "one or more GitHub Copilot CLI usage records were malformed"
	copilotRequestCountWarning = "GitHub Copilot CLI request count was unavailable for one or more model totals"
	copilotBoundaryWarning     = "GitHub Copilot CLI cumulative session usage crossed the report start boundary"
	copilotCacheTierWarning    = "GitHub Copilot CLI does not identify the cache-write retention tier"
)

type copilotShutdownLine struct {
	ID        string               `json:"id"`
	Timestamp string               `json:"timestamp"`
	Type      string               `json:"type"`
	Data      *copilotShutdownData `json:"data"`
}

type copilotShutdownData struct {
	SessionStartTime *int64                         `json:"sessionStartTime"`
	ModelMetrics     map[string]*copilotModelMetric `json:"modelMetrics"`
}

type copilotModelMetric struct {
	Requests *struct {
		Count *int64 `json:"count"`
	} `json:"requests"`
	Usage        *copilotUsage                  `json:"usage"`
	TokenDetails map[string]*copilotTokenDetail `json:"tokenDetails"`
}

type copilotUsage struct {
	InputTokens      *int64 `json:"inputTokens"`
	OutputTokens     *int64 `json:"outputTokens"`
	CacheReadTokens  *int64 `json:"cacheReadTokens"`
	CacheWriteTokens *int64 `json:"cacheWriteTokens"`
	ReasoningTokens  *int64 `json:"reasoningTokens"`
}

type copilotTokenDetail struct {
	TokenCount *int64 `json:"tokenCount"`
}

// ScanGitHubCopilotCLI reads the durable per-session usage aggregate that
// GitHub Copilot CLI appends at shutdown. It never decodes message event data.
//
// Compatibility was checked 2026-07-12 against the official
// @github/copilot@1.0.70 package (buildMetadata gitCommit 1a7a0a2e78). Its
// shipped schemas/session-events.schema.json defines session.shutdown with
// per-model request, input, output, cache-read, and cache-write totals.
func ScanGitHubCopilotCLI(dir string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanGitHubCopilotCLI(dir, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

func scanGitHubCopilotCLI(dir string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error) {
	sessions := 0
	scanner := newFileScanner(limits)
	err := scanner.walkJSONLMatching(dir, since, func(path string) bool {
		return filepath.Base(path) == "events.jsonl"
	}, func(path string) error {
		var latest *copilotShutdownLine
		ferr := scanner.eachLine(path, func(line []byte) {
			// events.jsonl contains prompts, responses, and tool payloads. The
			// shutdown discriminator lets us avoid decoding any of those records.
			if !bytes.Contains(line, []byte(`"session.shutdown"`)) {
				return
			}
			var value copilotShutdownLine
			if json.Unmarshal(line, &value) != nil {
				scanner.stats.Warn(copilotMalformedWarning)
				return
			}
			if value.Type != "session.shutdown" {
				return
			}
			if _, err := time.Parse(time.RFC3339Nano, value.Timestamp); err != nil ||
				value.ID == "" || value.Data == nil || value.Data.SessionStartTime == nil ||
				*value.Data.SessionStartTime < 0 || value.Data.ModelMetrics == nil {
				scanner.stats.Warn(copilotMalformedWarning)
				return
			}
			copy := value
			latest = &copy
		})
		if ferr != nil {
			return ferr
		}
		if latest == nil {
			return nil
		}

		shutdownTime, _ := time.Parse(time.RFC3339Nano, latest.Timestamp)
		if shutdownTime.Before(since) {
			return nil
		}
		sessionStart := time.UnixMilli(*latest.Data.SessionStartTime)
		if sessionStart.After(shutdownTime) {
			scanner.stats.Warn(copilotMalformedWarning)
			return nil
		}
		crossesBoundary := !since.IsZero() && sessionStart.Before(since)
		if crossesBoundary {
			scanner.stats.Warn(copilotBoundaryWarning)
		}

		models := make([]string, 0, len(latest.Data.ModelMetrics))
		for model := range latest.Data.ModelMetrics {
			models = append(models, model)
		}
		sort.Strings(models)
		contributed := false
		for _, model := range models {
			metric := latest.Data.ModelMetrics[model]
			uncachedInput, output, cacheRead, cacheWrite, ok := normalizedCopilotUsage(metric)
			if !ok {
				scanner.stats.Warn(copilotMalformedWarning)
				continue
			}
			if uncachedInput+output+cacheRead+cacheWrite == 0 {
				continue
			}
			calls := int64(1)
			confidence := sourceadapter.ConfidenceExact
			if metric.Requests.Count == nil || *metric.Requests.Count == 0 {
				scanner.stats.Warn(copilotRequestCountWarning)
				confidence = sourceadapter.ConfidencePartial
			} else if *metric.Requests.Count < 0 || *metric.Requests.Count > sourceadapter.MaxEventCalls {
				scanner.stats.Warn(copilotMalformedWarning)
				continue
			} else {
				calls = *metric.Requests.Count
			}
			if crossesBoundary {
				confidence = sourceadapter.ConfidencePartial
			}
			if cacheWrite > 0 {
				scanner.stats.Warn(copilotCacheTierWarning)
				confidence = sourceadapter.ConfidencePartial
			}
			event := Event{
				ID:       strings.Join([]string{filepath.Base(filepath.Dir(path)), latest.ID, model}, "/"),
				Provider: "github-copilot-cli", Model: model, Time: shutdownTime, Calls: calls,
				In: uncachedInput, Out: output,
				CacheRead: cacheRead, CacheWrite5m: cacheWrite,
				Confidence: confidence,
			}
			if err := event.Validate(); err != nil {
				scanner.stats.Warn(copilotMalformedWarning)
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

func normalizedCopilotUsage(metric *copilotModelMetric) (uncachedInput, output, cacheRead, cacheWrite int64, valid bool) {
	if metric == nil || metric.Requests == nil || metric.Usage == nil {
		return 0, 0, 0, 0, false
	}
	u := metric.Usage
	if u.InputTokens == nil || u.OutputTokens == nil || u.CacheReadTokens == nil || u.CacheWriteTokens == nil {
		return 0, 0, 0, 0, false
	}
	input, output, cacheRead, cacheWrite := *u.InputTokens, *u.OutputTokens, *u.CacheReadTokens, *u.CacheWriteTokens
	reasoning := int64(0)
	if u.ReasoningTokens != nil {
		reasoning = *u.ReasoningTokens
	}
	for _, value := range []int64{input, output, cacheRead, cacheWrite, reasoning} {
		if value < 0 || value > sourceadapter.MaxEventTokens {
			return 0, 0, 0, 0, false
		}
	}
	rawCached, ok := checkedAddUsage(cacheRead, cacheWrite)
	if !ok || rawCached > input {
		return 0, 0, 0, 0, false
	}
	rawTotal, ok := checkedAddUsage(input, output)
	if !ok || rawTotal > sourceadapter.MaxEventTokens || reasoning > output {
		return 0, 0, 0, 0, false
	}

	if len(metric.TokenDetails) > 0 {
		for _, detail := range metric.TokenDetails {
			if detail == nil || detail.TokenCount == nil || *detail.TokenCount < 0 || *detail.TokenCount > sourceadapter.MaxEventTokens {
				return 0, 0, 0, 0, false
			}
		}
		inputDetail, inputOK := copilotDetailCount(metric.TokenDetails, "input")
		outputDetail, outputOK := copilotDetailCount(metric.TokenDetails, "output")
		cacheReadDetail, cacheReadOK := copilotDetailCount(metric.TokenDetails, "cache_read")
		cacheWriteDetail, cacheWriteOK := copilotDetailCount(metric.TokenDetails, "cache_write")
		if !(inputOK && outputOK && cacheReadOK && cacheWriteOK) {
			return input - rawCached, output, cacheRead, cacheWrite, true
		}
		detailTotal := int64(0)
		for _, value := range []int64{inputDetail, outputDetail, cacheReadDetail, cacheWriteDetail} {
			detailTotal, ok = checkedAddUsage(detailTotal, value)
			if !ok || detailTotal > sourceadapter.MaxEventTokens {
				return 0, 0, 0, 0, false
			}
		}
		if detailTotal > 0 {
			if reasoning > outputDetail {
				return 0, 0, 0, 0, false
			}
			return inputDetail, outputDetail, cacheReadDetail, cacheWriteDetail, true
		}
	}
	return input - rawCached, output, cacheRead, cacheWrite, true
}

func copilotDetailCount(details map[string]*copilotTokenDetail, name string) (int64, bool) {
	detail, ok := details[name]
	if !ok || detail == nil || detail.TokenCount == nil {
		return 0, false
	}
	return *detail.TokenCount, true
}
