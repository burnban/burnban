package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/subsidy"
)

// cmdSubsidy prices local agent traffic that may never touch the proxy.
// Supported coding agents retain normalized token usage locally; the shared
// report engine reads those stores without modifying them.
func cmdSubsidy(args []string) error {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("subsidy", flag.ExitOnError)
	sinceArg := fs.String("since", "30d", `window: "today", "24h", "7d", "30d", or any Go duration`)
	claudeDir := fs.String("claude-dir", filepath.Join(home, ".claude", "projects"), "Claude Code session logs")
	codexDir := fs.String("codex-dir", filepath.Join(home, ".codex", "sessions"), "Codex rollout logs")
	geminiDir := fs.String("gemini-dir", subsidy.DefaultGeminiDir(home), "Gemini CLI project chat logs")
	copilotDir := fs.String("copilot-dir", subsidy.DefaultCopilotDir(home), "GitHub Copilot CLI session event logs")
	cursorDB := fs.String("cursor-db", subsidy.DefaultCursorDB(home), "Cursor global composer metadata database")
	openCodeDB := fs.String("opencode-db", subsidy.DefaultOpenCodeDB(home), "OpenCode usage database")
	hermesDB := fs.String("hermes-db", defaultHermesDB(home), "Hermes state database")
	openClawDir := fs.String("openclaw-dir", defaultOpenClawDir(home), "OpenClaw state directory")
	gooseDB := fs.String("goose-db", subsidy.DefaultGooseDB(home), "Goose session database")
	planCost := fs.Float64("plan-cost", 0, "what you pay per month across plans, for a single subsidy multiple")
	daily := fs.Bool("daily", false, "per-day breakdown")
	share := fs.Bool("share", false, "compact screenshot-ready card (defaults to a $200/mo plan comparison)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	meteredArg := fs.String("metered", "", "comma-separated sources known to be billed per token (e.g. claude-code,codex,gemini-cli,github-copilot-cli,cursor,opencode); auto-detected where auth proves it")
	noAutoMetered := fs.Bool("no-auto-metered", false, "do not auto-classify sources as metered from current API-key auth state")
	maxFiles := fs.Int("max-files", 5_000, "maximum local log files scanned per source")
	maxScanMB := fs.Int64("max-scan-mb", 512, "maximum local log MiB scanned per source")
	maxLineMB := fs.Int("max-line-mb", 32, "maximum size of one JSONL record in MiB")
	maxRecords := fs.Int("max-records", 1_000_000, "maximum records scanned per source")
	scanTimeout := fs.Duration("scan-timeout", 10*time.Second, "maximum scan time per local source")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *maxFiles <= 0 || *maxScanMB <= 0 || *maxLineMB <= 0 || *maxRecords <= 0 || *scanTimeout <= 0 {
		return fmt.Errorf("scan limits must all be greater than zero")
	}
	if *planCost < 0 {
		return fmt.Errorf("--plan-cost must be zero or greater")
	}
	if *maxScanMB > int64(^uint64(0)>>1)>>20 || uint64(*maxLineMB) > uint64(^uint(0)>>1)>>20 {
		return fmt.Errorf("scan size limit is too large for this platform")
	}

	from, label, err := parseSince(*sinceArg)
	if err != nil {
		return err
	}
	prices, err := pricing.Load()
	if err != nil {
		return err
	}
	metered := parseMeteredList(*meteredArg)
	if !*noAutoMetered {
		metered = append(metered, subsidy.DetectMeteredProviders(home)...)
	}
	until := time.Now()
	report, err := subsidy.BuildReport(prices, subsidy.ReportOptions{
		Since: from, Until: until,
		ClaudeDir: *claudeDir, CodexDir: *codexDir, GeminiDir: *geminiDir, CopilotDir: *copilotDir,
		CursorDB: *cursorDB, OpenCodeDB: *openCodeDB, HermesDB: *hermesDB, OpenClawDir: *openClawDir, GooseDB: *gooseDB,
		MeteredProviders: metered,
		ScanLimits: subsidy.ScanLimits{
			MaxFiles: *maxFiles, MaxBytes: *maxScanMB << 20, MaxLineBytes: *maxLineMB << 20,
			MaxRecords: *maxRecords, MaxDuration: *scanTimeout,
		},
	})
	if err != nil {
		return err
	}

	shareCard := subsidy.NewShareCard(report, label, *planCost)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if *share {
			return enc.Encode(shareCard)
		}
		return enc.Encode(map[string]any{
			"since": report.Since, "until": report.Until,
			"providers": report.Providers, "totals": report.Totals,
			"api_usd_total": report.APIUSD, "partial": report.Partial,
			"subscription_usd": report.SubscriptionUSD, "metered_usd": report.MeteredUSD,
			"unpriced_calls": report.UnpricedCalls, "unpriced_tokens": report.UnpricedTokens,
			"unpriced_models": report.UnpricedModels, "pricing": prices.Diagnostics(),
		})
	}

	if !report.HasUsage {
		fmt.Printf("no local agent usage in %s — checked:\n", label)
		for _, provider := range report.Providers {
			fmt.Printf("  %-12s %s\n", subsidyTitle(provider.Provider), terminalText(provider.Dir, 240))
			if provider.Detail != "" {
				fmt.Printf("    scan issue: %s\n", terminalText(provider.Detail, 240))
			}
			for _, warning := range provider.Scan.Warnings {
				fmt.Printf("    partial scan: %s\n", terminalText(warning, 200))
			}
		}
		if report.Partial {
			fmt.Println("PARTIAL REPORT: one or more local sources hit a scan limit or could not be read completely.")
		}
		printPricingDiagnostics(prices)
		return nil
	}
	if *share {
		fmt.Print(renderSubsidyShareCard(shareCard, stdoutIsTerminal() && os.Getenv("NO_COLOR") == ""))
		return nil
	}

	fmt.Printf("BURNBAN LOCAL USAGE — %s\n\n", label)
	fmt.Println("Auto-detected local agent logs. Subscription usage is priced at API rates")
	fmt.Println("for comparison; metered sources are real API spend that was already billed.")
	for _, provider := range report.Providers {
		if provider.Sessions == 0 && provider.Error == "" {
			continue
		}
		tag := "subscription · API-equivalent"
		if provider.MixedBilling {
			tag = fmt.Sprintf("MIXED · $%.2f subscription + $%.2f billed", provider.SubscriptionUSD, provider.MeteredUSD)
		} else if provider.Metered {
			tag = "REAL API SPEND · billed"
			if provider.BillingProvider != "" {
				tag = "REAL API SPEND · billed via " + provider.BillingProvider
			}
		}
		fmt.Printf("\n%s  %s · %d sessions · %s\n", subsidyTitle(provider.Provider), terminalText(provider.Dir, 240), provider.Sessions, tag)
		if provider.Detail != "" {
			fmt.Printf("  scan issue: %s\n", terminalText(provider.Detail, 240))
		} else if provider.Error != "" {
			fmt.Printf("  scan issue: %s\n", terminalText(provider.Error, 200))
		}
		for _, warning := range provider.Scan.Warnings {
			fmt.Printf("  partial scan: %s\n", terminalText(warning, 200))
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
			fmt.Fprintf(w, "  %s\t%d\t%s\t%s\t%s\t%s\t%s\n", terminalText(model.Model, 100), model.Calls,
				fmtTok(model.In), fmtTok(model.Out), fmtTok(model.CacheRead), fmtTok(model.CacheWrite), cost)
		}
		fmt.Fprintf(w, "  subtotal\t\t%s\t%s\t%s\t%s\t$%.2f\n",
			fmtTok(provider.In), fmtTok(provider.Out), fmtTok(provider.CacheRead), fmtTok(provider.CacheWrite), provider.APIUSD)
		w.Flush()
		if len(provider.ServiceTiers) > 0 {
			fmt.Printf("  service tiers  %s\n", formatCounts(provider.ServiceTiers))
		}
		if len(provider.InferenceGeos) > 0 {
			fmt.Printf("  inference geo  %s\n", formatCounts(provider.InferenceGeos))
		}
		if provider.ServerToolUse.WebSearchRequests > 0 || provider.ServerToolUse.WebFetchRequests > 0 {
			fmt.Printf("  server tools   web-search %d · web-fetch %d\n",
				provider.ServerToolUse.WebSearchRequests, provider.ServerToolUse.WebFetchRequests)
		}
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
	fmt.Printf("\nSUBSCRIPTION   $%.2f at API-equivalent prices (comparison, not a bill)\n", report.SubscriptionUSD)
	if report.MeteredUSD > 0 {
		fmt.Printf("REAL API SPEND $%.2f already billed per token (not a subsidy)\n", report.MeteredUSD)
	}
	subPace := report.SubscriptionUSD / windowDays * 30
	fmt.Printf("\nTOTAL  $%.2f API equivalent", report.APIUSD)
	if math.Abs(subPace-report.SubscriptionUSD) > .5 {
		fmt.Printf("  ·  subscription ≈ $%.2f/mo pace", subPace)
	}
	fmt.Println()
	if *planCost > 0 {
		fmt.Printf("\n  subscription vs your $%.0f/mo in plans → %.1fx the sticker price in API value\n", *planCost, subPace/(*planCost))
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
			// Compare only the subscription portion. A mixed provider can contain
			// separately billed events that must not inflate the plan subsidy.
			if provider.Metered || provider.SubscriptionUSD <= 0 || len(providerPlans) == 0 {
				continue
			}
			pace := provider.SubscriptionUSD / windowDays * 30
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
		fmt.Printf("\n  UNKNOWN PRICING: %d call(s), %s tokens across %s. Add them to\n",
			report.UnpricedCalls, fmtTok(report.UnpricedTokens), safeNames(report.UnpricedModels))
		fmt.Println("  ~/.burnban/pricing.json to include them in the dollar equivalent.")
	}
	if report.Partial {
		fmt.Println("\n  PARTIAL REPORT: one or more local sources hit a scan limit or could not be read completely.")
	}
	printPricingDiagnostics(prices)
	fmt.Println("\n  source logs are read-only · no traffic or usage leaves this machine")
	fmt.Println("  API-key agents routed through `burnban serve` appear separately as live spend.")
	return nil
}

const (
	shareCardInnerWidth = 58
	shareEmber          = "\033[38;5;202m"
	shareBold           = "\033[1m"
)

func renderSubsidyShareCard(card subsidy.ShareCard, color bool) string {
	border := func(left, fill, right string) string {
		return colorize(left+strings.Repeat(fill, shareCardInnerWidth+2)+right, cDim, color)
	}
	row := func(value, style string) string {
		value = terminalText(value, shareCardInnerWidth)
		padding := shareCardInnerWidth - utf8.RuneCountInString(value)
		if padding < 0 {
			padding = 0
		}
		return colorize("│", cDim, color) + " " + colorize(value+strings.Repeat(" ", padding), style, color) + " " + colorize("│", cDim, color) + "\n"
	}

	header := "BURNBAN SUBSIDY · " + strings.ToUpper(card.Window)
	if card.Partial {
		header = "PARTIAL · " + header
	}
	plan := fmt.Sprintf("$%.0f", card.PlanCostUSD)
	if math.Trunc(card.PlanCostUSD) != card.PlanCostUSD {
		plan = fmt.Sprintf("$%.2f", card.PlanCostUSD)
	}

	var b strings.Builder
	b.WriteString(border("┌", "─", "┐") + "\n")
	b.WriteString(row(header, shareBold))
	b.WriteString(row("", ""))
	b.WriteString(row("$"+formatShareUSD(card.APIEquivalentUSD)+" API-EQUIVALENT", shareEmber+shareBold))
	b.WriteString(row(fmt.Sprintf("%.1f× a %s/mo plan", card.Multiplier, plan), shareBold))
	b.WriteString(row("", ""))
	b.WriteString(row("Reproduce your number:", cDim))
	b.WriteString(row(card.InstallCommand, ""))
	b.WriteString(row(card.Website, shareEmber))
	b.WriteString(border("└", "─", "┘") + "\n")
	return b.String()
}

func formatShareUSD(value float64) string {
	parts := strings.SplitN(fmt.Sprintf("%.2f", value), ".", 2)
	whole := parts[0]
	sign := ""
	if strings.HasPrefix(whole, "-") {
		sign, whole = "-", strings.TrimPrefix(whole, "-")
	}
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	return sign + whole + "." + parts[1]
}

func printPricingDiagnostics(prices *pricing.Table) {
	diagnostics := prices.Diagnostics()
	fmt.Printf("\n  pricing table %s · effective %s · verified %s\n",
		diagnostics.Version, diagnostics.EffectiveDate, diagnostics.VerifiedDate)
	if len(diagnostics.ExpiredModels) > 0 {
		fmt.Printf("  pricing warning: validity window expired for %s\n", safeNames(diagnostics.ExpiredModels))
	}
}

func formatCounts(counts map[string]int64) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", terminalText(key, 80), counts[key]))
	}
	return strings.Join(parts, " · ")
}

func safeNames(names []string) string {
	safe := make([]string, 0, len(names))
	for _, name := range names {
		safe = append(safe, terminalText(name, 100))
	}
	return strings.Join(safe, ", ")
}

// parseMeteredList splits a comma-separated --metered value into normalized
// provider names, ignoring blanks and surrounding spaces.
func parseMeteredList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if name := strings.TrimSpace(part); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func defaultHermesDB(home string) string {
	return subsidy.DefaultHermesDB(home)
}

func defaultOpenClawDir(home string) string {
	return subsidy.DefaultOpenClawDir(home)
}

func subsidyTitle(provider string) string {
	switch provider {
	case "claude-code":
		return "CLAUDE CODE"
	case "codex":
		return "CODEX"
	case "gemini-cli":
		return "GEMINI CLI"
	case "github-copilot-cli":
		return "GITHUB COPILOT CLI"
	case "cursor":
		return "CURSOR"
	case "opencode":
		return "OPENCODE"
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
