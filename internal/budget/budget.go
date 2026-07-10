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

// Check returns a non-nil Denial when the burn ban is active or today's
// spend has reached the daily cap. A nil, nil return means proceed.
func (g *Guard) Check(now time.Time) (*Denial, error) {
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

	capStr, err := g.S.GetSetting(KeyDailyCapUSD)
	if err != nil {
		return nil, err
	}
	if capStr == "" {
		return nil, nil
	}
	capUSD, err := strconv.ParseFloat(capStr, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid %s setting %q", KeyDailyCapUSD, capStr)
	}

	if ov, err := g.S.GetSetting(KeyOverrideDay); err != nil {
		return nil, err
	} else if ov == now.Format("2006-01-02") {
		return nil, nil
	}

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
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
	return nil, nil
}
