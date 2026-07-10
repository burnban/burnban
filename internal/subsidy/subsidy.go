// Package subsidy reads the session logs that Claude Code and Codex already
// keep on disk and totals what that traffic would have cost at API prices.
// Nothing is proxied and nothing is billed — flat-rate subscribers can't
// meter spend that doesn't exist, but they can see what their plan is worth.
package subsidy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/syft8/burnban/internal/pricing"
)

// Event is one model call recovered from a local log, normalized the same
// way the proxy's meter normalizes live traffic: In and Out are full-price
// tokens (OpenAI's cached subset already subtracted), CacheRead was billed
// at the provider's cached-input discount.
type Event struct {
	Provider  string // "claude-code" or "codex"
	Model     string
	Time      time.Time
	In        int64
	Out       int64
	CacheRead int64
	// Anthropic bills 5-minute cache writes at 1.25x input and 1-hour
	// writes at 2x. The proxy never sees the split (the API reports one
	// total), but Claude Code's logs carry it, so subsidy can price it.
	CacheWrite5m int64
	CacheWrite1h int64
}

// write1hMult is Anthropic's 1-hour cache-write premium. The pricing table
// carries a single write multiplier (the 5-minute rate); the 1h tier only
// surfaces in local logs, so the constant lives here rather than in the table.
const write1hMult = 2.0

// Cost prices an event bundle at API rates, honoring the 1h write tier.
func Cost(p pricing.Price, in, out, cacheRead, w5m, w1h int64) float64 {
	return pricing.Cost(p, in, out, cacheRead, w5m) +
		float64(w1h)*p.InputPerMTok*write1hMult/1e6
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
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// ScanClaude walks Claude Code's session logs and emits one Event per
// assistant message in the window. A message with several content blocks is
// written once per block with identical usage, and resumed sessions can copy
// history into new files, so events dedup on message ID + request ID across
// the whole tree. Returns the number of session files that contributed.
func ScanClaude(dir string, since time.Time, emit func(Event)) (int, error) {
	seen := make(map[string]struct{})
	sessions := 0
	err := walkJSONL(dir, since, func(path string) error {
		contributed := false
		ferr := eachLine(path, func(line []byte) {
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
			w1h := v.Message.Usage.CacheDetail.Ephemeral1h
			w5m := u.CacheCreation - w1h
			if w5m < 0 {
				w5m, w1h = u.CacheCreation, 0
			}
			contributed = true
			emit(Event{
				Provider: "claude-code", Model: v.Message.Model, Time: ts,
				In: u.InputTokens, Out: u.OutputTokens, CacheRead: u.CacheRead,
				CacheWrite5m: w5m, CacheWrite1h: w1h,
			})
		})
		if contributed {
			sessions++
		}
		return ferr
	})
	return sessions, err
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
func ScanCodex(dir string, since time.Time, emit func(Event)) (int, error) {
	sessions := 0
	err := walkJSONL(dir, since, func(path string) error {
		model := "unknown"
		var prev codexTotals
		contributed := false
		ferr := eachLine(path, func(line []byte) {
			if !bytes.Contains(line, []byte(`"turn_context"`)) && !bytes.Contains(line, []byte(`"token_count"`)) {
				return
			}
			var v codexLine
			if json.Unmarshal(line, &v) != nil {
				return
			}
			switch v.Type {
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
				in := d.Input - d.Cached // OpenAI input counts include the cached subset
				if in < 0 {
					in = 0
				}
				contributed = true
				emit(Event{
					Provider: "codex", Model: model, Time: ts,
					In: in, Out: d.Output, CacheRead: d.Cached,
				})
			}
		})
		if contributed {
			sessions++
		}
		return ferr
	})
	return sessions, err
}

// walkJSONL visits every .jsonl under dir whose mtime is at or after since —
// session logs are append-only, so an older mtime means nothing in-window.
// A missing dir is not an error: the tool just isn't installed here.
func walkJSONL(dir string, since time.Time, visit func(path string) error) error {
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.ModTime().Before(since) {
			return nil
		}
		return visit(path)
	})
}

// eachLine streams a file line by line. Claude Code lines can carry whole
// base64 images, so the scanner buffer allows lines up to 512MB. Unreadable
// files are skipped rather than failing the whole scan.
func eachLine(path string, fn func(line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 512<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	return nil
}
