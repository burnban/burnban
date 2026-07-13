package budget

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// Fuse settings are local safety policy. Unlike calendar caps, fuse rules use
// rolling windows and persist a short cooldown when they trip so a runaway
// loop cannot resume merely by restarting the proxy.
const (
	KeyFuseHourlyUSD = "fuse_hourly_usd"
	KeyFuseBurst     = "fuse_burst"
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
	Rules         []FuseRuleState
	Cooldown      time.Duration
	Tripped       bool
	TripRule      string
	TripStartedAt time.Time
	TrippedUntil  time.Time
	TripLimitUSD  float64
	TripProjected float64
	DenialMessage string
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
	return []string{KeyFuseHourlyUSD, KeyFuseBurst, KeyFuseCooldown, KeyFuseTrip}
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

// FuseStatus reports every configured rolling rule and any live cooldown.
func FuseStatus(s statusReader, now time.Time) (FuseSnapshot, error) {
	vals, err := s.GetSettings(fuseSettingKeys()...)
	if err != nil {
		return FuseSnapshot{}, err
	}
	rules, cooldown, err := fuseRulesFromSettings(vals)
	if err != nil {
		return FuseSnapshot{}, err
	}
	out := FuseSnapshot{Rules: make([]FuseRuleState, 0, len(rules)), Cooldown: cooldown}
	starts := make([]time.Time, len(rules))
	for i, rule := range rules {
		starts[i] = rollingStart(now, rule.window)
	}
	spents, err := s.SpentSinceMulti(starts)
	if err != nil {
		return FuseSnapshot{}, err
	}
	for i, rule := range rules {
		out.Rules = append(out.Rules, FuseRuleState{
			Name: rule.name, Window: rule.window, CapUSD: rule.capUSD,
			SpentUSD: spents[i], StartAt: starts[i], Remaining: max(0, rule.capUSD-spents[i]),
		})
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
		out.DenialMessage = trip.message(now)
	}
	return out, nil
}

type fuseTrip struct {
	StartedAt    time.Time `json:"started_at"`
	Until        time.Time `json:"until"`
	Rule         string    `json:"rule"`
	Window       string    `json:"window"`
	LimitUSD     float64   `json:"limit_usd"`
	ProjectedUSD float64   `json:"projected_usd"`
}

func parseFuseTrip(raw string) (*fuseTrip, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
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
	if err != nil || ValidateFuseWindow(window) != nil {
		return nil, fmt.Errorf("invalid %s setting: bad trip window %q", KeyFuseTrip, trip.Window)
	}
	if trip.Rule != "burst" && trip.Rule != "hourly" {
		return nil, fmt.Errorf("invalid %s setting: bad trip rule %q", KeyFuseTrip, trip.Rule)
	}
	if trip.Rule == "hourly" && window != time.Hour {
		return nil, fmt.Errorf("invalid %s setting: hourly trip window is %q", KeyFuseTrip, trip.Window)
	}
	if trip.StartedAt.IsZero() || trip.Until.IsZero() || !trip.Until.After(trip.StartedAt) || trip.Until.Sub(trip.StartedAt) > MaxFuseCooldown {
		return nil, fmt.Errorf("invalid %s setting: bad cooldown timestamps", KeyFuseTrip)
	}
	if trip.LimitUSD < 0.01 || math.IsNaN(trip.LimitUSD) || math.IsInf(trip.LimitUSD, 0) ||
		trip.ProjectedUSD < 0 || math.IsNaN(trip.ProjectedUSD) || math.IsInf(trip.ProjectedUSD, 0) {
		return nil, fmt.Errorf("invalid %s setting: bad dollar values", KeyFuseTrip)
	}
	if trip.ProjectedUSD+1e-12 < trip.LimitUSD {
		return nil, fmt.Errorf("invalid %s setting: projected spend is below the trip limit", KeyFuseTrip)
	}
	return &trip, nil
}

func (t *fuseTrip) message(now time.Time) string {
	until := t.Until.In(now.Location()).Format("15:04:05 MST")
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
		Rule: position.fuseName, Window: FormatFuseDuration(position.fuseWindow),
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
