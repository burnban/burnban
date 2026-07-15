package localusage

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func writeCursorFixture(t *testing.T, records map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for key, value := range records {
		var raw []byte
		if text, ok := value.(string); ok {
			raw = []byte(text)
		} else {
			raw, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`INSERT INTO cursorDiskKV(key,value) VALUES (?,?)`, key, raw); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestScanCursorLegacyEmbeddedConversationMetadataOnly(t *testing.T) {
	secret := "private prompt and response must never escape"
	path := writeCursorFixture(t, map[string]any{
		"composerData:one": map[string]any{
			"createdAt": "2026-07-10T11:59:00Z",
			"conversation": []any{
				map[string]any{"type": 1, "bubbleId": "human-1", "createdAt": "2026-07-10T12:00:00Z", "modelInfo": map[string]any{"modelName": "claude-sonnet-4.6"}, "text": secret},
				map[string]any{"type": 2, "bubbleId": "ai-1", "usageUuid": "usage-1", "createdAt": "2026-07-10T12:00:01Z", "tokenCount": map[string]any{"inputTokens": 100, "outputTokens": 20}, "text": secret},
				map[string]any{"type": 2, "bubbleId": "tool-1", "tokenCount": map[string]any{"inputTokens": 0, "outputTokens": 0}, "toolFormerData": secret},
				map[string]any{"type": 1, "bubbleId": "human-old", "createdAt": "2026-06-01T12:00:00Z", "modelInfo": map[string]any{"modelName": "old-model"}},
				map[string]any{"type": 2, "bubbleId": "ai-old", "usageUuid": "usage-old", "tokenCount": map[string]any{"inputTokens": 999, "outputTokens": 1}},
			},
		},
		"composerData:two": map[string]any{
			"createdAt": 1783688400000,
			"conversation": []any{
				map[string]any{"type": 1, "bubbleId": "human-2", "createdAt": 1783688400000, "modelInfo": map[string]any{"modelName": "gpt-5.4"}},
				map[string]any{"type": 2, "bubbleId": "ai-2", "usageUuid": "usage-2", "tokenCount": map[string]any{"inputTokens": 40, "outputTokens": 5}},
			},
		},
	})
	var events []Event
	result, err := scanCursor(path, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 2 || len(events) != 2 {
		t.Fatalf("sessions=%d events=%+v stats=%+v", result.Sessions, events, result.Stats)
	}
	byModel := make(map[string]Event, len(events))
	for _, event := range events {
		byModel[event.Model] = event
	}
	if got := byModel["claude-sonnet-4.6"]; got.ID == "" || got.In != 100 || got.Out != 20 ||
		got.CacheRead != 0 || got.Confidence != "partial" {
		t.Fatalf("first composer event = %+v", got)
	}
	if got := byModel["gpt-5.4"]; got.ID == "" || got.In != 40 || got.Out != 5 {
		t.Fatalf("second composer event = %+v", got)
	}
	for _, event := range events {
		if strings.Contains(event.ID, secret) || strings.Contains(event.Model, secret) {
			t.Fatalf("event leaked private content: %+v", event)
		}
	}
	for _, warning := range result.Stats.Warnings {
		if strings.Contains(warning, secret) {
			t.Fatalf("warning leaked private content: %q", warning)
		}
	}
}

func TestScanCursorRejectsMalformedAssociationsAndDeduplicates(t *testing.T) {
	path := writeCursorFixture(t, map[string]any{
		"composerData:bad": map[string]any{"createdAt": "2026-07-10T12:00:00Z", "conversation": []any{
			map[string]any{"type": 2, "bubbleId": "orphan", "tokenCount": map[string]any{"inputTokens": 1, "outputTokens": 1}},
			map[string]any{"type": 1, "bubbleId": "human", "createdAt": "2026-07-10T12:00:00Z", "modelInfo": map[string]any{"modelName": "valid-model"}},
			map[string]any{"type": 2, "bubbleId": "negative", "tokenCount": map[string]any{"inputTokens": -1, "outputTokens": 1}},
			map[string]any{"type": 2, "bubbleId": "valid", "usageUuid": "same", "tokenCount": map[string]any{"inputTokens": 2, "outputTokens": 1}},
			map[string]any{"type": 2, "bubbleId": "duplicate", "usageUuid": "same", "tokenCount": map[string]any{"inputTokens": 200, "outputTokens": 100}},
		}},
		"composerData:invalid-json": "{private malformed payload",
	})
	var events []Event
	result, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(event Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].In != 2 || events[0].Out != 1 {
		t.Fatalf("events = %+v", events)
	}
	if !result.Stats.Partial || len(result.Stats.Warnings) == 0 {
		t.Fatalf("missing partial diagnostics: %+v", result.Stats)
	}
}

func TestScanCursorBoundsAndReadOnly(t *testing.T) {
	path := writeCursorFixture(t, map[string]any{
		"composerData:one": map[string]any{"createdAt": "2026-07-10T12:00:00Z", "conversation": []any{
			map[string]any{"type": 1, "bubbleId": "human", "modelInfo": map[string]any{"modelName": "model"}},
			map[string]any{"type": 2, "bubbleId": "ai", "tokenCount": map[string]any{"inputTokens": 2, "outputTokens": 1}},
		}},
	})
	limits := DefaultScanLimits()
	limits.MaxRecords = 1
	result, err := scanCursor(path, time.Time{}, limits, func(Event) { t.Fatal("record beyond limit was emitted") })
	if err != nil || !result.Stats.Partial || result.Stats.RecordsScanned != 1 {
		t.Fatalf("record bound result=%+v err=%v", result, err)
	}

	limits = DefaultScanLimits()
	limits.MaxBytes = 1
	result, err = scanCursor(path, time.Time{}, limits, func(Event) { t.Fatal("byte-bound scan emitted") })
	if err != nil || !result.Stats.Partial || result.Stats.FilesScanned != 0 {
		t.Fatalf("byte bound result=%+v err=%v", result, err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cursorDiskKV`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("source mutated rows=%d err=%v", rows, err)
	}
}

func TestScanCursorOversizedRecordIsNotDecoded(t *testing.T) {
	path := writeCursorFixture(t, map[string]any{
		"composerData:large": map[string]any{"conversation": []any{
			map[string]any{"type": 1, "text": strings.Repeat("private", 100)},
		}},
	})
	limits := DefaultScanLimits()
	limits.MaxLineBytes = 64
	result, err := scanCursor(path, time.Time{}, limits, func(Event) { t.Fatal("oversized record emitted") })
	if err != nil || !result.Stats.Partial || result.Stats.RecordsScanned != 0 {
		t.Fatalf("oversized result=%+v err=%v", result, err)
	}
}

func TestDefaultCursorDB(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home := filepath.Join("home", "user")
	got := DefaultCursorDB(home)
	switch runtime.GOOS {
	case "darwin":
		if want := filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb"); got != want {
			t.Fatalf("path=%q want=%q", got, want)
		}
	case "windows":
		if !strings.HasSuffix(got, filepath.Join("Cursor", "User", "globalStorage", "state.vscdb")) {
			t.Fatalf("path=%q", got)
		}
	default:
		if want := filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb"); got != want {
			t.Fatalf("path=%q want=%q", got, want)
		}
	}
}

func TestScanCursorMissingAndWrongSchema(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.vscdb")
	result, err := scanCursor(missing, time.Time{}, DefaultScanLimits(), func(Event) {})
	if err != nil || result.Sessions != 0 {
		t.Fatalf("missing result=%+v err=%v", result, err)
	}
	path := filepath.Join(t.TempDir(), "wrong.vscdb")
	if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(Event) {}); err == nil || strings.Contains(err.Error(), "not sqlite") {
		t.Fatalf("expected field-only schema error, got %v", err)
	}
}

func TestScanCursorHashesPrivateSourceIdentifiers(t *testing.T) {
	composerSecret := "private-composer-identifier-must-not-escape"
	usageSecret := "private-usage-identifier-must-not-escape"
	otherUsageSecret := "other-private-usage-identifier-must-not-escape"
	path := writeCursorFixture(t, map[string]any{
		"composerData:" + composerSecret: map[string]any{"conversation": []any{
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:00:00Z", "modelInfo": map[string]any{"modelName": "claude-sonnet-4.6"}},
			map[string]any{"type": 2, "usageUuid": usageSecret, "tokenCount": map[string]any{"inputTokens": 7, "outputTokens": 2}},
			map[string]any{"type": 2, "usageUuid": otherUsageSecret, "tokenCount": map[string]any{"inputTokens": 8, "outputTokens": 3}},
		}},
	})
	scan := func() (ScanResult, []Event) {
		var events []Event
		result, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(event Event) {
			events = append(events, event)
		})
		if err != nil {
			t.Fatal(err)
		}
		return result, events
	}
	firstResult, first := scan()
	secondResult, second := scan()
	if len(first) != 2 || len(second) != 2 || first[0].ID == "" || first[1].ID == "" || first[0].ID == first[1].ID {
		t.Fatalf("unstable hashed identity: first=%+v second=%+v", first, second)
	}
	secondIDs := map[string]struct{}{second[0].ID: {}, second[1].ID: {}}
	if _, ok := secondIDs[first[0].ID]; !ok {
		t.Fatalf("first hashed identity changed across scans: first=%+v second=%+v", first, second)
	}
	if _, ok := secondIDs[first[1].ID]; !ok {
		t.Fatalf("second hashed identity changed across scans: first=%+v second=%+v", first, second)
	}
	encoded, err := json.Marshal(struct {
		FirstResult  ScanResult
		SecondResult ScanResult
		First        []Event
		Second       []Event
	}{firstResult, secondResult, first, second})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{composerSecret, usageSecret, otherUsageSecret} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("Cursor source identifier leaked into normalized output: %q", secret)
		}
	}
}

func TestScanCursorRejectsExplicitInvalidAssociationMetadata(t *testing.T) {
	privateModel := strings.Repeat("private-model-payload-", 4096)
	path := writeCursorFixture(t, map[string]any{
		"composerData:association": map[string]any{"conversation": []any{
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:00:00Z", "modelInfo": map[string]any{"modelName": "model-a"}},
			map[string]any{"type": 2, "usageUuid": "bad-model-type", "modelInfo": map[string]any{"modelName": map[string]any{"private": privateModel}}, "tokenCount": map[string]any{"inputTokens": 1, "outputTokens": 1}},
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:01:00Z", "modelInfo": map[string]any{"modelName": "model-b"}},
			map[string]any{"type": 2, "usageUuid": "bad-model-size", "modelInfo": map[string]any{"modelName": privateModel}, "tokenCount": map[string]any{"inputTokens": 2, "outputTokens": 1}},
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:02:00Z", "modelInfo": map[string]any{"modelName": "model-c"}},
			map[string]any{"type": 2, "usageUuid": "bad-time", "createdAt": "not-a-time", "tokenCount": map[string]any{"inputTokens": 3, "outputTokens": 1}},
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:03:00Z", "modelInfo": map[string]any{"modelName": "model-d"}},
			map[string]any{"type": 2, "usageUuid": "valid-inherited", "tokenCount": map[string]any{"inputTokens": 4, "outputTokens": 1}},
		}},
	})
	var events []Event
	result, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Model != "model-d" || events[0].In != 4 || events[0].Out != 1 {
		t.Fatalf("explicit malformed metadata inherited prior association: events=%+v stats=%+v", events, result.Stats)
	}
	encoded, err := json.Marshal(struct {
		Result ScanResult
		Events []Event
	}{result, events})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(privateModel)) {
		t.Fatal("private malformed Cursor metadata escaped field-only diagnostics")
	}
	if !result.Stats.Partial {
		t.Fatalf("malformed explicit association was not surfaced: %+v", result.Stats)
	}
}

func TestScanCursorSkipsPrimitiveConversationElements(t *testing.T) {
	secret := "private primitive conversation content must not escape"
	path := writeCursorFixture(t, map[string]any{
		"composerData:primitive": map[string]any{"conversation": []any{
			map[string]any{"type": 1, "createdAt": "2026-07-10T11:59:00Z", "modelInfo": map[string]any{"modelName": "must-not-cross-primitive"}},
			secret,
			map[string]any{"type": 2, "usageUuid": "orphan-after-primitive", "tokenCount": map[string]any{"inputTokens": 99, "outputTokens": 1}},
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:00:00Z", "modelInfo": map[string]any{"modelName": "model"}},
			map[string]any{"type": 2, "usageUuid": "usage", "tokenCount": map[string]any{"inputTokens": 5, "outputTokens": 1}},
		}},
	})
	var events []Event
	result, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("primitive conversation element aborted bounded scan: %v", err)
	}
	if len(events) != 1 || events[0].Model != "model" || events[0].In != 5 || !result.Stats.Partial {
		t.Fatalf("primitive element result=%+v events=%+v", result, events)
	}
	encoded, err := json.Marshal(struct {
		Result ScanResult
		Events []Event
	}{result, events})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(secret)) {
		t.Fatal("primitive private content escaped field-only diagnostics")
	}
}

func TestScanCursorAssociationNeverCrossesComposerOrRunsBackward(t *testing.T) {
	path := writeCursorFixture(t, map[string]any{
		"composerData:first": map[string]any{"conversation": []any{
			map[string]any{"type": 2, "usageUuid": "orphan-first", "createdAt": "2026-07-10T11:59:00Z", "tokenCount": map[string]any{"inputTokens": 100, "outputTokens": 1}},
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:00:00Z", "modelInfo": map[string]any{"modelName": "model-first"}},
			map[string]any{"type": 2, "usageUuid": "valid-first", "tokenCount": map[string]any{"inputTokens": 1, "outputTokens": 1}},
		}},
		"composerData:second": map[string]any{"conversation": []any{
			map[string]any{"type": 2, "usageUuid": "orphan-second", "createdAt": "2026-07-10T12:00:30Z", "tokenCount": map[string]any{"inputTokens": 200, "outputTokens": 1}},
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:01:00Z", "modelInfo": map[string]any{"modelName": "model-second"}},
			map[string]any{"type": 2, "usageUuid": "valid-second", "tokenCount": map[string]any{"inputTokens": 2, "outputTokens": 1}},
		}},
	})
	var events []Event
	result, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, event := range events {
		got[event.Model] += event.In
	}
	if len(events) != 2 || got["model-first"] != 1 || got["model-second"] != 2 {
		t.Fatalf("cross-composer or reverse association: events=%+v stats=%+v", events, result.Stats)
	}
}

func TestScanCursorStructuredIdentityAvoidsDelimiterCollisions(t *testing.T) {
	path := writeCursorFixture(t, map[string]any{
		"composerData:a/b": map[string]any{"conversation": []any{
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:00:00Z", "modelInfo": map[string]any{"modelName": "model-one"}},
			map[string]any{"type": 2, "usageUuid": "c", "tokenCount": map[string]any{"inputTokens": 1, "outputTokens": 1}},
		}},
		"composerData:a": map[string]any{"conversation": []any{
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:01:00Z", "modelInfo": map[string]any{"modelName": "model-two"}},
			map[string]any{"type": 2, "usageUuid": "b/c", "tokenCount": map[string]any{"inputTokens": 2, "outputTokens": 1}},
		}},
	})
	var events []Event
	result, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].ID == events[1].ID {
		t.Fatalf("ambiguous source IDs collapsed distinct usage: events=%+v stats=%+v", events, result.Stats)
	}
}

func TestScanCursorPreservesDatabaseBytesAndCreatesNoSidecars(t *testing.T) {
	path := writeCursorFixture(t, map[string]any{
		"composerData:integrity": map[string]any{"conversation": []any{
			map[string]any{"type": 1, "createdAt": "2026-07-10T12:00:00Z", "modelInfo": map[string]any{"modelName": "model"}},
			map[string]any{"type": 2, "usageUuid": "usage", "tokenCount": map[string]any{"inputTokens": 1, "outputTokens": 1}},
		}},
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only Cursor scan changed source database bytes")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("read-only Cursor scan created %s sidecar: %v", suffix, err)
		}
	}
}

func TestScanCursorCurrentSplitMessageSchema(t *testing.T) {
	privatePayload := "private split-message payload must not escape"
	path := writeCursorFixture(t, map[string]any{
		"composerData:current-v2": map[string]any{
			"_v":         2,
			"composerId": "current-v2",
			"fullConversationHeadersOnly": []any{
				map[string]any{"bubbleId": "human-v2", "type": 1, "createdAt": "2026-07-10T12:00:00Z"},
				map[string]any{"bubbleId": "ai-v2", "type": 2, "createdAt": "2026-07-10T12:00:01Z"},
			},
		},
		"bubbleId:current-v2:human-v2": map[string]any{
			"bubbleId": "human-v2", "type": 1, "createdAt": "2026-07-10T12:00:00Z",
			"modelInfo": map[string]any{"modelName": "model-v2"}, "text": privatePayload,
		},
		"bubbleId:current-v2:ai-v2": map[string]any{
			"bubbleId": "ai-v2", "usageUuid": "usage-v2", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 12, "outputTokens": 3}, "text": privatePayload,
		},
		"composerData:current-v17": map[string]any{
			"_v":         17,
			"composerId": "current-v17",
			"fullConversationHeadersOnly": []any{
				map[string]any{"bubbleId": "human-v17", "type": 1, "createdAt": "2026-07-10T12:01:00Z"},
				map[string]any{"bubbleId": "ai-v17", "type": 2, "createdAt": "2026-07-10T12:01:01Z"},
			},
		},
		"bubbleId:current-v17:human-v17": map[string]any{
			"bubbleId": "human-v17", "type": 1, "createdAt": "2026-07-10T12:01:00Z",
			"modelInfo": map[string]any{"modelName": "model-v17"}, "text": privatePayload,
		},
		"bubbleId:current-v17:ai-v17": map[string]any{
			"bubbleId": "ai-v17", "usageUuid": "usage-v17", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 20, "outputTokens": 4}, "text": privatePayload,
		},
		// A well-formed but unreferenced message is not part of either composer.
		"bubbleId:current-v17:unreferenced-private": map[string]any{
			"bubbleId": "unreferenced-private", "usageUuid": privatePayload, "type": 2,
			"createdAt": "2026-07-10T12:02:00Z", "modelInfo": map[string]any{"modelName": "must-not-emit"},
			"tokenCount": map[string]any{"inputTokens": 999, "outputTokens": 1}, "text": privatePayload,
		},
	})
	var events []Event
	result, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 2 || len(events) != 2 {
		t.Fatalf("current split schema result=%+v events=%+v", result, events)
	}
	byModel := make(map[string]Event, len(events))
	for _, event := range events {
		byModel[event.Model] = event
	}
	if got := byModel["model-v2"]; got.In != 12 || got.Out != 3 || got.ID == "" {
		t.Fatalf("v2 event = %+v", got)
	}
	if got := byModel["model-v17"]; got.In != 20 || got.Out != 4 || got.ID == "" {
		t.Fatalf("v17 event = %+v", got)
	}
	encoded, err := json.Marshal(struct {
		Result ScanResult
		Events []Event
	}{result, events})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(privatePayload)) {
		t.Fatal("private current-schema message content escaped metadata projection")
	}
}

func TestScanCursorCurrentSchemaFailsClosedAtMissingOrCrossComposerMessage(t *testing.T) {
	privateID := "private-cross-composer-id-must-not-escape"
	path := writeCursorFixture(t, map[string]any{
		"composerData:alpha": map[string]any{
			"_v":         17,
			"composerId": "alpha",
			"fullConversationHeadersOnly": []any{
				map[string]any{"bubbleId": "human-before-gap", "type": 1},
				map[string]any{"bubbleId": privateID, "type": 2},
				map[string]any{"bubbleId": "orphan-after-gap", "type": 2},
				map[string]any{"bubbleId": "human-after-gap", "type": 1},
				map[string]any{"bubbleId": "valid-after-gap", "type": 2},
			},
		},
		"bubbleId:alpha:human-before-gap": map[string]any{
			"bubbleId": "human-before-gap", "type": 1, "createdAt": "2026-07-10T12:00:00Z",
			"modelInfo": map[string]any{"modelName": "must-not-cross-gap"},
		},
		// This row has the requested bubble ID but belongs to another composer.
		"bubbleId:beta:" + privateID: map[string]any{
			"bubbleId": privateID, "usageUuid": privateID, "type": 2,
			"tokenCount": map[string]any{"inputTokens": 500, "outputTokens": 1},
		},
		"bubbleId:alpha:orphan-after-gap": map[string]any{
			"bubbleId": "orphan-after-gap", "usageUuid": "orphan-after-gap", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 400, "outputTokens": 1},
		},
		"bubbleId:alpha:human-after-gap": map[string]any{
			"bubbleId": "human-after-gap", "type": 1, "createdAt": "2026-07-10T12:01:00Z",
			"modelInfo": map[string]any{"modelName": "valid-model"},
		},
		"bubbleId:alpha:valid-after-gap": map[string]any{
			"bubbleId": "valid-after-gap", "usageUuid": "valid-after-gap", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 7, "outputTokens": 2},
		},
	})
	var events []Event
	result, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Model != "valid-model" || events[0].In != 7 || events[0].Out != 2 {
		t.Fatalf("message association crossed a missing/current-schema boundary: result=%+v events=%+v", result, events)
	}
	if !result.Stats.Partial {
		t.Fatalf("missing current-schema message was not surfaced: %+v", result.Stats)
	}
	encoded, err := json.Marshal(struct {
		Result ScanResult
		Events []Event
	}{result, events})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(privateID)) {
		t.Fatal("private cross-composer source identifier escaped field-only diagnostics")
	}
}

func TestScanCursorCurrentSchemaRequiresExactParentHeaderAndMessageBinding(t *testing.T) {
	path := writeCursorFixture(t, map[string]any{
		"composerData:binding": map[string]any{
			"_v":         17,
			"composerId": "binding",
			"fullConversationHeadersOnly": []any{
				map[string]any{"bubbleId": "human", "type": 1},
				map[string]any{"bubbleId": "role-mismatch", "type": 1},
				map[string]any{"bubbleId": "orphan-after-role", "type": 2},
				map[string]any{"bubbleId": "human-two", "type": 1},
				map[string]any{"bubbleId": "id-mismatch", "type": 2},
				map[string]any{"bubbleId": "orphan-after-id", "type": 2},
				map[string]any{"bubbleId": "human-three", "type": 1},
				map[string]any{"bubbleId": "valid", "type": 2},
				map[string]any{"bubbleId": "valid", "type": 2},
			},
		},
		"bubbleId:binding:human": map[string]any{
			"bubbleId": "human", "type": 1, "createdAt": "2026-07-10T12:00:00Z",
			"modelInfo": map[string]any{"modelName": "must-not-cross-role-mismatch"},
		},
		"bubbleId:binding:role-mismatch": map[string]any{
			"bubbleId": "role-mismatch", "usageUuid": "bad-role", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 100, "outputTokens": 1},
		},
		"bubbleId:binding:orphan-after-role": map[string]any{
			"bubbleId": "orphan-after-role", "usageUuid": "orphan-role", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 200, "outputTokens": 1},
		},
		"bubbleId:binding:human-two": map[string]any{
			"bubbleId": "human-two", "type": 1, "createdAt": "2026-07-10T12:01:00Z",
			"modelInfo": map[string]any{"modelName": "must-not-cross-id-mismatch"},
		},
		"bubbleId:binding:id-mismatch": map[string]any{
			"bubbleId": "different-id", "usageUuid": "bad-id", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 300, "outputTokens": 1},
		},
		"bubbleId:binding:orphan-after-id": map[string]any{
			"bubbleId": "orphan-after-id", "usageUuid": "orphan-id", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 400, "outputTokens": 1},
		},
		"bubbleId:binding:human-three": map[string]any{
			"bubbleId": "human-three", "type": 1, "createdAt": "2026-07-10T12:02:00Z",
			"modelInfo": map[string]any{"modelName": "valid-model"},
		},
		"bubbleId:binding:valid": map[string]any{
			"bubbleId": "valid", "usageUuid": "valid-usage", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 9, "outputTokens": 3},
		},
		"composerData:wrong-parent": map[string]any{
			"_v":         17,
			"composerId": "different-parent",
			"fullConversationHeadersOnly": []any{
				map[string]any{"bubbleId": "human", "type": 1},
				map[string]any{"bubbleId": "ai", "type": 2},
			},
		},
		"bubbleId:wrong-parent:human": map[string]any{
			"bubbleId": "human", "type": 1, "createdAt": "2026-07-10T12:03:00Z",
			"modelInfo": map[string]any{"modelName": "wrong-parent-model"},
		},
		"bubbleId:wrong-parent:ai": map[string]any{
			"bubbleId": "ai", "usageUuid": "wrong-parent-usage", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 500, "outputTokens": 1},
		},
	})
	var events []Event
	result, err := scanCursor(path, time.Time{}, DefaultScanLimits(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Model != "valid-model" || events[0].In != 9 || events[0].Out != 3 {
		t.Fatalf("non-exact current-schema binding emitted usage: result=%+v events=%+v", result, events)
	}
	if !result.Stats.Partial {
		t.Fatalf("invalid current-schema bindings were not surfaced: %+v", result.Stats)
	}
}

func TestScanCursorCurrentSchemaBoundsAndTypeGatesMessageValues(t *testing.T) {
	privatePayload := "private-current-message-content-must-not-escape"
	oversized := strings.Repeat(privatePayload, 128)
	path := writeCursorFixture(t, map[string]any{
		"composerData:bounded": map[string]any{
			"_v":         17,
			"composerId": "bounded",
			"fullConversationHeadersOnly": []any{
				map[string]any{"bubbleId": "human-before-large", "type": 1},
				map[string]any{"bubbleId": "oversized", "type": 2},
				map[string]any{"bubbleId": "orphan-after-large", "type": 2},
				map[string]any{"bubbleId": "human-before-model", "type": 1},
				map[string]any{"bubbleId": "bad-model", "type": 2},
				map[string]any{"bubbleId": "human-before-array", "type": 1},
				map[string]any{"bubbleId": "array-message", "type": 2},
				map[string]any{"bubbleId": "orphan-after-array", "type": 2},
				map[string]any{"bubbleId": "human-final", "type": 1},
				map[string]any{"bubbleId": "valid", "type": 2},
			},
		},
		"bubbleId:bounded:human-before-large": map[string]any{
			"bubbleId": "human-before-large", "type": 1, "createdAt": "2026-07-10T12:00:00Z",
			"modelInfo": map[string]any{"modelName": "must-not-cross-oversized"},
		},
		"bubbleId:bounded:oversized": map[string]any{
			"bubbleId": "oversized", "usageUuid": "oversized", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 100, "outputTokens": 1}, "text": oversized,
		},
		"bubbleId:bounded:orphan-after-large": map[string]any{
			"bubbleId": "orphan-after-large", "usageUuid": "orphan-large", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 200, "outputTokens": 1},
		},
		"bubbleId:bounded:human-before-model": map[string]any{
			"bubbleId": "human-before-model", "type": 1, "createdAt": "2026-07-10T12:01:00Z",
			"modelInfo": map[string]any{"modelName": "must-not-cross-bad-model"},
		},
		"bubbleId:bounded:bad-model": map[string]any{
			"bubbleId": "bad-model", "usageUuid": "bad-model", "type": 2,
			"modelInfo":  map[string]any{"modelName": map[string]any{"private": privatePayload}},
			"tokenCount": map[string]any{"inputTokens": 300, "outputTokens": 1},
		},
		"bubbleId:bounded:human-before-array": map[string]any{
			"bubbleId": "human-before-array", "type": 1, "createdAt": "2026-07-10T12:02:00Z",
			"modelInfo": map[string]any{"modelName": "must-not-cross-array"},
		},
		"bubbleId:bounded:array-message": []any{privatePayload},
		"bubbleId:bounded:orphan-after-array": map[string]any{
			"bubbleId": "orphan-after-array", "usageUuid": "orphan-array", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 400, "outputTokens": 1},
		},
		"bubbleId:bounded:human-final": map[string]any{
			"bubbleId": "human-final", "type": 1, "createdAt": "2026-07-10T12:03:00Z",
			"modelInfo": map[string]any{"modelName": "valid-model"},
		},
		"bubbleId:bounded:valid": map[string]any{
			"bubbleId": "valid", "usageUuid": "valid", "type": 2,
			"tokenCount": map[string]any{"inputTokens": 11, "outputTokens": 4},
		},
	})
	limits := DefaultScanLimits()
	limits.MaxLineBytes = 2 << 10
	var events []Event
	result, err := scanCursor(path, time.Time{}, limits, func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Model != "valid-model" || events[0].In != 11 || events[0].Out != 4 {
		t.Fatalf("malformed current message crossed an association boundary: result=%+v events=%+v", result, events)
	}
	if !result.Stats.Partial {
		t.Fatalf("bounded/malformed current messages were not surfaced: %+v", result.Stats)
	}
	encoded, err := json.Marshal(struct {
		Result ScanResult
		Events []Event
	}{result, events})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(privatePayload)) || bytes.Contains(encoded, []byte(oversized)) {
		t.Fatal("private malformed current message value escaped field-only diagnostics")
	}
}
