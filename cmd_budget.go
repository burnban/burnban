package main

import (
	"flag"
	"fmt"
	"strconv"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/store"
)

func cmdCap(args []string) error {
	fs := flag.NewFlagSet("cap", flag.ExitOnError)
	daily := fs.Float64("daily", 0, "daily cap in USD")
	weekly := fs.Float64("weekly", 0, "weekly cap in USD (resets Monday)")
	monthly := fs.Float64("monthly", 0, "monthly cap in USD (resets on the 1st)")
	warn := fs.Float64("warn", -1, "webhook warning threshold as % of any cap (default 80; 0 disables)")
	agent := fs.String("agent", "", "apply the cap to a single agent (name as shown in reports)")
	off := fs.Bool("off", false, "remove the cap(s)")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	if *agent != "" {
		if *weekly > 0 || *monthly > 0 {
			return fmt.Errorf("per-agent caps are daily-only for now — drop --weekly/--monthly or the --agent")
		}
		key := budget.KeyAgentCapPrefix + *agent
		scope := fmt.Sprintf("daily cap for agent %q", *agent)
		switch {
		case *off:
			if err := s.DeleteSetting(key); err != nil {
				return err
			}
			fmt.Printf("%s removed\n", scope)
		case *daily > 0:
			if err := s.SetSetting(key, strconv.FormatFloat(*daily, 'f', 2, 64)); err != nil {
				return err
			}
			fmt.Printf("%s set: $%.2f — the proxy returns 402 once it is reached\n", scope, *daily)
		default:
			return fmt.Errorf("nothing to do: give --daily, or --off to remove")
		}
		return nil
	}

	if *off {
		for _, w := range budget.Windows() {
			if err := s.DeleteSetting(w.Key); err != nil {
				return err
			}
		}
		if err := s.DeleteSetting(budget.KeyWarnPct); err != nil {
			return err
		}
		fmt.Println("all global caps removed (per-agent caps kept — clear with --agent NAME --off)")
		return nil
	}

	set := false
	for _, w := range []struct {
		name string
		key  string
		usd  float64
	}{
		{"daily", budget.KeyDailyCapUSD, *daily},
		{"weekly", budget.KeyWeeklyCapUSD, *weekly},
		{"monthly", budget.KeyMonthlyCapUSD, *monthly},
	} {
		if w.usd <= 0 {
			continue
		}
		if err := s.SetSetting(w.key, strconv.FormatFloat(w.usd, 'f', 2, 64)); err != nil {
			return err
		}
		fmt.Printf("%s cap set: $%.2f — the proxy returns 402 once it is reached\n", w.name, w.usd)
		set = true
	}
	if *warn >= 0 {
		if err := s.SetSetting(budget.KeyWarnPct, strconv.FormatFloat(*warn, 'f', 0, 64)); err != nil {
			return err
		}
		if *warn == 0 {
			fmt.Println("early warnings disabled")
		} else {
			fmt.Printf("early warning at %.0f%% of any cap (needs a webhook: burnban alert --webhook URL)\n", *warn)
		}
		set = true
	}
	if set {
		return nil
	}

	// No flags: show status with live spend against each window.
	now := time.Now()
	shown := false
	for _, w := range budget.Windows() {
		v, err := s.GetSetting(w.Key)
		if err != nil {
			return err
		}
		if v == "" {
			continue
		}
		capUSD, _ := strconv.ParseFloat(v, 64)
		spent, err := s.SpentSince(w.Start(now))
		if err != nil {
			return err
		}
		pct := 0.0
		if capUSD > 0 {
			pct = spent / capUSD * 100
		}
		fmt.Printf("%-8s $%.2f of $%.2f (%.0f%%, resets %s)\n", w.Name, spent, capUSD, pct, w.Reset)
		shown = true
	}
	if !shown {
		fmt.Println("no global caps set. Set one: burnban cap --daily 10 [--weekly 40] [--monthly 120]")
	}
	if wp, err := s.GetSetting(budget.KeyWarnPct); err != nil {
		return err
	} else if wp != "" && wp != "0" {
		fmt.Printf("warn     at %s%% of any cap\n", wp)
	} else if wp == "0" {
		fmt.Println("warn     disabled")
	}
	agents, err := s.SettingsWithPrefix(budget.KeyAgentCapPrefix)
	if err != nil {
		return err
	}
	for name, cap := range agents {
		fmt.Printf("agent %-24s $%s/day\n", name, cap)
	}
	return nil
}

func cmdBan(args []string) error {
	fs := flag.NewFlagSet("ban", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	if err := s.SetSetting(budget.KeyBanActive, "1"); err != nil {
		return err
	}
	fmt.Println("🚫 burn ban in effect — all agent spend is paused until `burnban lift`")
	return nil
}

func cmdLift(args []string) error {
	fs := flag.NewFlagSet("lift", flag.ExitOnError)
	today := fs.Bool("today", false, "also override all caps for the rest of today")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	if err := s.DeleteSetting(budget.KeyBanActive); err != nil {
		return err
	}
	msg := "burn ban lifted"
	if *today {
		if err := s.SetSetting(budget.KeyOverrideDay, time.Now().Format("2006-01-02")); err != nil {
			return err
		}
		msg += " — caps overridden for the rest of today"
	}
	fmt.Println(msg)
	return nil
}
