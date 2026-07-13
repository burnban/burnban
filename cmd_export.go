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
	"cost_source", "cost_source_ref", "cost_effective_from", "cost_valid_through", "cost_confidence",
	"identity_tenant", "identity_device", "principal", "service_account", "project", "cost_center", "identity_confidence",
	"requested_provider", "requested_model", "requested_route", "downshift_action", "downshift_rule",
	"downshift_trigger", "downshift_reason", "downshift_config_digest", "downshift_features_json",
	"downshift_source_estimated_usd", "downshift_target_estimated_usd",
	"policy_decision_id", "policy_digest", "policy_revision", "policy_name",
	"policy_namespace", "policy_mode", "policy_outcome", "policy_admitted", "policy_confidence", "policy_context_json",
	"policy_explanation_json",
}

func writeCSVExport(out io.Writer, s *store.Store, from time.Time) error {
	w := csv.NewWriter(out)
	if err := w.Write(exportCSVHeader); err != nil {
		return err
	}
	streamErr := s.StreamExport(from, func(r store.Request) error {
		row := []string{
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
			spreadsheetText(string(r.CostSource)), spreadsheetText(r.CostSourceRef),
			spreadsheetText(r.CostEffectiveFrom), spreadsheetText(r.CostValidThrough),
			spreadsheetText(string(r.CostConfidence)),
			spreadsheetText(r.IdentityTenant), spreadsheetText(r.IdentityDevice), spreadsheetText(r.Principal),
			spreadsheetText(r.ServiceAccount), spreadsheetText(r.Project), spreadsheetText(r.CostCenter),
			spreadsheetText(r.IdentityConfidence),
			spreadsheetText(r.RequestedProvider), spreadsheetText(r.RequestedModel), spreadsheetText(r.RequestedRoute),
			spreadsheetText(r.DownshiftAction), spreadsheetText(r.DownshiftRule), spreadsheetText(r.DownshiftTrigger),
			spreadsheetText(r.DownshiftReason), spreadsheetText(r.DownshiftDigest), spreadsheetText(r.DownshiftFeatures),
			strconv.FormatFloat(r.DownshiftSourceUSD, 'f', -1, 64), strconv.FormatFloat(r.DownshiftTargetUSD, 'f', -1, 64),
			"0", "", "0", "", "", "", "", "false", "", "", "",
		}
		if r.Policy != nil {
			start := len(row) - 11
			row[start] = strconv.FormatInt(r.Policy.DecisionID, 10)
			row[start+1] = spreadsheetText(r.Policy.Digest)
			row[start+2] = strconv.FormatInt(r.Policy.Revision, 10)
			row[start+3] = spreadsheetText(r.Policy.Name)
			row[start+4] = spreadsheetText(r.Policy.Namespace)
			row[start+5] = spreadsheetText(r.Policy.Mode)
			row[start+6] = spreadsheetText(r.Policy.Outcome)
			row[start+7] = strconv.FormatBool(r.Policy.Admitted)
			row[start+8] = spreadsheetText(r.Policy.Confidence)
			row[start+9] = spreadsheetText(r.Policy.ContextJSON)
			row[start+10] = spreadsheetText(r.Policy.ExplanationJSON)
		}
		return w.Write(row)
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
