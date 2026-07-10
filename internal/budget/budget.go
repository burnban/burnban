// Package budget decides whether a request may spend money right now:
// it enforces the manual burn ban and the daily/weekly/monthly dollar caps,
// and reports when spend crosses the early-warning threshold. It is on the
// request path, so reads are batched: one settings query, one ledger scan.
package budget

import (
	"fmt"
	"strconv"
	"time"

	"github.com/syft8/burnban/internal/store"
)

// Settings keys shared between the proxy and the CLI.
const (
	KeyDailyCapUSD   = "cap_daily_usd"
	KeyWeeklyCapUSD  = "cap_weekly_usd"
	KeyMonthlyCapUSD = "cap_monthly_usd"
	KeyBanActive     = "ban_active"
	KeyOverrideDay   = "cap_override_day"
	KeyWebhookURL    = "webhook_url"
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
	Name  string // "daily", "weekly", "monthly" — also the cap flag name
	Key   string // settings key holding the cap
	Start func(now time.Time) time.Time
	Reset string // when the window rolls over, for human-facing copy
}

// Windows lists the enforced budget windows, tightest first. It is the
// single source of truth: the CLI, MCP, dashboard, and metrics all derive
// their window vocabulary from it.
func Windows() []Window {
	return []Window{
		{"daily", KeyDailyCapUSD, DayStart, "at midnight"},
		{"weekly", KeyWeeklyCapUSD, WeekStart, "Monday"},
		{"monthly", KeyMonthlyCapUSD, MonthStart, "on the 1st"},
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
	Window            // embedded definition (Name, Key, Start, Reset)
	CapUSD  float64   // 0 when unset
	Set     bool      // a valid cap is configured
	Spent   float64   // spend since the window opened
	StartAt time.Time // when the window opened, at the queried instant
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
	keys := make([]string, len(wins))
	starts := make([]time.Time, len(wins))
	for i, w := range wins {
		keys[i] = w.Key
		starts[i] = w.Start(now)
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
		st := WindowState{Window: w, Spent: spents[i], StartAt: starts[i]}
		if cap, ok, err := parseCap(w.Key, vals[w.Key]); err != nil {
			return nil, err
		} else if ok {
			st.CapUSD, st.Set = cap, true
		}
		out[i] = st
	}
	return out, nil
}

// Check returns a non-nil Denial when the burn ban is active, or when any
// window's spend has reached its cap, or the calling agent has reached its
// own daily cap. A nil, nil return means proceed.
func (g *Guard) Check(now time.Time, agent string) (*Denial, error) {
	keys := []string{KeyBanActive, KeyOverrideDay}
	agentKey := ""
	if agent != "" {
		agentKey = KeyAgentCapPrefix + agent
		keys = append(keys, agentKey)
	}
	for _, w := range Windows() {
		keys = append(keys, w.Key)
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
	if vals[KeyOverrideDay] == now.Format("2006-01-02") {
		return nil, nil
	}

	// One scan covers every configured window.
	var capped []Window
	var caps []float64
	var starts []time.Time
	for _, w := range Windows() {
		cap, ok, err := parseCap(w.Key, vals[w.Key])
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		capped = append(capped, w)
		caps = append(caps, cap)
		starts = append(starts, w.Start(now))
	}
	if len(capped) > 0 {
		spents, err := g.S.SpentSinceMulti(starts)
		if err != nil {
			return nil, err
		}
		for i, w := range capped {
			if spents[i] >= caps[i] {
				return &Denial{
					Type: "burnban_cap_reached",
					Message: fmt.Sprintf(
						"%s burn cap reached: $%.2f spent of $%.2f (resets %s). Raise it (`burnban cap --%s %.0f`) or override for today (`burnban lift --today`).",
						w.Name, spents[i], caps[i], w.Reset, w.Name, caps[i]*2),
					Window:      w.Name,
					WindowStart: starts[i],
				}, nil
			}
		}
	}

	if agentKey == "" {
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
		if perr != nil {
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
	if perr != nil {
		return 0, false, fmt.Errorf("invalid %s setting %q", key, s)
	}
	if v <= 0 {
		return 0, false, nil
	}
	return v, true, nil
}
