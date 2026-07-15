package main

import (
	"flag"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/store"
)

func cmdCap(args []string) error {
	fs := flag.NewFlagSet("cap", flag.ExitOnError)
	daily := fs.Float64("daily", 0, "daily cap in USD (min 0.01)")
	weekly := fs.Float64("weekly", 0, "weekly cap in USD (resets Monday)")
	monthly := fs.Float64("monthly", 0, "monthly cap in USD (resets on the 1st)")
	warn := fs.Float64("warn", -1, "webhook warning threshold as % of any cap, 1-100 (default 80; 0 disables)")
	agent := fs.String("agent", "", "apply the cap to a single agent (name as shown in reports)")
	off := fs.Bool("off", false, "remove the cap(s)")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	// Sub-cent caps would round to $0.00 in display and read as
	// "cap everything"; refuse them instead of storing a footgun.
	for _, usd := range []float64{*daily, *weekly, *monthly} {
		if math.IsNaN(usd) || math.IsInf(usd, 0) {
			return fmt.Errorf("caps must be finite dollar amounts")
		}
		if usd != 0 && usd < 0.01 {
			return fmt.Errorf("caps below $0.01 are not enforceable — use `burnban ban` to stop all spend")
		}
	}
	if math.IsNaN(*warn) || math.IsInf(*warn, 0) {
		return fmt.Errorf("--warn must be a finite percentage")
	}

	if *agent != "" {
		return capAgent(s, *agent, *daily, *weekly, *monthly, *warn, *off)
	}

	if *off {
		for _, w := range budget.Windows() {
			if err := s.DeleteSetting(w.Key); err != nil {
				return err
			}
			if err := budget.ClearMarks(s, w.Name); err != nil {
				return err
			}
		}
		fmt.Println("all local global caps removed (external policy, per-agent caps, and --warn kept)")
		return nil
	}

	// An explicitly passed `--daily 0` removes just that window's cap;
	// an omitted flag (also 0) is simply not mentioned.
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })

	byName := map[string]float64{"daily": *daily, "weekly": *weekly, "monthly": *monthly}
	set := false
	armed := false
	for _, w := range budget.Windows() {
		usd := byName[w.Name]
		if usd <= 0 {
			if usd == 0 && passed[w.Name] {
				if err := s.DeleteSetting(w.Key); err != nil {
					return err
				}
				if err := budget.ClearMarks(s, w.Name); err != nil {
					return err
				}
				fmt.Printf("local %s cap removed\n", w.Name)
				set = true
			}
			continue
		}
		if err := s.SetSetting(w.Key, strconv.FormatFloat(usd, 'f', -1, 64)); err != nil {
			return err
		}
		// A new threshold means the old "already warned/alerted" marks no
		// longer describe anything — re-arm both for this window.
		if err := budget.ClearMarks(s, w.Name); err != nil {
			return err
		}
		fmt.Printf("local %s cap set: $%.2f — the proxy returns 402 once it is reached\n", w.Name, usd)
		set = true
		armed = true
	}
	// A freshly set cap must actually enforce: drop any `lift --today`
	// override, which would otherwise silently suspend it until midnight.
	if armed {
		if cleared, err := budget.ClearOverride(s, time.Now()); err != nil {
			return err
		} else if cleared {
			fmt.Println("today's cap override (lift --today) cleared: caps enforce again")
		}
	}
	if *warn >= 0 {
		if *warn > 100 {
			return fmt.Errorf("--warn is a percentage of the cap: use 1-100, or 0 to disable")
		}
		if err := s.SetSetting(budget.KeyWarnPct, strconv.FormatFloat(*warn, 'f', -1, 64)); err != nil {
			return err
		}
		if *warn == 0 {
			fmt.Println("early warnings disabled")
		} else {
			fmt.Printf("early warning at %.4g%% of any cap (needs a webhook: burnban alert --webhook URL)\n", *warn)
		}
		set = true
	}
	if set {
		return nil
	}
	return printCapStatus(s)
}

// capAgent handles every --agent form: set, remove, or show one agent's cap.
func capAgent(s *store.Store, agent string, daily, weekly, monthly, warn float64, off bool) error {
	if weekly > 0 || monthly > 0 {
		return fmt.Errorf("per-agent caps are daily-only for now — drop --weekly/--monthly or the --agent")
	}
	if warn >= 0 {
		return fmt.Errorf("--warn is global, not per-agent — set it without --agent")
	}
	key := budget.KeyAgentCapPrefix + agent
	scope := fmt.Sprintf("daily cap for agent %q", agent)
	switch {
	case off:
		if err := s.DeleteSetting(key); err != nil {
			return err
		}
		fmt.Printf("%s removed\n", scope)
	case daily > 0:
		if err := s.SetSetting(key, strconv.FormatFloat(daily, 'f', -1, 64)); err != nil {
			return err
		}
		fmt.Printf("%s set: $%.2f — the proxy returns 402 once it is reached\n", scope, daily)
		if cleared, err := budget.ClearOverride(s, time.Now()); err != nil {
			return err
		} else if cleared {
			fmt.Println("today's cap override (lift --today) cleared: caps enforce again")
		}
	default:
		v, err := s.GetSetting(key)
		if err != nil {
			return err
		}
		if v == "" {
			fmt.Printf("no cap set for agent %q. Set one: burnban cap --agent %q --daily 5\n", terminalText(agent, 100), terminalText(agent, 100))
			return nil
		}
		spent, err := s.SpentSinceForAgent(budget.DayStart(time.Now()), agent)
		if err != nil {
			return err
		}
		fmt.Printf("agent %-24s $%.2f of $%s today\n", agent, spent, v)
	}
	return nil
}

// printCapStatus shows every window's live position, the warn threshold,
// and per-agent caps.
func printCapStatus(s *store.Store) error {
	if override, err := budget.OverrideActive(s, time.Now()); err != nil {
		return err
	} else if override {
		fmt.Println("caps OVERRIDDEN until midnight (lift --today). Re-arm by setting any cap, e.g. burnban cap --daily 10")
	}
	states, err := budget.Status(s, time.Now())
	if err != nil {
		return err
	}
	shown := false
	for _, st := range states {
		if !st.Set {
			continue
		}
		fmt.Printf("%-8s $%.2f of $%.2f (%.0f%%, %s, resets %s)\n", st.Name, st.Spent, st.CapUSD, st.Pct(), st.Source, st.Reset)
		shown = true
	}
	if !shown {
		fmt.Println("no global caps set. Set one: burnban cap --daily 10 [--weekly 40] [--monthly 120]")
	}
	if wp, err := s.GetSetting(budget.KeyWarnPct); err != nil {
		return err
	} else if wp == "0" {
		fmt.Println("warn     disabled")
	} else if wp != "" {
		fmt.Printf("warn     at %s%% of any cap\n", wp)
	}
	agents, err := s.SettingsWithPrefix(budget.KeyAgentCapPrefix)
	if err != nil {
		return err
	}
	for name, cap := range agents {
		fmt.Printf("agent %-24s $%s/day\n", terminalText(name, 80), terminalText(cap, 40))
	}
	return nil
}

func cmdBan(args []string) error {
	fs := flag.NewFlagSet("ban", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	if err := s.SetSetting(budget.KeyBanActive, "1"); err != nil {
		return err
	}
	msg := "local burn ban in effect — all agent spend is paused until `burnban lift`"
	// The emergency stop also re-arms caps, so `ban` then `lift` returns to
	// enforced budgets instead of resurrecting an earlier `lift --today`.
	if cleared, err := budget.ClearOverride(s, time.Now()); err != nil {
		return err
	} else if cleared {
		msg += "; today's cap override cleared"
	}
	fmt.Println(msg)
	return nil
}

func cmdLift(args []string) error {
	fs := flag.NewFlagSet("lift", flag.ExitOnError)
	today := fs.Bool("today", false, "also override all caps for the rest of today")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	if err := s.DeleteSetting(budget.KeyBanActive); err != nil {
		return err
	}
	msg := "local burn ban lifted"
	if *today {
		if err := s.SetSetting(budget.KeyOverrideDay, time.Now().Format("2006-01-02")); err != nil {
			return err
		}
		msg += " — local caps overridden for the rest of today"
	}
	if _, external, err := budget.BanStatus(s); err != nil {
		return err
	} else if external {
		msg += "; external burn ban remains in effect"
	}
	if fuse, err := budget.FuseStatus(s, time.Now()); err != nil {
		return err
	} else if fuse.Tripped {
		msg += "; spend-velocity fuse remains tripped — reset it with `burnban fuse --reset`"
	}
	fmt.Println(msg)
	return nil
}
