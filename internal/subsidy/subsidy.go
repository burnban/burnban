// Package subsidy reads supported local usage stores, then totals what those
// tokens would cost at API prices. Nothing is proxied and source logs are never
// modified.
package subsidy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/sourceadapter"
)

// Event is one model call recovered from a local log, normalized the same
// way the proxy's meter normalizes live traffic: In and Out are full-price
// tokens (OpenAI's cached subset already subtracted), CacheRead was billed
// at the provider's cached-input discount.
// Type aliases preserve the original subsidy API while the adapter contract is
// public for first- and third-party source implementations.
type Event = sourceadapter.Event
type ServerToolUsage = sourceadapter.ServerToolUsage
type ScanLimits = sourceadapter.ScanLimits
type ScanStats = sourceadapter.ScanStats
type ScanResult = sourceadapter.ScanResult

func DefaultScanLimits() ScanLimits {
	return ScanLimits{
		MaxFiles: 5_000, MaxBytes: 512 << 20, MaxLineBytes: 32 << 20,
		MaxRecords: 1_000_000, MaxDuration: 10 * time.Second,
	}
}

func normalizeScanLimits(limits ScanLimits) ScanLimits {
	defaults := DefaultScanLimits()
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = defaults.MaxFiles
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = defaults.MaxBytes
	}
	if limits.MaxLineBytes <= 0 {
		limits.MaxLineBytes = defaults.MaxLineBytes
	}
	if limits.MaxRecords <= 0 {
		limits.MaxRecords = defaults.MaxRecords
	}
	if limits.MaxDuration <= 0 {
		limits.MaxDuration = defaults.MaxDuration
	}
	return limits
}

// write1hMult is Anthropic's 1-hour cache-write premium. The pricing table
// carries a single write multiplier (the 5-minute rate); the 1h tier only
// surfaces in local logs, so the constant lives here rather than in the table.
const write1hMult = 2.0

// Anthropic bills web search at $10 per 1,000 successful searches. Web fetch
// currently has no per-call fee beyond the tokens already counted above.
const webSearchRequestUSD = 0.01

// Cost prices an event bundle at API rates, honoring the 1h write tier.
func Cost(p pricing.Price, in, out, cacheRead, w5m, w1h int64) float64 {
	return pricing.Cost(p, in, out, cacheRead, w5m) +
		float64(w1h)*p.InputPerMTok*write1hMult/1e6
}

func serverToolCost(usage ServerToolUsage) float64 {
	return float64(max(usage.WebSearchRequests, 0)) * webSearchRequestUSD
}

// ---- Claude Code: ~/.claude/projects/**/*.jsonl ----

type claudeLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens   int64 `json:"input_tokens"`
			OutputTokens  int64 `json:"output_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheDetail   struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
			ServiceTier   string          `json:"service_tier"`
			InferenceGeo  string          `json:"inference_geo"`
			ServerToolUse ServerToolUsage `json:"server_tool_use"`
		} `json:"usage"`
	} `json:"message"`
}

// ScanClaude walks Claude Code's session logs and emits one Event per
// assistant message in the window. A message with several content blocks is
// written once per block with identical usage, and resumed sessions can copy
// history into new files, so events dedup on message ID + request ID across
// the whole tree. Returns the number of session files that contributed.
func ScanClaude(dir string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanClaude(dir, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

func scanClaude(dir string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error) {
	seen := make(map[string]struct{})
	sessions := 0
	scanner := newFileScanner(limits)
	err := scanner.walkJSONL(dir, since, func(path string) error {
		contributed := false
		ferr := scanner.eachLine(path, func(line []byte) {
			if !bytes.Contains(line, []byte(`"usage"`)) {
				return
			}
			var v claudeLine
			if json.Unmarshal(line, &v) != nil || v.Type != "assistant" {
				return
			}
			u := v.Message.Usage
			if v.Message.Model == "" || v.Message.Model == "<synthetic>" ||
				u.InputTokens+u.OutputTokens+u.CacheCreation+u.CacheRead == 0 {
				return
			}
			if v.Message.ID != "" {
				key := v.Message.ID + "/" + v.RequestID
				if _, dup := seen[key]; dup {
					return
				}
				seen[key] = struct{}{}
			}
			ts, terr := time.Parse(time.RFC3339, v.Timestamp)
			if terr != nil || ts.Before(since) {
				return
			}
			w5m, w1h := cacheWriteSplit(u.CacheCreation, u.CacheDetail.Ephemeral5m, u.CacheDetail.Ephemeral1h)
			contributed = true
			emit(Event{
				Provider: "claude-code", Model: v.Message.Model, Time: ts, Calls: 1,
				In: u.InputTokens, Out: u.OutputTokens, CacheRead: u.CacheRead,
				CacheWrite5m: w5m, CacheWrite1h: w1h,
				ServiceTier: u.ServiceTier, InferenceGeo: u.InferenceGeo, ServerToolUse: u.ServerToolUse,
				Confidence: sourceadapter.ConfidenceExact,
			})
		})
		if contributed {
			sessions++
		}
		return ferr
	})
	return ScanResult{Sessions: sessions, Stats: scanner.stats}, err
}

func cacheWriteSplit(total, explicit5m, explicit1h int64) (int64, int64) {
	total, explicit5m, explicit1h = max(total, 0), max(explicit5m, 0), max(explicit1h, 0)
	if explicit5m+explicit1h == 0 {
		return total, 0
	}
	if explicit1h > total {
		return total, 0
	}
	if explicit5m+explicit1h > total {
		return total - explicit1h, explicit1h
	}
	return total - explicit1h, explicit1h
}

// ---- Codex: ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl ----

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexTotals struct {
	Input  int64 `json:"input_tokens"`
	Cached int64 `json:"cached_input_tokens"`
	Output int64 `json:"output_tokens"`
}

// ScanCodex walks Codex rollout logs. Rollouts don't record per-request
// usage; they record token_count events carrying a cumulative session total,
// so each event's contribution is the delta from the previous one — deltas
// outside the window still advance the baseline (otherwise the first
// in-window event would swallow the whole pre-window session), and a total
// that shrinks means the counter reset on restart, so the new total becomes
// the delta. The model comes from the most recent turn_context.
//
// Codex subagent rollouts begin with a replay of the parent's history. Those
// replayed token_count records retain the parent's cumulative counter and
// must advance the child's baseline, but must not be emitted again. The first
// trigger-turn inter-agent record marks the transition to live child traffic.
func ScanCodex(dir string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanCodex(dir, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

func codexSubagentSource(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var label string
	if json.Unmarshal(raw, &label) == nil {
		return label == "subagent"
	}
	var source struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	return json.Unmarshal(raw, &source) == nil && len(source.Subagent) > 0 && string(source.Subagent) != "null"
}

func scanCodex(dir string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error) {
	sessions := 0
	scanner := newFileScanner(limits)
	err := scanner.walkJSONL(dir, since, func(path string) error {
		model := "unknown"
		var prev codexTotals
		contributed := false
		sessionMetaSeen := false
		subagent := false
		replayingParent := false
		liveBoundarySeen := false
		ferr := scanner.eachLine(path, func(line []byte) {
			if !bytes.Contains(line, []byte(`"session_meta"`)) &&
				!bytes.Contains(line, []byte(`"inter_agent_communication_metadata"`)) &&
				!bytes.Contains(line, []byte(`"turn_context"`)) && !bytes.Contains(line, []byte(`"token_count"`)) {
				return
			}
			var v codexLine
			if json.Unmarshal(line, &v) != nil {
				return
			}
			switch v.Type {
			case "session_meta":
				// A fork replay can contain the parent's session_meta immediately
				// after the child's. Only the first record describes this file.
				if sessionMetaSeen {
					return
				}
				sessionMetaSeen = true
				var meta struct {
					Source json.RawMessage `json:"source"`
				}
				if json.Unmarshal(v.Payload, &meta) == nil && codexSubagentSource(meta.Source) {
					subagent = true
					replayingParent = true
				}
			case "inter_agent_communication_metadata":
				if !replayingParent {
					return
				}
				var communication struct {
					TriggerTurn bool `json:"trigger_turn"`
				}
				if json.Unmarshal(v.Payload, &communication) == nil && communication.TriggerTurn {
					replayingParent = false
					liveBoundarySeen = true
				}
			case "turn_context":
				var tc struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(v.Payload, &tc) == nil && tc.Model != "" {
					model = tc.Model
				}
			case "event_msg":
				var ev struct {
					Type string `json:"type"`
					Info *struct {
						Total codexTotals `json:"total_token_usage"`
					} `json:"info"`
				}
				if json.Unmarshal(v.Payload, &ev) != nil || ev.Type != "token_count" || ev.Info == nil {
					return
				}
				cur := ev.Info.Total
				d := codexTotals{cur.Input - prev.Input, cur.Cached - prev.Cached, cur.Output - prev.Output}
				if d.Input < 0 || d.Cached < 0 || d.Output < 0 {
					d = cur // counter reset (process restart): fresh baseline
				}
				prev = cur
				if d.Input+d.Output == 0 {
					return
				}
				ts, terr := time.Parse(time.RFC3339, v.Timestamp)
				if terr != nil || ts.Before(since) {
					return
				}
				if replayingParent {
					return
				}
				in := d.Input - d.Cached // OpenAI input counts include the cached subset
				if in < 0 {
					in = 0
				}
				contributed = true
				emit(Event{
					Provider: "codex", Model: model, Time: ts, Calls: 1,
					In: in, Out: d.Output, CacheRead: d.Cached,
					Confidence: sourceadapter.ConfidenceExact,
				})
			}
		})
		if contributed {
			sessions++
		}
		if subagent && !liveBoundarySeen {
			scanner.stats.Warn("one or more Codex subagent logs did not expose a live-usage boundary")
		}
		return ferr
	})
	return ScanResult{Sessions: sessions, Stats: scanner.stats}, err
}

var errScanLimit = errors.New("scan limit reached")
var errNonRegularLog = errors.New("log file is not a stable regular file")

type fileScanner struct {
	limits   ScanLimits
	stats    ScanStats
	deadline time.Time
}

func newFileScanner(limits ScanLimits) *fileScanner {
	limits = normalizeScanLimits(limits)
	return &fileScanner{limits: limits, deadline: time.Now().Add(limits.MaxDuration)}
}

// walkJSONL visits append-only JSONL files in a bounded resource envelope. A
// missing source means the tool is not installed; inaccessible sources are an
// error so callers can distinguish them from a clean empty result.
func (s *fileScanner) walkJSONL(dir string, since time.Time, visit func(path string) error) error {
	return s.walkJSONLMatching(dir, since, nil, visit)
}

func (s *fileScanner) walkJSONLMatching(dir string, since time.Time, accept func(path string) bool, visit func(path string) error) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("source unavailable: %w", err)
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if time.Now().After(s.deadline) {
			s.stats.Warn("scan time limit reached")
			return errScanLimit
		}
		if walkErr != nil {
			s.stats.FilesSkipped++
			s.stats.Warn("one or more log paths could not be read")
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if accept != nil && !accept(path) {
			return nil
		}
		if d.Type()&fs.ModeType != 0 {
			s.stats.FilesSkipped++
			s.stats.Warn("one or more non-regular log files were skipped")
			return nil
		}
		info, err := d.Info()
		if err != nil {
			s.stats.FilesSkipped++
			s.stats.Warn("one or more log files could not be inspected")
			return nil
		}
		if !info.Mode().IsRegular() {
			s.stats.FilesSkipped++
			s.stats.Warn("one or more non-regular log files were skipped")
			return nil
		}
		if info.ModTime().Before(since) {
			return nil
		}
		if s.stats.FilesScanned >= s.limits.MaxFiles {
			s.stats.Warn("file scan limit reached")
			return errScanLimit
		}
		if info.Size() > s.limits.MaxBytes-s.stats.BytesScanned {
			s.stats.FilesSkipped++
			s.stats.Warn("byte scan limit reached")
			return errScanLimit
		}
		s.stats.FilesScanned++
		if err := visit(path); err != nil {
			if errors.Is(err, errScanLimit) {
				return err
			}
			s.stats.FilesSkipped++
			s.stats.Warn("one or more log files could not be read completely")
		}
		return nil
	})
	if errors.Is(err, errScanLimit) {
		return nil
	}
	return err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func (s *fileScanner) eachLine(path string, fn func(line []byte)) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return errNonRegularLog
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return errNonRegularLog
	}
	reader := &countingReader{r: f}
	remaining := s.limits.MaxBytes - s.stats.BytesScanned
	if remaining <= 0 {
		s.stats.Warn("byte scan limit reached")
		return errScanLimit
	}
	sc := bufio.NewScanner(io.LimitReader(reader, remaining+1))
	initial := min(64<<10, s.limits.MaxLineBytes)
	sc.Buffer(make([]byte, initial), s.limits.MaxLineBytes)
	for sc.Scan() {
		if time.Now().After(s.deadline) {
			s.stats.BytesScanned += min(reader.n, remaining)
			s.stats.Warn("scan time limit reached")
			return errScanLimit
		}
		if s.stats.RecordsScanned >= s.limits.MaxRecords {
			s.stats.BytesScanned += reader.n
			s.stats.Warn("record scan limit reached")
			return errScanLimit
		}
		s.stats.RecordsScanned++
		fn(sc.Bytes())
	}
	s.stats.BytesScanned += reader.n
	if reader.n > remaining {
		s.stats.BytesScanned -= reader.n - remaining
		s.stats.Warn("byte scan limit reached")
		return errScanLimit
	}
	if err := sc.Err(); err != nil {
		s.stats.Warn("one or more log lines exceeded the line limit or could not be read")
		return err
	}
	return nil
}
