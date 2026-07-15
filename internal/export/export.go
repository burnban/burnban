// Package export encodes raw ledger rows for finance and audit consumers.
// The CLI `burnban export` and the dashboard's download endpoint share these
// writers so both surfaces emit byte-identical evidence for the same window.
package export

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/burnban/burnban/internal/store"
)

// CSVHeader is the stable export column order. Append-only: downstream
// spreadsheets and pipelines key on these names.
var CSVHeader = []string{
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

// WriteCSV streams every ledger row since `from` as spreadsheet-safe CSV.
func WriteCSV(out io.Writer, s *store.Store, from time.Time) error {
	w := csv.NewWriter(out)
	if err := w.Write(CSVHeader); err != nil {
		return err
	}
	streamErr := s.StreamExport(from, func(r store.Request) error {
		return w.Write(csvRow(r))
	})
	w.Flush()
	return errors.Join(streamErr, w.Error())
}

func csvRow(r store.Request) []string {
	row := []string{
		r.Ts.UTC().Format(time.RFC3339), SpreadsheetText(r.Provider), SpreadsheetText(r.Model), SpreadsheetText(r.Agent), SpreadsheetText(r.Session),
		strconv.FormatInt(r.InTokens, 10), strconv.FormatInt(r.OutTokens, 10),
		strconv.FormatInt(r.CacheReadTokens, 10), strconv.FormatInt(r.CacheWriteTokens, 10),
		strconv.FormatInt(r.CacheWrite1hTokens, 10),
		strconv.FormatFloat(r.CostUSD, 'f', -1, 64), strconv.FormatInt(r.LatencyMs, 10),
		strconv.Itoa(r.Status), strconv.FormatBool(r.Streamed),
		SpreadsheetText(string(r.UsageState)), SpreadsheetText(string(r.PricingState)), strconv.FormatBool(r.Incomplete),
		strconv.FormatBool(r.EnforcementUnsafe), SpreadsheetText(r.Route),
		SpreadsheetText(r.ServiceTier), SpreadsheetText(r.InferenceGeo),
		strconv.FormatInt(r.ServerToolCalls, 10), strconv.FormatBool(r.FeeUnpriced),
		SpreadsheetText(string(r.CostSource)), SpreadsheetText(r.CostSourceRef),
		SpreadsheetText(r.CostEffectiveFrom), SpreadsheetText(r.CostValidThrough),
		SpreadsheetText(string(r.CostConfidence)),
		SpreadsheetText(r.IdentityTenant), SpreadsheetText(r.IdentityDevice), SpreadsheetText(r.Principal),
		SpreadsheetText(r.ServiceAccount), SpreadsheetText(r.Project), SpreadsheetText(r.CostCenter),
		SpreadsheetText(r.IdentityConfidence),
		SpreadsheetText(r.RequestedProvider), SpreadsheetText(r.RequestedModel), SpreadsheetText(r.RequestedRoute),
		SpreadsheetText(r.DownshiftAction), SpreadsheetText(r.DownshiftRule), SpreadsheetText(r.DownshiftTrigger),
		SpreadsheetText(r.DownshiftReason), SpreadsheetText(r.DownshiftDigest), SpreadsheetText(r.DownshiftFeatures),
		strconv.FormatFloat(r.DownshiftSourceUSD, 'f', -1, 64), strconv.FormatFloat(r.DownshiftTargetUSD, 'f', -1, 64),
		"0", "", "0", "", "", "", "", "false", "", "", "",
	}
	if r.Policy != nil {
		start := len(row) - 11
		row[start] = strconv.FormatInt(r.Policy.DecisionID, 10)
		row[start+1] = SpreadsheetText(r.Policy.Digest)
		row[start+2] = strconv.FormatInt(r.Policy.Revision, 10)
		row[start+3] = SpreadsheetText(r.Policy.Name)
		row[start+4] = SpreadsheetText(r.Policy.Namespace)
		row[start+5] = SpreadsheetText(r.Policy.Mode)
		row[start+6] = SpreadsheetText(r.Policy.Outcome)
		row[start+7] = strconv.FormatBool(r.Policy.Admitted)
		row[start+8] = SpreadsheetText(r.Policy.Confidence)
		row[start+9] = SpreadsheetText(r.Policy.ContextJSON)
		row[start+10] = SpreadsheetText(r.Policy.ExplanationJSON)
	}
	return row
}

// WriteJSON streams every ledger row since `from` as one JSON array.
func WriteJSON(out io.Writer, s *store.Store, from time.Time) error {
	return WriteJSONRequestStream(out, func(visit func(store.Request) error) error {
		return s.StreamExport(from, visit)
	})
}

// WriteJSONRequestStream closes the JSON array even if SQLite reports an
// iteration error after earlier rows. Apart from an output-device failure,
// callers therefore never receive malformed partial JSON.
func WriteJSONRequestStream(out io.Writer, stream func(func(store.Request) error) error) error {
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

// TerminalText makes provider-controlled attribution safe to print in a
// terminal. Model, agent, and session names can originate in HTTP headers or
// upstream JSON, so control/format characters must never reach an ANSI-aware
// terminal verbatim.
func TerminalText(value string, maxRunes int) string {
	value = strings.ToValidUTF8(value, "�")
	var b strings.Builder
	count := 0
	for _, r := range value {
		if maxRunes > 0 && count >= maxRunes {
			b.WriteRune('…')
			break
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
		count++
	}
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}

// SpreadsheetText prevents cells controlled by upstream metadata from being
// interpreted as formulas when a CSV is opened in Excel, Numbers, or Sheets.
func SpreadsheetText(value string) string {
	value = TerminalText(value, 0)
	probe := strings.TrimLeftFunc(value, unicode.IsSpace)
	if probe == "" {
		return value
	}
	r, _ := utf8.DecodeRuneInString(probe)
	if strings.ContainsRune("=+-@", r) {
		return "'" + value
	}
	return value
}
