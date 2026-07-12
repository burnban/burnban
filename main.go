// Command burnban is a local pass-through proxy that meters, itemizes, and
// caps what your AI agents spend. Meters watch. Burnban acts.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

var version = "0.4.0-dev"

func main() {
	if len(os.Args) < 2 {
		// A brand-new install gets the guided first run. Once configured, the
		// bare command opens the interface the user chose during setup.
		cfg := loadConfig()
		if !cfg.SetupDone {
			if err := cmdSetup(nil); err != nil {
				fmt.Fprintln(os.Stderr, "burnban:", terminalText(err.Error(), 500))
				os.Exit(1)
			}
			return
		}
		if err := launchConfiguredInterface(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "burnban:", terminalText(err.Error(), 500))
			os.Exit(1)
		}
		return
	}
	var err error
	switch os.Args[1] {
	case "setup":
		err = cmdSetup(os.Args[2:])
	case "guide":
		err = cmdGuide(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "desktop":
		err = cmdDesktop(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "stop":
		err = cmdStop(os.Args[2:])
	case "top":
		err = cmdTop(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "cap":
		err = cmdCap(os.Args[2:])
	case "ban":
		err = cmdBan(os.Args[2:])
	case "lift":
		err = cmdLift(os.Args[2:])
	case "mcp":
		err = cmdMCP(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "alert":
		err = cmdAlert(os.Args[2:])
	case "demo":
		err = cmdDemo(os.Args[2:])
	case "whatif":
		err = cmdWhatif(os.Args[2:])
	case "subsidy":
		err = cmdSubsidy(os.Args[2:])
	case "bench":
		err = cmdBench(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "pricing":
		err = cmdPricing(os.Args[2:])
	case "prune":
		err = cmdPrune(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("burnban", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "burnban: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "burnban:", terminalText(err.Error(), 500))
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`burnban — meter, itemize, and cap what your AI agents spend

usage: burnban <command> [flags]

  setup    guided first-run: see local usage or connect live enforcement
  guide    plain-language walkthrough of what burnban does
  serve    run the metering proxy (point your agents at it)
  desktop  run the real meter and open its dashboard (desktop launcher)
  status   show the running meter, URL, database, and health
  stop     gracefully stop the meter using its private local control file
  demo     seed fake traffic and serve the dashboard — see it alive in 5s
  top      live spend view, refreshed in place
  report   spend + waste receipts for a window (--since today|24h|7d)
  whatif   reprice a window's traffic onto other models ("what would 7d cost on haiku?")
  subsidy  price local agent/subscription logs at API rates — no proxy needed
  cap      set budgets (--daily 10 --weekly 40 --monthly 120 [--agent NAME] [--warn 80] | --off)
  ban      pause ALL agent spend immediately
  lift     lift the local ban (--today also overrides local caps)
  mcp      MCP server over stdio — plug burnban into Claude Code, Cursor, etc.
  export   dump raw request rows for finance (--since 7d --format csv|json)
  alert    webhook for cap alerts and 80% warnings (--webhook URL | --off)
  bench    measure burnban's own added latency against a loopback upstream
  doctor   verify pricing, database health, server health, and agent routing
  pricing  inspect the effective price table and its provenance
  prune    delete old ledger rows after an explicit retention window
  version  print version

Commands that use the ledger accept --db (default ~/.burnban/burnban.db, or $BURNBAN_DB).
Run burnban <command> --help for command-specific flags.
`)
}

func defaultDBPath() string {
	if v := os.Getenv("BURNBAN_DB"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "burnban.db"
	}
	return filepath.Join(home, ".burnban", "burnban.db")
}
