package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/burnban/burnban/internal/approvalclient"
	"github.com/burnban/burnban/internal/mcp"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	allowBudgetAdmin := fs.Bool("allow-budget-admin", false, "allow MCP tools to change caps or the burn ban (default is read-only)")
	allowBudgetRequests := fs.Bool("allow-budget-requests", false, "allow an agent to create pending Team budget requests (never grants them)")
	approvalURL := fs.String("approval-url", os.Getenv("BURNBAN_TEAMS_URL"), "Team control-plane base URL for budget requests")
	approvalMeterID := fs.String("approval-meter-id", os.Getenv("BURNBAN_TEAMS_METER_ID"), "enrolled Team meter ID for budget requests")
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
	var requester approvalclient.Requester
	if *allowBudgetRequests {
		token := os.Getenv("BURNBAN_TEAMS_METER_TOKEN")
		client, err := approvalclient.New(*approvalURL, *approvalMeterID, token)
		if err != nil {
			return fmt.Errorf("budget request configuration: %w", err)
		}
		requester = client
	}

	srv := &mcp.Server{
		S: s, Prices: prices, Version: version, In: os.Stdin, Out: os.Stdout,
		AllowBudgetAdmin: *allowBudgetAdmin, AllowBudgetRequests: *allowBudgetRequests, ApprovalRequester: requester,
	}
	return srv.Run()
}
