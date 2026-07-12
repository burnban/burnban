package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/store"
	"github.com/mattn/go-isatty"
)

func cmdTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	interval := fs.Duration("interval", 2*time.Second, "refresh interval")
	once := fs.Bool("once", false, "print one plain-text snapshot and exit")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *interval <= 0 {
		return fmt.Errorf("--interval must be greater than zero")
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	isTTY := stdoutIsTerminal()
	if *once || !isTTY {
		frame, err := renderTop(s, false)
		if err == nil {
			fmt.Print(frame)
		}
		return err
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	tick := time.NewTicker(*interval)
	defer tick.Stop()

	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h\n")
	for {
		frame, err := renderTop(s, true)
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

func renderTop(s *store.Store, color bool) (string, error) {
	now := time.Now()
	sum, err := s.Top(budget.DayStart(now), 5)
	if err != nil {
		return "", err
	}
	lastHour, err := s.SpentSince(now.Add(-time.Hour))
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "burnban top — %s\n\n", now.Format("15:04:05"))
	fmt.Fprintf(&b, "today   $%.4f · %d req · cache hit %s\n", sum.Cost, sum.Requests, cachePct(sum.CacheRead, sum.In))
	fmt.Fprintf(&b, "rate    $%.4f/hr\n\n", lastHour)

	if local, external, err := budget.BanStatus(s); err != nil {
		return "", err
	} else if local || external {
		message := "BURN BAN IN EFFECT — all spend paused (burnban lift)"
		if external {
			message = "EXTERNAL BURN BAN — contact the external policy owner"
		}
		b.WriteString(colorize(message, cRed, color) + "\n\n")
	} else if states, err := budget.Status(s, now); err != nil {
		return "", err
	} else {
		any := false
		for _, st := range states {
			if !st.Set {
				continue
			}
			frac := st.Spent / st.CapUSD
			barColor := cGreen
			switch {
			case frac >= 0.9:
				barColor = cRed
			case frac >= 0.6:
				barColor = cYellow
			}
			fmt.Fprintf(&b, "%-7s %s $%.2f / $%.2f (%s)\n", st.Name, colorize(bar(frac, 30), barColor, color), st.Spent, st.CapUSD, terminalText(st.Source, 40))
			any = true
		}
		if any {
			b.WriteString("\n")
		} else {
			b.WriteString(colorize("budget  no cap set — burnban cap --daily 10", cDim, color) + "\n\n")
		}
	}

	if len(sum.Models) > 0 {
		b.WriteString("BY MODEL (today)\n")
		for _, m := range sum.Models {
			fmt.Fprintf(&b, "  %-34s %6d req  $%.4f\n", terminalText(m.Model, 34), m.Requests, m.Cost)
		}
		b.WriteString("\n")
	}
	if len(sum.Agents) > 0 {
		b.WriteString("BY AGENT (today)\n")
		for _, a := range sum.Agents {
			fmt.Fprintf(&b, "  %-34s %6d req  $%.4f\n", terminalText(a.Agent, 34), a.Requests, a.Cost)
		}
		b.WriteString("\n")
	}
	if color {
		b.WriteString(colorize("ctrl+c to quit", cDim, true) + "\n")
	}
	return b.String(), nil
}

func colorize(value, code string, enabled bool) string {
	if !enabled {
		return value
	}
	return code + value + cReset
}

func stdoutIsTerminal() bool {
	return fileIsTerminal(os.Stdout)
}

func fileIsTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	fd := file.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
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
