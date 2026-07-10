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
	agent := fs.String("agent", "", "apply the cap to a single agent (name as shown in reports)")
	off := fs.Bool("off", false, "remove the cap")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	key := budget.KeyDailyCapUSD
	scope := "daily cap"
	if *agent != "" {
		key = budget.KeyAgentCapPrefix + *agent
		scope = fmt.Sprintf("daily cap for agent %q", *agent)
	}

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
		v, err := s.GetSetting(budget.KeyDailyCapUSD)
		if err != nil {
			return err
		}
		if v == "" {
			fmt.Println("no global daily cap set. Set one: burnban cap --daily 10")
		} else {
			fmt.Printf("daily cap: $%s\n", v)
		}
		agents, err := s.SettingsWithPrefix(budget.KeyAgentCapPrefix)
		if err != nil {
			return err
		}
		for name, cap := range agents {
			fmt.Printf("agent %-24s $%s/day\n", name, cap)
		}
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
	today := fs.Bool("today", false, "also override the daily cap for the rest of today")
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
		msg += " — daily cap overridden for the rest of today"
	}
	fmt.Println(msg)
	return nil
}
