package main

import (
	"flag"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/store"
)

func cmdFuse(args []string) error {
	fs := flag.NewFlagSet("fuse", flag.ExitOnError)
	hourly := fs.Float64("hourly", 0, "maximum spend in any rolling hour in USD (0 removes)")
	burst := fs.String("burst", "", "short rolling limit as DURATION:USD, for example 5m:4 (off removes)")
	fanout := fs.String("fanout", "", "request breaker as DURATION:REQUESTS, for example 1m:120 (off removes)")
	baseline := fs.String("baseline", "", "same-time UTC spend deviation multiplier, for example 3x or 3x-baseline (off removes)")
	baselineWindow := fs.Duration("baseline-window", time.Hour, "fixed UTC comparison slot (must evenly divide 24h)")
	baselineDays := fs.Int("baseline-days", 14, "previous same-time slots used for the median (7-89)")
	baselineMinimum := fs.Float64("baseline-minimum", 0.25, "minimum USD threshold even when historical median is zero")
	cooldown := fs.Duration("cooldown", 0, "automatic pause after a trip (default 15m; min 1m, max 24h)")
	reset := fs.Bool("reset", false, "clear the current cooldown; retrips if velocity is still high")
	off := fs.Bool("off", false, "remove all velocity-fuse rules and the current cooldown")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}

	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })
	baselineOptions := passed["baseline-window"] || passed["baseline-days"] || passed["baseline-minimum"]
	if baselineOptions && !passed["baseline"] {
		return fmt.Errorf("baseline tuning flags require --baseline MULTIPLIER")
	}
	configChange := passed["hourly"] || passed["burst"] || passed["fanout"] || passed["baseline"] || passed["cooldown"]
	if *off && (*reset || configChange) {
		return fmt.Errorf("--off cannot be combined with --reset or fuse rule flags")
	}
	if *reset && configChange {
		return fmt.Errorf("--reset cannot be combined with fuse rule flags")
	}
	if passed["hourly"] {
		if math.IsNaN(*hourly) || math.IsInf(*hourly, 0) || *hourly < 0 {
			return fmt.Errorf("--hourly must be a finite non-negative dollar amount")
		}
		if *hourly != 0 && *hourly < 0.01 {
			return fmt.Errorf("hourly fuse limits below $0.01 are not enforceable")
		}
	}
	if passed["cooldown"] {
		if err := budget.ValidateFuseCooldown(*cooldown); err != nil {
			return err
		}
	}
	var burstWindow time.Duration
	var burstUSD float64
	removeBurst := false
	if passed["burst"] {
		switch strings.ToLower(strings.TrimSpace(*burst)) {
		case "off", "0":
			removeBurst = true
		default:
			var err error
			burstWindow, burstUSD, err = budget.ParseFuseBurst(*burst)
			if err != nil {
				return err
			}
		}
	}
	var fanoutWindow time.Duration
	var fanoutRequests int64
	removeFanout := false
	if passed["fanout"] {
		switch strings.ToLower(strings.TrimSpace(*fanout)) {
		case "off", "0":
			removeFanout = true
		default:
			var err error
			fanoutWindow, fanoutRequests, err = budget.ParseFuseFanout(*fanout)
			if err != nil {
				return err
			}
		}
	}
	removeBaseline := false
	baselineRaw := ""
	if passed["baseline"] {
		value := strings.ToLower(strings.TrimSpace(*baseline))
		switch value {
		case "off", "0":
			removeBaseline = true
		default:
			value = strings.TrimSuffix(value, "-baseline")
			value = strings.TrimSuffix(value, "x")
			multiplier, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("--baseline must be a multiplier such as 3x or off")
			}
			baselineRaw, err = budget.EncodeFuseBaseline(budget.FuseBaselinePolicy{
				Version: 1, Window: *baselineWindow, Multiplier: multiplier,
				LookbackDays: *baselineDays, MinimumUSD: *baselineMinimum,
			})
			if err != nil {
				return err
			}
		}
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	if *off {
		for _, key := range []string{budget.KeyFuseHourlyUSD, budget.KeyFuseBurst, budget.KeyFuseFanout, budget.KeyFuseBaseline, budget.KeyFuseCooldown, budget.KeyFuseTrip} {
			if err := s.DeleteSetting(key); err != nil {
				return err
			}
		}
		if err := s.DeleteSettingsWithPrefix(budget.KeyFuseAlertedPrefix); err != nil {
			return err
		}
		fmt.Println("spend-velocity fuse removed")
		return nil
	}
	if *reset {
		if err := s.DeleteSetting(budget.KeyFuseTrip); err != nil {
			return err
		}
		fmt.Println("fuse cooldown reset — new spend is eligible, but the fuse will retrip if rolling velocity is still above its limit")
		return nil
	}

	if passed["hourly"] {
		if *hourly == 0 {
			if err := s.DeleteSetting(budget.KeyFuseHourlyUSD); err != nil {
				return err
			}
			fmt.Println("rolling hourly fuse removed")
		} else {
			if err := s.SetSetting(budget.KeyFuseHourlyUSD, strconv.FormatFloat(*hourly, 'f', -1, 64)); err != nil {
				return err
			}
			fmt.Printf("rolling hourly fuse set: $%.2f\n", *hourly)
		}
	}
	if passed["burst"] {
		if removeBurst {
			if err := s.DeleteSetting(budget.KeyFuseBurst); err != nil {
				return err
			}
			fmt.Println("rolling burst fuse removed")
		} else {
			if err := s.SetSetting(budget.KeyFuseBurst, budget.FormatFuseBurst(burstWindow, burstUSD)); err != nil {
				return err
			}
			fmt.Printf("rolling %s burst fuse set: $%.2f\n", budget.FormatFuseDuration(burstWindow), burstUSD)
		}
	}
	if passed["fanout"] {
		if removeFanout {
			if err := s.DeleteSetting(budget.KeyFuseFanout); err != nil {
				return err
			}
			fmt.Println("request fan-out fuse removed")
		} else {
			if err := s.SetSetting(budget.KeyFuseFanout, budget.FormatFuseFanout(fanoutWindow, fanoutRequests)); err != nil {
				return err
			}
			fmt.Printf("rolling %s fan-out fuse set: %d requests\n", budget.FormatFuseDuration(fanoutWindow), fanoutRequests)
		}
	}
	if passed["baseline"] {
		if removeBaseline {
			if err := s.DeleteSetting(budget.KeyFuseBaseline); err != nil {
				return err
			}
			fmt.Println("same-time baseline fuse removed")
		} else {
			if err := s.SetSetting(budget.KeyFuseBaseline, baselineRaw); err != nil {
				return err
			}
			fmt.Printf("same-time baseline fuse set: %s slot, %d-day lookback, minimum $%.2f\n",
				budget.FormatFuseDuration(*baselineWindow), *baselineDays, *baselineMinimum)
		}
	}
	if passed["cooldown"] {
		if err := s.SetSetting(budget.KeyFuseCooldown, budget.FormatFuseDuration(*cooldown)); err != nil {
			return err
		}
		fmt.Printf("fuse cooldown set: %s\n", budget.FormatFuseDuration(*cooldown))
	}
	if configChange {
		snapshot, err := budget.FuseStatus(s, time.Now())
		if err != nil {
			return err
		}
		if snapshot.Tripped {
			fmt.Printf("fuse remains tripped until %s — use `burnban fuse --reset` to recover early\n",
				snapshot.TrippedUntil.In(time.Now().Location()).Format(time.RFC3339))
		} else if len(snapshot.Rules) == 0 && snapshot.Fanout == nil {
			fmt.Println("no active velocity-fuse rules remain")
		} else {
			fmt.Printf("fuse armed — a trip pauses new spend for %s\n", budget.FormatFuseDuration(snapshot.Cooldown))
		}
		return nil
	}
	return printFuseStatus(s)
}

func printFuseStatus(s *store.Store) error {
	now := time.Now()
	snapshot, err := budget.FuseStatus(s, now)
	if err != nil {
		return err
	}
	if len(snapshot.Rules) == 0 && snapshot.Fanout == nil {
		fmt.Println("no velocity fuse set. Arm one: burnban fuse --hourly 20 --burst 5m:4 --fanout 1m:120")
	} else {
		for _, rule := range snapshot.Rules {
			windowKind := "rolling " + budget.FormatFuseDuration(rule.Window)
			if rule.Name == "baseline" {
				windowKind = "the current fixed " + budget.FormatFuseDuration(rule.Window) + " UTC slot"
			}
			fmt.Printf("%-8s $%.4f of $%.2f in %s (%.0f%%, $%.4f remaining)\n",
				rule.Name, rule.SpentUSD, rule.CapUSD, windowKind, rule.Pct(), rule.Remaining)
			if rule.Name == "baseline" {
				fmt.Printf("         median $%.4f × %.2f, with configured minimum floor\n",
					rule.BaselineMedianUSD, rule.BaselineMultiplier)
			}
			if rule.ProjectedTimeToLimit > 0 {
				fmt.Printf("         current rate projects limit in %s\n", budget.FormatFuseDuration(max(rule.ProjectedTimeToLimit.Round(time.Second), time.Second)))
			}
		}
		if snapshot.Fanout != nil {
			fmt.Printf("fanout   %d of %d requests in rolling %s (%d remaining)\n",
				snapshot.Fanout.Requests, snapshot.Fanout.LimitRequests,
				budget.FormatFuseDuration(snapshot.Fanout.Window), snapshot.Fanout.RemainingRequests)
		}
		fmt.Printf("action   temporary ban for %s\n", budget.FormatFuseDuration(snapshot.Cooldown))
	}
	if snapshot.Tripped {
		fmt.Printf("state    TRIPPED until %s\n", snapshot.TrippedUntil.In(now.Location()).Format(time.RFC3339))
		fmt.Printf("reason   %s\n", terminalText(snapshot.DenialMessage, 500))
	} else if len(snapshot.Rules) > 0 || snapshot.Fanout != nil {
		fmt.Println("state    armed")
	}
	return nil
}
