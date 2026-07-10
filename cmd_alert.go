package main

import (
	"flag"
	"fmt"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/store"
)

func cmdAlert(args []string) error {
	fs := flag.NewFlagSet("alert", flag.ExitOnError)
	webhook := fs.String("webhook", "", "Slack-compatible webhook URL, POSTed when the daily cap is reached")
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
		if err := s.DeleteSetting(budget.KeyAlertedDay); err != nil {
			return err
		}
		fmt.Println("webhook removed")
	case *webhook != "":
		if err := s.SetSetting(budget.KeyWebhookURL, *webhook); err != nil {
			return err
		}
		fmt.Println("webhook set — burnban will POST there the first time the cap trips each day")
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
