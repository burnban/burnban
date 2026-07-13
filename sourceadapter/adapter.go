// Package sourceadapter defines Burnban's stable contract for read-only local
// usage-source adapters. Adapters normalize tool-specific stores into
// metadata-only usage events; pricing and reporting remain owned by Burnban.
package sourceadapter

import (
	"fmt"
	"regexp"
	"time"
)

// APIVersion changes only when an adapter must be updated to keep compiling or
// when the meaning of an Event field changes.
const APIVersion = "burnban.source/v1"

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
