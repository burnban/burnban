package downshift

import "fmt"

type Action string

const (
	ActionNone      Action = "none"
	ActionWarn      Action = "warn"
	ActionDownshift Action = "downshift"
)

type Trigger string

const (
	TriggerNone      Trigger = "none"
	TriggerWarning   Trigger = "budget_warning"
	TriggerThreshold Trigger = "budget_threshold"
	TriggerDenial    Trigger = "budget_denial"
)

type Input struct {
	Route             string
	Model             string
	Dialect           string
	TargetRouteExists bool
	TargetDialect     string
	Identity          Identity
	Features          Features
	BudgetPct         float64 // negative means no active dollar budget
	BudgetDenial      bool
}

type Decision struct {
	Action          Action
	Trigger         Trigger
	RuleID          string
	Reason          string
	SourceRoute     string
	SourceModel     string
	TargetRoute     string
	TargetModel     string
	ConfigRevision  int64
	ConfigDigest    string
	BudgetPct       float64
	Features        Features
	CompatibilityOK bool
}

func Decide(compiled *Compiled, input Input) Decision {
	decision := Decision{
		Action: ActionNone, Trigger: TriggerNone, Reason: "no active downshift configuration",
		SourceRoute: input.Route, SourceModel: input.Model, BudgetPct: input.BudgetPct,
		Features: input.Features,
	}
	if compiled == nil {
		return decision
	}
	decision.ConfigRevision, decision.ConfigDigest = compiled.Config.Revision, compiled.Digest
	rule := compiled.Rule(input.Route, input.Model)
	if rule == nil {
		decision.Reason = "source route/model has no allowlisted equivalent target"
		return decision
	}
	decision.RuleID = rule.ID
	decision.TargetRoute, decision.TargetModel = rule.Target.Route, rule.Target.Model
	if input.Dialect != rule.Source.Dialect {
		decision.Reason = "configured source dialect does not match the live route"
		return decision
	}
	if !input.TargetRouteExists {
		decision.Reason = "allowlisted target route is not configured on this proxy"
		return decision
	}
	if input.TargetDialect != rule.Target.Dialect {
		decision.Reason = "allowlisted target dialect does not match the live route"
		return decision
	}
	if ok, reason := ScopeMatches(rule.Scope, input.Identity); !ok {
		decision.Reason = reason
		return decision
	}
	if ok, reason := Eligible(rule, input.Features); !ok {
		decision.Reason = reason
		return decision
	}
	decision.CompatibilityOK = true

	trigger := TriggerNone
	switch {
	case input.BudgetDenial:
		if !compiled.Config.DownshiftOnDenial {
			decision.Reason = "budget denied source request and downshift_on_denial is disabled"
			return decision
		}
		trigger = TriggerDenial
	case input.BudgetPct >= compiled.Config.DownshiftAtPct:
		trigger = TriggerThreshold
	case input.BudgetPct >= compiled.Config.WarnAtPct:
		trigger = TriggerWarning
	default:
		decision.Reason = "budget utilization is below the configured warning threshold"
		return decision
	}
	decision.Trigger = trigger
	if compiled.Config.Mode == ModeObserve || trigger == TriggerWarning {
		decision.Action = ActionWarn
		if trigger == TriggerWarning {
			decision.Reason = fmt.Sprintf("budget utilization %.2f%% reached warning threshold %.2f%%; compatible target %s/%s is available",
				input.BudgetPct, compiled.Config.WarnAtPct, rule.Target.Route, rule.Target.Model)
		} else if trigger == TriggerDenial {
			decision.Reason = "observe mode: source was budget-denied; compatible target would be " + rule.Target.Route + "/" + rule.Target.Model
		} else {
			decision.Reason = fmt.Sprintf("observe mode: budget utilization %.2f%% reached downshift threshold %.2f%%; compatible target would be %s/%s",
				input.BudgetPct, compiled.Config.DownshiftAtPct, rule.Target.Route, rule.Target.Model)
		}
		return decision
	}
	decision.Action = ActionDownshift
	if trigger == TriggerDenial {
		decision.Reason = "source request exceeded remaining budget; selected compatible allowlisted target"
	} else {
		decision.Reason = fmt.Sprintf("budget utilization %.2f%% reached downshift threshold %.2f%%; selected compatible allowlisted target",
			input.BudgetPct, compiled.Config.DownshiftAtPct)
	}
	return decision
}
