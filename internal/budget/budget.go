// Package budget decides whether a request may spend money right now:
// it enforces the manual burn ban and the daily dollar cap.
package budget

import (
	"fmt"
	"strconv"
	"time"

	"github.com/syft8/burnban/internal/store"
)

// Settings keys shared between the proxy and the CLI.
const (
	KeyDailyCapUSD = "cap_daily_usd"
	KeyBanActive   = "ban_active"
	KeyOverrideDay = "cap_override_day"
	KeyWebhookURL  = "webhook_url"
	KeyAlertedDay  = "alert_sent_day"
	// KeyAgentCapPrefix + <agent name> holds that agent's own daily cap.
	KeyAgentCapPrefix = "cap_agent_daily_usd:"
)

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

// Check returns a non-nil Denial when the burn ban is active, or when
// today's spend has reached the global daily cap or the calling agent's
// own cap. A nil, nil return means proceed.
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

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	if capStr, err := g.S.GetSetting(KeyDailyCapUSD); err != nil {
		return nil, err
	} else if capStr != "" {
		capUSD, perr := strconv.ParseFloat(capStr, 64)
		if perr != nil {
			return nil, fmt.Errorf("invalid %s setting %q", KeyDailyCapUSD, capStr)
		}
		spent, err := g.S.SpentSince(midnight)
		if err != nil {
			return nil, err
		}
		if spent >= capUSD {
			return &Denial{
				Type: "burnban_cap_reached",
				Message: fmt.Sprintf(
					"daily burn cap reached: $%.2f spent of $%.2f. Raise it (`burnban cap --daily %.0f`) or override for today (`burnban lift --today`).",
					spent, capUSD, capUSD*2),
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
	spent, err := g.S.SpentSinceForAgent(midnight, agent)
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
