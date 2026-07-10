// Package pricing maps model IDs to per-token prices so every proxied
// request can be converted to dollars at ingest time.
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed models.json
var embedded []byte

// Price is what a model costs per million tokens. Cache multipliers are
// applied to the input rate: Anthropic bills cache reads at 0.1x and cache
// writes at 1.25x; OpenAI-style cached input is 0.1x with no write charge.
type Price struct {
	InputPerMTok   float64 `json:"input_per_mtok"`
	OutputPerMTok  float64 `json:"output_per_mtok"`
	CacheReadMult  float64 `json:"cache_read_mult"`
	CacheWriteMult float64 `json:"cache_write_mult"`
}

type Table struct {
	Models map[string]Price `json:"models"`
}

// Load returns the embedded price table overlaid with entries from
// ~/.burnban/pricing.json when present, so users can correct prices the
// moment a vendor changes them without waiting for a release.
func Load() (*Table, error) {
	var t Table
	if err := json.Unmarshal(embedded, &t); err != nil {
		return nil, fmt.Errorf("embedded pricing: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return &t, nil
	}
	b, err := os.ReadFile(filepath.Join(home, ".burnban", "pricing.json"))
	if err != nil {
		return &t, nil
	}
	var overlay Table
	if err := json.Unmarshal(b, &overlay); err != nil {
		return nil, fmt.Errorf("~/.burnban/pricing.json: %w", err)
	}
	for k, v := range overlay.Models {
		t.Models[k] = v
	}
	return &t, nil
}

// Lookup finds a price by exact model ID, then by the longest table key
// that prefixes it — but only when the leftover suffix is a version or
// date tag (starts like "-20260301", ".1", "@2026"), so
// claude-opus-4-7-20260301 matches claude-opus-4-7 while a genuinely
// different tier like gemini-2.5-flash-lite does NOT silently bill at
// gemini-2.5-flash rates. Unknown models return false; burnban records
// them as unpriced rather than guessing.
func (t *Table) Lookup(model string) (Price, bool) {
	if p, ok := t.Models[model]; ok {
		return p, true
	}
	best := ""
	for k := range t.Models {
		if strings.HasPrefix(model, k) && versionSuffix(model[len(k):]) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return Price{}, false
	}
	return t.Models[best], true
}

// versionSuffix reports whether s looks like a version/date tag rather
// than a distinct model variant: a separator followed by a digit.
func versionSuffix(s string) bool {
	return len(s) >= 2 && strings.ContainsRune("-.@:", rune(s[0])) &&
		s[1] >= '0' && s[1] <= '9'
}

// Cost prices normalized usage. in and out are full-price tokens; cacheRead
// and cacheWrite are billed as multiples of the input rate.
func Cost(p Price, in, out, cacheRead, cacheWrite int64) float64 {
	return (float64(in)*p.InputPerMTok +
		float64(out)*p.OutputPerMTok +
		float64(cacheRead)*p.InputPerMTok*p.CacheReadMult +
		float64(cacheWrite)*p.InputPerMTok*p.CacheWriteMult) / 1e6
}

// Reprice costs a token bundle as if it had run on a different model. It
// differs from Cost on one point: a zero cache multiplier here means the
// target has no such cache tier, so those tokens fall back to the full
// input rate — on that provider they would have been ordinary input, not
// free. (At ingest time, Cost's zero write multiplier is correct because
// such providers never report cache-write tokens in the first place.)
// The formula itself lives only in Cost.
func Reprice(p Price, in, out, cacheRead, cacheWrite int64) float64 {
	if p.CacheReadMult <= 0 {
		p.CacheReadMult = 1
	}
	if p.CacheWriteMult <= 0 {
		p.CacheWriteMult = 1
	}
	return Cost(p, in, out, cacheRead, cacheWrite)
}
