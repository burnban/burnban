package optimize

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/burnban/burnban/internal/store"
)

const (
	CacheReportSchema      = "burnban.cache-recommendations/v1"
	AllocationReportSchema = "burnban.allocation-recommendations/v1"
)

type CacheOptions struct {
	LargeContextTokens int64
	LowReuseRatio      float64
	MinRepeatedLarge   int
	MaxReceipts        int
}

func DefaultCacheOptions() CacheOptions {
	return CacheOptions{LargeContextTokens: 32_000, LowReuseRatio: 0.20, MinRepeatedLarge: 3, MaxReceipts: 100}
}

type CacheReport struct {
	Schema              string         `json:"schema"`
	From                time.Time      `json:"from"`
	Through             time.Time      `json:"through"`
	SampledRows         int            `json:"sampled_rows"`
	Truncated           bool           `json:"truncated"`
	PromptContentStored bool           `json:"prompt_content_stored"`
	Receipts            []CacheReceipt `json:"receipts"`
	Limitations         []string       `json:"limitations"`
}

type CacheReceipt struct {
	ScopeType         string    `json:"scope_type"`
	Scope             string    `json:"scope"`
	Provider          string    `json:"provider"`
	Model             string    `json:"model"`
	Route             string    `json:"route,omitempty"`
	Requests          int64     `json:"requests"`
	LargeContextCalls int64     `json:"large_context_calls"`
	InputTokens       int64     `json:"input_tokens"`
	CacheReadTokens   int64     `json:"cache_read_tokens"`
	CacheWriteTokens  int64     `json:"cache_write_tokens"`
	CacheReuseRatio   float64   `json:"cache_reuse_ratio"`
	FirstObservedAt   time.Time `json:"first_observed_at"`
	LastObservedAt    time.Time `json:"last_observed_at"`
	Pattern           string    `json:"pattern"`
	Confidence        string    `json:"confidence"`
	PrefixStability   string    `json:"prefix_stability"`
	Recommendation    string    `json:"recommendation"`
}

type cacheKey struct {
	scopeType, scope, provider, model, route string
}

type cacheAggregate struct {
	requests, large              int64
	input, cacheRead, cacheWrite int64
	first, last                  time.Time
}

// AnalyzeCache emits evidence only for low cache reuse backed by repeated
// large input contexts. It never labels a prefix stable or unstable: the
// ledger has token counts, not prompt-prefix structure.
func AnalyzeCache(sample store.OptimizationSample, from, through time.Time, options CacheOptions) (CacheReport, error) {
	report := CacheReport{
		Schema: CacheReportSchema, From: from.UTC(), Through: through.UTC(),
		SampledRows: len(sample.Rows), Truncated: sample.Truncated,
		PromptContentStored: false,
		Limitations: []string{
			"Prompt-prefix stability is unobserved: Burnban stores no prompt bodies or prefix fingerprints.",
			"A low cache-read ratio does not prove a provider/model supports caching or that a prefix changed.",
			"Recommendations estimate no savings because cache eligibility, TTLs, and rates are provider/model specific.",
		},
	}
	if options.LargeContextTokens < 1_000 || options.LargeContextTokens > 10_000_000 {
		return report, errors.New("large-context threshold must be between 1,000 and 10,000,000 tokens")
	}
	if math.IsNaN(options.LowReuseRatio) || math.IsInf(options.LowReuseRatio, 0) || options.LowReuseRatio <= 0 || options.LowReuseRatio >= 1 {
		return report, errors.New("low-reuse ratio must be between 0 and 1")
	}
	if options.MinRepeatedLarge < 2 || options.MinRepeatedLarge > 1000 {
		return report, errors.New("minimum repeated-large calls must be between 2 and 1000")
	}
	if options.MaxReceipts < 1 || options.MaxReceipts > 500 {
		return report, errors.New("maximum cache receipts must be between 1 and 500")
	}
	aggregates := map[cacheKey]*cacheAggregate{}
	for i, row := range sample.Rows {
		if row.Ts.Before(from) || !row.Ts.Before(through) {
			return report, fmt.Errorf("optimization row %d is outside the requested window", i)
		}
		if row.InTokens < 0 || row.CacheReadTokens < 0 || row.CacheWriteTokens < 0 {
			return report, fmt.Errorf("optimization row %d contains negative token metadata", i)
		}
		contextTokens, err := checkedAddPositive(row.InTokens, row.CacheReadTokens, row.CacheWriteTokens)
		if err != nil {
			return report, fmt.Errorf("optimization row %d token overflow: %w", i, err)
		}
		scopeType, scope := recommendationScope(row)
		key := cacheKey{
			scopeType: scopeType, scope: safeMetadataLabel(scope),
			provider: safeMetadataLabel(row.Provider), model: safeMetadataLabel(row.Model),
			route: safeMetadataLabel(row.Route),
		}
		aggregate := aggregates[key]
		if aggregate == nil {
			aggregate = &cacheAggregate{first: row.Ts, last: row.Ts}
			aggregates[key] = aggregate
		}
		aggregate.requests++
		if contextTokens >= options.LargeContextTokens {
			aggregate.large++
		}
		if aggregate.input, err = checkedAddPositive(aggregate.input, row.InTokens); err != nil {
			return report, fmt.Errorf("cache input aggregate overflow: %w", err)
		}
		if aggregate.cacheRead, err = checkedAddPositive(aggregate.cacheRead, row.CacheReadTokens); err != nil {
			return report, fmt.Errorf("cache-read aggregate overflow: %w", err)
		}
		if aggregate.cacheWrite, err = checkedAddPositive(aggregate.cacheWrite, row.CacheWriteTokens); err != nil {
			return report, fmt.Errorf("cache-write aggregate overflow: %w", err)
		}
		if row.Ts.Before(aggregate.first) {
			aggregate.first = row.Ts
		}
		if row.Ts.After(aggregate.last) {
			aggregate.last = row.Ts
		}
	}
	for key, aggregate := range aggregates {
		if aggregate.large < int64(options.MinRepeatedLarge) {
			continue
		}
		total, err := checkedAddPositive(aggregate.input, aggregate.cacheRead, aggregate.cacheWrite)
		if err != nil {
			return report, fmt.Errorf("cache aggregate overflow: %w", err)
		}
		ratio := 0.0
		if total > 0 {
			ratio = float64(aggregate.cacheRead) / float64(total)
		}
		if ratio >= options.LowReuseRatio {
			continue
		}
		confidence := "medium"
		if aggregate.large >= 10 && !sample.Truncated {
			confidence = "high"
		}
		if sample.Truncated || aggregate.large < 5 {
			confidence = "low"
		}
		report.Receipts = append(report.Receipts, CacheReceipt{
			ScopeType: key.scopeType, Scope: key.scope, Provider: key.provider, Model: key.model, Route: key.route,
			Requests: aggregate.requests, LargeContextCalls: aggregate.large,
			InputTokens: aggregate.input, CacheReadTokens: aggregate.cacheRead, CacheWriteTokens: aggregate.cacheWrite,
			CacheReuseRatio: ratio, FirstObservedAt: aggregate.first.UTC(), LastObservedAt: aggregate.last.UTC(),
			Pattern: "repeated_large_context_with_low_cache_reuse", Confidence: confidence,
			PrefixStability: "unobserved",
			Recommendation:  "Check whether this provider/model supports prompt caching; if it does, place reusable instructions and tool definitions before request-specific context, configure the documented cache TTL, and measure this receipt again.",
		})
	}
	sort.Slice(report.Receipts, func(i, j int) bool {
		left, right := report.Receipts[i], report.Receipts[j]
		if left.LargeContextCalls != right.LargeContextCalls {
			return left.LargeContextCalls > right.LargeContextCalls
		}
		if left.InputTokens != right.InputTokens {
			return left.InputTokens > right.InputTokens
		}
		return receiptIdentity(left) < receiptIdentity(right)
	})
	if len(report.Receipts) > options.MaxReceipts {
		report.Receipts = report.Receipts[:options.MaxReceipts]
		report.Limitations = append(report.Limitations, "Additional recommendation groups were omitted by the configured receipt limit.")
	}
	if sample.Truncated {
		report.Limitations = append(report.Limitations, "The ledger row limit was reached; receipts cover the most recent sample and confidence is reduced.")
	}
	return report, nil
}

func receiptIdentity(receipt CacheReceipt) string {
	return receipt.ScopeType + "\x00" + receipt.Scope + "\x00" + receipt.Provider + "\x00" + receipt.Model + "\x00" + receipt.Route
}

func recommendationScope(row store.OptimizationRow) (string, string) {
	if strings.TrimSpace(row.Project) != "" {
		return "project", row.Project
	}
	if strings.TrimSpace(row.Agent) != "" {
		return "agent", row.Agent
	}
	return "unattributed", "unattributed"
}

type AllocationOptions struct {
	Dimension       string
	Days            int
	HeadroomPercent int
	Percentile      int
	MaxScopes       int
}

func DefaultAllocationOptions(dimension string, days int) AllocationOptions {
	return AllocationOptions{Dimension: dimension, Days: days, HeadroomPercent: 20, Percentile: 90, MaxScopes: 100}
}

type AllocationReport struct {
	Schema                   string                     `json:"schema"`
	Dimension                string                     `json:"dimension"`
	From                     time.Time                  `json:"from"`
	Through                  time.Time                  `json:"through"`
	HistoricalDays           int                        `json:"historical_days"`
	SampledRows              int                        `json:"sampled_rows"`
	Truncated                bool                       `json:"truncated"`
	UnattributedRows         int64                      `json:"unattributed_rows"`
	ExcludedInvalidScopeRows int64                      `json:"excluded_invalid_scope_rows"`
	ExcludedUntrustedRows    int64                      `json:"excluded_untrusted_scope_rows"`
	ExcludedUnpricedRows     int64                      `json:"excluded_unpriced_rows"`
	ExcludedScopes           int                        `json:"excluded_scopes"`
	Recommendations          []AllocationRecommendation `json:"recommendations"`
	Limitations              []string                   `json:"limitations"`
}

type AllocationRecommendation struct {
	Scope                          string  `json:"scope"`
	ProposedWeight                 float64 `json:"proposed_weight"`
	ProposedDailyBudgetMicros      int64   `json:"proposed_daily_budget_micros"`
	HistoricalAverageMicros        int64   `json:"historical_average_daily_micros"`
	HistoricalPercentileMicros     int64   `json:"historical_percentile_daily_micros"`
	RecentVelocityMicros           int64   `json:"recent_velocity_daily_micros"`
	HeadroomAtRecentVelocityMicros int64   `json:"headroom_at_recent_velocity_micros"`
	PricedCalls                    int64   `json:"priced_calls"`
	ExcludedCalls                  int64   `json:"excluded_calls"`
	EnforcementUnsafeCalls         int64   `json:"enforcement_unsafe_calls"`
	SimulatedBlockedCalls          int64   `json:"simulated_blocked_calls"`
	SimulatedBlockedSpendMicros    int64   `json:"simulated_blocked_spend_micros"`
	SimulatedBlockedCallRate       float64 `json:"simulated_blocked_call_rate"`
	Confidence                     string  `json:"confidence"`
	ApplyCommand                   string  `json:"apply_command,omitempty"`
	OperatorAction                 string  `json:"operator_action"`
}

type allocationEvent struct {
	id   int64
	ts   time.Time
	day  int
	cost int64
}

type allocationAggregate struct {
	scope       string
	daily       []int64
	events      []allocationEvent
	pricedCalls int64
	excluded    int64
	unsafe      int64
	total       int64
	base        int64
}

// RecommendAllocations replays historical priced calls against proposed daily
// limits. It returns proposals only and performs no settings or policy writes.
func RecommendAllocations(sample store.OptimizationSample, from, through time.Time, options AllocationOptions) (AllocationReport, error) {
	report := AllocationReport{
		Schema: AllocationReportSchema, Dimension: options.Dimension, From: from.UTC(), Through: through.UTC(),
		HistoricalDays: options.Days, SampledRows: len(sample.Rows), Truncated: sample.Truncated,
		Limitations: []string{
			"These are local historical proposals, not applied budgets or policy changes.",
			"Blocked-call simulation replays recorded exact costs in timestamp order; it cannot model changed traffic, concurrency reservations, or target-model quality.",
			"Weights describe relative historical demand and are not entitlements.",
		},
	}
	if options.Dimension != "agent" && options.Dimension != "project" &&
		options.Dimension != "meter" && options.Dimension != "team" {
		return report, errors.New("allocation dimension must be agent, project, meter, or team")
	}
	if options.Days < 7 || options.Days > 90 || through.Sub(from) != time.Duration(options.Days)*24*time.Hour {
		return report, errors.New("allocation history must contain 7 to 90 complete UTC days")
	}
	if !from.Equal(from.UTC().Truncate(24*time.Hour)) || !through.Equal(through.UTC().Truncate(24*time.Hour)) {
		return report, errors.New("allocation history boundaries must be UTC midnights")
	}
	if options.HeadroomPercent < 0 || options.HeadroomPercent > 200 {
		return report, errors.New("allocation headroom must be between 0 and 200 percent")
	}
	if options.Percentile < 50 || options.Percentile > 99 {
		return report, errors.New("allocation percentile must be between 50 and 99")
	}
	if options.MaxScopes < 1 || options.MaxScopes > 500 {
		return report, errors.New("maximum allocation scopes must be between 1 and 500")
	}
	if sample.Truncated {
		report.Limitations = append(report.Limitations, "The row bound was reached. No allocation is proposed because blocked-call impact would be incomplete; narrow the window or raise --max-rows.")
		return report, nil
	}
	aggregates := map[string]*allocationAggregate{}
	for i, row := range sample.Rows {
		if row.Ts.Before(from) || !row.Ts.Before(through) {
			return report, fmt.Errorf("optimization row %d is outside the requested window", i)
		}
		scope := row.Agent
		switch options.Dimension {
		case "project":
			scope = row.Project
		case "meter":
			scope = row.Meter
		case "team":
			scope = row.Team
		}
		if strings.TrimSpace(scope) == "" {
			report.UnattributedRows++
			continue
		}
		if (options.Dimension == "meter" || options.Dimension == "team") && row.IdentityConfidence != "authenticated" {
			report.ExcludedUntrustedRows++
			continue
		}
		if !validAllocationScope(scope) {
			report.ExcludedInvalidScopeRows++
			continue
		}
		aggregate := aggregates[scope]
		if aggregate == nil {
			aggregate = &allocationAggregate{scope: scope, daily: make([]int64, options.Days)}
			aggregates[scope] = aggregate
		}
		if row.PricingState != store.PricingPriced {
			aggregate.excluded++
			report.ExcludedUnpricedRows++
			continue
		}
		micros, err := dollarsToMicros(row.CostUSD)
		if err != nil {
			return report, fmt.Errorf("optimization row %d cost: %w", i, err)
		}
		day := int(row.Ts.Sub(from) / (24 * time.Hour))
		if day < 0 || day >= options.Days {
			return report, fmt.Errorf("optimization row %d has invalid day index", i)
		}
		if aggregate.daily[day], err = checkedAddPositive(aggregate.daily[day], micros); err != nil {
			return report, fmt.Errorf("scope %q daily spend overflow: %w", scope, err)
		}
		if aggregate.total, err = checkedAddPositive(aggregate.total, micros); err != nil {
			return report, fmt.Errorf("scope %q total spend overflow: %w", scope, err)
		}
		aggregate.pricedCalls++
		if row.EnforcementUnsafe || row.UsageState == store.UsagePartial {
			aggregate.unsafe++
		}
		aggregate.events = append(aggregate.events, allocationEvent{id: row.ID, ts: row.Ts, day: day, cost: micros})
	}
	ordered := make([]*allocationAggregate, 0, len(aggregates))
	for _, aggregate := range aggregates {
		if aggregate.pricedCalls == 0 || aggregate.total == 0 {
			continue
		}
		daily := append([]int64(nil), aggregate.daily...)
		sort.Slice(daily, func(i, j int) bool { return daily[i] < daily[j] })
		index := int(math.Ceil(float64(options.Percentile)/100*float64(len(daily)))) - 1
		index = max(0, min(index, len(daily)-1))
		percentile := daily[index]
		recentDays := min(7, options.Days)
		var recent int64
		var err error
		for _, amount := range aggregate.daily[options.Days-recentDays:] {
			if recent, err = checkedAddPositive(recent, amount); err != nil {
				return report, fmt.Errorf("scope %q recent velocity overflow: %w", aggregate.scope, err)
			}
		}
		recent /= int64(recentDays)
		aggregate.base = max(percentile, recent)
		ordered = append(ordered, aggregate)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].base != ordered[j].base {
			return ordered[i].base > ordered[j].base
		}
		return ordered[i].scope < ordered[j].scope
	})
	if len(ordered) > options.MaxScopes {
		report.ExcludedScopes = len(ordered) - options.MaxScopes
		ordered = ordered[:options.MaxScopes]
		report.Limitations = append(report.Limitations, "Lower-demand scopes were omitted by the configured scope limit.")
	}
	var totalBase int64
	for _, aggregate := range ordered {
		var err error
		if totalBase, err = checkedAddPositive(totalBase, aggregate.base); err != nil {
			return report, fmt.Errorf("allocation weight denominator overflow: %w", err)
		}
	}
	for _, aggregate := range ordered {
		capMicros, err := multiplyDivideCeil(aggregate.base, uint64(100+options.HeadroomPercent), 100)
		if err != nil {
			return report, fmt.Errorf("scope %q proposed budget overflow: %w", aggregate.scope, err)
		}
		capMicros, err = roundUp(capMicros, 10_000) // whole cents
		if err != nil {
			return report, fmt.Errorf("scope %q proposed budget overflow: %w", aggregate.scope, err)
		}
		capMicros = max(capMicros, int64(10_000))
		var recent int64
		recentDays := min(7, options.Days)
		for _, amount := range aggregate.daily[options.Days-recentDays:] {
			recent, err = checkedAddPositive(recent, amount)
			if err != nil {
				return report, err
			}
		}
		recent /= int64(recentDays)
		var blockedCalls, blockedSpend int64
		used := make([]int64, options.Days)
		sort.Slice(aggregate.events, func(i, j int) bool {
			if !aggregate.events[i].ts.Equal(aggregate.events[j].ts) {
				return aggregate.events[i].ts.Before(aggregate.events[j].ts)
			}
			return aggregate.events[i].id < aggregate.events[j].id
		})
		for _, event := range aggregate.events {
			if event.cost > capMicros-used[event.day] {
				blockedCalls++
				blockedSpend, err = checkedAddPositive(blockedSpend, event.cost)
				if err != nil {
					return report, err
				}
				continue
			}
			used[event.day] += event.cost
		}
		confidence := allocationConfidence(options.Days, aggregate.pricedCalls, aggregate.excluded, aggregate.unsafe)
		weight := 0.0
		if totalBase > 0 {
			weight = float64(aggregate.base) / float64(totalBase)
		}
		average := aggregate.total / int64(options.Days)
		daily := append([]int64(nil), aggregate.daily...)
		sort.Slice(daily, func(i, j int) bool { return daily[i] < daily[j] })
		percentileIndex := int(math.Ceil(float64(options.Percentile)/100*float64(len(daily)))) - 1
		percentile := daily[max(0, min(percentileIndex, len(daily)-1))]
		apply := ""
		action := "Review this proposal and encode it explicitly in a versioned Policy v2 scope if accepted."
		if options.Dimension == "agent" {
			apply = fmt.Sprintf("burnban cap --agent %s --daily %.2f", posixShellQuote(aggregate.scope), float64(capMicros)/1e6)
			action = "Review the simulation, then explicitly set the local daily cap only if the operator accepts it. The suggested command uses POSIX shell quoting."
		}
		recommendation := AllocationRecommendation{
			Scope: aggregate.scope, ProposedWeight: weight, ProposedDailyBudgetMicros: capMicros,
			HistoricalAverageMicros: average, HistoricalPercentileMicros: percentile,
			RecentVelocityMicros: recent, HeadroomAtRecentVelocityMicros: max(int64(0), capMicros-recent),
			PricedCalls: aggregate.pricedCalls, ExcludedCalls: aggregate.excluded, EnforcementUnsafeCalls: aggregate.unsafe,
			SimulatedBlockedCalls: blockedCalls, SimulatedBlockedSpendMicros: blockedSpend,
			Confidence: confidence, ApplyCommand: apply, OperatorAction: action,
		}
		if aggregate.pricedCalls > 0 {
			recommendation.SimulatedBlockedCallRate = float64(blockedCalls) / float64(aggregate.pricedCalls)
		}
		report.Recommendations = append(report.Recommendations, recommendation)
	}
	if report.ExcludedInvalidScopeRows > 0 {
		report.Limitations = append(report.Limitations, "Rows with unbounded or unsafe scope labels were excluded rather than merged into an actionable allocation.")
	}
	if report.ExcludedUntrustedRows > 0 {
		report.Limitations = append(report.Limitations, "Meter and team proposals exclude self-reported or unverified identity rows; only server-authorized signed attribution can influence fleet weights.")
	}
	return report, nil
}

func allocationConfidence(days int, priced, excluded, unsafe int64) string {
	if days >= 28 && priced >= 100 && excluded == 0 && unsafe == 0 {
		return "high"
	}
	if days >= 14 && priced >= 30 && unsafe == 0 && (excluded == 0 || float64(excluded)/float64(priced+excluded) <= 0.05) {
		return "medium"
	}
	return "low"
}

func dollarsToMicros(value float64) (int64, error) {
	// Leave one whole dollar of headroom below MaxInt64 so float64 rounding at
	// the 2^63 boundary can never turn a seemingly valid amount into MinInt64
	// during conversion.
	const maxRepresentableUSD = float64(math.MaxInt64-1_000_000) / 1e6
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > maxRepresentableUSD {
		return 0, errors.New("cost must be a finite, nonnegative representable amount")
	}
	result := math.Round(value * 1e6)
	if result < 0 || result > float64(math.MaxInt64-1_000_000) {
		return 0, errors.New("cost overflows micro-dollar representation")
	}
	return int64(result), nil
}

func checkedAddPositive(values ...int64) (int64, error) {
	var result int64
	for _, value := range values {
		if value < 0 || result > math.MaxInt64-value {
			return 0, errors.New("positive integer aggregate overflow")
		}
		result += value
	}
	return result, nil
}

func multiplyDivideCeil(value int64, numerator, denominator uint64) (int64, error) {
	if value < 0 || denominator == 0 {
		return 0, errors.New("invalid fixed-point multiplication")
	}
	hi, lo := bits.Mul64(uint64(value), numerator)
	if hi >= denominator {
		return 0, errors.New("fixed-point multiplication overflow")
	}
	quotient, remainder := bits.Div64(hi, lo, denominator)
	if remainder != 0 {
		if quotient == math.MaxUint64 {
			return 0, errors.New("fixed-point multiplication overflow")
		}
		quotient++
	}
	if quotient > math.MaxInt64 {
		return 0, errors.New("fixed-point multiplication overflow")
	}
	return int64(quotient), nil
}

func roundUp(value, unit int64) (int64, error) {
	if value < 0 || unit <= 0 {
		return 0, errors.New("invalid rounding input")
	}
	remainder := value % unit
	if remainder == 0 {
		return value, nil
	}
	return checkedAddPositive(value, unit-remainder)
}

func safeMetadataLabel(value string) string {
	original := value
	value = strings.ToValidUTF8(value, "�")
	var out strings.Builder
	changed := value != original
	runes := 0
	for _, r := range value {
		if runes >= 180 {
			changed = true
			break
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			out.WriteRune(' ')
			changed = true
		} else {
			out.WriteRune(r)
		}
		runes++
	}
	clean := strings.TrimSpace(strings.Join(strings.Fields(out.String()), " "))
	if clean == "" {
		clean = "unattributed"
		changed = true
	}
	if !utf8.ValidString(clean) {
		clean = "invalid-metadata"
		changed = true
	}
	if changed || clean != original {
		digest := sha256.Sum256([]byte(original))
		clean += fmt.Sprintf("…#%x", digest[:6])
	}
	return clean
}

func validAllocationScope(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			return false
		}
	}
	return true
}

func posixShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
