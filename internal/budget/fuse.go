package budget

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/store"
)

// Fuse settings are local safety policy. Unlike calendar caps, fuse rules use
// rolling windows and persist a short cooldown when they trip so a runaway
// loop cannot resume merely by restarting the proxy.
const (
	KeyFuseHourlyUSD = "fuse_hourly_usd"
	KeyFuseBurst     = "fuse_burst"
	KeyFuseFanout    = "fuse_fanout"
	KeyFuseBaseline  = "fuse_baseline"
	KeyFuseCooldown  = "fuse_cooldown"
	KeyFuseTrip      = "fuse_trip"

	KeyFuseAlertedPrefix = KeyAlertedPrefix + "fuse:"
)

const (
	DefaultFuseCooldown = 15 * time.Minute
	MinFuseWindow       = time.Second
	MaxFuseWindow       = time.Hour
	MinFuseCooldown     = time.Minute
	MaxFuseCooldown     = 24 * time.Hour
	MinBaselineWindow   = time.Minute
	MaxBaselineWindow   = 24 * time.Hour
	MinBaselineDays     = 7
	MaxBaselineDays     = 89
	MaxFanoutRequests   = 1_000_000
)

type fuseRule struct {
	name   string
	window time.Duration
	capUSD float64
}

// FuseRuleState is one live rolling-window position for CLI, MCP, dashboard,
// and metrics consumers.
type FuseRuleState struct {
	Name      string
	Window    time.Duration
	CapUSD    float64
	SpentUSD  float64
	StartAt   time.Time
	Remaining float64

	BaselineMedianUSD    float64
	BaselineMultiplier   float64
	ProjectedTimeToLimit time.Duration
}

func (s FuseRuleState) Pct() float64 {
	if s.CapUSD <= 0 {
		return 0
	}
	return s.SpentUSD / s.CapUSD * 100
}

// FuseSnapshot describes configured velocity rules and the persisted
// cooldown, if one is currently active.
type FuseSnapshot struct {
	Rules                 []FuseRuleState
	Fanout                *FuseFanoutState
	Cooldown              time.Duration
	Tripped               bool
	TripRule              string
	TripStartedAt         time.Time
	TrippedUntil          time.Time
	TripLimitUSD          float64
	TripProjected         float64
	TripLimitRequests     int64
	TripProjectedRequests int64
	DenialMessage         string
}

// FuseFanoutState is the live position of a request-count circuit breaker.
// It is separate from Rules so dollar charts never mislabel request counts as
// spend.
type FuseFanoutState struct {
	Window            time.Duration
	LimitRequests     int64
	Requests          int64
	RemainingRequests int64
}

// FuseBaselinePolicy configures a deterministic comparison with the median
// spend in the same UTC time slot over previous days. The minimum floor avoids
// treating a new/idle installation's zero baseline as an immediate ban.
type FuseBaselinePolicy struct {
	Version      int           `json:"version"`
	Window       time.Duration `json:"-"`
	Multiplier   float64       `json:"multiplier"`
	LookbackDays int           `json:"lookback_days"`
	MinimumUSD   float64       `json:"minimum_usd"`
}

// ParseFuseBurst parses the CLI/storage form DURATION:USD, for example 5m:4.
// Keeping both fields in one setting makes reconfiguration atomic for a live
// proxy reading the same SQLite database.
func ParseFuseBurst(raw string) (time.Duration, float64, error) {
	raw = strings.TrimSpace(raw)
	windowText, usdText, ok := strings.Cut(raw, ":")
	if !ok || strings.TrimSpace(windowText) == "" || strings.TrimSpace(usdText) == "" {
		return 0, 0, fmt.Errorf("burst must be DURATION:USD, for example 5m:4")
	}
	window, err := time.ParseDuration(strings.TrimSpace(windowText))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid burst duration %q: %w", strings.TrimSpace(windowText), err)
	}
	if err := ValidateFuseWindow(window); err != nil {
		return 0, 0, err
	}
	usd, err := strconv.ParseFloat(strings.TrimSpace(usdText), 64)
	if err != nil || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, 0, fmt.Errorf("burst limit must be a finite dollar amount")
	}
	if usd < 0.01 {
		return 0, 0, fmt.Errorf("burst limits below $0.01 are not enforceable")
	}
	return window, usd, nil
}

// ParseFuseFanout parses DURATION:REQUESTS, for example 1m:120. The request
// breaker counts settled plus in-flight provider requests independently of
// pricing, so it remains useful for free, local, or temporarily unpriced
// models.
func ParseFuseFanout(raw string) (time.Duration, int64, error) {
	raw = strings.TrimSpace(raw)
	windowText, requestsText, ok := strings.Cut(raw, ":")
	if !ok || strings.TrimSpace(windowText) == "" || strings.TrimSpace(requestsText) == "" {
		return 0, 0, fmt.Errorf("fanout must be DURATION:REQUESTS, for example 1m:120")
	}
	window, err := time.ParseDuration(strings.TrimSpace(windowText))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid fanout duration %q: %w", strings.TrimSpace(windowText), err)
	}
	if err := ValidateFuseWindow(window); err != nil {
		return 0, 0, err
	}
	requests, err := strconv.ParseInt(strings.TrimSpace(requestsText), 10, 64)
	if err != nil || requests < 1 || requests > MaxFanoutRequests {
		return 0, 0, fmt.Errorf("fanout request limit must be an integer from 1 through %d", MaxFanoutRequests)
	}
	return window, requests, nil
}

func FormatFuseFanout(window time.Duration, requests int64) string {
	return FormatFuseDuration(window) + ":" + strconv.FormatInt(requests, 10)
}

type fuseBaselineJSON struct {
	Version      int     `json:"version"`
	Window       string  `json:"window"`
	Multiplier   float64 `json:"multiplier"`
	LookbackDays int     `json:"lookback_days"`
	MinimumUSD   float64 `json:"minimum_usd"`
}

// EncodeFuseBaseline validates and returns the canonical settings value.
func EncodeFuseBaseline(policy FuseBaselinePolicy) (string, error) {
	if policy.Version == 0 {
		policy.Version = 1
	}
	if err := ValidateFuseBaseline(policy); err != nil {
		return "", err
	}
	raw, err := json.Marshal(fuseBaselineJSON{
		Version: policy.Version, Window: FormatFuseDuration(policy.Window),
		Multiplier: policy.Multiplier, LookbackDays: policy.LookbackDays, MinimumUSD: policy.MinimumUSD,
	})
	return string(raw), err
}

// ParseFuseBaseline strictly decodes the versioned baseline configuration.
func ParseFuseBaseline(raw string) (*FuseBaselinePolicy, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if err := rejectAmbiguousFuseObject(raw, map[string]struct{}{
		"version": {}, "window": {}, "multiplier": {}, "lookback_days": {}, "minimum_usd": {},
	}); err != nil {
		return nil, fmt.Errorf("invalid %s setting: %w", KeyFuseBaseline, err)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var stored fuseBaselineJSON
	if err := dec.Decode(&stored); err != nil {
		return nil, fmt.Errorf("invalid %s setting: %w", KeyFuseBaseline, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("invalid %s setting: trailing JSON", KeyFuseBaseline)
	}
	window, err := time.ParseDuration(stored.Window)
	if err != nil {
		return nil, fmt.Errorf("invalid %s setting: bad window %q", KeyFuseBaseline, stored.Window)
	}
	policy := &FuseBaselinePolicy{
		Version: stored.Version, Window: window, Multiplier: stored.Multiplier,
		LookbackDays: stored.LookbackDays, MinimumUSD: stored.MinimumUSD,
	}
	if err := ValidateFuseBaseline(*policy); err != nil {
		return nil, fmt.Errorf("invalid %s setting: %w", KeyFuseBaseline, err)
	}
	return policy, nil
}

func ValidateFuseBaseline(policy FuseBaselinePolicy) error {
	if policy.Version != 1 {
		return fmt.Errorf("baseline version must be 1")
	}
	if policy.Window < MinBaselineWindow || policy.Window > MaxBaselineWindow || (24*time.Hour)%policy.Window != 0 {
		return fmt.Errorf("baseline window must evenly divide 24h and be between %s and %s",
			FormatFuseDuration(MinBaselineWindow), FormatFuseDuration(MaxBaselineWindow))
	}
	if math.IsNaN(policy.Multiplier) || math.IsInf(policy.Multiplier, 0) || policy.Multiplier < 1.1 || policy.Multiplier > 100 {
		return fmt.Errorf("baseline multiplier must be between 1.1 and 100")
	}
	if policy.LookbackDays < MinBaselineDays || policy.LookbackDays > MaxBaselineDays {
		return fmt.Errorf("baseline lookback must be between %d and %d days", MinBaselineDays, MaxBaselineDays)
	}
	if math.IsNaN(policy.MinimumUSD) || math.IsInf(policy.MinimumUSD, 0) || policy.MinimumUSD < 0.01 || policy.MinimumUSD > 1e9 {
		return fmt.Errorf("baseline minimum must be between $0.01 and $1000000000")
	}
	return nil
}

func FormatFuseBurst(window time.Duration, usd float64) string {
	return FormatFuseDuration(window) + ":" + strconv.FormatFloat(usd, 'f', -1, 64)
}

// FormatFuseDuration keeps human-facing policy windows compact ("5m", "1h")
// while retaining time.Duration's lossless form for sub-second values.
func FormatFuseDuration(d time.Duration) string {
	switch {
	case d != 0 && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d != 0 && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	case d != 0 && d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	default:
		return d.String()
	}
}

func ValidateFuseWindow(window time.Duration) error {
	if window < MinFuseWindow || window > MaxFuseWindow {
		return fmt.Errorf("burst duration must be between %s and %s", FormatFuseDuration(MinFuseWindow), FormatFuseDuration(MaxFuseWindow))
	}
	return nil
}

func ValidateFuseCooldown(cooldown time.Duration) error {
	if cooldown < MinFuseCooldown || cooldown > MaxFuseCooldown {
		return fmt.Errorf("cooldown must be between %s and %s", FormatFuseDuration(MinFuseCooldown), FormatFuseDuration(MaxFuseCooldown))
	}
	return nil
}

func fuseSettingKeys() []string {
	return []string{KeyFuseHourlyUSD, KeyFuseBurst, KeyFuseFanout, KeyFuseBaseline, KeyFuseCooldown, KeyFuseTrip}
}

func fuseRulesFromSettings(vals map[string]string) ([]fuseRule, time.Duration, error) {
	rules := make([]fuseRule, 0, 2)
	if vals[KeyFuseBurst] != "" {
		window, usd, err := ParseFuseBurst(vals[KeyFuseBurst])
		if err != nil {
			return nil, 0, fmt.Errorf("invalid %s setting %q: %w", KeyFuseBurst, vals[KeyFuseBurst], err)
		}
		rules = append(rules, fuseRule{name: "burst", window: window, capUSD: usd})
	}
	if usd, set, err := parseCap(KeyFuseHourlyUSD, vals[KeyFuseHourlyUSD]); err != nil {
		return nil, 0, err
	} else if set {
		if usd < 0.01 {
			return nil, 0, fmt.Errorf("invalid %s setting %q: limits below $0.01 are not enforceable", KeyFuseHourlyUSD, vals[KeyFuseHourlyUSD])
		}
		rules = append(rules, fuseRule{name: "hourly", window: time.Hour, capUSD: usd})
	}

	cooldown := DefaultFuseCooldown
	if raw := strings.TrimSpace(vals[KeyFuseCooldown]); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid %s setting %q: %w", KeyFuseCooldown, raw, err)
		}
		if err := ValidateFuseCooldown(parsed); err != nil {
			return nil, 0, fmt.Errorf("invalid %s setting %q: %w", KeyFuseCooldown, raw, err)
		}
		cooldown = parsed
	}
	return rules, cooldown, nil
}

func rollingStart(now time.Time, window time.Duration) time.Time {
	// Request timestamps are persisted at RFC3339 second precision. Matching
	// that precision avoids pretending sub-second cutoff accuracy exists.
	return now.Add(-window).Truncate(time.Second)
}

type fuseStatusReader interface {
	statusReader
	BudgetUsageSinceMulti([]time.Time) ([]store.BudgetUsage, error)
	BudgetUsageWindows([]time.Time, time.Duration) ([]store.BudgetUsage, error)
}

// FuseStatus reports every configured rolling/baseline rule, the request
// fan-out breaker, and any live cooldown.
func FuseStatus(s fuseStatusReader, now time.Time) (FuseSnapshot, error) {
	vals, err := s.GetSettings(fuseSettingKeys()...)
	if err != nil {
		return FuseSnapshot{}, err
	}
	rules, cooldown, err := fuseRulesFromSettings(vals)
	if err != nil {
		return FuseSnapshot{}, err
	}
	baseline, err := ParseFuseBaseline(vals[KeyFuseBaseline])
	if err != nil {
		return FuseSnapshot{}, err
	}
	var fanoutWindow time.Duration
	var fanoutLimit int64
	if raw := strings.TrimSpace(vals[KeyFuseFanout]); raw != "" {
		fanoutWindow, fanoutLimit, err = ParseFuseFanout(raw)
		if err != nil {
			return FuseSnapshot{}, fmt.Errorf("invalid %s setting %q: %w", KeyFuseFanout, raw, err)
		}
	}

	out := FuseSnapshot{Rules: make([]FuseRuleState, 0, len(rules)+1), Cooldown: cooldown}
	starts := make([]time.Time, 0, len(rules)+2)
	for i, rule := range rules {
		_ = i
		starts = append(starts, rollingStart(now, rule.window))
	}
	baselineIndex := -1
	if baseline != nil {
		baselineIndex = len(starts)
		starts = append(starts, baselineWindowStart(now, baseline.Window))
	}
	fanoutIndex := -1
	if fanoutLimit != 0 {
		fanoutIndex = len(starts)
		starts = append(starts, rollingStart(now, fanoutWindow))
	}
	usages, err := s.BudgetUsageSinceMulti(starts)
	if err != nil {
		return FuseSnapshot{}, err
	}
	if err := validateFuseUsages(usages); err != nil {
		return FuseSnapshot{}, err
	}
	for i, rule := range rules {
		spent := usages[i].SpentUSD
		out.Rules = append(out.Rules, FuseRuleState{
			Name: rule.name, Window: rule.window, CapUSD: rule.capUSD,
			SpentUSD: spent, StartAt: starts[i], Remaining: max(0, rule.capUSD-spent),
			ProjectedTimeToLimit: projectedTimeToLimit(spent, rule.capUSD, rule.window),
		})
	}
	if baseline != nil {
		historyStarts := baselineHistoryStarts(starts[baselineIndex], baseline.LookbackDays)
		history, err := s.BudgetUsageWindows(historyStarts, baseline.Window)
		if err != nil {
			return FuseSnapshot{}, err
		}
		median, err := medianSpend(history)
		if err != nil {
			return FuseSnapshot{}, err
		}
		capUSD := max(baseline.MinimumUSD, median*baseline.Multiplier)
		spent := usages[baselineIndex].SpentUSD
		out.Rules = append(out.Rules, FuseRuleState{
			Name: "baseline", Window: baseline.Window, CapUSD: capUSD, SpentUSD: spent,
			StartAt: starts[baselineIndex], Remaining: max(0, capUSD-spent),
			BaselineMedianUSD: median, BaselineMultiplier: baseline.Multiplier,
			// A fixed baseline slot may only be partly elapsed. Project from the
			// observed portion instead of pretending its current spend took the
			// full slot, which would overstate runway late in a hot interval.
			ProjectedTimeToLimit: projectedTimeToLimit(spent, capUSD, max(0, now.Sub(starts[baselineIndex]))),
		})
	}
	if fanoutIndex >= 0 {
		requests := usages[fanoutIndex].Requests
		out.Fanout = &FuseFanoutState{
			Window: fanoutWindow, LimitRequests: fanoutLimit, Requests: requests,
			RemainingRequests: max(0, fanoutLimit-requests),
		}
	}
	trip, err := parseFuseTrip(vals[KeyFuseTrip])
	if err != nil {
		return FuseSnapshot{}, err
	}
	if trip != nil {
		out.Tripped = now.Before(trip.Until)
		out.TripRule = trip.Rule
		out.TripStartedAt = trip.StartedAt
		out.TrippedUntil = trip.Until
		out.TripLimitUSD = trip.LimitUSD
		out.TripProjected = trip.ProjectedUSD
		out.TripLimitRequests = trip.LimitRequests
		out.TripProjectedRequests = trip.ProjectedRequests
		out.DenialMessage = trip.message(now)
	}
	return out, nil
}

func baselineWindowStart(now time.Time, window time.Duration) time.Time {
	return now.UTC().Truncate(window)
}

func baselineHistoryStarts(current time.Time, days int) []time.Time {
	out := make([]time.Time, days)
	for i := range out {
		out[i] = current.Add(-time.Duration(i+1) * 24 * time.Hour)
	}
	return out
}

func medianSpend(usages []store.BudgetUsage) (float64, error) {
	if len(usages) == 0 {
		return 0, nil
	}
	values := make([]float64, len(usages))
	for i := range usages {
		if math.IsNaN(usages[i].SpentUSD) || math.IsInf(usages[i].SpentUSD, 0) || usages[i].SpentUSD < 0 ||
			usages[i].EnforcementGaps < 0 || usages[i].Requests < 0 {
			return 0, fmt.Errorf("baseline usage contains invalid aggregate values")
		}
		values[i] = usages[i].SpentUSD
	}
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle], nil
	}
	median := values[middle-1]/2 + values[middle]/2
	if math.IsInf(median, 0) || math.IsNaN(median) {
		return 0, fmt.Errorf("baseline median overflowed")
	}
	return median, nil
}

func validateFuseUsages(usages []store.BudgetUsage) error {
	for _, usage := range usages {
		if math.IsNaN(usage.SpentUSD) || math.IsInf(usage.SpentUSD, 0) || usage.SpentUSD < 0 ||
			usage.EnforcementGaps < 0 || usage.Requests < 0 {
			return fmt.Errorf("fuse usage contains invalid aggregate values")
		}
	}
	return nil
}

func projectedTimeToLimit(spent, limit float64, window time.Duration) time.Duration {
	if spent <= 0 || limit <= spent || window <= 0 {
		return 0
	}
	seconds := (limit - spent) / (spent / window.Seconds())
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > float64((365*24*time.Hour)/time.Second) {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func (g *Guard) clearBaselineCacheLocked() {
	g.baselineCacheRaw = ""
	g.baselineCacheStart = 0
	g.baselineCacheRevision = 0
	g.baselineCacheMedian = 0
}

func (g *Guard) baselineMedianLocked(raw string, policy FuseBaselinePolicy, currentStart time.Time) (float64, error) {
	for attempt := 0; attempt < 3; attempt++ {
		revision := g.S.RequestRevision()
		if revision%2 != 0 {
			return 0, fmt.Errorf("request ledger mutation is in progress; baseline admission failed closed")
		}
		if g.baselineCacheRaw == raw && g.baselineCacheStart == currentStart.UnixNano() &&
			g.baselineCacheRevision == revision {
			return g.baselineCacheMedian, nil
		}
		history, err := g.S.BudgetUsageWindows(baselineHistoryStarts(currentStart, policy.LookbackDays), policy.Window)
		if err != nil {
			return 0, err
		}
		if current := g.S.RequestRevision(); current != revision || current%2 != 0 {
			g.clearBaselineCacheLocked()
			continue
		}
		median, err := medianSpend(history)
		if err != nil {
			return 0, err
		}
		g.baselineCacheRaw = raw
		g.baselineCacheStart = currentStart.UnixNano()
		g.baselineCacheRevision = revision
		g.baselineCacheMedian = median
		return median, nil
	}
	return 0, fmt.Errorf("request ledger changed repeatedly while loading baseline; admission failed closed")
}

type fuseTrip struct {
	StartedAt         time.Time `json:"started_at"`
	Until             time.Time `json:"until"`
	Rule              string    `json:"rule"`
	Metric            string    `json:"metric,omitempty"`
	Window            string    `json:"window"`
	LimitUSD          float64   `json:"limit_usd,omitempty"`
	ProjectedUSD      float64   `json:"projected_usd,omitempty"`
	LimitRequests     int64     `json:"limit_requests,omitempty"`
	ProjectedRequests int64     `json:"projected_requests,omitempty"`
}

func parseFuseTrip(raw string) (*fuseTrip, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if err := rejectAmbiguousFuseObject(raw, map[string]struct{}{
		"started_at": {}, "until": {}, "rule": {}, "metric": {}, "window": {},
		"limit_usd": {}, "projected_usd": {}, "limit_requests": {}, "projected_requests": {},
	}); err != nil {
		return nil, fmt.Errorf("invalid %s setting: %w", KeyFuseTrip, err)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var trip fuseTrip
	if err := dec.Decode(&trip); err != nil {
		return nil, fmt.Errorf("invalid %s setting: %w", KeyFuseTrip, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("invalid %s setting: trailing JSON", KeyFuseTrip)
	}
	window, err := time.ParseDuration(trip.Window)
	if err != nil {
		return nil, fmt.Errorf("invalid %s setting: bad trip window %q", KeyFuseTrip, trip.Window)
	}
	if trip.Metric == "" {
		trip.Metric = "usd"
	}
	if trip.Rule != "burst" && trip.Rule != "hourly" && trip.Rule != "baseline" && trip.Rule != "fanout" {
		return nil, fmt.Errorf("invalid %s setting: bad trip rule %q", KeyFuseTrip, trip.Rule)
	}
	if trip.Rule == "hourly" && window != time.Hour {
		return nil, fmt.Errorf("invalid %s setting: hourly trip window is %q", KeyFuseTrip, trip.Window)
	}
	switch trip.Rule {
	case "burst", "hourly", "fanout":
		if ValidateFuseWindow(window) != nil {
			return nil, fmt.Errorf("invalid %s setting: bad trip window %q", KeyFuseTrip, trip.Window)
		}
	case "baseline":
		if window < MinBaselineWindow || window > MaxBaselineWindow || (24*time.Hour)%window != 0 {
			return nil, fmt.Errorf("invalid %s setting: bad baseline trip window %q", KeyFuseTrip, trip.Window)
		}
	}
	if trip.StartedAt.IsZero() || trip.Until.IsZero() || !trip.Until.After(trip.StartedAt) || trip.Until.Sub(trip.StartedAt) > MaxFuseCooldown {
		return nil, fmt.Errorf("invalid %s setting: bad cooldown timestamps", KeyFuseTrip)
	}
	switch trip.Metric {
	case "usd":
		if trip.Rule == "fanout" || trip.LimitUSD < 0.01 || math.IsNaN(trip.LimitUSD) || math.IsInf(trip.LimitUSD, 0) ||
			trip.ProjectedUSD < 0 || math.IsNaN(trip.ProjectedUSD) || math.IsInf(trip.ProjectedUSD, 0) {
			return nil, fmt.Errorf("invalid %s setting: bad dollar values", KeyFuseTrip)
		}
		if trip.ProjectedUSD+1e-12 < trip.LimitUSD {
			return nil, fmt.Errorf("invalid %s setting: projected spend is below the trip limit", KeyFuseTrip)
		}
	case "requests":
		if trip.Rule != "fanout" || trip.LimitRequests < 1 || trip.LimitRequests > MaxFanoutRequests ||
			trip.ProjectedRequests < trip.LimitRequests {
			return nil, fmt.Errorf("invalid %s setting: bad request values", KeyFuseTrip)
		}
	default:
		return nil, fmt.Errorf("invalid %s setting: bad metric %q", KeyFuseTrip, trip.Metric)
	}
	return &trip, nil
}

func rejectAmbiguousFuseObject(raw string, allowed map[string]struct{}) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	opening, err := dec.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	seen := make(map[string]string, len(allowed))
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("object key is not a string")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown or non-canonical field %q", key)
		}
		folded := strings.ToLower(key)
		if previous, duplicate := seen[folded]; duplicate {
			return fmt.Errorf("duplicate or case-ambiguous fields %q and %q", previous, key)
		}
		seen[folded] = key
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
	}
	closing, err := dec.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return fmt.Errorf("expected a JSON object terminator")
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func (t *fuseTrip) message(now time.Time) string {
	until := t.Until.In(now.Location()).Format("15:04:05 MST")
	if t.Metric == "requests" {
		return fmt.Sprintf(
			"request fan-out fuse tripped: projected %d requests in a rolling %s window reached the limit of %d; new requests are paused until %s. Reset early with `burnban fuse --reset` (it will retrip if fan-out is still high).",
			t.ProjectedRequests, t.Window, t.LimitRequests, until)
	}
	return fmt.Sprintf(
		"spend-velocity fuse tripped: projected $%.4f in a rolling %s window reached the %s limit of $%.2f; new spend is paused until %s. Reset early with `burnban fuse --reset` (it will retrip if velocity is still high).",
		t.ProjectedUSD, t.Window, t.Rule, t.LimitUSD, until)
}

func (t *fuseTrip) denial(now time.Time) *Denial {
	return &Denial{
		Type: "burnban_fuse_tripped", Message: t.message(now), Window: "fuse_" + t.Rule,
		AlertKey: KeyFuseAlertedPrefix + t.StartedAt.UTC().Format("20060102T150405.000000000Z"),
	}
}

func (g *Guard) tripFuseLocked(now time.Time, position capPosition, additionalUSD float64) (*Denial, error) {
	projected := position.spent + position.reserved + max(0, additionalUSD)
	trip := fuseTrip{
		StartedAt: now.UTC(), Until: now.Add(position.fuseCooldown).UTC(),
		Rule: position.fuseName, Metric: "usd", Window: FormatFuseDuration(position.fuseWindow),
		LimitUSD: position.cap, ProjectedUSD: projected,
	}
	raw, err := json.Marshal(trip)
	if err != nil {
		return nil, err
	}
	// Incident alert marks are unique so an in-flight webhook from one trip
	// cannot suppress the next. Prune older marks here to keep settings growth
	// bounded even under an aggressively short operator-configured cooldown.
	if err := g.S.DeleteSettingsWithPrefix(KeyFuseAlertedPrefix); err != nil {
		return nil, err
	}
	if err := g.S.SetSetting(KeyFuseTrip, string(raw)); err != nil {
		return nil, err
	}
	return trip.denial(now), nil
}

func (g *Guard) tripFuseRequestsLocked(now time.Time, window time.Duration, cooldown time.Duration, limit, projected int64) (*Denial, error) {
	trip := fuseTrip{
		StartedAt: now.UTC(), Until: now.Add(cooldown).UTC(), Rule: "fanout", Metric: "requests",
		Window: FormatFuseDuration(window), LimitRequests: limit, ProjectedRequests: projected,
	}
	raw, err := json.Marshal(trip)
	if err != nil {
		return nil, err
	}
	if err := g.S.DeleteSettingsWithPrefix(KeyFuseAlertedPrefix); err != nil {
		return nil, err
	}
	if err := g.S.SetSetting(KeyFuseTrip, string(raw)); err != nil {
		return nil, err
	}
	return trip.denial(now), nil
}
