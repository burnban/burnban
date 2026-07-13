package store

import (
	"fmt"
	"strings"
	"time"
)

// BudgetUsageWindows returns usage inside each exact [start,start+window)
// interval. It is used by the deterministic same-time-of-day baseline fuse;
// explicit upper bounds keep activity in adjacent periods out of the sample.
// All windows are read in one bounded query.
func (s *Store) BudgetUsageWindows(starts []time.Time, window time.Duration) ([]BudgetUsage, error) {
	if len(starts) == 0 {
		return nil, nil
	}
	if window < time.Second || window > 24*time.Hour {
		return nil, fmt.Errorf("budget usage window must be between 1s and 24h")
	}
	if len(starts) > 90 {
		return nil, fmt.Errorf("budget usage window count exceeds 90")
	}

	minimum, maximum := starts[0], starts[0].Add(window)
	columns := make([]string, 0, len(starts)*3)
	args := make([]any, 0, len(starts)*6+2)
	for _, start := range starts {
		if start.IsZero() {
			return nil, fmt.Errorf("budget usage window start must not be zero")
		}
		end := start.Add(window)
		if start.Before(minimum) {
			minimum = start
		}
		if end.After(maximum) {
			maximum = end
		}
		startText, endText := start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)
		columns = append(columns,
			"COALESCE(SUM(CASE WHEN ts >= ? AND ts < ? THEN cost_usd ELSE 0 END),0)",
			"COALESCE(SUM(CASE WHEN ts >= ? AND ts < ? THEN enforcement_unsafe ELSE 0 END),0)",
			"COALESCE(SUM(CASE WHEN ts >= ? AND ts < ? THEN 1 ELSE 0 END),0)")
		args = append(args, startText, endText, startText, endText, startText, endText)
	}
	args = append(args, minimum.UTC().Format(time.RFC3339), maximum.UTC().Format(time.RFC3339))

	out := make([]BudgetUsage, len(starts))
	destinations := make([]any, len(starts)*3)
	for i := range out {
		destinations[i*3] = &out[i].SpentUSD
		destinations[i*3+1] = &out[i].EnforcementGaps
		destinations[i*3+2] = &out[i].Requests
	}
	err := s.readQueryer().QueryRow(`SELECT `+strings.Join(columns, ", ")+
		` FROM requests WHERE ts >= ? AND ts < ?`, args...).Scan(destinations...)
	return out, err
}
