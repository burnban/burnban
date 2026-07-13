package optimize

import (
	"bytes"
	"strings"
	"testing"
)

const validQualityJSON = `{
  "schema":"burnban.external-quality/v1",
  "source":"braintrust",
  "metric":"task_success",
  "cohort":"release-2026-07",
  "direction":"higher_is_better",
  "scores":[{
    "id":"score-1",
    "observed_at":"2026-07-12T12:00:00Z",
    "model":"model-a",
    "case_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "score":"0.875"
  }]
}`

func TestParseQualityDocumentStrictAndExact(t *testing.T) {
	records, err := ParseQualityDocument(strings.NewReader(validQualityJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ScorePPM != 875_000 || records[0].Model != "model-a" || records[0].Direction != "higher_is_better" {
		t.Fatalf("records = %+v", records)
	}
	for input, want := range map[string]int64{
		"0": 0, "0.1": 100_000, "0.000001": 1, "1": 1_000_000, "1.000000": 1_000_000,
	} {
		got, err := ParseScorePPM(input)
		if err != nil || got != want {
			t.Errorf("ParseScorePPM(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
}

func TestParseQualityDocumentRejectsAmbiguityAndAdversarialNumbers(t *testing.T) {
	cases := []string{
		strings.Replace(validQualityJSON, `"source":"braintrust",`, `"source":"braintrust","source":"other",`, 1),
		strings.Replace(validQualityJSON, `"source":"braintrust",`, `"source":"braintrust","Source":"other",`, 1),
		strings.Replace(validQualityJSON, `"source":"braintrust"`, `"Source":"braintrust"`, 1),
		strings.Replace(validQualityJSON, `"score":"0.875"`, `"score":"8.75e-1"`, 1),
		strings.Replace(validQualityJSON, `"score":"0.875"`, `"score":"0.8750001"`, 1),
		strings.Replace(validQualityJSON, `"score":"0.875"`, `"score":"NaN"`, 1),
		strings.Replace(validQualityJSON, `"score":"0.875"`, `"score":0.875`, 1),
		strings.Replace(validQualityJSON, `"model":"model-a",`, `"model":"model-a","prompt":"secret",`, 1),
		validQualityJSON + `{}`,
	}
	for i, input := range cases {
		if _, err := ParseQualityDocument(strings.NewReader(input)); err == nil {
			t.Errorf("case %d accepted adversarial input", i)
		}
	}
	invalidUTF8 := append([]byte(`{"schema":"burnban.external-quality/v1","source":"`), 0xff)
	if _, err := ParseQualityDocument(bytes.NewReader(invalidUTF8)); err == nil {
		t.Fatal("invalid UTF-8 quality JSON accepted")
	}
	hugeKey := strings.Replace(validQualityJSON, `"source"`, `"`+strings.Repeat("s", 129)+`"`, 1)
	if _, err := ParseQualityDocument(strings.NewReader(hugeKey)); err == nil || !strings.Contains(err.Error(), "field name") {
		t.Fatalf("oversized field-name error=%v", err)
	}
	hugeTimestamp := strings.Replace(validQualityJSON, `"2026-07-12T12:00:00Z"`, `"`+strings.Repeat("2", 100_000)+`"`, 1)
	if _, err := ParseQualityDocument(strings.NewReader(hugeTimestamp)); err == nil || len(err.Error()) > 200 {
		t.Fatalf("oversized value error was missing or reflected input: length=%d err=%v", len(errString(err)), err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestParseQualityDocumentBounded(t *testing.T) {
	input := strings.Repeat(" ", MaxQualityPayloadBytes+1)
	if _, err := ParseQualityDocument(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized input err = %v", err)
	}
}

func TestParseQualityDocumentPreflightsScoreCountBeforeSliceAllocation(t *testing.T) {
	input := `{"schema":"burnban.external-quality/v1","source":"s","metric":"m","cohort":"c","direction":"higher_is_better","scores":[` +
		strings.Repeat(`{},`, 10_000) + `{}` + `]}`
	if _, err := ParseQualityDocument(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "exceeds 10000") {
		t.Fatalf("oversized score batch err = %v", err)
	}
}

func TestParseQualityDocumentRejectsExcessiveNesting(t *testing.T) {
	input := `{"schema":` + strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40) + `,"scores":[]}`
	if _, err := ParseQualityDocument(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("deep nesting err = %v", err)
	}
}

func TestParseScorePPMRejectsNonCanonicalValues(t *testing.T) {
	for _, input := range []string{"", "00.1", ".5", "1.", "+0.1", "-0", "1.1", " 0.5", "0x1", "Infinity"} {
		if _, err := ParseScorePPM(input); err == nil {
			t.Errorf("ParseScorePPM(%q) succeeded", input)
		}
	}
}
