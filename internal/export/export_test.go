package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestStreamingExportProducesSafeCSVAndJSON(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	from := time.Now().Add(-time.Minute)
	if err := s.Insert(store.Request{
		Ts: time.Now(), Provider: "=FORMULA", Model: "\x1b[31mmodel", Agent: "+agent", Session: "@session",
		Route: "-route", ServiceTier: "=tier", InferenceGeo: "+geo", BodyHash: "private-fingerprint",
		UsageState: store.UsageState("=usage"), PricingState: store.PricingState("+pricing"),
		Principal: "=principal", ServiceAccount: "+service", Project: "-project", CostCenter: "@cost",
		RequestedProvider: "=requested-provider", RequestedModel: "+requested-model", RequestedRoute: "@requested-route",
		DownshiftAction: "warn", DownshiftRule: "=rule", DownshiftTrigger: "+trigger", DownshiftReason: "@reason",
	}); err != nil {
		t.Fatal(err)
	}

	var csvOutput bytes.Buffer
	if err := WriteCSV(&csvOutput, s, from); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(csvOutput.String())).ReadAll()
	if err != nil {
		t.Fatalf("read CSV: %v; output=%q", err, csvOutput.String())
	}
	if len(records) != 2 || len(records[0]) != len(CSVHeader) || len(records[1]) != len(CSVHeader) {
		t.Fatalf("CSV shape = %#v", records)
	}
	fields := make(map[string]string, len(records[0]))
	for i, name := range records[0] {
		fields[name] = records[1][i]
	}
	for _, name := range []string{"provider", "agent", "session", "route", "service_tier", "inference_geo", "usage_state", "pricing_state", "principal", "service_account", "project", "cost_center", "requested_provider", "requested_model", "requested_route", "downshift_rule", "downshift_trigger", "downshift_reason"} {
		if !strings.HasPrefix(fields[name], "'") {
			t.Errorf("CSV field %s was not formula-neutralized: %q", name, fields[name])
		}
	}
	if strings.Contains(csvOutput.String(), "\x1b") {
		t.Fatalf("CSV retained terminal escape: %q", csvOutput.String())
	}

	var jsonOutput bytes.Buffer
	if err := WriteJSON(&jsonOutput, s, from); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(jsonOutput.Bytes()) || strings.Contains(jsonOutput.String(), "\x1b") || strings.Contains(jsonOutput.String(), "private-fingerprint") {
		t.Fatalf("unsafe/invalid JSON export: %q", jsonOutput.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(jsonOutput.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["provider"] != "=FORMULA" {
		t.Fatalf("JSON rows = %+v", rows)
	}
	if _, exists := rows[0]["body_hash"]; exists {
		t.Fatalf("request fingerprint leaked: %+v", rows[0])
	}
}

func TestJSONExportClosesArrayAfterStreamFailure(t *testing.T) {
	wantErr := errors.New("database iteration failed")
	var out bytes.Buffer
	err := WriteJSONRequestStream(&out, func(visit func(store.Request) error) error {
		if err := visit(store.Request{Ts: time.Unix(1, 0).UTC(), Provider: "openai"}); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("stream error = %v", err)
	}
	if !json.Valid(out.Bytes()) {
		t.Fatalf("partial stream produced invalid JSON: %q", out.String())
	}
	var rows []store.Request
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("closed JSON rows=%+v err=%v", rows, err)
	}
}

func TestEmptyJSONExportIsValidArray(t *testing.T) {
	var out bytes.Buffer
	if err := WriteJSONRequestStream(&out, func(func(store.Request) error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "[]\n" {
		t.Fatalf("empty JSON export = %q", got)
	}
}
