// Package budget decides whether a request may spend money right now:
// it enforces the manual burn ban and the daily/weekly/monthly dollar caps,
// and reports when spend crosses the early-warning threshold.
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
	KeyAlertedDay    = "alert_sent_day"
	// KeyWarnPct holds the early-warning threshold as a percentage of any
	// window's cap. Empty means DefaultWarnPct; "0" disables warnings.
	KeyWarnPct = "warn_pct"
	// KeyWarnedPrefix + "<window>:<window start date>" marks that window
	// instance as already warned about, so each fires at most once.
	KeyWarnedPrefix = "warned:"
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

// Windows lists the enforced budget windows, tightest first.
func Windows() []Window {
	return []Window{
		{"daily", KeyDailyCapUSD, DayStart, "at midnight"},
		{"weekly", KeyWeeklyCapUSD, WeekStart, "Monday"},
		{"monthly", KeyMonthlyCapUSD, MonthStart, "on the 1st"},
	}
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
// will show the user verbatim.
type Denial struct {
	Type    string
	Message string
}

func (d *Denial) Error() string { return d.Message }

// Check returns a non-nil Denial when the burn ban is active, or when any
// window's spend has reached its cap, or the calling agent has reached its
// own daily cap. A nil, nil return means proceed.
func (g *Guard) Check(now time.Time, agent string) (*Denial, error) {
	ban, err := g.S.GetSetting(KeyBanActive)
	if err != nil {
		return nil, err
	}
	if ban == "1" {
		return &Denial{
			Type:    "burnban_banned",
			Message: "burn ban in effect: all agent spend is paused. Lift it with `burnban lift`.",
		}, nil
	}

	if ov, err := g.S.GetSetting(KeyOverrideDay); err != nil {
		return nil, err
	} else if ov == now.Format("2006-01-02") {
		return nil, nil
	}

	for _, w := range Windows() {
		capUSD, ok, err := g.capFor(w.Key)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		spent, err := g.S.SpentSince(w.Start(now))
		if err != nil {
			return nil, err
		}
		if spent >= capUSD {
			return &Denial{
				Type: "burnban_cap_reached",
				Message: fmt.Sprintf(
					"%s burn cap reached: $%.2f spent of $%.2f (resets %s). Raise it (`burnban cap --%s %.0f`) or override for today (`burnban lift --today`).",
					w.Name, spent, capUSD, w.Reset, w.Name, capUSD*2),
			}, nil
		}
	}

	if agent == "" {
		return nil, nil
	}
	capStr, err := g.S.GetSetting(KeyAgentCapPrefix + agent)
	if err != nil {
		return nil, err
	}
	if capStr == "" {
		return nil, nil
	}
	capUSD, perr := strconv.ParseFloat(capStr, 64)
	if perr != nil {
		return nil, fmt.Errorf("invalid agent cap for %q: %q", agent, capStr)
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
	threshold := DefaultWarnPct
	if pctStr, err := g.S.GetSetting(KeyWarnPct); err != nil {
		return nil, err
	} else if pctStr != "" {
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
	for _, w := range Windows() {
		capUSD, ok, err := g.capFor(w.Key)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		spent, err := g.S.SpentSince(w.Start(now))
		if err != nil {
			return nil, err
		}
		pct := spent / capUSD * 100
		if pct < threshold {
			continue
		}
		mark := KeyWarnedPrefix + w.Name + ":" + w.Start(now).Format("2006-01-02")
		if done, err := g.S.GetSetting(mark); err != nil {
			return nil, err
		} else if done == "1" {
			continue
		}
		if worst == nil || pct > worst.Pct {
			worst = &Warning{Window: w.Name, Pct: pct, Spent: spent, Cap: capUSD, MarkKey: mark, Reset: w.Reset}
		}
	}
	return worst, nil
}

func (g *Guard) capFor(key string) (float64, bool, error) {
	s, err := g.S.GetSetting(key)
	if err != nil || s == "" {
		return 0, false, err
	}
	v, perr := strconv.ParseFloat(s, 64)
	if perr != nil {
		return 0, false, fmt.Errorf("invalid %s setting %q", key, s)
	}
	return v, true, nil
}
