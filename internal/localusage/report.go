package localusage

import (
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/sourceadapter"
)

// Totals is the common token and API-equivalent cost shape used by the CLI
// and dashboard. Cache writes retain their two Anthropic billing tiers while
// also exposing a combined value in JSON for simple clients.
type Totals struct {
	Calls         int64            `json:"calls"`
	In            int64            `json:"in_tokens"`
	Out           int64            `json:"out_tokens"`
	CacheRead     int64            `json:"cache_read_tokens"`
	CacheWrite5m  int64            `json:"cache_write_5m_tokens"`
	CacheWrite1h  int64            `json:"cache_write_1h_tokens"`
	CacheWrite    int64            `json:"cache_write_tokens"`
	APIUSD        float64          `json:"api_usd"`
	ServerToolUSD float64          `json:"server_tool_usd"`
	ServiceTiers  map[string]int64 `json:"service_tiers,omitempty"`
	InferenceGeos map[string]int64 `json:"inference_geos,omitempty"`
	ServerToolUse ServerToolUsage  `json:"server_tool_use,omitempty"`
}

type ModelUsage struct {
	Model         string `json:"model"`
	Priced        bool   `json:"priced"`
	PricingSource string `json:"pricing_source"`
	Totals
}

type DayUsage struct {
	Day string `json:"day"`
	Totals
}

type ProviderUsage struct {
	Provider       string                `json:"provider"`
	AdapterVersion string                `json:"adapter_version"`
	Privacy        sourceadapter.Privacy `json:"privacy"`
	Dir            string                `json:"-"`
	Detected       bool                  `json:"detected"`
	// Metered is true when all priced usage from this source is billed per
	// token. MixedBilling is true when the source contains both subscription
	// and metered events. The per-event buckets always sum to APIUSD.
	Metered         bool    `json:"metered"`
	MixedBilling    bool    `json:"mixed_billing,omitempty"`
	SubscriptionUSD float64 `json:"subscription_usd"`
	MeteredUSD      float64 `json:"metered_usd"`
	// BillingProvider names the pay-per-token provider when the source records
	// it (e.g. "openrouter"), or "multiple" when events name several providers.
	BillingProvider string       `json:"billing_provider,omitempty"`
	Error           string       `json:"error,omitempty"`
	Detail          string       `json:"-"`
	Sessions        int          `json:"sessions"`
	Partial         bool         `json:"partial"`
	Warnings        []string     `json:"warnings,omitempty"`
	SkippedFiles    int          `json:"skipped_files,omitempty"`
	Scan            ScanStats    `json:"-"`
	Models          []ModelUsage `json:"models"`
	Days            []DayUsage   `json:"days"`
	Totals
}

type Report struct {
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	HasUsage bool      `json:"has_usage"`
	Partial  bool      `json:"partial"`
	// SubscriptionUSD is the API-equivalent value of flat-rate subscription
	// usage used for the plan comparison. MeteredUSD is real pay-per-token
	// spend already billed. They sum to Totals.APIUSD.
	SubscriptionUSD float64         `json:"subscription_usd"`
	MeteredUSD      float64         `json:"metered_usd"`
	UnpricedCalls   int64           `json:"unpriced_calls"`
	UnpricedTokens  int64           `json:"unpriced_tokens"`
	UnpricedModels  []string        `json:"unpriced_models"`
	Providers       []ProviderUsage `json:"providers"`
	Totals
}

// PlanEquivalentUSD returns the API-equivalent value used for the plan
// comparison: flat-rate subscription usage only. Reports built before billing
// classification (both buckets zero) fall back to the full total for backward
// compatibility.
func (r Report) PlanEquivalentUSD() float64 {
	if r.SubscriptionUSD > 0 || r.MeteredUSD > 0 {
		return r.SubscriptionUSD
	}
	return r.APIUSD
}

type ReportOptions struct {
	Since       time.Time
	Until       time.Time
	ClaudeDir   string
	CodexDir    string
	GeminiDir   string
	CopilotDir  string
	CursorDB    string
	OpenCodeDB  string
	HermesDB    string
	OpenClawDir string
	GooseDB     string
	ScanLimits  ScanLimits
	// AdditionalAdapters are compiled-in extensions of the v1 adapter
	// contract. SourcePaths can override the path for any built-in or added
	// adapter by manifest ID.
	AdditionalAdapters []sourceadapter.Adapter
	SourcePaths        map[string]string
	// MeteredProviders forces named sources (e.g. "claude-code", "codex") into
	// the real-spend bucket, for API-key auth or Max-20x overage that the local
	// logs cannot reveal on their own.
	MeteredProviders []string
}

// BuildReport auto-detects every registered local adapter and prices its events
// in the requested window. It performs no network calls and never modifies the
// source stores.
func BuildReport(prices *pricing.Table, opts ReportOptions) (Report, error) {
	if opts.Until.IsZero() {
		opts.Until = time.Now()
	}
	if opts.Since.IsZero() {
		opts.Since = opts.Until.Add(-30 * 24 * time.Hour)
	}
	if opts.Since.After(opts.Until) {
		return Report{}, fmt.Errorf("report start must not be after report end")
	}
	home, _ := os.UserHomeDir()
	pathOverrides := map[string]string{
		"claude-code":        opts.ClaudeDir,
		"codex":              opts.CodexDir,
		"gemini-cli":         opts.GeminiDir,
		"github-copilot-cli": opts.CopilotDir,
		"cursor":             opts.CursorDB,
		"opencode":           opts.OpenCodeDB,
		"hermes":             opts.HermesDB,
		"openclaw":           opts.OpenClawDir,
		"goose":              opts.GooseDB,
	}
	for id, path := range opts.SourcePaths {
		pathOverrides[id] = path
	}

	report := Report{
		Since: opts.Since, Until: opts.Until, Providers: []ProviderUsage{}, UnpricedModels: []string{},
	}
	unpricedModels := map[string]struct{}{}
	type priced struct {
		price pricing.Price
		ok    bool
	}
	lookup := map[string]priced{}
	priceFor := func(model string) (pricing.Price, bool) {
		if cached, ok := lookup[model]; ok {
			return cached.price, cached.ok
		}
		price, ok := prices.Lookup(model)
		if !ok {
			// Hermes/OpenClaw/Goose commonly qualify models as provider/model (and
			// OpenRouter may use provider/vendor/model). Burnban prices the
			// terminal model ID while preserving the full source label in UI.
			if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
				price, ok = prices.Lookup(model[i+1:])
			}
		}
		lookup[model] = priced{price: price, ok: ok}
		return price, ok
	}

	type source struct {
		manifest sourceadapter.Manifest
		dir      string
		adapter  sourceadapter.Adapter
	}
	adapters := append(BuiltinAdapters(), opts.AdditionalAdapters...)
	sources := make([]source, 0, len(adapters))
	adapterIDs := map[string]struct{}{}
	for _, adapter := range adapters {
		if adapter == nil {
			return Report{}, fmt.Errorf("source adapter must not be nil")
		}
		manifest := adapter.Manifest()
		if err := manifest.Validate(); err != nil {
			return Report{}, err
		}
		if _, duplicate := adapterIDs[manifest.ID]; duplicate {
			return Report{}, fmt.Errorf("duplicate source adapter %q", manifest.ID)
		}
		adapterIDs[manifest.ID] = struct{}{}
		path := pathOverrides[manifest.ID]
		if path == "" {
			path = adapter.DefaultPath(home)
		}
		if path == "" {
			return Report{}, fmt.Errorf("source adapter %q returned an empty default path", manifest.ID)
		}
		sources = append(sources, source{manifest: manifest, dir: path, adapter: adapter})
	}
	for _, src := range sources {
		provider := ProviderUsage{
			Provider: src.manifest.ID, AdapterVersion: src.manifest.APIVersion,
			Privacy: src.manifest.Privacy, Dir: src.dir, Models: []ModelUsage{}, Days: []DayUsage{},
		}
		forcedMetered := slices.Contains(opts.MeteredProviders, src.manifest.ID)
		providerBilling := ""
		multipleBillingProviders := false
		hasSubscriptionUsage := false
		hasMeteredUsage := false
		if _, err := os.Stat(src.dir); err == nil {
			provider.Detected = true
		}
		models := map[string]*Totals{}
		modelPriced := map[string]bool{}
		modelPricingSource := map[string]string{}
		days := map[string]*Totals{}
		seenEvents := map[string]struct{}{}
		invalidEvents := false
		aggregationRejected := false
		estimatedEvents := false
		partialEvents := false
		result, err := src.adapter.Scan(src.dir, opts.Since, opts.ScanLimits, func(event Event) {
			event = normalizeEvent(event)
			event.Provider = src.manifest.ID
			if !event.Time.IsZero() && (event.Time.Before(opts.Since) || event.Time.After(opts.Until)) {
				return
			}
			if validationErr := event.Validate(); validationErr != nil {
				invalidEvents = true
				return
			}
			switch event.Confidence {
			case sourceadapter.ConfidenceEstimated:
				estimatedEvents = true
			case sourceadapter.ConfidencePartial:
				partialEvents = true
			}
			if event.ID != "" {
				if _, duplicate := seenEvents[event.ID]; duplicate {
					return
				}
				seenEvents[event.ID] = struct{}{}
			}
			price, priced := priceFor(event.Model)
			var usd, serverToolUSD float64
			pricingSource := "unknown"
			if priced {
				usd = Cost(price, event.In, event.Out, event.CacheRead, event.CacheWrite5m, event.CacheWrite1h)
				if event.InferenceGeo == "us" {
					usd *= 1.1
				}
				serverToolUSD = serverToolCost(event.ServerToolUse)
				usd += serverToolUSD
				pricingSource = "table"
			} else if event.CostKnown {
				usd = event.CostUSD
				priced = true
				pricingSource = "source"
			}
			model := models[event.Model]
			newModel := model == nil
			if model == nil {
				model = &Totals{}
			}
			dayKey := event.Time.Local().Format("2006-01-02")
			day := days[dayKey]
			newDay := day == nil
			if day == nil {
				day = &Totals{}
			}
			targets := []*Totals{model, day, &provider.Totals, &report.Totals}
			for _, target := range targets {
				if !canAddTotals(target, event, usd, serverToolUSD) {
					aggregationRejected = true
					return
				}
			}
			var nextUnpricedCalls, nextUnpricedTokens int64
			if !priced {
				var ok bool
				nextUnpricedCalls, ok = checkedAddUsage(report.UnpricedCalls, event.Calls)
				if !ok {
					aggregationRejected = true
					return
				}
				eventTokens, ok := eventTokenTotal(event)
				if !ok {
					aggregationRejected = true
					return
				}
				nextUnpricedTokens, ok = checkedAddUsage(report.UnpricedTokens, eventTokens)
				if !ok {
					aggregationRejected = true
					return
				}
			}
			eventMetered := forcedMetered || event.BillingProvider != ""
			providerBucket := provider.SubscriptionUSD
			reportBucket := report.SubscriptionUSD
			if eventMetered {
				providerBucket = provider.MeteredUSD
				reportBucket = report.MeteredUSD
			}
			nextProviderBucket, ok := checkedAddUSD(providerBucket, usd)
			if !ok {
				aggregationRejected = true
				return
			}
			nextReportBucket, ok := checkedAddUSD(reportBucket, usd)
			if !ok {
				aggregationRejected = true
				return
			}

			for _, target := range targets {
				addTotals(target, event, usd, serverToolUSD)
			}
			if newModel {
				models[event.Model] = model
			}
			if newDay {
				days[dayKey] = day
			}
			if eventMetered {
				provider.MeteredUSD = nextProviderBucket
				report.MeteredUSD = nextReportBucket
				hasMeteredUsage = true
			} else {
				provider.SubscriptionUSD = nextProviderBucket
				report.SubscriptionUSD = nextReportBucket
				hasSubscriptionUsage = true
			}
			if event.BillingProvider != "" {
				if providerBilling == "" {
					providerBilling = event.BillingProvider
				} else if providerBilling != event.BillingProvider {
					multipleBillingProviders = true
				}
			}
			if priced {
				modelPriced[event.Model] = true
				if pricingSource == "table" || modelPricingSource[event.Model] == "" || modelPricingSource[event.Model] == "unknown" {
					modelPricingSource[event.Model] = pricingSource
				}
			} else if modelPricingSource[event.Model] == "" {
				modelPricingSource[event.Model] = "unknown"
			}
			if !priced {
				report.UnpricedCalls = nextUnpricedCalls
				report.UnpricedTokens = nextUnpricedTokens
				unpricedModels[event.Model] = struct{}{}
			}
		})
		if invalidEvents {
			result.Stats.Warn("one or more invalid adapter events were rejected")
		}
		if aggregationRejected {
			result.Stats.Warn("one or more adapter events exceeded report aggregation limits")
		}
		if estimatedEvents {
			result.Stats.Warn("one or more adapter events contain estimated usage")
		}
		if partialEvents {
			result.Stats.Warn("one or more adapter events contain partial usage")
		}
		provider.Sessions = result.Sessions
		provider.Scan = result.Stats
		provider.Partial = result.Stats.Partial
		provider.Warnings = append([]string(nil), result.Stats.Warnings...)
		provider.SkippedFiles = result.Stats.FilesSkipped
		if result.Stats.Partial {
			report.Partial = true
		}
		if err != nil {
			// One incompatible or temporarily locked agent store must not hide
			// usage from every other detected tool. Surface the source error in
			// the report and keep the successful partial data.
			provider.Error = "unable to scan " + src.manifest.ID + " usage"
			provider.Detail = err.Error()
			report.Partial = true
		}
		for model, totals := range models {
			provider.Models = append(provider.Models, ModelUsage{
				Model: model, Priced: modelPriced[model], PricingSource: modelPricingSource[model], Totals: *totals,
			})
		}
		sort.Slice(provider.Models, func(i, j int) bool {
			if provider.Models[i].APIUSD != provider.Models[j].APIUSD {
				return provider.Models[i].APIUSD > provider.Models[j].APIUSD
			}
			return provider.Models[i].Model < provider.Models[j].Model
		})
		for day, totals := range days {
			provider.Days = append(provider.Days, DayUsage{Day: day, Totals: *totals})
		}
		sort.Slice(provider.Days, func(i, j int) bool { return provider.Days[i].Day < provider.Days[j].Day })

		provider.Metered = hasMeteredUsage && !hasSubscriptionUsage
		provider.MixedBilling = hasMeteredUsage && hasSubscriptionUsage
		if multipleBillingProviders {
			provider.BillingProvider = "multiple"
		} else {
			provider.BillingProvider = providerBilling
		}
		report.Providers = append(report.Providers, provider)
	}
	for model := range unpricedModels {
		report.UnpricedModels = append(report.UnpricedModels, model)
	}
	sort.Strings(report.UnpricedModels)
	report.HasUsage = report.Calls > 0
	return report, nil
}

func normalizeEvent(event Event) Event {
	if event.Calls == 0 {
		event.Calls = 1
	}
	return event
}

const maxInt64 = int64(^uint64(0) >> 1)

func checkedAddUsage(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > maxInt64-b {
		return 0, false
	}
	return a + b, true
}

func checkedAddUSD(a, b float64) (float64, bool) {
	if a < 0 || b < 0 || math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return 0, false
	}
	sum := a + b
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return 0, false
	}
	return sum, true
}

func eventTokenTotal(event Event) (int64, bool) {
	total := int64(0)
	var ok bool
	for _, value := range []int64{event.In, event.Out, event.CacheRead, event.CacheWrite5m, event.CacheWrite1h} {
		total, ok = checkedAddUsage(total, value)
		if !ok {
			return 0, false
		}
	}
	return total, true
}

func canAddTotals(t *Totals, event Event, usd, serverToolUSD float64) bool {
	write, ok := checkedAddUsage(event.CacheWrite5m, event.CacheWrite1h)
	if !ok {
		return false
	}
	integerPairs := [][2]int64{
		{t.Calls, event.Calls},
		{t.In, event.In},
		{t.Out, event.Out},
		{t.CacheRead, event.CacheRead},
		{t.CacheWrite5m, event.CacheWrite5m},
		{t.CacheWrite1h, event.CacheWrite1h},
		{t.CacheWrite, write},
		{t.ServerToolUse.WebSearchRequests, event.ServerToolUse.WebSearchRequests},
		{t.ServerToolUse.WebFetchRequests, event.ServerToolUse.WebFetchRequests},
	}
	for _, pair := range integerPairs {
		if _, ok := checkedAddUsage(pair[0], pair[1]); !ok {
			return false
		}
	}
	if event.ServiceTier != "" {
		if _, ok := checkedAddUsage(t.ServiceTiers[event.ServiceTier], event.Calls); !ok {
			return false
		}
	}
	if event.InferenceGeo != "" {
		if _, ok := checkedAddUsage(t.InferenceGeos[event.InferenceGeo], event.Calls); !ok {
			return false
		}
	}
	if _, ok := checkedAddUSD(t.APIUSD, usd); !ok {
		return false
	}
	if _, ok := checkedAddUSD(t.ServerToolUSD, serverToolUSD); !ok {
		return false
	}
	return true
}

func addTotals(t *Totals, event Event, usd, serverToolUSD float64) {
	write := event.CacheWrite5m + event.CacheWrite1h
	t.Calls += event.Calls
	t.In += event.In
	t.Out += event.Out
	t.CacheRead += event.CacheRead
	t.CacheWrite5m += event.CacheWrite5m
	t.CacheWrite1h += event.CacheWrite1h
	t.CacheWrite += write
	t.APIUSD += usd
	t.ServerToolUSD += serverToolUSD
	if event.ServiceTier != "" {
		if t.ServiceTiers == nil {
			t.ServiceTiers = map[string]int64{}
		}
		t.ServiceTiers[event.ServiceTier] += event.Calls
	}
	if event.InferenceGeo != "" {
		if t.InferenceGeos == nil {
			t.InferenceGeos = map[string]int64{}
		}
		t.InferenceGeos[event.InferenceGeo] += event.Calls
	}
	t.ServerToolUse.WebSearchRequests += event.ServerToolUse.WebSearchRequests
	t.ServerToolUse.WebFetchRequests += event.ServerToolUse.WebFetchRequests
}
