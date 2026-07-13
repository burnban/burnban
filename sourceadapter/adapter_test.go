package sourceadapter

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestManifestValidate(t *testing.T) {
	valid := Manifest{
		APIVersion: APIVersion, ID: "fixture-source", DisplayName: "Fixture Source",
		Store:   "append-only fixture records",
		Privacy: Privacy{ReadOnly: true},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Manifest)
	}{
		{"version", func(m *Manifest) { m.APIVersion = "burnban.source/v2" }},
		{"id", func(m *Manifest) { m.ID = "Fixture Source" }},
		{"write", func(m *Manifest) { m.Privacy.ReadOnly = false }},
		{"network", func(m *Manifest) { m.Privacy.NetworkAccess = true }},
		{"content", func(m *Manifest) { m.Privacy.EmitsPromptOrResponse = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := valid
			tt.edit(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatalf("invalid manifest accepted: %+v", manifest)
			}
		})
	}
}

func TestEventValidate(t *testing.T) {
	valid := Event{
		ID: "session/request", Provider: "fixture-source", Model: "fixture-model",
		Time: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC), Calls: 1,
		In: 100, Out: 20, CacheRead: 30, Confidence: ConfidenceExact,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid event: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Event)
	}{
		{"time", func(e *Event) { e.Time = time.Time{} }},
		{"model", func(e *Event) { e.Model = "" }},
		{"model control", func(e *Event) { e.Model = "private\nlabel" }},
		{"model bound", func(e *Event) { e.Model = strings.Repeat("x", MaxEventModelBytes+1) }},
		{"calls negative", func(e *Event) { e.Calls = -1 }},
		{"calls bound", func(e *Event) { e.Calls = MaxEventCalls + 1 }},
		{"tokens negative", func(e *Event) { e.In = -1 }},
		{"tokens bound", func(e *Event) { e.In = MaxEventTokens; e.Out = 1 }},
		{"tool bound", func(e *Event) { e.ServerToolUse.WebSearchRequests = MaxEventToolRequests + 1 }},
		{"cost nan", func(e *Event) { e.CostKnown = true; e.CostUSD = math.NaN() }},
		{"cost inf", func(e *Event) { e.CostKnown = true; e.CostUSD = math.Inf(1) }},
		{"cost inconsistent", func(e *Event) { e.CostUSD = 1 }},
		{"cost false zero", func(e *Event) { e.CostKnown = true }},
		{"confidence missing", func(e *Event) { e.Confidence = "" }},
		{"confidence unknown", func(e *Event) { e.Confidence = "certain" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := valid
			tt.edit(&event)
			if err := event.Validate(); err == nil {
				t.Fatalf("invalid event accepted: %+v", event)
			}
		})
	}
}
