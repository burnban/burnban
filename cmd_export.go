package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

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
			"in_tokens", "out_tokens", "cache_read_tokens", "cache_write_tokens", "cache_write_1h_tokens",
			"cost_usd", "latency_ms", "status", "streamed", "usage_state", "pricing_state",
			"incomplete", "enforcement_unsafe", "route", "service_tier", "inference_geo",
			"server_tool_calls", "fee_unpriced"}); err != nil {
			return err
		}
		for _, r := range rows {
			if err := w.Write([]string{
				r.Ts.UTC().Format(time.RFC3339), spreadsheetText(r.Provider), spreadsheetText(r.Model), spreadsheetText(r.Agent), spreadsheetText(r.Session),
				strconv.FormatInt(r.InTokens, 10), strconv.FormatInt(r.OutTokens, 10),
				strconv.FormatInt(r.CacheReadTokens, 10), strconv.FormatInt(r.CacheWriteTokens, 10),
				strconv.FormatInt(r.CacheWrite1hTokens, 10),
				strconv.FormatFloat(r.CostUSD, 'f', -1, 64), strconv.FormatInt(r.LatencyMs, 10),
				strconv.Itoa(r.Status), strconv.FormatBool(r.Streamed),
				string(r.UsageState), string(r.PricingState), strconv.FormatBool(r.Incomplete),
				strconv.FormatBool(r.EnforcementUnsafe), spreadsheetText(r.Route),
				spreadsheetText(r.ServiceTier), spreadsheetText(r.InferenceGeo),
				strconv.FormatInt(r.ServerToolCalls, 10), strconv.FormatBool(r.FeeUnpriced),
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
