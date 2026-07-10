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
	off := fs.Bool("off", false, "remove the daily cap")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	switch {
	case *off:
		if err := s.DeleteSetting(budget.KeyDailyCapUSD); err != nil {
			return err
		}
		fmt.Println("daily cap removed — spend is uncapped")
	case *daily > 0:
		if err := s.SetSetting(budget.KeyDailyCapUSD, strconv.FormatFloat(*daily, 'f', 2, 64)); err != nil {
			return err
		}
		fmt.Printf("daily cap set: $%.2f — the proxy returns 402 once today's spend reaches it\n", *daily)
	default:
		v, err := s.GetSetting(budget.KeyDailyCapUSD)
		if err != nil {
			return err
		}
		if v == "" {
			fmt.Println("no daily cap set. Set one: burnban cap --daily 10")
		} else {
			fmt.Printf("daily cap: $%s\n", v)
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
