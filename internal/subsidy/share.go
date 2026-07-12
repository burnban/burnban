package subsidy

import "math"

const (
	// DefaultSharePlanCostUSD is used only for the compact share card when the
	// user has not supplied --plan-cost. It matches the top individual plans
	// used by the supported Claude Code and Codex subscription comparisons.
	DefaultSharePlanCostUSD = 200.0
	ShareInstallCommand     = "curl -fsSL https://burnban.sh/install | sh"
	ShareWebsite            = "burnban.dev"
)

// ShareCard is the structured form of the compact subsidy share output.
// Multiplier compares the report's normalized 30-day pace with PlanCostUSD;
// APIEquivalentUSD remains the actual value observed in Window.
type ShareCard struct {
	HasUsage         bool    `json:"has_usage"`
	APIEquivalentUSD float64 `json:"api_equivalent_usd"`
	MonthlyPaceUSD   float64 `json:"monthly_pace_usd"`
	PlanCostUSD      float64 `json:"plan_cost_usd"`
	Multiplier       float64 `json:"multiplier"`
	Window           string  `json:"window"`
	InstallCommand   string  `json:"install_command"`
	Website          string  `json:"website"`
	Partial          bool    `json:"partial"`
}

// NewShareCard derives the public, reproducible share fields from a report.
func NewShareCard(report Report, window string, planCostUSD float64) ShareCard {
	if planCostUSD <= 0 {
		planCostUSD = DefaultSharePlanCostUSD
	}
	windowDays := report.Until.Sub(report.Since).Hours() / 24
	if windowDays <= 0 {
		windowDays = 1
	}
	monthlyPaceUSD := math.Round(report.APIUSD/windowDays*30*100) / 100
	multiplier := math.Round(monthlyPaceUSD/planCostUSD*10) / 10
	return ShareCard{
		HasUsage:         report.HasUsage,
		APIEquivalentUSD: report.APIUSD,
		MonthlyPaceUSD:   monthlyPaceUSD,
		PlanCostUSD:      planCostUSD,
		Multiplier:       multiplier,
		Window:           window,
		InstallCommand:   ShareInstallCommand,
		Website:          ShareWebsite,
		Partial:          report.Partial,
	}
}
