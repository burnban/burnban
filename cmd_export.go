package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
		return writeCSVExport(os.Stdout, s, from)
	case "json":
		return writeJSONExport(os.Stdout, s, from)
	}
	return nil
}

var exportCSVHeader = []string{
	"ts", "provider", "model", "agent", "session",
	"in_tokens", "out_tokens", "cache_read_tokens", "cache_write_tokens", "cache_write_1h_tokens",
	"cost_usd", "latency_ms", "status", "streamed", "usage_state", "pricing_state",
	"incomplete", "enforcement_unsafe", "route", "service_tier", "inference_geo",
	"server_tool_calls", "fee_unpriced",
}

func writeCSVExport(out io.Writer, s *store.Store, from time.Time) error {
	w := csv.NewWriter(out)
	if err := w.Write(exportCSVHeader); err != nil {
		return err
	}
	streamErr := s.StreamExport(from, func(r store.Request) error {
		return w.Write([]string{
			r.Ts.UTC().Format(time.RFC3339), spreadsheetText(r.Provider), spreadsheetText(r.Model), spreadsheetText(r.Agent), spreadsheetText(r.Session),
			strconv.FormatInt(r.InTokens, 10), strconv.FormatInt(r.OutTokens, 10),
			strconv.FormatInt(r.CacheReadTokens, 10), strconv.FormatInt(r.CacheWriteTokens, 10),
			strconv.FormatInt(r.CacheWrite1hTokens, 10),
			strconv.FormatFloat(r.CostUSD, 'f', -1, 64), strconv.FormatInt(r.LatencyMs, 10),
			strconv.Itoa(r.Status), strconv.FormatBool(r.Streamed),
			spreadsheetText(string(r.UsageState)), spreadsheetText(string(r.PricingState)), strconv.FormatBool(r.Incomplete),
			strconv.FormatBool(r.EnforcementUnsafe), spreadsheetText(r.Route),
			spreadsheetText(r.ServiceTier), spreadsheetText(r.InferenceGeo),
			strconv.FormatInt(r.ServerToolCalls, 10), strconv.FormatBool(r.FeeUnpriced),
		})
	})
	w.Flush()
	return errors.Join(streamErr, w.Error())
}

func writeJSONExport(out io.Writer, s *store.Store, from time.Time) error {
	return writeJSONRequestStream(out, func(visit func(store.Request) error) error {
		return s.StreamExport(from, visit)
	})
}

// writeJSONRequestStream closes the JSON array even if SQLite reports an
// iteration error after earlier rows. Apart from an output-device failure,
// callers therefore never receive malformed partial JSON.
func writeJSONRequestStream(out io.Writer, stream func(func(store.Request) error) error) error {
	if _, err := io.WriteString(out, "["); err != nil {
		return err
	}
	first := true
	streamErr := stream(func(r store.Request) error {
		encoded, err := json.Marshal(r)
		if err != nil {
			return err
		}
		prefix := ",\n  "
		if first {
			prefix = "\n  "
			first = false
		}
		if _, err := io.WriteString(out, prefix); err != nil {
			return err
		}
		_, err = out.Write(encoded)
		return err
	})
	suffix := "]\n"
	if !first {
		suffix = "\n]\n"
	}
	_, closeErr := io.WriteString(out, suffix)
	return errors.Join(streamErr, closeErr)
}
