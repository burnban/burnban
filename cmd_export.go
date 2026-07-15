package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/burnban/burnban/internal/export"
	"github.com/burnban/burnban/internal/store"
)

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	since := fs.String("since", "7d", `window: "today", "24h", "7d", or any Go duration`)
	format := fs.String("format", "csv", "csv or json")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *format != "csv" && *format != "json" {
		return fmt.Errorf("bad --format %q: use csv or json", *format)
	}
	from, _, err := parseSince(*since)
	if err != nil {
		return err
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	switch *format {
	case "csv":
		return export.WriteCSV(os.Stdout, s, from)
	case "json":
		return export.WriteJSON(os.Stdout, s, from)
	}
	return nil
}
