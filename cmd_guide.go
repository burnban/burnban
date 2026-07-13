package main

import (
	"flag"
	"fmt"
	"os"
)

// cmdGuide prints a plain-language walkthrough for people who just installed
// burnban and want to know what it does and which command to run.
func cmdGuide(args []string) error {
	fs := flag.NewFlagSet("guide", flag.ExitOnError)
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	color := stdoutIsTerminal() && os.Getenv("NO_COLOR") == ""

	h := func(s string) { fmt.Println("\n" + colorize(s, shareEmber, color)) }
	fmt.Println()
	fmt.Println(colorize("  burnban", shareEmber, color) + "  the field guide")

	h("  What it does")
	fmt.Println("  burnban shows what your AI agents cost and lets you cap it. It reads")
	fmt.Println("  two things, and they are not the same number:")

	h("  1. Subscription usage  (no setup, works right now)")
	fmt.Println("  Claude Code, Codex, and friends keep local logs of what they ran.")
	fmt.Println("  burnban reads those and prices them at API rates, so you can see")
	fmt.Println("  what a flat-rate plan would have cost on metered keys.")
	fmt.Println("    " + colorize("burnban subsidy", shareEmber, color) + "            today's usage, priced")
	fmt.Println("    " + colorize("burnban subsidy --since 30d", shareEmber, color) + " a month of it")

	h("  2. Live proxy meter  (opt in, and it can enforce)")
	fmt.Println("  Point an agent at burnban and every call flows through it. Now the")
	fmt.Println("  dashboard fills in live, and a cap can actually stop spend.")
	fmt.Println("    " + colorize("ANTHROPIC_BASE_URL=http://127.0.0.1:4141/anthropic", cDim, color))
	fmt.Println("    " + colorize("OPENAI_BASE_URL=http://127.0.0.1:4141/openai/v1", cDim, color))
	fmt.Println("  A fresh dashboard reads $0 until an agent is pointed here. That is")
	fmt.Println("  expected, not a bug: nothing has been routed through the meter yet.")

	h("  How to watch it")
	fmt.Println("    " + colorize("burnban desktop", shareEmber, color) + "  dashboard as a desktop app (dock icon)")
	fmt.Println("    " + colorize("burnban serve", shareEmber, color) + "    dashboard in your browser at localhost:4141")
	fmt.Println("    " + colorize("burnban top", shareEmber, color) + "      live numbers in your terminal")

	h("  How to cap it")
	fmt.Println("  Caps apply only to live API traffic routed through the proxy above.")
	fmt.Println("  They do not stop or alter the local subscription usage scan.")
	fmt.Println("    " + colorize("burnban cap --daily 10", shareEmber, color) + "  stop spend past $10/day")
	fmt.Println("    " + colorize("burnban fuse --burst 5m:4", shareEmber, color) + "  stop loops past $4/5m")
	fmt.Println("    " + colorize("burnban ban", shareEmber, color) + "             pause all agent spend now")
	fmt.Println("    " + colorize("burnban lift", shareEmber, color) + "            undo the ban")

	h("  Good to know")
	fmt.Println("  Everything is local. Your keys pass through, they are never stored,")
	fmt.Println("  and nothing leaves your machine. Data lives in ~/.burnban.")
	fmt.Println("  Change your interface anytime: " + colorize("burnban setup", shareEmber, color))
	fmt.Println()
	return nil
}
