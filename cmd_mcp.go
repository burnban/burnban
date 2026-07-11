package main

import (
	"flag"
	"os"

	"github.com/burnban/burnban/internal/mcp"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	allowBudgetAdmin := fs.Bool("allow-budget-admin", false, "allow MCP tools to change caps or the burn ban (default is read-only)")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	prices, err := pricing.Load()
	if err != nil {
		return err
	}

	srv := &mcp.Server{
		S: s, Prices: prices, Version: version, In: os.Stdin, Out: os.Stdout,
		AllowBudgetAdmin: *allowBudgetAdmin,
	}
	return srv.Run()
}
