package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/store"
)

func cmdTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	interval := fs.Duration("interval", 2*time.Second, "refresh interval")
	fs.Parse(args)

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	tick := time.NewTicker(*interval)
	defer tick.Stop()

	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h\n")
	for {
		frame, err := renderTop(s)
		if err != nil {
			return err
		}
		fmt.Print("\033[2J\033[H" + frame)
		select {
		case <-sig:
			return nil
		case <-tick.C:
		}
	}
}

const (
	cReset  = "\033[0m"
	cRed    = "\033[31m"
	cYellow = "\033[33m"
	cGreen  = "\033[32m"
	cDim    = "\033[2m"
)

func renderTop(s *store.Store) (string, error) {
	now := time.Now()
	sum, err := s.Summarize(budget.DayStart(now))
	if err != nil {
		return "", err
	}
	lastHour, err := s.SpentSince(now.Add(-time.Hour))
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🔥 burnban top — %s\n\n", now.Format("15:04:05"))
	fmt.Fprintf(&b, "today   $%.4f · %d req · cache hit %s\n", sum.Cost, sum.Requests, cachePct(sum.CacheRead, sum.In))
	fmt.Fprintf(&b, "rate    $%.4f/hr\n\n", lastHour)

	if local, external, err := budget.BanStatus(s); err != nil {
		return "", err
	} else if local || external {
		message := "🚫 BURN BAN IN EFFECT — all spend paused (burnban lift)"
		if external {
			message = "🚫 ORGANIZATION BURN BAN — external policy; contact your administrator"
		}
		b.WriteString(cRed + message + cReset + "\n\n")
	} else if states, err := budget.Status(s, now); err != nil {
		return "", err
	} else {
		any := false
		for _, st := range states {
			if !st.Set {
				continue
			}
			frac := st.Spent / st.CapUSD
			color := cGreen
			switch {
			case frac >= 0.9:
				color = cRed
			case frac >= 0.6:
				color = cYellow
			}
			fmt.Fprintf(&b, "%-7s %s%s%s $%.2f / $%.2f (%s)\n", st.Name, color, bar(frac, 30), cReset, st.Spent, st.CapUSD, st.Source)
			any = true
		}
		if any {
			b.WriteString("\n")
		} else {
			b.WriteString(cDim + "budget  no cap set — burnban cap --daily 10" + cReset + "\n\n")
		}
	}

	if len(sum.Models) > 0 {
		b.WriteString("BY MODEL (today)\n")
		for i, m := range sum.Models {
			if i == 5 {
				break
			}
			fmt.Fprintf(&b, "  %-34s %6d req  $%.4f\n", m.Model, m.Requests, m.Cost)
		}
		b.WriteString("\n")
	}
	if len(sum.Agents) > 0 {
		b.WriteString("BY AGENT (today)\n")
		for i, a := range sum.Agents {
			if i == 5 {
				break
			}
			fmt.Fprintf(&b, "  %-34s %6d req  $%.4f\n", a.Agent, a.Requests, a.Cost)
		}
		b.WriteString("\n")
	}
	b.WriteString(cDim + "ctrl+c to quit" + cReset + "\n")
	return b.String(), nil
}

func bar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	fill := int(frac * float64(width))
	return strings.Repeat("█", fill) + strings.Repeat("░", width-fill)
}
