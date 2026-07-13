package reconcile

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestParseCSVPresetsAndExplicitMapping(t *testing.T) {
	for _, test := range []struct {
		name, header, row string
		format            Format
	}{
		{"canonical", "line_id,occurred_at,billed_usd,model,service_tier,region,line_type,reference_line_id,description", "a,2026-07-12T10:00:00-07:00,1.250001,gpt-5,priority,us,usage,,ok", FormatCanonical},
		{"openai", "id,usage_start_time,cost_usd,model,service_tier,region,type,reference_id,description", "a,2026-07-12T17:00:00Z,1.250001,gpt-5,priority,us,usage,,ok", FormatOpenAI},
		{"anthropic", "id,usage_start_time,cost_usd,model,service_tier,inference_geo,type,reference_id,description", "a,2026-07-12T17:00:00Z,1.250001,claude,standard,us,usage,,ok", FormatAnthropic},
		{"gemini", "id,usage_start_time,cost_usd,model,service_tier,location,type,reference_id,description", "a,2026-07-12T17:00:00Z,1.250001,gemini,default,us,usage,,ok", FormatGemini},
	} {
		t.Run(test.name, func(t *testing.T) {
			invoice, err := ParseCSV(strings.NewReader(test.header+"\n"+test.row+"\n"), CSVOptions{Format: test.format, InvoiceID: "inv-1", Provider: test.name, Currency: "usd"})
			if err != nil {
				t.Fatal(err)
			}
			if len(invoice.Lines) != 1 || invoice.Lines[0].BilledMicros != 1_250_001 || invoice.Lines[0].OccurredAt.Location().String() != "UTC" || len(invoice.ContentHash) != 64 {
				t.Fatalf("invoice = %+v", invoice)
			}
		})
	}
	mapping, err := ParseMapping("line_id=custom_id,occurred_at=when,billed_usd=dollars")
	if err != nil {
		t.Fatal(err)
	}
	invoice, err := ParseCSV(strings.NewReader("custom_id,when,dollars\nx,2026-07-12T00:00:00Z,2\n"), CSVOptions{Format: FormatOpenAI, InvoiceID: "i", Provider: "openai", Currency: "USD", Mapping: mapping})
	if err != nil || invoice.Lines[0].BilledMicros != 2_000_000 {
		t.Fatalf("explicit mapping = %+v err=%v", invoice, err)
	}
}

func TestParseCSVAdjustmentsAndMoneyRules(t *testing.T) {
	body := "line_id,occurred_at,billed_usd,line_type,reference_line_id\n" +
		"u,2026-07-12T00:00:00Z,2,usage,\n" +
		"c,2026-07-13T00:00:00Z,-0.25,credit,u\n" +
		"d,2026-07-14T00:00:00Z,0.125,delayed,u\n" +
		"b,2026-07-14T00:00:00Z,-0.01,batch,\n"
	invoice, err := ParseCSV(strings.NewReader(body), CSVOptions{Format: FormatCanonical, InvoiceID: "inv", Provider: "openai", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if got := []int64{invoice.Lines[0].BilledMicros, invoice.Lines[1].BilledMicros, invoice.Lines[2].BilledMicros, invoice.Lines[3].BilledMicros}; fmt.Sprint(got) != "[2000000 -250000 125000 -10000]" {
		t.Fatalf("amounts = %v", got)
	}
	for _, value := range []string{"", " 1", "+1", ".1", "1e3", "1.0000001", "1000000001", "--1"} {
		if _, err := ParseMoneyMicros(value); err == nil {
			t.Errorf("unsafe money %q accepted", value)
		}
	}
	for value, want := range map[string]int64{"0": 0, "-0": 0, "1.2": 1_200_000, "1000000000": MaxMoneyMicros, "-0.000001": -1} {
		got, err := ParseMoneyMicros(value)
		if err != nil || got != want {
			t.Errorf("ParseMoneyMicros(%q)=%d,%v want %d", value, got, err, want)
		}
	}
}

func TestParseCSVRejectsAmbiguityAndUnsafeData(t *testing.T) {
	base := func(header, row string) error {
		_, err := ParseCSV(strings.NewReader(header+"\n"+row+"\n"), CSVOptions{Format: FormatCanonical, InvoiceID: "inv", Provider: "openai", Currency: "USD"})
		return err
	}
	for name, err := range map[string]error{
		"duplicate header": base("line_id,occurred_at,billed_usd,LINE_ID", "a,2026-01-01T00:00:00Z,1,b"),
		"short row":        base("line_id,occurred_at,billed_usd", "a,2026-01-01T00:00:00Z"),
		"bad timezone":     base("line_id,occurred_at,billed_usd", "a,2026-01-01 00:00:00,1"),
		"negative usage":   base("line_id,occurred_at,billed_usd", "a,2026-01-01T00:00:00Z,-1"),
		"positive credit":  base("line_id,occurred_at,billed_usd,line_type", "a,2026-01-01T00:00:00Z,1,credit"),
		"unicode control":  base("line_id,occurred_at,billed_usd,model", "a,2026-01-01T00:00:00Z,1,evil\u202e"),
		"duplicate id": func() error {
			_, err := ParseCSV(strings.NewReader("line_id,occurred_at,billed_usd\na,2026-01-01T00:00:00Z,1\na,2026-01-01T00:00:00Z,1\n"), CSVOptions{Format: FormatCanonical, InvoiceID: "inv", Provider: "openai", Currency: "USD"})
			return err
		}(),
	} {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := ParseCSV(bytes.NewReader(append([]byte("line_id,occurred_at,billed_usd\n"), bytes.Repeat([]byte{'x'}, MaxImportBytes)...)), CSVOptions{Format: FormatCanonical, InvoiceID: "inv", Provider: "openai", Currency: "USD"}); err == nil {
		t.Fatal("oversized import accepted")
	}
	if _, err := ParseMapping("line_id=id,line_id=other"); err == nil {
		t.Fatal("duplicate logical mapping accepted")
	}
	if _, err := ParseMapping("line_id=id,occurred_at=ID"); err == nil {
		t.Fatal("duplicate source mapping accepted")
	}
	oversizedField := strings.Repeat("x", MaxFieldBytes+1)
	if err := base("line_id,occurred_at,billed_usd,ignored", "a,2026-01-01T00:00:00Z,1,"+oversizedField); err == nil {
		t.Fatal("oversized unmapped CSV field accepted")
	}
}

func TestParseCanonicalJSONStrict(t *testing.T) {
	body := `{"schema":"burnban.invoice/v1","invoice_id":"inv","provider":"openai","currency":"USD","lines":[{"line_id":"l1","occurred_at":"2026-07-12T00:00:00Z","billed_usd":"1.2","model":"gpt"}]}`
	invoice, err := ParseJSON(strings.NewReader(body))
	if err != nil || invoice.Lines[0].BilledMicros != 1_200_000 {
		t.Fatalf("invoice=%+v err=%v", invoice, err)
	}
	for _, bad := range []string{
		`{"schema":"burnban.invoice/v1","invoice_id":"one","invoice_id":"two","provider":"openai","currency":"USD","lines":[]}`,
		`{"schema":"burnban.invoice/v1","invoice_id":"one","provider":"openai","Provider":"anthropic","currency":"USD","lines":[]}`,
		`{"Schema":"burnban.invoice/v1","invoice_id":"one","provider":"openai","currency":"USD","lines":[]}`,
		`{"schema":"burnban.invoice/v1","invoice_id":"one","provider":"openai","currency":"USD","extra":1,"lines":[]}`,
		body + `{}`,
	} {
		if _, err := ParseJSON(strings.NewReader(bad)); err == nil {
			t.Fatalf("bad JSON accepted: %s", bad)
		}
	}
	deep := `{"schema":` + strings.Repeat(`{"lines":`, 257) + `null` + strings.Repeat(`}`, 257) + `}`
	if _, err := ParseJSON(strings.NewReader(deep)); err == nil {
		t.Fatal("deeply nested invoice JSON accepted")
	}
	invalidUTF8 := append([]byte(`{"schema":"burnban.invoice/v1","invoice_id":"`), 0xff)
	if _, err := ParseJSON(bytes.NewReader(invalidUTF8)); err == nil {
		t.Fatal("invalid UTF-8 invoice JSON accepted")
	}

	fractional := strings.Replace(body, "2026-07-12T00:00:00Z", "2026-07-12T00:00:00.123456789Z", 1)
	fractionalInvoice, err := ParseJSON(strings.NewReader(fractional))
	if err != nil || fractionalInvoice.Lines[0].OccurredAt.Nanosecond() != 123456789 || ValidateInvoice(fractionalInvoice) != nil {
		t.Fatalf("fractional timestamp was not preserved: invoice=%+v err=%v", fractionalInvoice, err)
	}

	for name, mutate := range map[string]func(Invoice) Invoice{
		"provider case":      func(in Invoice) Invoice { in.Provider = "OpenAI"; return in },
		"invoice whitespace": func(in Invoice) Invoice { in.InvoiceID = " inv"; return in },
		"source format":      func(in Invoice) Invoice { in.SourceFormat = "invented"; return in },
		"hash case":          func(in Invoice) Invoice { in.ContentHash = strings.ToUpper(in.ContentHash); return in },
	} {
		if err := ValidateInvoice(mutate(fractionalInvoice)); err == nil {
			t.Errorf("noncanonical %s accepted", name)
		}
	}
}

func TestSpreadsheetTextAndCheckedAdd(t *testing.T) {
	for _, input := range []string{"=SUM(A1)", "\u00a0-1", "\u2009@cmd", "+x"} {
		if got := SpreadsheetText(input); !strings.HasPrefix(got, "'") {
			t.Errorf("formula %q not neutralized: %q", input, got)
		}
	}
	if got := SpreadsheetText("safe\nvalue\u202e"); got != "safe value " {
		t.Fatalf("control sanitization = %q", got)
	}
	if _, err := CheckedAdd(math.MaxInt64, 1); err == nil {
		t.Fatal("positive overflow accepted")
	}
	if _, err := CheckedAdd(math.MinInt64, -1); err == nil {
		t.Fatal("negative overflow accepted")
	}
}
