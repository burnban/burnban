package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
	"github.com/burnban/burnban/internal/subsidy"
)

// burnbanConfig persists the one-time onboarding choice so bare `burnban`
// knows whether to greet a new user or send them straight to their meter.
type burnbanConfig struct {
	Version   int    `json:"version"`
	SetupDone bool   `json:"setup_done"`
	Interface string `json:"interface"`       // "desktop" | "web" | "terminal"
	Goal      string `json:"goal,omitempty"`  // "observe" | "enforce"
	Agent     string `json:"agent,omitempty"` // "claude" | "codex" | "other"
}

func defaultConfigPath() string {
	if v := os.Getenv("BURNBAN_CONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "burnban.config.json"
	}
	return filepath.Join(home, ".burnban", "config.json")
}

// loadConfig never fails loudly: a missing or corrupt file just means "not set
// up yet", which is the safe default that triggers onboarding.
func loadConfig() burnbanConfig {
	var cfg burnbanConfig
	b, err := os.ReadFile(defaultConfigPath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(b, &cfg)
	return cfg
}

func saveConfig(cfg burnbanConfig) error {
	path := defaultConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// cmdSetup is the guided first run. It initializes the ledger, shows local
// API-equivalent usage, separates observation from live enforcement, remembers
// the chosen interface, and starts it when requested.
func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	noLaunch := fs.Bool("no-launch", false, "configure only; do not start the chosen interface")
	ifNeeded := fs.Bool("if-needed", false, "skip setup when first-run configuration already exists")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}

	color := stdoutIsTerminal() && os.Getenv("NO_COLOR") == ""
	interactive := fileIsTerminal(os.Stdin) && fileIsTerminal(os.Stdout)
	if cfg := loadConfig(); *ifNeeded && cfg.SetupDone {
		fmt.Println("burnban setup already complete — run " + colorize("burnban", shareEmber, color) + " to open your meter")
		return nil
	}
	if err := initializeLedger(); err != nil {
		return fmt.Errorf("initialize the local meter: %w", err)
	}

	printSetupWelcome(color)

	// Instant value: reprice the last 24h of local subscription traffic.
	if card, err := scanSubsidyCard("24h"); err != nil {
		fmt.Println("  Could not scan your agent logs just now. That's fine, burnban still works.")
	} else if !card.HasUsage {
		fmt.Println("  No agent traffic found yet. Once Claude Code, Codex, or others run,")
		fmt.Println("  burnban prices it here automatically.")
	} else {
		printSetupNumber(card, color)
	}
	fmt.Println()

	if !interactive {
		// Piped installer or CI: initialize the ledger, but leave choices to a
		// real terminal instead of guessing how this machine will be used.
		fmt.Println("  Finish setup in your terminal with:")
		fmt.Println("    " + colorize("burnban setup", shareEmber, color))
		return nil
	}

	reader := bufio.NewReader(os.Stdin)
	goal := promptGoal(reader, color)
	agent := ""
	if goal == "enforce" {
		agent = promptAgent(reader, color)
		printAgentRoute(agent, color)
		maybeSetCap(reader, color)
	}
	choice := promptInterface(reader, color)
	cfg := burnbanConfig{Version: 2, SetupDone: true, Interface: choice, Goal: goal, Agent: agent}

	if err := saveConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "burnban: could not save your setup choice (%v); continuing anyway\n", err)
	}

	printSetupDone(cfg, !*noLaunch, color)

	if *noLaunch {
		return nil
	}
	return launchConfiguredInterface(cfg)
}

func initializeLedger() error {
	s, err := store.Open(defaultDBPath())
	if err != nil {
		return err
	}
	return s.Close()
}

func launchConfiguredInterface(cfg burnbanConfig) error {
	switch cfg.Interface {
	case "desktop":
		return cmdDesktop(nil)
	case "web":
		return cmdServeMode(nil, true)
	case "terminal":
		return cmdTop(nil)
	default:
		return fmt.Errorf("saved interface %q is invalid; run `burnban setup` to choose again", cfg.Interface)
	}
}

// scanSubsidyCard reprices a window of local agent logs, mirroring the default
// sources of `burnban subsidy`, and returns the compact share card.
func scanSubsidyCard(window string) (subsidy.ShareCard, error) {
	from, label, err := parseSince(window)
	if err != nil {
		return subsidy.ShareCard{}, err
	}
	prices, err := pricing.Load()
	if err != nil {
		return subsidy.ShareCard{}, err
	}
	home, _ := os.UserHomeDir()
	report, err := subsidy.BuildReport(prices, subsidy.ReportOptions{
		Since:       from,
		Until:       time.Now(),
		ClaudeDir:   filepath.Join(home, ".claude", "projects"),
		CodexDir:    filepath.Join(home, ".codex", "sessions"),
		GeminiDir:   subsidy.DefaultGeminiDir(home),
		OpenCodeDB:  subsidy.DefaultOpenCodeDB(home),
		HermesDB:    defaultHermesDB(home),
		OpenClawDir: defaultOpenClawDir(home),
		GooseDB:     subsidy.DefaultGooseDB(home),
		ScanLimits: subsidy.ScanLimits{
			MaxFiles:     5_000,
			MaxBytes:     512 << 20,
			MaxLineBytes: 32 << 20,
			MaxRecords:   1_000_000,
			// Keep first-run snappy; a deep audit is `burnban subsidy`.
			MaxDuration: 6 * time.Second,
		},
	})
	if err != nil {
		return subsidy.ShareCard{}, err
	}
	return subsidy.NewShareCard(report, label, 0), nil
}

func printSetupWelcome(color bool) {
	fmt.Println()
	fmt.Println("  " + colorize("burnban", shareEmber, color) + "  meter, itemize, and cap what your AI agents spend")
	fmt.Println()
	fmt.Println("  " + colorize("Read-only local scan. Nothing is uploaded or sent to Burnban.", cDim, color))
	fmt.Println("  " + colorize("Scanning supported agent logs from the last 24 hours...", cDim, color))
	fmt.Println()
}

func printSetupNumber(card subsidy.ShareCard, color bool) {
	line := func(label, value string) {
		fmt.Printf("  %-32s %s\n", label, colorize(value, shareEmber, color))
	}
	line("Your agents, last 24h", fmtUSD(card.APIEquivalentUSD)+" at API rates")
	if card.Partial {
		fmt.Println("  " + colorize("Partial scan: some local records could not be included in this quick pass.", cDim, color))
	}
	fmt.Println()
	fmt.Println("  " + colorize("This is an API-price comparison, not a provider bill.", cDim, color))
	fmt.Println("  " + colorize("Caps apply only to API traffic you explicitly route through burnban.", cDim, color))
}

func printGoalMenu(color bool) {
	fmt.Println("  What do you want to do first?")
	fmt.Println()
	fmt.Println("    " + colorize("1", shareEmber, color) + "  See local usage    price supported logs already on this machine")
	fmt.Println("    " + colorize("2", shareEmber, color) + "  Enforce a cap      route live API traffic through burnban")
}

func promptGoal(reader *bufio.Reader, color bool) string {
	printGoalMenu(color)
	for {
		fmt.Print("\n  Pick 1-2 [1]: ")
		line, err := reader.ReadString('\n')
		switch strings.TrimSpace(line) {
		case "", "1":
			return "observe"
		case "2":
			return "enforce"
		default:
			fmt.Println("  Please enter 1 or 2.")
		}
		if err != nil {
			return "observe"
		}
	}
}

func promptAgent(reader *bufio.Reader, color bool) string {
	fmt.Println("\n  Which live agent do you want to connect?")
	fmt.Println()
	fmt.Println("    " + colorize("1", shareEmber, color) + "  Claude Code / Anthropic")
	fmt.Println("    " + colorize("2", shareEmber, color) + "  Codex / OpenAI")
	fmt.Println("    " + colorize("3", shareEmber, color) + "  Another agent / do this later")
	for {
		fmt.Print("\n  Pick 1-3 [1]: ")
		line, err := reader.ReadString('\n')
		switch strings.TrimSpace(line) {
		case "", "1":
			return "claude"
		case "2":
			return "codex"
		case "3":
			return "other"
		default:
			fmt.Println("  Please enter 1, 2, or 3.")
		}
		if err != nil {
			return "claude"
		}
	}
}

func routeCommand(agent, goos string) string {
	name, value := "ANTHROPIC_BASE_URL", "http://127.0.0.1:4141/anthropic"
	if agent == "codex" {
		name, value = "OPENAI_BASE_URL", "http://127.0.0.1:4141/openai/v1"
	}
	if goos == "windows" {
		return fmt.Sprintf("$env:%s='%s'", name, value)
	}
	return fmt.Sprintf("export %s='%s'", name, value)
}

func printAgentRoute(agent string, color bool) {
	fmt.Println()
	fmt.Println("  " + colorize("Caps cover routed API traffic only.", shareEmber, color))
	if agent == "other" {
		fmt.Println("  Start the dashboard and open ‘Connect an API-key agent’ for exact routes.")
		return
	}
	fmt.Println("  After the meter starts, run this before starting your agent:")
	fmt.Println("    " + colorize(routeCommand(agent, runtime.GOOS), shareEmber, color))
	fmt.Println("  Restart the agent, then verify the route with: " + colorize("burnban doctor", shareEmber, color))
}

func printInterfaceMenu(color bool) {
	fmt.Println("\n  How do you want to watch it?")
	fmt.Println()
	fmt.Println("    " + colorize("1", shareEmber, color) + "  Desktop app    launcher icon that opens the dashboard")
	fmt.Println("    " + colorize("2", shareEmber, color) + "  Web dashboard  opens in your browser at localhost:4141")
	fmt.Println("    " + colorize("3", shareEmber, color) + "  Terminal       live numbers right here, no browser")
}

func promptInterface(reader *bufio.Reader, color bool) string {
	choice := recommendedInterface(runtime.GOOS, os.Getenv("DISPLAY"), os.Getenv("WAYLAND_DISPLAY"))
	return promptInterfaceDefault(reader, color, choice)
}

func recommendedInterface(goos, display, wayland string) string {
	if goos == "linux" && strings.TrimSpace(display) == "" && strings.TrimSpace(wayland) == "" {
		return "terminal"
	}
	return "desktop"
}

func promptInterfaceDefault(reader *bufio.Reader, color bool, defaultChoice string) string {
	printInterfaceMenu(color)
	defaultNumber := map[string]string{"desktop": "1", "web": "2", "terminal": "3"}[defaultChoice]
	if defaultNumber == "" {
		defaultChoice, defaultNumber = "desktop", "1"
	}
	for {
		fmt.Printf("\n  Pick 1-3 [%s]: ", defaultNumber)
		line, err := reader.ReadString('\n')
		switch strings.TrimSpace(line) {
		case "":
			return defaultChoice
		case "1":
			return "desktop"
		case "2":
			return "web"
		case "3":
			return "terminal"
		default:
			fmt.Println("  Please enter 1, 2, or 3.")
		}
		if err != nil {
			return defaultChoice
		}
	}
}

func maybeSetCap(reader *bufio.Reader, color bool) {
	fmt.Print("\n  Arm a daily cap for routed API traffic? Enter dollars, or press Enter to skip: $")
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "$"))
	if line == "" {
		fmt.Println("  No cap set. You can add one later: " + colorize("burnban cap --daily 10", shareEmber, color))
		return
	}
	amount, err := strconv.ParseFloat(line, 64)
	if err != nil || amount < 0.01 {
		fmt.Println("  Not a valid amount, skipping. Set one later: " + colorize("burnban cap --daily 10", shareEmber, color))
		return
	}
	if err := cmdCap([]string{"--daily", strconv.FormatFloat(amount, 'f', -1, 64)}); err != nil {
		fmt.Printf("  Could not set cap: %v\n", err)
	}
}

func printSetupDone(cfg burnbanConfig, willLaunch, color bool) {
	launch := map[string]string{"desktop": "burnban desktop", "web": "burnban serve", "terminal": "burnban top"}[cfg.Interface]
	fmt.Println()
	fmt.Println("  " + colorize(strings.Repeat("─", 52), cDim, color))
	fmt.Println("  " + colorize("You're set.", shareEmber, color) + " Run " + colorize("burnban", shareEmber, color) + " to open this view again.")
	fmt.Println()
	fmt.Printf("  Watch your spend     %s\n", colorize(launch, shareEmber, color))
	fmt.Printf("  Price your usage     %s\n", colorize("burnban subsidy", shareEmber, color))
	fmt.Printf("  Full walkthrough     %s\n", colorize("burnban guide", shareEmber, color))
	if cfg.Goal == "enforce" {
		fmt.Printf("  Verify agent route   %s\n", colorize("burnban doctor", shareEmber, color))
	}
	fmt.Println()
	if willLaunch {
		fmt.Println("  " + colorize("Starting it now. Press ctrl+c to stop.", cDim, color))
	} else {
		fmt.Println("  " + colorize("Start it now with:  ", cDim, color) + colorize(launch, shareEmber, color))
	}
	fmt.Println()
}

// fmtUSD renders a dollar value with thousands separators, e.g. $12,715.80.
func fmtUSD(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	cents := int64(math.Round(v * 100))
	whole := strconv.FormatInt(cents/100, 10)
	frac := cents % 100

	var grouped strings.Builder
	for i, digit := range []byte(whole) {
		if i > 0 && (len(whole)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteByte(digit)
	}
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s$%s.%02d", sign, grouped.String(), frac)
}
