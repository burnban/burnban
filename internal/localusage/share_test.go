package localusage

import (
	"math"
	"testing"
	"time"
)

func TestNewShareCardNormalizesMonthlyPace(t *testing.T) {
	report := Report{
		Since:    time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		HasUsage: true,
		Totals:   Totals{APIUSD: 4173.49},
	}
	card := NewShareCard(report, "last 30 days", 0)
	if card.PlanCostUSD != 200 || math.Abs(card.MonthlyPaceUSD-4173.49) > 1e-9 || card.Multiplier != 20.9 {
		t.Fatalf("card = %+v", card)
	}
	if card.Window != "last 30 days" || card.InstallCommand != ShareInstallCommand || card.Website != ShareWebsite {
		t.Fatalf("reproduction fields = %+v", card)
	}
}
