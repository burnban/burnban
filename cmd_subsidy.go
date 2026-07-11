package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/syft8/burnban/internal/pricing"
	"github.com/syft8/burnban/internal/subsidy"
)

// cmdSubsidy prices local agent traffic that may never touch the proxy.
// Claude Code, Codex, Hermes, OpenClaw, and Goose retain normalized token usage
// locally; the shared report engine reads those stores without modifying them.
func cmdSubsidy(args []string) error {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("subsidy", flag.ExitOnError)
	sinceArg := fs.String("since", "30d", `window: "today", "24h", "7d", "30d", or any Go duration`)
	claudeDir := fs.String("claude-dir", filepath.Join(home, ".claude", "projects"), "Claude Code session logs")
	codexDir := fs.String("codex-dir", filepath.Join(home, ".codex", "sessions"), "Codex rollout logs")
	hermesDB := fs.String("hermes-db", defaultHermesDB(home), "Hermes state database")
	openClawDir := fs.String("openclaw-dir", defaultOpenClawDir(home), "OpenClaw state directory")
	gooseDB := fs.String("goose-db", subsidy.DefaultGooseDB(home), "Goose session database")
	planCost := fs.Float64("plan-cost", 0, "what you pay per month across plans, for a single subsidy multiple")
	daily := fs.Bool("daily", false, "per-day breakdown")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Parse(args)

	from, label, err := parseSince(*sinceArg)
	if err != nil {
		return err
	}
	prices, err := pricing.Load()
	if err != nil {
		return err
	}
	until := time.Now()
	report, err := subsidy.BuildReport(prices, subsidy.ReportOptions{
		Since: from, Until: until,
		ClaudeDir: *claudeDir, CodexDir: *codexDir,
		HermesDB: *hermesDB, OpenClawDir: *openClawDir, GooseDB: *gooseDB,
	})
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"since": report.Since, "until": report.Until,
			"providers": report.Providers, "totals": report.Totals,
			"api_usd_total": report.APIUSD, "unpriced_tokens": report.UnpricedTokens,
		})
	}

	if !report.HasUsage {
		fmt.Printf("no local agent usage in %s — checked:\n", label)
		for _, provider := range report.Providers {
			fmt.Printf("  %-12s %s\n", subsidyTitle(provider.Provider), provider.Dir)
		}
		return nil
	}

	fmt.Printf("BURNBAN LOCAL USAGE — %s at API-equivalent prices\n\n", label)
	fmt.Println("Auto-detected local agent logs. These dollar values show what the same")
	fmt.Println("tokens cost at API rates; they are not a provider invoice.")
	for _, provider := range report.Providers {
		if provider.Sessions == 0 && provider.Error == "" {
			continue
		}
		fmt.Printf("\n%s  %s · %d sessions\n", subsidyTitle(provider.Provider), provider.Dir, provider.Sessions)
		if provider.Error != "" {
			fmt.Printf("  scan issue: %s\n", provider.Error)
		}
		if len(provider.Models) == 0 {
			continue
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  model\tcalls\tin\tout\tcache-r\tcache-w\tAPI equiv.")
		for _, model := range provider.Models {
			cost := fmt.Sprintf("$%.2f", model.APIUSD)
			if !model.Priced {
				cost = "unpriced"
			}
			fmt.Fprintf(w, "  %s\t%d\t%s\t%s\t%s\t%s\t%s\n", model.Model, model.Calls,
				fmtTok(model.In), fmtTok(model.Out), fmtTok(model.CacheRead), fmtTok(model.CacheWrite), cost)
		}
		fmt.Fprintf(w, "  subtotal\t\t%s\t%s\t%s\t%s\t$%.2f\n",
			fmtTok(provider.In), fmtTok(provider.Out), fmtTok(provider.CacheRead), fmtTok(provider.CacheWrite), provider.APIUSD)
		w.Flush()
		if *daily {
			dw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(dw, "\n  day\tcalls\ttokens\tAPI equiv.")
			for _, day := range provider.Days {
				tokens := day.In + day.Out + day.CacheRead + day.CacheWrite
				fmt.Fprintf(dw, "  %s\t%d\t%s\t$%.2f\n", day.Day, day.Calls, fmtTok(tokens), day.APIUSD)
			}
			dw.Flush()
		}
	}

	windowDays := until.Sub(from).Hours() / 24
	if windowDays <= 0 {
		windowDays = 1
	}
	monthlyPace := report.APIUSD / windowDays * 30
	fmt.Printf("\nTOTAL  $%.2f API equivalent", report.APIUSD)
	if math.Abs(monthlyPace-report.APIUSD) > .5 {
		fmt.Printf("  ·  ≈ $%.2f/mo pace", monthlyPace)
	}
	fmt.Println()
	if *planCost > 0 {
		fmt.Printf("\n  vs your $%.0f/mo in plans → %.1fx the sticker price in API value\n", *planCost, monthlyPace/(*planCost))
	} else {
		// Without an explicit --plan-cost, compare against the vendor's public
		// tiers — but only the ones you could plausibly be on. A vendor's tiers
		// scale usage limits roughly with price (Claude Max 5x/20x are named for
		// it), so the believable subsidy multiple is about the same on every tier.
		// A multiple far above that means the plan's rate limits could never have
		// produced this usage, so printing it would be an inflated, misleading
		// number — the exact thing burnban exists to kill. Hide those tiers, but
		// always keep the top one so there is still a comparison. Plans are listed
		// cheapest-first, so the last entry is the highest tier.
		const plausibleSubsidy = 30.0
		type plan struct {
			name string
			usd  float64
		}
		plans := map[string][]plan{
			"claude-code": {{"Claude Pro", 20}, {"Claude Max 5x", 100}, {"Claude Max 20x", 200}},
			"codex":       {{"ChatGPT Plus", 20}, {"ChatGPT Team", 30}, {"ChatGPT Pro", 200}},
		}
		for _, provider := range report.Providers {
			providerPlans := plans[provider.Provider]
			if provider.APIUSD <= 0 || len(providerPlans) == 0 {
				continue
			}
			pace := provider.APIUSD / windowDays * 30
			var shown []plan
			for _, item := range providerPlans {
				if pace/item.usd <= plausibleSubsidy {
					shown = append(shown, item)
				}
			}
			if len(shown) == 0 {
				shown = providerPlans[len(providerPlans)-1:]
			}
			fmt.Printf("\n  %s pace ≈ $%.2f/mo vs", provider.Provider, pace)
			for i, item := range shown {
				if i > 0 {
					fmt.Print("  ·")
				}
				fmt.Printf("  %s $%.0f → %.1fx", item.name, item.usd, pace/item.usd)
			}
			fmt.Println()
		}
	}
	if report.UnpricedTokens > 0 {
		fmt.Printf("\n  %s tokens ran on models with no known price. Add them to\n", fmtTok(report.UnpricedTokens))
		fmt.Println("  ~/.burnban/pricing.json to include them in the dollar equivalent.")
	}
	fmt.Println("\n  source logs are read-only · no traffic or usage leaves this machine")
	fmt.Println("  API-key agents routed through `burnban serve` appear separately as live spend.")
	return nil
}

func defaultHermesDB(home string) string {
	base := os.Getenv("HERMES_HOME")
	if base == "" {
		base = filepath.Join(home, ".hermes")
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			native := filepath.Join(local, "hermes")
			if _, err := os.Stat(filepath.Join(native, "state.db")); err == nil {
				base = native
			}
		}
	}
	return filepath.Join(base, "state.db")
}

func defaultOpenClawDir(home string) string {
	if value := os.Getenv("OPENCLAW_STATE_DIR"); value != "" {
		return value
	}
	return filepath.Join(home, ".openclaw")
}

func subsidyTitle(provider string) string {
	switch provider {
	case "claude-code":
		return "CLAUDE CODE"
	case "codex":
		return "CODEX"
	case "hermes":
		return "HERMES AGENT"
	case "openclaw":
		return "OPENCLAW"
	case "goose":
		return "GOOSE"
	default:
		return provider
	}
}
