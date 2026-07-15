// Package localusage reads supported local usage stores, then totals what those
// tokens would cost at API prices. Nothing is proxied and source logs are never
// modified.
package localusage

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
	"sort"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/sourceadapter"
)

// Event is one model call recovered from a local log, normalized the same
// way the proxy's meter normalizes live traffic: In and Out are full-price
// tokens (OpenAI's cached subset already subtracted), CacheRead was billed
// at the provider's cached-input discount.
// Type aliases keep adapter events aligned with the public contract used by
// first- and third-party source implementations.
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
// Codex forks begin with a replay of their parent's history. Those replayed
// token_count records retain the parent's cumulative counter and must advance
// the child's baseline, but must not be emitted again. Burnban matches each
// fork against the pre-fork suffix of the canonical rollout named by its
// lineage metadata and emits only records after the inherited usage prefix.
func ScanCodex(dir string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanCodex(dir, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

type codexUsageRecord struct {
	Time  time.Time
	Model string
	Total codexTotals
}

type codexRollout struct {
	SessionID     string
	ParentID      string
	StartedAt     time.Time
	Fork          bool
	MetadataValid bool
	Complete      bool
	Usage         []codexUsageRecord
}

func readCodexRollout(scanner *fileScanner, path string) (codexRollout, error) {
	// Historical ordinary rollouts can omit session_meta entirely. They remain
	// valid independent usage; a present-but-malformed first metadata record is
	// different because it could conceal fork lineage and must fail closed.
	rollout := codexRollout{MetadataValid: true}
	model := "unknown"
	sessionMetaSeen := false
	firstPhysicalLine := true
	legacyCandidate := false
	err := scanner.eachLine(path, func(line []byte) {
		isFirst := firstPhysicalLine
		firstPhysicalLine = false
		relevant := isFirst || bytes.Contains(line, []byte(`"session_meta"`)) ||
			bytes.Contains(line, []byte(`"turn_context"`)) || bytes.Contains(line, []byte(`"token_count"`))
		if !relevant {
			// A torn line can end before its type marker. Validate every physical
			// JSONL record so an incomplete parent projection never looks exact.
			if !json.Valid(line) {
				rollout.MetadataValid = false
			}
			return
		}
		var v codexLine
		if json.Unmarshal(line, &v) != nil {
			// Malformed relevant metadata cannot safely establish lineage or a
			// cumulative baseline. A bad leading session_meta must also prevent a
			// copied parent record later in the replay becoming this file's identity.
			rollout.MetadataValid = false
			if !sessionMetaSeen && (isFirst || bytes.Contains(line, []byte(`"session_meta"`))) {
				sessionMetaSeen = true
			}
			return
		}
		if isFirst && v.Type != "session_meta" {
			legacyCandidate = true
		}
		switch v.Type {
		case "session_meta":
			// A fork replay can contain its parent's session_meta. Only the first
			// record identifies the rollout stored in this file.
			if sessionMetaSeen {
				return
			}
			sessionMetaSeen = true
			rollout.MetadataValid = false
			if legacyCandidate {
				// A legacy rollout is valid only if no later record tries to
				// retroactively assign it a session identity.
				return
			}
			var meta struct {
				ID           string          `json:"id"`
				ForkedFromID json.RawMessage `json:"forked_from_id"`
			}
			if json.Unmarshal(v.Payload, &meta) != nil || meta.ID == "" {
				return
			}
			startedAt, timeErr := time.Parse(time.RFC3339, v.Timestamp)
			if timeErr != nil {
				return
			}
			var forkedFromID string
			if len(meta.ForkedFromID) > 0 && string(meta.ForkedFromID) != "null" {
				if json.Unmarshal(meta.ForkedFromID, &forkedFromID) != nil {
					return
				}
			}
			rollout.SessionID = meta.ID
			rollout.StartedAt = startedAt
			rollout.MetadataValid = true
			if forkedFromID != "" {
				rollout.Fork = true
				rollout.ParentID = forkedFromID
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
			if json.Unmarshal(v.Payload, &ev) != nil {
				rollout.MetadataValid = false
				return
			}
			if ev.Type != "token_count" || ev.Info == nil {
				return
			}
			ts, timeErr := time.Parse(time.RFC3339, v.Timestamp)
			if timeErr != nil {
				rollout.MetadataValid = false
				return
			}
			record := codexUsageRecord{Time: ts, Model: model, Total: ev.Info.Total}
			// Codex can serialize the same cumulative counter more than once.
			// These records carry no usage, and keeping an asymmetric duplicate in
			// only one side of a fork would make an inherited suffix look live.
			if len(rollout.Usage) == 0 || rollout.Usage[len(rollout.Usage)-1].Total != record.Total {
				rollout.Usage = append(rollout.Usage, record)
			}
		}
	})
	rollout.Complete = err == nil
	return rollout, err
}

func codexUsagePrefixAtParentEnd(child, parent []codexUsageRecord, deadline time.Time) (int, bool) {
	if len(child) == 0 || len(parent) == 0 {
		return 0, !time.Now().After(deadline)
	}
	// A compacted child can omit leading parent counters. KMP finds the longest
	// prefix of the child that is also a suffix of the parent's pre-fork usage in
	// O(child+parent) time and O(child) bounded metadata. Requiring the match to
	// reach the parent's fork-time end prevents a truncated middle match from
	// making copied counters look live.
	if time.Now().After(deadline) {
		return 0, false
	}
	fallback := make([]int, len(child))
	for i, matched := 1, 0; i < len(child); i++ {
		if i&1023 == 0 && time.Now().After(deadline) {
			return 0, false
		}
		for matched > 0 && child[i].Total != child[matched].Total {
			matched = fallback[matched-1]
		}
		if child[i].Total == child[matched].Total {
			matched++
		}
		fallback[i] = matched
	}
	matched := 0
	for i := range parent {
		if i&1023 == 0 && time.Now().After(deadline) {
			return 0, false
		}
		for matched > 0 && parent[i].Total != child[matched].Total {
			matched = fallback[matched-1]
		}
		if parent[i].Total == child[matched].Total {
			matched++
			if matched == len(child) {
				if i == len(parent)-1 {
					return matched, true
				}
				matched = fallback[matched-1]
			}
		}
	}
	return matched, true
}

func canonicalCodexRollouts(rollouts []codexRollout) map[string]*codexRollout {
	canonical := make(map[string]*codexRollout, len(rollouts))
	for i := range rollouts {
		candidate := &rollouts[i]
		if candidate.SessionID == "" || !candidate.MetadataValid {
			continue
		}
		current := canonical[candidate.SessionID]
		if current == nil || (!current.Complete && candidate.Complete) ||
			(current.Complete == candidate.Complete && len(candidate.Usage) > len(current.Usage)) {
			canonical[candidate.SessionID] = candidate
		}
	}
	return canonical
}

func codexUsageStartsWith(usage, prefix []codexUsageRecord) bool {
	if len(prefix) > len(usage) {
		return false
	}
	for i := range prefix {
		if usage[i].Total != prefix[i].Total || usage[i].Model != prefix[i].Model ||
			!usage[i].Time.Equal(prefix[i].Time) {
			return false
		}
	}
	return true
}

func ambiguousCodexSessionIDs(rollouts []codexRollout, canonical map[string]*codexRollout) map[string]struct{} {
	ambiguous := make(map[string]struct{})
	for i := range rollouts {
		candidate := &rollouts[i]
		selected := canonical[candidate.SessionID]
		if candidate.SessionID == "" || candidate == selected || !candidate.MetadataValid || !candidate.Complete ||
			selected == nil || !selected.MetadataValid || !selected.Complete {
			continue
		}
		if candidate.Fork != selected.Fork || candidate.ParentID != selected.ParentID ||
			!candidate.StartedAt.Equal(selected.StartedAt) ||
			!codexUsageStartsWith(selected.Usage, candidate.Usage) {
			ambiguous[candidate.SessionID] = struct{}{}
		}
	}
	return ambiguous
}

func invalidCodexLineageIDs(canonical map[string]*codexRollout) map[string]struct{} {
	state := make(map[string]uint8, len(canonical))
	invalid := make(map[string]struct{})
	var visit func(string) bool
	visit = func(sessionID string) bool {
		switch state[sessionID] {
		case 1:
			invalid[sessionID] = struct{}{}
			return true
		case 2:
			_, bad := invalid[sessionID]
			return bad
		}
		state[sessionID] = 1
		rollout := canonical[sessionID]
		bad := false
		if rollout != nil && rollout.Fork {
			if _, parentPresent := canonical[rollout.ParentID]; parentPresent {
				bad = visit(rollout.ParentID)
			}
		}
		state[sessionID] = 2
		if bad {
			invalid[sessionID] = struct{}{}
		}
		return bad
	}
	for sessionID := range canonical {
		visit(sessionID)
	}
	return invalid
}

func missingCodexParentIDs(rollouts []codexRollout) map[string]struct{} {
	canonical := canonicalCodexRollouts(rollouts)
	missing := make(map[string]struct{})
	for i := range rollouts {
		rollout := &rollouts[i]
		if rollout.Fork && rollout.ParentID != "" && canonical[rollout.ParentID] == nil {
			missing[rollout.ParentID] = struct{}{}
		}
	}
	return missing
}

func codexRolloutFilenameMatches(path string, sessionIDs map[string]struct{}, deadline time.Time) bool {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	checked := 0
	for sessionID := range sessionIDs {
		if checked&255 == 0 && time.Now().After(deadline) {
			return false
		}
		checked++
		// Codex's rollout filename ends in "-<session UUID>". Enforcing that
		// boundary avoids broad substring matches from untrusted lineage data.
		start := len(name) - len(sessionID)
		if sessionID != "" && start >= 0 && strings.HasSuffix(name, sessionID) &&
			(start == 0 || name[start-1] == '-') {
			return true
		}
	}
	return false
}

func scanCodex(dir string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error) {
	scanner := newFileScanner(limits)
	// File, byte, record, and duration limits enforced by fileScanner also bound
	// this metadata-only in-memory projection. No conversation content is kept.
	rollouts := make([]codexRollout, 0)
	seenPaths := make(map[string]struct{})
	readRollout := func(path string) error {
		if _, seen := seenPaths[path]; seen {
			return nil
		}
		seenPaths[path] = struct{}{}
		rollout, readErr := readCodexRollout(scanner, path)
		rollouts = append(rollouts, rollout)
		return readErr
	}
	err := scanner.walkJSONL(dir, since, readRollout)

	// A fork modified inside the report window can point to a parent rollout
	// whose file was last modified before it. Resolve only those exact session
	// IDs through Codex's standard UUID-bearing filenames, recursively for
	// nested forks, while sharing the same resource limits and deadline.
	for err == nil {
		missing := missingCodexParentIDs(rollouts)
		if len(missing) == 0 {
			break
		}
		before := len(rollouts)
		resolveErr := scanner.walkJSONLMatching(dir, time.Time{}, func(path string) bool {
			if _, seen := seenPaths[path]; seen {
				return false
			}
			return codexRolloutFilenameMatches(path, missing, scanner.deadline)
		}, readRollout)
		if resolveErr != nil {
			err = resolveErr
			break
		}
		if len(rollouts) == before {
			break
		}
	}

	canonical := canonicalCodexRollouts(rollouts)
	ambiguous := ambiguousCodexSessionIDs(rollouts, canonical)
	invalidLineage := invalidCodexLineageIDs(canonical)
	sessions := 0
rolloutLoop:
	for i := range rollouts {
		if time.Now().After(scanner.deadline) {
			scanner.stats.Warn("scan time limit reached")
			break
		}
		rollout := &rollouts[i]
		if len(rollout.Usage) > 0 && !rollout.MetadataValid {
			scanner.stats.Warn("one or more Codex rollout metadata records were invalid; rollout usage was not counted")
			continue
		}
		if _, bad := ambiguous[rollout.SessionID]; bad {
			scanner.stats.Warn("one or more duplicate Codex rollout IDs had conflicting histories; rollout usage was not counted")
			continue
		}
		if _, bad := invalidLineage[rollout.SessionID]; bad {
			scanner.stats.Warn("one or more Codex fork lineages contained a cycle; fork usage was not counted")
			continue
		}
		if rollout.SessionID != "" && canonical[rollout.SessionID] != rollout {
			// Resumed/duplicated files for one session are represented by the
			// single longest complete canonical projection.
			continue
		}
		skip := 0
		if rollout.Fork {
			parent := canonical[rollout.ParentID]
			_, parentAmbiguous := ambiguous[rollout.ParentID]
			_, parentLineageInvalid := invalidLineage[rollout.ParentID]
			if rollout.ParentID == "" || parent == nil || !parent.MetadataValid || !parent.Complete ||
				parentAmbiguous || parentLineageInvalid {
				scanner.stats.Warn("one or more Codex fork parents were unavailable; fork usage was not counted")
				continue
			}
			if rollout.StartedAt.IsZero() {
				scanner.stats.Warn("one or more Codex fork histories did not match their parent; fork usage was not counted")
				continue
			}
			parentUsage := parent.Usage
			for recordIndex, record := range parentUsage {
				if recordIndex&1023 == 0 && time.Now().After(scanner.deadline) {
					scanner.stats.Warn("scan time limit reached")
					break rolloutLoop
				}
				if record.Time.IsZero() || !record.Time.Before(rollout.StartedAt) {
					parentUsage = parentUsage[:recordIndex]
					break
				}
			}
			if len(rollout.Usage) > 0 && len(parent.Usage) == 0 {
				scanner.stats.Warn("one or more Codex fork histories did not match their parent; fork usage was not counted")
				continue
			}
			var matched bool
			skip, matched = codexUsagePrefixAtParentEnd(rollout.Usage, parentUsage, scanner.deadline)
			if !matched {
				scanner.stats.Warn("scan time limit reached")
				break
			}
			if len(rollout.Usage) > 0 && len(parentUsage) > 0 && skip == 0 {
				scanner.stats.Warn("one or more Codex fork histories did not match their parent; fork usage was not counted")
				continue
			}
		}

		var prev codexTotals
		contributed := false
		for recordIndex, record := range rollout.Usage {
			if recordIndex&1023 == 0 && time.Now().After(scanner.deadline) {
				scanner.stats.Warn("scan time limit reached")
				break rolloutLoop
			}
			cur := record.Total
			d := codexTotals{cur.Input - prev.Input, cur.Cached - prev.Cached, cur.Output - prev.Output}
			if d.Input < 0 || d.Cached < 0 || d.Output < 0 {
				d = cur // counter reset (process restart): fresh baseline
			}
			prev = cur
			if recordIndex < skip || d.Input+d.Output == 0 || record.Time.IsZero() || record.Time.Before(since) {
				continue
			}
			in := d.Input - d.Cached // OpenAI input counts include the cached subset
			if in < 0 {
				in = 0
			}
			contributed = true
			emit(Event{
				Provider: "codex", Model: record.Model, Time: record.Time, Calls: 1,
				In: in, Out: d.Output, CacheRead: d.Cached,
				Confidence: sourceadapter.ConfidenceExact,
			})
		}
		if contributed {
			sessions++
		}
	}
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
	// Collect candidates first, then visit newest-first. When a byte, file,
	// or time budget runs out mid-scan the dropped files are then the oldest
	// ones, so a partial report understates history rather than today —
	// several sources (Codex especially) lay files out so a lexical walk is
	// chronological and a mid-walk abort would drop the newest days instead.
	type logFile struct {
		path string
		size int64
		mod  time.Time
	}
	var files []logFile
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
		files = append(files, logFile{path: path, size: info.Size(), mod: info.ModTime()})
		return nil
	})
	if err != nil && !errors.Is(err, errScanLimit) {
		return err
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}
		return files[i].path > files[j].path
	})
	for _, f := range files {
		if time.Now().After(s.deadline) {
			s.stats.Warn("scan time limit reached")
			return nil
		}
		if s.stats.FilesScanned >= s.limits.MaxFiles {
			s.stats.Warn("file scan limit reached")
			return nil
		}
		if f.size > s.limits.MaxBytes-s.stats.BytesScanned {
			s.stats.FilesSkipped++
			s.stats.Warn("byte scan limit reached")
			return nil
		}
		s.stats.FilesScanned++
		if err := visit(f.path); err != nil {
			if errors.Is(err, errScanLimit) {
				return nil
			}
			s.stats.FilesSkipped++
			s.stats.Warn("one or more log files could not be read completely")
		}
	}
	return nil
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
