package main

import (
	"flag"
	"fmt"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/store"
)

func cmdAlert(args []string) error {
	fs := flag.NewFlagSet("alert", flag.ExitOnError)
	webhook := fs.String("webhook", "", "Slack-compatible webhook URL, POSTed at the warn threshold and when a cap trips")
	off := fs.Bool("off", false, "remove the webhook")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	switch {
	case *off:
		if err := s.DeleteSetting(budget.KeyWebhookURL); err != nil {
			return err
		}
		// Re-arm every window's sent-notification marks alongside.
		for _, w := range budget.Windows() {
			if err := budget.ClearMarks(s, w.Name); err != nil {
				return err
			}
		}
		fmt.Println("webhook removed")
	case *webhook != "":
		if err := s.SetSetting(budget.KeyWebhookURL, *webhook); err != nil {
			return err
		}
		fmt.Println("webhook set — burnban will POST there at the warn threshold and once per tripped cap window")
	default:
		v, err := s.GetSetting(budget.KeyWebhookURL)
		if err != nil {
			return err
		}
		if v == "" {
			fmt.Println("no webhook set. Set one: burnban alert --webhook https://hooks.slack.com/...")
		} else {
			fmt.Printf("webhook: %s\n", v)
		}
	}
	return nil
}
