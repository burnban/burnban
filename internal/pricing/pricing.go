// Package pricing maps model IDs to per-token prices so every proxied
// request can be converted to dollars at ingest time.
package pricing

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	// Free must be explicit. A missing or misspelled paid rate must never turn
	// into a plausible-looking $0 model through Go's numeric zero values.
	Free          bool   `json:"free,omitempty"`
	Source        string `json:"source,omitempty"`
	EffectiveFrom string `json:"effective_from,omitempty"`
	ValidThrough  string `json:"valid_through,omitempty"`
	VerifiedDate  string `json:"verified_date,omitempty"`
}

type Source struct {
	Provider     string `json:"provider"`
	URL          string `json:"url"`
	VerifiedDate string `json:"verified_date"`
}

type Metadata struct {
	Version       string   `json:"version"`
	EffectiveDate string   `json:"effective_date"`
	VerifiedDate  string   `json:"verified_date"`
	Sources       []Source `json:"sources"`
}

type Table struct {
	Metadata Metadata         `json:"metadata,omitempty"`
	Models   map[string]Price `json:"models"`

	overrideModels []string
}

type Diagnostics struct {
	Version        string   `json:"version"`
	EffectiveDate  string   `json:"effective_date"`
	VerifiedDate   string   `json:"verified_date"`
	ModelCount     int      `json:"model_count"`
	Sources        []Source `json:"sources"`
	OverrideFile   string   `json:"override_file,omitempty"`
	OverrideModels []string `json:"override_models,omitempty"`
	ExpiredModels  []string `json:"expired_models,omitempty"`
}

// Load returns the embedded price table overlaid with entries from
// ~/.burnban/pricing.json when present, so users can correct prices the
// moment a vendor changes them without waiting for a release.
func Load() (*Table, error) {
	var t Table
	if err := decodeStrict(embedded, &t); err != nil {
		return nil, fmt.Errorf("embedded pricing: %w", err)
	}
	if err := validateMetadata(t.Metadata, true); err != nil {
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
	if err := decodeStrict(b, &overlay); err != nil {
		return nil, fmt.Errorf("~/.burnban/pricing.json: %w", err)
	}
	if len(overlay.Models) == 0 {
		return nil, fmt.Errorf("~/.burnban/pricing.json: models must contain at least one override")
	}
	if err := validateMetadata(overlay.Metadata, false); err != nil {
		return nil, fmt.Errorf("~/.burnban/pricing.json: %w", err)
	}
	if err := validateModels(overlay.Models); err != nil {
		return nil, fmt.Errorf("~/.burnban/pricing.json: %w", err)
	}
	for k, v := range overlay.Models {
		t.Models[k] = v
		t.overrideModels = append(t.overrideModels, k)
	}
	sort.Strings(t.overrideModels)
	return &t, nil
}

func decodeStrict(data []byte, dst any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return scanJSONValue(dec)
}

func scanJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key must be a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func validateMetadata(m Metadata, required bool) error {
	empty := m.Version == "" && m.EffectiveDate == "" && m.VerifiedDate == "" && len(m.Sources) == 0
	if empty && !required {
		return nil
	}
	if m.Version == "" || m.EffectiveDate == "" || m.VerifiedDate == "" || len(m.Sources) == 0 {
		return fmt.Errorf("metadata requires version, effective_date, verified_date, and sources")
	}
	if err := validateDate("metadata effective_date", m.EffectiveDate); err != nil {
		return err
	}
	if err := validateDate("metadata verified_date", m.VerifiedDate); err != nil {
		return err
	}
	if m.VerifiedDate < m.EffectiveDate {
		return fmt.Errorf("metadata verified_date must not precede effective_date")
	}
	for i, source := range m.Sources {
		if strings.TrimSpace(source.Provider) == "" {
			return fmt.Errorf("metadata source %d requires provider", i)
		}
		if err := validateSourceURL(source.URL); err != nil {
			return fmt.Errorf("metadata source %q: %w", source.Provider, err)
		}
		if err := validateDate("metadata source verified_date", source.VerifiedDate); err != nil {
			return err
		}
	}
	return nil
}

func validateModels(models map[string]Price) error {
	if len(models) == 0 {
		return fmt.Errorf("models must not be empty")
	}
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
		if p.Free {
			if p.InputPerMTok != 0 || p.OutputPerMTok != 0 || p.CacheReadMult != 0 || p.CacheWriteMult != 0 ||
				p.LongContextThreshold != 0 || p.LongInputMult != 0 || p.LongOutputMult != 0 {
				return fmt.Errorf("model %q marked free must have zero rates and no paid tiers", name)
			}
		} else if p.InputPerMTok <= 0 || p.OutputPerMTok <= 0 {
			return fmt.Errorf("model %q requires nonzero input_per_mtok and output_per_mtok (use free: true for a free model)", name)
		}
		if p.LongContextThreshold < 0 {
			return fmt.Errorf("model %q has invalid long_context_threshold", name)
		}
		if p.LongContextThreshold > 0 && (p.LongInputMult <= 0 || p.LongOutputMult <= 0) {
			return fmt.Errorf("model %q long-context multipliers must be positive", name)
		}
		if p.Source != "" {
			if err := validateSourceURL(p.Source); err != nil {
				return fmt.Errorf("model %q source: %w", name, err)
			}
		}
		for field, value := range map[string]string{
			"effective_from": p.EffectiveFrom, "valid_through": p.ValidThrough, "verified_date": p.VerifiedDate,
		} {
			if value != "" {
				if err := validateDate("model "+name+" "+field, value); err != nil {
					return err
				}
			}
		}
		hasProvenance := p.Source != "" || p.EffectiveFrom != "" || p.ValidThrough != "" || p.VerifiedDate != ""
		if hasProvenance && (p.Source == "" || p.EffectiveFrom == "" || p.VerifiedDate == "") {
			return fmt.Errorf("model %q provenance requires source, effective_from, and verified_date", name)
		}
		if p.VerifiedDate != "" && p.VerifiedDate < p.EffectiveFrom {
			return fmt.Errorf("model %q verified_date must not precede effective_from", name)
		}
		if p.ValidThrough != "" && p.ValidThrough < p.EffectiveFrom {
			return fmt.Errorf("model %q valid_through must not precede effective_from", name)
		}
	}
	return nil
}

func validateDate(field, value string) error {
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return fmt.Errorf("%s must be YYYY-MM-DD", field)
	}
	return nil
}

func validateSourceURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("must be an http or https URL with a host")
	}
	return nil
}

// Diagnostics describes the price snapshot and any user overrides without
// exposing the user's absolute home directory. Expired entries remain visible
// so callers can fail or warn according to their own risk tolerance.
func (t *Table) Diagnostics() Diagnostics {
	d := Diagnostics{
		Version: t.Metadata.Version, EffectiveDate: t.Metadata.EffectiveDate,
		VerifiedDate: t.Metadata.VerifiedDate, ModelCount: len(t.Models),
		Sources:        append([]Source(nil), t.Metadata.Sources...),
		OverrideModels: append([]string(nil), t.overrideModels...),
	}
	if len(t.overrideModels) > 0 {
		d.OverrideFile = "~/.burnban/pricing.json"
	}
	today := time.Now().Format("2006-01-02")
	for name, p := range t.Models {
		if p.ValidThrough != "" && p.ValidThrough < today {
			d.ExpiredModels = append(d.ExpiredModels, name)
		}
	}
	sort.Strings(d.ExpiredModels)
	return d
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

// RepriceRequest prices one request on a target model. Unlike Reprice's
// aggregate-bundle contract, this has enough information to apply the target's
// per-request long-context threshold accurately. Unsupported cache tiers fall
// back to ordinary full-price input tokens.
func RepriceRequest(p Price, in, out, cacheRead, cacheWrite int64) float64 {
	if p.CacheReadMult <= 0 {
		p.CacheReadMult = 1
	}
	if p.CacheWriteMult <= 0 {
		p.CacheWriteMult = 1
	}
	return Cost(p, in, out, cacheRead, cacheWrite)
}
