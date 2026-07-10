// Package pricing maps model IDs to per-token prices so every proxied
// request can be converted to dollars at ingest time.
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

//go:embed models.json
var embedded []byte

// Price is what a model costs per million tokens. Cache and long-context
// multipliers are applied to the base input/output rates.
type Price struct {
	InputPerMTok         float64 `json:"input_per_mtok"`
	OutputPerMTok        float64 `json:"output_per_mtok"`
	CacheReadMult        float64 `json:"cache_read_mult"`
	CacheWriteMult       float64 `json:"cache_write_mult"`
	LongContextThreshold int64   `json:"long_context_threshold,omitempty"`
	LongInputMult        float64 `json:"long_input_mult,omitempty"`
	LongOutputMult       float64 `json:"long_output_mult,omitempty"`
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
	if err := validateModels(t.Models); err != nil {
		return nil, fmt.Errorf("embedded pricing: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return &t, nil
	}
	b, err := os.ReadFile(filepath.Join(home, ".burnban", "pricing.json"))
	if os.IsNotExist(err) {
		return &t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("~/.burnban/pricing.json: %w", err)
	}
	var overlay Table
	if err := json.Unmarshal(b, &overlay); err != nil {
		return nil, fmt.Errorf("~/.burnban/pricing.json: %w", err)
	}
	if err := validateModels(overlay.Models); err != nil {
		return nil, fmt.Errorf("~/.burnban/pricing.json: %w", err)
	}
	for k, v := range overlay.Models {
		t.Models[k] = v
	}
	return &t, nil
}

func validateModels(models map[string]Price) error {
	for name, p := range models {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("model name must not be empty")
		}
		values := map[string]float64{
			"input_per_mtok": p.InputPerMTok, "output_per_mtok": p.OutputPerMTok,
			"cache_read_mult": p.CacheReadMult, "cache_write_mult": p.CacheWriteMult,
			"long_input_mult": p.LongInputMult, "long_output_mult": p.LongOutputMult,
		}
		for field, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return fmt.Errorf("model %q has invalid %s", name, field)
			}
		}
		if p.LongContextThreshold < 0 {
			return fmt.Errorf("model %q has invalid long_context_threshold", name)
		}
		if p.LongContextThreshold > 0 && (p.LongInputMult <= 0 || p.LongOutputMult <= 0) {
			return fmt.Errorf("model %q long-context multipliers must be positive", name)
		}
	}
	return nil
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
	in, out = max(in, 0), max(out, 0)
	cacheRead, cacheWrite = max(cacheRead, 0), max(cacheWrite, 0)
	inputMult, outputMult := 1.0, 1.0
	totalInput := float64(in) + float64(cacheRead) + float64(cacheWrite)
	if p.LongContextThreshold > 0 && totalInput > float64(p.LongContextThreshold) {
		if p.LongInputMult > 0 {
			inputMult = p.LongInputMult
		}
		if p.LongOutputMult > 0 {
			outputMult = p.LongOutputMult
		}
	}
	return baseCost(p, in, out, cacheRead, cacheWrite, inputMult, outputMult)
}

func baseCost(p Price, in, out, cacheRead, cacheWrite int64, inputMult, outputMult float64) float64 {
	return (float64(in)*p.InputPerMTok*inputMult +
		float64(out)*p.OutputPerMTok*outputMult +
		float64(cacheRead)*p.InputPerMTok*p.CacheReadMult*inputMult +
		float64(cacheWrite)*p.InputPerMTok*p.CacheWriteMult*inputMult) / 1e6
}

// Reprice costs a token bundle as if it had run on a different model. It
// differs from Cost on one point: a zero cache multiplier here means the
// target has no such cache tier, so those tokens fall back to the full
// input rate — on that provider they would have been ordinary input, not
// free. (At ingest time, Cost's zero write multiplier is correct because
// such providers never report cache-write tokens in the first place.)
// Aggregate bundles cannot reveal which individual requests crossed a
// long-context threshold, so Reprice intentionally uses base rates.
func Reprice(p Price, in, out, cacheRead, cacheWrite int64) float64 {
	in, out = max(in, 0), max(out, 0)
	cacheRead, cacheWrite = max(cacheRead, 0), max(cacheWrite, 0)
	if p.CacheReadMult <= 0 {
		p.CacheReadMult = 1
	}
	if p.CacheWriteMult <= 0 {
		p.CacheWriteMult = 1
	}
	return baseCost(p, in, out, cacheRead, cacheWrite, 1, 1)
}
