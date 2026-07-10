package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/syft8/burnban/internal/store"
)

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	since := fs.String("since", "7d", `window: "today", "24h", "7d", or any Go duration`)
	format := fs.String("format", "csv", "csv or json")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	from, _, err := parseSince(*since)
	if err != nil {
		return err
	}
	rows, err := s.Export(from)
	if err != nil {
		return err
	}

	switch *format {
	case "csv":
		w := csv.NewWriter(os.Stdout)
		if err := w.Write([]string{"ts", "provider", "model", "agent", "session",
			"in_tokens", "out_tokens", "cache_read_tokens", "cache_write_tokens",
			"cost_usd", "latency_ms", "status", "streamed", "estimated", "priced"}); err != nil {
			return err
		}
		for _, r := range rows {
			if err := w.Write([]string{
				r.Ts.UTC().Format(time.RFC3339), r.Provider, r.Model, r.Agent, r.Session,
				strconv.FormatInt(r.InTokens, 10), strconv.FormatInt(r.OutTokens, 10),
				strconv.FormatInt(r.CacheReadTokens, 10), strconv.FormatInt(r.CacheWriteTokens, 10),
				strconv.FormatFloat(r.CostUSD, 'f', -1, 64), strconv.FormatInt(r.LatencyMs, 10),
				strconv.Itoa(r.Status), strconv.FormatBool(r.Streamed),
				strconv.FormatBool(r.Estimated), strconv.FormatBool(r.Priced),
			}); err != nil {
				return err
			}
		}
		w.Flush()
		return w.Error()
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	default:
		return fmt.Errorf("bad --format %q: use csv or json", *format)
	}
}
