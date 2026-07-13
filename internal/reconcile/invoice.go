// Package reconcile parses bounded provider-invoice evidence and defines the
// immutable records used to compare that evidence with Burnban's ledger.
// It deliberately uses integer micro-dollars at the import boundary so CSV
// rounding and floating-point overflow cannot silently change billed totals.
package reconcile

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxImportBytes = 32 << 20
	MaxImportRows  = 100_000
	MaxColumns     = 64
	MaxFieldBytes  = 16 << 10
	MaxMoneyMicros = int64(1_000_000_000_000_000) // USD 1 billion
	// TimestampFormat uses fixed fractional width so normalized UTC timestamps
	// retain nanoseconds and remain chronologically sortable as SQLite TEXT.
	TimestampFormat = "2006-01-02T15:04:05.000000000Z07:00"
)

type Format string

const (
	FormatCanonical Format = "canonical"
	FormatOpenAI    Format = "openai"
	FormatAnthropic Format = "anthropic"
	FormatGemini    Format = "gemini"
)

type LineType string

const (
	LineUsage           LineType = "usage"
	LineDelayed         LineType = "delayed"
	LineCredit          LineType = "credit"
	LineBatchAdjustment LineType = "batch"
	LineTax             LineType = "tax"
	LineFee             LineType = "fee"
)

type Line struct {
	LineID          string    `json:"line_id"`
	OccurredAt      time.Time `json:"occurred_at"`
	BilledMicros    int64     `json:"billed_micros"`
	Model           string    `json:"model,omitempty"`
	ServiceTier     string    `json:"service_tier,omitempty"`
	Region          string    `json:"region,omitempty"`
	Type            LineType  `json:"line_type"`
	ReferenceLineID string    `json:"reference_line_id,omitempty"`
	Description     string    `json:"description,omitempty"`
}

type Invoice struct {
	Schema       string `json:"schema"`
	InvoiceID    string `json:"invoice_id"`
	Provider     string `json:"provider"`
	Currency     string `json:"currency"`
	SourceFormat Format `json:"source_format"`
	ContentHash  string `json:"content_sha256"`
	Lines        []Line `json:"lines"`
}

type CSVOptions struct {
	Format    Format
	InvoiceID string
	Provider  string
	Currency  string
	// Mapping maps canonical logical names to source CSV headers. It overlays
	// the selected provider preset and makes schema drift explicit.
	Mapping map[string]string
}

var logicalFields = map[string]struct{}{
	"line_id": {}, "occurred_at": {}, "billed_usd": {}, "model": {},
	"service_tier": {}, "region": {}, "line_type": {},
	"reference_line_id": {}, "description": {},
}

func preset(format Format) (map[string]string, error) {
	var mapping map[string]string
	switch format {
	case FormatCanonical:
		mapping = map[string]string{
			"line_id": "line_id", "occurred_at": "occurred_at", "billed_usd": "billed_usd",
			"model": "model", "service_tier": "service_tier", "region": "region",
			"line_type": "line_type", "reference_line_id": "reference_line_id", "description": "description",
		}
	case FormatOpenAI:
		mapping = map[string]string{
			"line_id": "id", "occurred_at": "usage_start_time", "billed_usd": "cost_usd",
			"model": "model", "service_tier": "service_tier", "region": "region",
			"line_type": "type", "reference_line_id": "reference_id", "description": "description",
		}
	case FormatAnthropic:
		mapping = map[string]string{
			"line_id": "id", "occurred_at": "usage_start_time", "billed_usd": "cost_usd",
			"model": "model", "service_tier": "service_tier", "region": "inference_geo",
			"line_type": "type", "reference_line_id": "reference_id", "description": "description",
		}
	case FormatGemini:
		mapping = map[string]string{
			"line_id": "id", "occurred_at": "usage_start_time", "billed_usd": "cost_usd",
			"model": "model", "service_tier": "service_tier", "region": "location",
			"line_type": "type", "reference_line_id": "reference_id", "description": "description",
		}
	default:
		return nil, fmt.Errorf("unknown invoice format %q", format)
	}
	return mapping, nil
}

// ParseMapping parses logical=source_header pairs. Repeated logical fields or
// source columns are rejected rather than using last-write-wins semantics.
func ParseMapping(value string) (map[string]string, error) {
	out := map[string]string{}
	usedSource := map[string]string{}
	if strings.TrimSpace(value) == "" {
		return out, nil
	}
	for _, pair := range strings.Split(value, ",") {
		parts := strings.Split(pair, "=")
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad mapping %q: use logical=source_header", pair)
		}
		logical, source := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if _, ok := logicalFields[logical]; !ok {
			return nil, fmt.Errorf("unknown logical field %q", logical)
		}
		if source == "" {
			return nil, fmt.Errorf("mapping for %q has an empty source header", logical)
		}
		if _, duplicate := out[logical]; duplicate {
			return nil, fmt.Errorf("duplicate mapping for %q", logical)
		}
		folded := strings.ToLower(source)
		if previous, duplicate := usedSource[folded]; duplicate {
			return nil, fmt.Errorf("source header %q is mapped to both %q and %q", source, previous, logical)
		}
		out[logical] = source
		usedSource[folded] = logical
	}
	return out, nil
}

func ParseCSV(r io.Reader, options CSVOptions) (Invoice, error) {
	data, hash, err := boundedBytes(r)
	if err != nil {
		return Invoice{}, err
	}
	if !utf8.Valid(data) {
		return Invoice{}, errors.New("invoice CSV is not valid UTF-8")
	}
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		return Invoice{}, errors.New("invoice CSV must not contain a UTF-8 BOM")
	}
	mapping, err := preset(options.Format)
	if err != nil {
		return Invoice{}, err
	}
	for logical, source := range options.Mapping {
		if _, ok := logicalFields[logical]; !ok {
			return Invoice{}, fmt.Errorf("unknown logical field %q", logical)
		}
		mapping[logical] = source
	}
	invoice := Invoice{
		Schema: "burnban.invoice/v1", InvoiceID: options.InvoiceID,
		Provider: strings.ToLower(options.Provider), Currency: strings.ToUpper(options.Currency),
		SourceFormat: options.Format, ContentHash: hash,
	}
	if invoice.Currency == "" {
		invoice.Currency = "USD"
	}
	if err := validateInvoiceIdentity(invoice); err != nil {
		return Invoice{}, err
	}

	reader := csv.NewReader(bytes.NewReader(data))
	reader.ReuseRecord = true
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return Invoice{}, fmt.Errorf("read invoice header: %w", err)
	}
	if len(header) == 0 || len(header) > MaxColumns {
		return Invoice{}, fmt.Errorf("invoice CSV must contain between 1 and %d columns", MaxColumns)
	}
	index := make(map[string]int, len(header))
	for i, value := range header {
		if err := validateText("header", value, 200, false); err != nil {
			return Invoice{}, err
		}
		if value != strings.TrimSpace(value) {
			return Invoice{}, fmt.Errorf("header %q has surrounding whitespace", value)
		}
		folded := strings.ToLower(value)
		if _, duplicate := index[folded]; duplicate {
			return Invoice{}, fmt.Errorf("duplicate invoice header %q", value)
		}
		index[folded] = i
	}
	columns := map[string]int{}
	for logical, source := range mapping {
		if i, ok := index[strings.ToLower(source)]; ok {
			columns[logical] = i
		}
	}
	for _, required := range []string{"line_id", "occurred_at", "billed_usd"} {
		if _, ok := columns[required]; !ok {
			return Invoice{}, fmt.Errorf("invoice CSV is missing mapped %s column %q", required, mapping[required])
		}
	}

	seen := map[string]struct{}{}
	for rowNumber := 2; ; rowNumber++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Invoice{}, fmt.Errorf("invoice row %d: %w", rowNumber, err)
		}
		if len(record) != len(header) {
			return Invoice{}, fmt.Errorf("invoice row %d has %d columns, want %d", rowNumber, len(record), len(header))
		}
		for column, value := range record {
			if len(value) > MaxFieldBytes {
				return Invoice{}, fmt.Errorf("invoice row %d column %d exceeds %d bytes", rowNumber, column+1, MaxFieldBytes)
			}
		}
		if len(invoice.Lines) >= MaxImportRows {
			return Invoice{}, fmt.Errorf("invoice exceeds %d data rows", MaxImportRows)
		}
		get := func(name string) string {
			if i, ok := columns[name]; ok {
				return record[i]
			}
			return ""
		}
		line, err := parseLine(rowNumber, get)
		if err != nil {
			return Invoice{}, err
		}
		if _, duplicate := seen[line.LineID]; duplicate {
			return Invoice{}, fmt.Errorf("invoice row %d repeats line_id %q", rowNumber, line.LineID)
		}
		seen[line.LineID] = struct{}{}
		invoice.Lines = append(invoice.Lines, line)
	}
	if len(invoice.Lines) == 0 {
		return Invoice{}, errors.New("invoice contains no data rows")
	}
	return invoice, nil
}

func parseLine(row int, get func(string) string) (Line, error) {
	line := Line{
		LineID: get("line_id"), Model: get("model"), ServiceTier: get("service_tier"),
		Region: get("region"), ReferenceLineID: get("reference_line_id"), Description: get("description"),
		Type: LineType(strings.ToLower(get("line_type"))),
	}
	if line.Type == "" {
		line.Type = LineUsage
	}
	for field, value := range map[string]string{
		"line_id": line.LineID, "model": line.Model, "service_tier": line.ServiceTier,
		"region": line.Region, "reference_line_id": line.ReferenceLineID,
	} {
		if err := validateText(field, value, 200, field != "line_id"); err != nil {
			return Line{}, fmt.Errorf("invoice row %d: %w", row, err)
		}
	}
	if err := validateText("description", line.Description, 2_000, true); err != nil {
		return Line{}, fmt.Errorf("invoice row %d: %w", row, err)
	}
	if line.LineID == "" {
		return Line{}, fmt.Errorf("invoice row %d: line_id is required", row)
	}
	var err error
	line.OccurredAt, err = time.Parse(time.RFC3339, get("occurred_at"))
	if err != nil {
		return Line{}, fmt.Errorf("invoice row %d: occurred_at must be RFC3339 with a timezone", row)
	}
	line.OccurredAt = line.OccurredAt.UTC()
	line.BilledMicros, err = ParseMoneyMicros(get("billed_usd"))
	if err != nil {
		return Line{}, fmt.Errorf("invoice row %d: billed_usd: %w", row, err)
	}
	switch line.Type {
	case LineUsage:
		if line.BilledMicros < 0 {
			return Line{}, fmt.Errorf("invoice row %d: usage must not have a negative amount", row)
		}
	case LineCredit:
		if line.BilledMicros >= 0 {
			return Line{}, fmt.Errorf("invoice row %d: credit must have a negative amount", row)
		}
	case LineDelayed, LineBatchAdjustment, LineTax, LineFee:
		if line.BilledMicros == 0 {
			return Line{}, fmt.Errorf("invoice row %d: %s adjustment must not be zero", row, line.Type)
		}
	default:
		return Line{}, fmt.Errorf("invoice row %d: unknown line_type %q", row, line.Type)
	}
	return line, nil
}

// ParseMoneyMicros parses a non-exponent decimal with at most six fractional
// digits and a bounded magnitude.
func ParseMoneyMicros(value string) (int64, error) {
	if value == "" || value != strings.TrimSpace(value) {
		return 0, errors.New("must be a decimal without surrounding whitespace")
	}
	sign := int64(1)
	if value[0] == '-' {
		sign, value = -1, value[1:]
	} else if value[0] == '+' {
		return 0, errors.New("leading plus signs are not allowed")
	}
	if value == "" || strings.Count(value, ".") > 1 {
		return 0, errors.New("invalid decimal")
	}
	parts := strings.SplitN(value, ".", 2)
	if parts[0] == "" {
		return 0, errors.New("a whole-dollar digit is required")
	}
	for _, part := range parts {
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return 0, errors.New("only decimal digits are allowed")
			}
		}
	}
	if len(parts) == 2 && len(parts[1]) > 6 {
		return 0, errors.New("at most six fractional digits are allowed")
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > MaxMoneyMicros/1_000_000 {
		return 0, errors.New("amount exceeds the supported range")
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	fraction += strings.Repeat("0", 6-len(fraction))
	frac := int64(0)
	if fraction != "" {
		frac, _ = strconv.ParseInt(fraction, 10, 64)
	}
	if whole > (MaxMoneyMicros-frac)/1_000_000 {
		return 0, errors.New("amount exceeds the supported range")
	}
	amount := whole*1_000_000 + frac
	if amount > MaxMoneyMicros {
		return 0, errors.New("amount exceeds the supported range")
	}
	if amount == 0 {
		return 0, nil
	}
	return sign * amount, nil
}

func FormatMoneyMicros(value int64) string {
	sign := ""
	if value < 0 {
		sign, value = "-", -value
	}
	return fmt.Sprintf("%s%d.%06d", sign, value/1_000_000, value%1_000_000)
}

func boundedBytes(r io.Reader) ([]byte, string, error) {
	if r == nil {
		return nil, "", errors.New("invoice reader is nil")
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxImportBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > MaxImportBytes {
		return nil, "", fmt.Errorf("invoice exceeds %d bytes", MaxImportBytes)
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func validateInvoiceIdentity(invoice Invoice) error {
	if err := validateText("invoice_id", invoice.InvoiceID, 200, false); err != nil {
		return err
	}
	if invoice.InvoiceID != strings.TrimSpace(invoice.InvoiceID) {
		return errors.New("invoice_id must not have surrounding whitespace")
	}
	if err := validateText("provider", invoice.Provider, 100, false); err != nil {
		return err
	}
	if invoice.Provider != strings.TrimSpace(invoice.Provider) || invoice.Provider != strings.ToLower(invoice.Provider) {
		return errors.New("provider must be lowercase without surrounding whitespace")
	}
	if invoice.Currency != "USD" {
		return fmt.Errorf("currency %q is unsupported; convert with documented evidence before import", invoice.Currency)
	}
	return nil
}

// ValidateInvoice applies the import contract to an Invoice constructed by an
// API caller rather than ParseCSV/ParseJSON.
func ValidateInvoice(invoice Invoice) error {
	if invoice.Schema != "burnban.invoice/v1" {
		return fmt.Errorf("unsupported invoice schema %q", invoice.Schema)
	}
	if err := validateInvoiceIdentity(invoice); err != nil {
		return err
	}
	if _, err := preset(invoice.SourceFormat); err != nil {
		return err
	}
	if len(invoice.Lines) == 0 || len(invoice.Lines) > MaxImportRows {
		return fmt.Errorf("invoice lines must contain between 1 and %d rows", MaxImportRows)
	}
	if len(invoice.ContentHash) != sha256.Size*2 {
		return errors.New("invoice content_sha256 must be a SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(invoice.ContentHash); err != nil || invoice.ContentHash != strings.ToLower(invoice.ContentHash) {
		return errors.New("invoice content_sha256 must be a SHA-256 hex digest")
	}
	seen := map[string]struct{}{}
	for i, line := range invoice.Lines {
		values := map[string]string{
			"line_id": line.LineID, "occurred_at": line.OccurredAt.UTC().Format(TimestampFormat),
			"billed_usd": FormatMoneyMicros(line.BilledMicros), "model": line.Model,
			"service_tier": line.ServiceTier, "region": line.Region, "line_type": string(line.Type),
			"reference_line_id": line.ReferenceLineID, "description": line.Description,
		}
		validated, err := parseLine(i+1, func(name string) string { return values[name] })
		if err != nil {
			return err
		}
		if line.OccurredAt.IsZero() || !line.OccurredAt.Equal(validated.OccurredAt) {
			return fmt.Errorf("invoice row %d: occurred_at is invalid", i+1)
		}
		if _, duplicate := seen[line.LineID]; duplicate {
			return fmt.Errorf("invoice repeats line_id %q", line.LineID)
		}
		seen[line.LineID] = struct{}{}
	}
	return nil
}

func validateText(field, value string, maxBytes int, emptyOK bool) error {
	if !emptyOK && value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(value) > maxBytes || len(value) > MaxFieldBytes {
		return fmt.Errorf("%s exceeds %d bytes", field, min(maxBytes, MaxFieldBytes))
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	for _, ch := range value {
		if unicode.IsControl(ch) || unicode.In(ch, unicode.Cf, unicode.Co, unicode.Cs) {
			return fmt.Errorf("%s contains unsafe Unicode", field)
		}
	}
	return nil
}

// SpreadsheetText neutralizes formula cells and strips display controls when
// reconciliation data is exported back to CSV.
func SpreadsheetText(value string) string {
	var clean strings.Builder
	clean.Grow(len(value))
	for _, ch := range value {
		if unicode.IsControl(ch) || unicode.In(ch, unicode.Cf, unicode.Co, unicode.Cs) {
			clean.WriteRune(' ')
			continue
		}
		clean.WriteRune(ch)
	}
	value = clean.String()
	probe := strings.TrimLeftFunc(value, unicode.IsSpace)
	if probe != "" && strings.ContainsRune("=+-@", rune(probe[0])) {
		return "'" + value
	}
	return value
}

// CheckedAdd refuses integer overflow in invoice and report totals.
func CheckedAdd(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, errors.New("money total overflow")
	}
	return a + b, nil
}

// ParseJSON accepts the canonical API representation with the same byte/row,
// identity, money, and line-type validation as CSV. Unknown or duplicate JSON
// fields are rejected.
func ParseJSON(r io.Reader) (Invoice, error) {
	data, hash, err := boundedBytes(r)
	if err != nil {
		return Invoice{}, err
	}
	if !utf8.Valid(data) {
		return Invoice{}, errors.New("invoice JSON is not valid UTF-8")
	}
	if err := rejectDuplicateJSON(data); err != nil {
		return Invoice{}, err
	}
	var wire struct {
		Schema    string `json:"schema"`
		InvoiceID string `json:"invoice_id"`
		Provider  string `json:"provider"`
		Currency  string `json:"currency"`
		Lines     []struct {
			LineID          string `json:"line_id"`
			OccurredAt      string `json:"occurred_at"`
			BilledUSD       string `json:"billed_usd"`
			Model           string `json:"model,omitempty"`
			ServiceTier     string `json:"service_tier,omitempty"`
			Region          string `json:"region,omitempty"`
			LineType        string `json:"line_type,omitempty"`
			ReferenceLineID string `json:"reference_line_id,omitempty"`
			Description     string `json:"description,omitempty"`
		} `json:"lines"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Invoice{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Invoice{}, errors.New("invoice JSON must contain exactly one value")
	}
	if wire.Schema != "burnban.invoice/v1" {
		return Invoice{}, fmt.Errorf("unsupported invoice schema %q", wire.Schema)
	}
	invoice := Invoice{Schema: wire.Schema, InvoiceID: wire.InvoiceID, Provider: strings.ToLower(wire.Provider), Currency: strings.ToUpper(wire.Currency), SourceFormat: FormatCanonical, ContentHash: hash}
	if err := validateInvoiceIdentity(invoice); err != nil {
		return Invoice{}, err
	}
	if len(wire.Lines) == 0 || len(wire.Lines) > MaxImportRows {
		return Invoice{}, fmt.Errorf("invoice lines must contain between 1 and %d rows", MaxImportRows)
	}
	seen := map[string]struct{}{}
	for i, raw := range wire.Lines {
		values := map[string]string{
			"line_id": raw.LineID, "occurred_at": raw.OccurredAt, "billed_usd": raw.BilledUSD,
			"model": raw.Model, "service_tier": raw.ServiceTier, "region": raw.Region,
			"line_type": raw.LineType, "reference_line_id": raw.ReferenceLineID, "description": raw.Description,
		}
		line, err := parseLine(i+1, func(name string) string { return values[name] })
		if err != nil {
			return Invoice{}, err
		}
		if _, duplicate := seen[line.LineID]; duplicate {
			return Invoice{}, fmt.Errorf("invoice repeats line_id %q", line.LineID)
		}
		seen[line.LineID] = struct{}{}
		invoice.Lines = append(invoice.Lines, line)
	}
	return invoice, nil
}

func rejectDuplicateJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	return scanJSON(decoder, 0)
}

var canonicalInvoiceJSONFields = map[string]struct{}{
	"schema": {}, "invoice_id": {}, "provider": {}, "currency": {}, "lines": {},
	"line_id": {}, "occurred_at": {}, "billed_usd": {}, "model": {},
	"service_tier": {}, "region": {}, "line_type": {}, "reference_line_id": {},
	"description": {},
}

func scanJSON(decoder *json.Decoder, depth int) error {
	if depth > 256 {
		return errors.New("invoice JSON nesting exceeds 256 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]string{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key := keyToken.(string)
			if _, canonical := canonicalInvoiceJSONFields[key]; !canonical {
				return fmt.Errorf("unknown or non-canonical JSON field %q", key)
			}
			folded := strings.ToLower(key)
			if previous, duplicate := seen[folded]; duplicate {
				return fmt.Errorf("duplicate or case-ambiguous JSON fields %q and %q", previous, key)
			}
			seen[folded] = key
			if err := scanJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}
