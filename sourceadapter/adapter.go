// Package sourceadapter defines Burnban's stable contract for read-only local
// usage-source adapters. Adapters normalize tool-specific stores into
// metadata-only usage events; pricing and reporting remain owned by Burnban.
package sourceadapter

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// APIVersion changes only when an adapter must be updated to keep compiling or
// when the meaning of an Event field changes.
const APIVersion = "burnban.source/v1"

// Event bounds keep a corrupt or hostile local store from overflowing report
// aggregation or turning nonsensical metadata into authoritative usage. They
// are intentionally far above plausible usage for one call or session while
// remaining useful to first- and third-party adapters for preflight checks.
const (
	MaxEventCalls          int64   = 1_000_000_000
	MaxEventTokens         int64   = 1_000_000_000_000_000
	MaxEventToolRequests   int64   = 1_000_000_000
	MaxEventCostUSD        float64 = 1_000_000_000_000
	MaxEventIDBytes                = 1_024
	MaxEventModelBytes             = 512
	MaxEventDimensionBytes         = 256
)

var adapterIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Confidence describes how directly an adapter recovered token usage.
type Confidence string

const (
	ConfidenceExact     Confidence = "exact"
	ConfidenceEstimated Confidence = "estimated"
	ConfidencePartial   Confidence = "partial"
)

// Privacy declares the invariants required of every Burnban source adapter.
// Validate rejects adapters that can mutate the source, use the network, or
// emit prompt/response content into Burnban's reporting pipeline.
type Privacy struct {
	ReadOnly              bool `json:"read_only"`
	NetworkAccess         bool `json:"network_access"`
	EmitsPromptOrResponse bool `json:"emits_prompt_or_response"`
}

// Manifest identifies an adapter and makes its privacy boundary inspectable.
type Manifest struct {
	APIVersion  string  `json:"api_version"`
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name"`
	Store       string  `json:"store"`
	Privacy     Privacy `json:"privacy"`
}

// Validate checks compatibility and the local, metadata-only safety contract.
func (m Manifest) Validate() error {
	if m.APIVersion != APIVersion {
		return fmt.Errorf("adapter %q uses API %q, want %q", m.ID, m.APIVersion, APIVersion)
	}
	if !adapterIDPattern.MatchString(m.ID) {
		return fmt.Errorf("adapter ID %q must match %s", m.ID, adapterIDPattern)
	}
	if m.DisplayName == "" {
		return fmt.Errorf("adapter %q requires a display name", m.ID)
	}
	if m.Store == "" {
		return fmt.Errorf("adapter %q requires a store description", m.ID)
	}
	if !m.Privacy.ReadOnly {
		return fmt.Errorf("adapter %q must be read-only", m.ID)
	}
	if m.Privacy.NetworkAccess {
		return fmt.Errorf("adapter %q must not require network access", m.ID)
	}
	if m.Privacy.EmitsPromptOrResponse {
		return fmt.Errorf("adapter %q must emit metadata-only events", m.ID)
	}
	return nil
}

// Event is one model call recovered from a local source. In contains only
// full-price input tokens: when a provider reports cached tokens as a subset of
// its input total, the adapter must subtract that subset into CacheRead.
//
// ID is a stable, source-local deduplication key. It should be populated when
// the source exposes one; Burnban deduplicates non-empty IDs per adapter.
type Event struct {
	ID              string
	Provider        string
	Model           string
	Time            time.Time
	Calls           int64
	In              int64
	Out             int64
	CacheRead       int64
	CacheWrite5m    int64
	CacheWrite1h    int64
	CostUSD         float64
	CostKnown       bool
	BillingProvider string
	Confidence      Confidence
	ServiceTier     string
	InferenceGeo    string
	ServerToolUse   ServerToolUsage
}

type ServerToolUsage struct {
	WebSearchRequests int64 `json:"web_search_requests,omitempty"`
	WebFetchRequests  int64 `json:"web_fetch_requests,omitempty"`
}

// Validate checks one normalized event before it enters pricing or report
// aggregation. It deliberately returns field-only diagnostics: source labels
// can originate in private local stores and must not be copied into errors.
func (e Event) Validate() error {
	if e.Time.IsZero() {
		return fmt.Errorf("event time is required")
	}
	if err := validateTextField("event ID", e.ID, MaxEventIDBytes, false); err != nil {
		return err
	}
	if err := validateTextField("event provider", e.Provider, MaxEventDimensionBytes, false); err != nil {
		return err
	}
	if err := validateTextField("event model", e.Model, MaxEventModelBytes, true); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"event billing provider": e.BillingProvider,
		"event service tier":     e.ServiceTier,
		"event inference geo":    e.InferenceGeo,
	} {
		if err := validateTextField(name, value, MaxEventDimensionBytes, false); err != nil {
			return err
		}
	}
	if e.Calls <= 0 || e.Calls > MaxEventCalls {
		return fmt.Errorf("event calls are outside the accepted range")
	}

	tokenCounters := []struct {
		name  string
		value int64
	}{
		{"input tokens", e.In},
		{"output tokens", e.Out},
		{"cache-read tokens", e.CacheRead},
		{"5-minute cache-write tokens", e.CacheWrite5m},
		{"1-hour cache-write tokens", e.CacheWrite1h},
	}
	var tokenTotal int64
	for _, counter := range tokenCounters {
		if counter.value < 0 || counter.value > MaxEventTokens {
			return fmt.Errorf("event %s are outside the accepted range", counter.name)
		}
		if tokenTotal > MaxEventTokens-counter.value {
			return fmt.Errorf("event token total is outside the accepted range")
		}
		tokenTotal += counter.value
	}
	for name, value := range map[string]int64{
		"web-search requests": e.ServerToolUse.WebSearchRequests,
		"web-fetch requests":  e.ServerToolUse.WebFetchRequests,
	} {
		if value < 0 || value > MaxEventToolRequests {
			return fmt.Errorf("event %s are outside the accepted range", name)
		}
	}
	if math.IsNaN(e.CostUSD) || math.IsInf(e.CostUSD, 0) || e.CostUSD < 0 || e.CostUSD > MaxEventCostUSD {
		return fmt.Errorf("event cost is outside the accepted range")
	}
	if !e.CostKnown && e.CostUSD != 0 {
		return fmt.Errorf("event cost is present without cost-known metadata")
	}
	if e.CostKnown && e.CostUSD == 0 && tokenTotal > 0 {
		return fmt.Errorf("event cost is marked known zero for nonzero usage")
	}
	switch e.Confidence {
	case ConfidenceExact, ConfidenceEstimated, ConfidencePartial:
	default:
		return fmt.Errorf("event confidence is required and must be exact, estimated, or partial")
	}
	return nil
}

func validateTextField(name, value string, maxBytes int, required bool) error {
	if required && value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds its size limit", name)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	if strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
	}) >= 0 {
		return fmt.Errorf("%s contains control characters", name)
	}
	return nil
}

// ScanLimits bound filesystem, record, and wall-clock work independently for
// each adapter.
type ScanLimits struct {
	MaxFiles     int
	MaxBytes     int64
	MaxLineBytes int
	MaxRecords   int
	MaxDuration  time.Duration
}

type ScanStats struct {
	Partial        bool     `json:"partial"`
	FilesScanned   int      `json:"files_scanned,omitempty"`
	FilesSkipped   int      `json:"files_skipped,omitempty"`
	RecordsScanned int      `json:"records_scanned,omitempty"`
	BytesScanned   int64    `json:"bytes_scanned,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

// Warn records one stable warning and marks the scan partial.
func (s *ScanStats) Warn(message string) {
	s.Partial = true
	for _, existing := range s.Warnings {
		if existing == message {
			return
		}
	}
	s.Warnings = append(s.Warnings, message)
}

type ScanResult struct {
	Sessions int
	Stats    ScanStats
}

// Adapter is the v1 source contract. DefaultPath must be deterministic and
// side-effect free. Scan must not modify path or perform network requests.
type Adapter interface {
	Manifest() Manifest
	DefaultPath(home string) string
	Scan(path string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error)
}
