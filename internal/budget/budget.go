// Package budget decides whether a request may spend money right now:
// it enforces the manual burn ban and the daily/weekly/monthly dollar caps,
// and reports when spend crosses the early-warning threshold. It is on the
// request path, so reads are batched: one settings query, one ledger scan.
package budget

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/burnban/burnban/internal/store"
)

// Settings keys shared between the proxy and the CLI.
const (
	KeyDailyCapUSD   = "cap_daily_usd"
	KeyWeeklyCapUSD  = "cap_weekly_usd"
	KeyMonthlyCapUSD = "cap_monthly_usd"
	KeyBanActive     = "ban_active"
	KeyOverrideDay   = "cap_override_day"
	KeyWebhookURL    = "webhook_url"
	// External policy is a local SQLite contract for optional sidecars. The
	// MIT binary reads these keys but contains no sync client or remote URL.
	KeyExternalDailyCapUSD   = "external_cap_daily_usd"
	KeyExternalWeeklyCapUSD  = "external_cap_weekly_usd"
	KeyExternalMonthlyCapUSD = "external_cap_monthly_usd"
	KeyExternalBanActive     = "external_ban_active"
	KeyExternalPolicyVersion = "external_policy_version"
	KeyExternalPolicySource  = "external_policy_source"
	KeyExternalPolicyAt      = "external_policy_updated_at"
	// KeyWarnPct holds the early-warning threshold as a percentage of any
	// window's cap. Empty means DefaultWarnPct; "0" disables warnings.
	KeyWarnPct = "warn_pct"
	// KeyWarnedPrefix + "<window>:<window start date>" marks that window
	// instance as already warned about; KeyAlertedPrefix marks its
	// cap-reached alert as sent. Both are cleared when the cap changes.
	KeyWarnedPrefix  = "warned:"
	KeyAlertedPrefix = "alerted:"
	// KeyAgentCapPrefix + <agent name> holds that agent's own daily cap.
	KeyAgentCapPrefix = "cap_agent_daily_usd:"
)

// DefaultWarnPct is the warning threshold used when a webhook is configured
// but warn_pct was never set explicitly.
const DefaultWarnPct = 80.0

// Window is one budget enforcement window. Starts are computed in local
// time, matching how people read their bills.
type Window struct {
	Name        string // "daily", "weekly", "monthly" — also the cap flag name
	Key         string // settings key holding the user's local cap
	ExternalKey string // optional sidecar-managed allocation
	Start       func(now time.Time) time.Time
	Reset       string // when the window rolls over, for human-facing copy
}

// Windows lists the enforced budget windows, tightest first. It is the
// single source of truth: the CLI, MCP, dashboard, and metrics all derive
// their window vocabulary from it.
func Windows() []Window {
	return []Window{
		{"daily", KeyDailyCapUSD, KeyExternalDailyCapUSD, DayStart, "at midnight"},
		{"weekly", KeyWeeklyCapUSD, KeyExternalWeeklyCapUSD, WeekStart, "Monday"},
		{"monthly", KeyMonthlyCapUSD, KeyExternalMonthlyCapUSD, MonthStart, "on the 1st"},
	}
}

// WindowByName resolves a window name (empty means daily), so frontends
// never hand-copy the name→key mapping.
func WindowByName(name string) (Window, bool) {
	if name == "" {
		name = "daily"
	}
	for _, w := range Windows() {
		if w.Name == name {
			return w, true
		}
	}
	return Window{}, false
}

func DayStart(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// WeekStart is the most recent Monday at 00:00 — ISO weeks, like invoices.
func WeekStart(now time.Time) time.Time {
	d := DayStart(now)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

func MonthStart(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
}

type Guard struct {
	S *store.Store
}

// Denial explains why spend is paused, in words an agent's error surface
// will show the user verbatim. Window and WindowStart identify which cap
// tripped so alerting can dedup per window instance, not per day.
type Denial struct {
	Type        string
	Message     string
	Window      string
	WindowStart time.Time
}

func (d *Denial) Error() string { return d.Message }

// AlertMark is the settings key that dedups this denial's webhook alert.
// Empty for denial types that don't alert (ban, agent caps).
func (d *Denial) AlertMark() string {
	if d.Window == "" || d.WindowStart.IsZero() {
		return ""
	}
	return KeyAlertedPrefix + d.Window + ":" + d.WindowStart.Format("2006-01-02")
}

// WindowState is one window's live budget position, shared by the CLI
// status view, dashboard, metrics, and MCP so they can never disagree.
type WindowState struct {
	Window                   // embedded definition (Name, Key, Start, Reset)
	CapUSD         float64   // effective (stricter) cap; 0 when unset
	LocalCapUSD    float64   // user-managed local cap; 0 when unset
	ExternalCapUSD float64   // sidecar-managed allocation; 0 when unset
	LocalSpent     float64   // spend in the machine-local calendar window
	ExternalSpent  float64   // spend in the organization UTC calendar window
	Source         string    // "local", "external", or "both"
	Set            bool      // a valid effective cap is configured
	Spent          float64   // spend since the window opened
	StartAt        time.Time // when the window opened, at the queried instant
}

// Pct is spend as a percentage of the cap; 0 when no cap is set.
func (w WindowState) Pct() float64 {
	if !w.Set || w.CapUSD <= 0 {
		return 0
	}
	return w.Spent / w.CapUSD * 100
}

// Status reports every window's cap and live spend with one settings query
// and one ledger scan.
func Status(s *store.Store, now time.Time) ([]WindowState, error) {
	wins := Windows()
	keys := make([]string, 0, len(wins)*2)
	starts := make([]time.Time, 0, len(wins)*2)
	for _, w := range wins {
		keys = append(keys, w.Key, w.ExternalKey)
		starts = append(starts, w.Start(now), externalStart(w, now))
	}
	vals, err := s.GetSettings(keys...)
	if err != nil {
		return nil, err
	}
	spents, err := s.SpentSinceMulti(starts)
	if err != nil {
		return nil, err
	}
	out := make([]WindowState, len(wins))
	for i, w := range wins {
		localStart, externalStartAt := starts[i*2], starts[i*2+1]
		st := WindowState{
			Window: w, LocalSpent: spents[i*2], ExternalSpent: spents[i*2+1],
			Spent: spents[i*2], StartAt: localStart,
		}
		local, localSet, err := parseCap(w.Key, vals[w.Key])
		if err != nil {
			return nil, err
		}
		external, externalSet, err := parseCap(w.ExternalKey, vals[w.ExternalKey])
		if err != nil {
			return nil, err
		}
		st.LocalCapUSD, st.ExternalCapUSD = local, external
		selectEffectiveState(&st, local, localSet, external, externalSet, localStart, externalStartAt)
		out[i] = st
	}
	return out, nil
}

// Check returns a non-nil Denial when the burn ban is active, or when any
// window's spend has reached its cap, or the calling agent has reached its
// own daily cap. A nil, nil return means proceed.
func (g *Guard) Check(now time.Time, agent string) (*Denial, error) {
	keys := []string{KeyBanActive, KeyExternalBanActive, KeyOverrideDay}
	agentKey := ""
	if agent != "" {
		agentKey = KeyAgentCapPrefix + agent
		keys = append(keys, agentKey)
	}
	for _, w := range Windows() {
		keys = append(keys, w.Key, w.ExternalKey)
	}
	vals, err := g.S.GetSettings(keys...)
	if err != nil {
		return nil, err
	}

	if vals[KeyBanActive] == "1" {
		return &Denial{
			Type:    "burnban_banned",
			Message: "burn ban in effect: all agent spend is paused. Lift it with `burnban lift`.",
		}, nil
	}
	if vals[KeyExternalBanActive] == "1" {
		return &Denial{
			Type:    "burnban_external_ban",
			Message: "organization burn ban in effect: spend is paused by external policy. Contact your Burnban administrator.",
		}, nil
	}
	overrideLocal := vals[KeyOverrideDay] == now.Format("2006-01-02")

	// Local caps use the machine's calendar; external fleet allocations use
	// UTC so every meter enforces the same organization window. One scan still
	// covers every configured cutoff.
	type capCheck struct {
		window Window
		cap    float64
		start  time.Time
		source string
	}
	var checks []capCheck
	for _, w := range Windows() {
		local, localSet, err := parseCap(w.Key, vals[w.Key])
		if err != nil {
			return nil, err
		}
		if overrideLocal {
			local, localSet = 0, false
		}
		external, externalSet, err := parseCap(w.ExternalKey, vals[w.ExternalKey])
		if err != nil {
			return nil, err
		}
		if localSet {
			checks = append(checks, capCheck{window: w, cap: local, start: w.Start(now), source: "local"})
		}
		if externalSet {
			checks = append(checks, capCheck{window: w, cap: external, start: externalStart(w, now), source: "external"})
		}
	}
	if len(checks) > 0 {
		starts := make([]time.Time, len(checks))
		for i, check := range checks {
			starts[i] = check.start
		}
		spents, err := g.S.SpentSinceMulti(starts)
		if err != nil {
			return nil, err
		}
		for i, check := range checks {
			if spents[i] >= check.cap {
				if check.source == "external" {
					return &Denial{
						Type: "burnban_external_cap_reached",
						Message: fmt.Sprintf(
							"organization %s burn allocation reached: $%.2f spent of $%.2f (resets on the UTC boundary). Contact your Burnban administrator.",
							check.window.Name, spents[i], check.cap),
						Window: check.window.Name, WindowStart: check.start,
					}, nil
				}
				return &Denial{
					Type: "burnban_cap_reached",
					Message: fmt.Sprintf(
						"%s burn cap reached: $%.2f spent of $%.2f (resets %s). Raise it (`burnban cap --%s %.0f`) or override for today (`burnban lift --today`).",
						check.window.Name, spents[i], check.cap, check.window.Reset, check.window.Name, check.cap*2),
					Window:      check.window.Name,
					WindowStart: check.start,
				}, nil
			}
		}
	}

	if agentKey == "" || overrideLocal {
		return nil, nil
	}
	capUSD, ok, err := parseCap(agentKey, vals[agentKey])
	if err != nil || !ok {
		return nil, err
	}
	spent, err := g.S.SpentSinceForAgent(DayStart(now), agent)
	if err != nil {
		return nil, err
	}
	if spent >= capUSD {
		return &Denial{
			Type: "burnban_agent_cap_reached",
			Message: fmt.Sprintf(
				"daily cap for agent %q reached: $%.2f spent of $%.2f. Raise it: `burnban cap --agent %s --daily %.0f`.",
				agent, spent, capUSD, agent, capUSD*2),
		}, nil
	}
	return nil, nil
}

// BanStatus reports the independently managed local and external ban states.
func BanStatus(s *store.Store) (local, external bool, err error) {
	vals, err := s.GetSettings(KeyBanActive, KeyExternalBanActive)
	if err != nil {
		return false, false, err
	}
	return vals[KeyBanActive] == "1", vals[KeyExternalBanActive] == "1", nil
}

func selectEffectiveState(st *WindowState, local float64, localSet bool, external float64, externalSet bool, localStart, externalStartAt time.Time) {
	switch {
	case localSet && externalSet:
		localPct := st.LocalSpent / local
		externalPct := st.ExternalSpent / external
		switch {
		case localPct > externalPct:
			st.CapUSD, st.Spent, st.StartAt, st.Source = local, st.LocalSpent, localStart, "local"
		case externalPct > localPct:
			st.CapUSD, st.Spent, st.StartAt, st.Source = external, st.ExternalSpent, externalStartAt, "external"
		default:
			if local <= external {
				st.CapUSD, st.Spent, st.StartAt = local, st.LocalSpent, localStart
			} else {
				st.CapUSD, st.Spent, st.StartAt = external, st.ExternalSpent, externalStartAt
			}
			st.Source = "both"
		}
		st.Set = true
	case localSet:
		st.CapUSD, st.Spent, st.StartAt, st.Source, st.Set = local, st.LocalSpent, localStart, "local", true
	case externalSet:
		st.CapUSD, st.Spent, st.StartAt, st.Source, st.Set = external, st.ExternalSpent, externalStartAt, "external", true
	}
}

func externalStart(w Window, now time.Time) time.Time {
	utc := now.UTC()
	switch w.Name {
	case "daily":
		return DayStart(utc)
	case "weekly":
		return WeekStart(utc)
	default:
		return MonthStart(utc)
	}
}

// Warning reports a window at or past the early-warning threshold.
type Warning struct {
	Window     string
	Pct        float64
	Spent, Cap float64
	MarkKey    string // settings key that dedups this window instance
	Reset      string
}

// WarnStatus returns the most-burned window at or past the warning
// threshold that has not been warned about yet, or nil. The caller owns
// setting MarkKey and delivering the message — the guard only decides.
func (g *Guard) WarnStatus(now time.Time) (*Warning, error) {
	states, err := Status(g.S, now)
	if err != nil {
		return nil, err
	}
	markKeys := make([]string, 0, len(states))
	for _, st := range states {
		markKeys = append(markKeys, KeyWarnedPrefix+st.Name+":"+st.StartAt.Format("2006-01-02"))
	}
	vals, err := g.S.GetSettings(append(markKeys, KeyWarnPct)...)
	if err != nil {
		return nil, err
	}

	threshold := DefaultWarnPct
	if pctStr := vals[KeyWarnPct]; pctStr != "" {
		v, perr := strconv.ParseFloat(pctStr, 64)
		if perr != nil || math.IsNaN(v) || math.IsInf(v, 0) || v > 100 {
			return nil, fmt.Errorf("invalid %s setting %q", KeyWarnPct, pctStr)
		}
		threshold = v
	}
	if threshold <= 0 {
		return nil, nil
	}

	var worst *Warning
	for i, st := range states {
		if !st.Set || st.Pct() < threshold || vals[markKeys[i]] == "1" {
			continue
		}
		if worst == nil || st.Pct() > worst.Pct {
			worst = &Warning{
				Window: st.Name, Pct: st.Pct(), Spent: st.Spent, Cap: st.CapUSD,
				MarkKey: markKeys[i], Reset: st.Reset,
			}
		}
	}
	return worst, nil
}

// ClearMarks forgets a window's sent warning and alert, re-arming both.
// Called whenever that window's cap is set, raised, or removed.
func ClearMarks(s *store.Store, window string) error {
	if err := s.DeleteSettingsWithPrefix(KeyWarnedPrefix + window + ":"); err != nil {
		return err
	}
	return s.DeleteSettingsWithPrefix(KeyAlertedPrefix + window + ":")
}

// parseCap interprets a stored cap. Values ≤ 0 are treated as unset rather
// than as a cap that denies everything: a zero cap is never what a user
// meant, and it would otherwise poison percentage math downstream.
func parseCap(key, s string) (float64, bool, error) {
	if s == "" {
		return 0, false, nil
	}
	v, perr := strconv.ParseFloat(s, 64)
	if perr != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false, fmt.Errorf("invalid %s setting %q", key, s)
	}
	if v <= 0 {
		return 0, false, nil
	}
	return v, true, nil
}
