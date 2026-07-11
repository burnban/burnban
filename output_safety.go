package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// terminalText makes provider-controlled attribution safe to print in a
// terminal. Model, agent, and session names can originate in HTTP headers or
// upstream JSON, so control/format characters must never reach an ANSI-aware
// terminal verbatim.
func terminalText(value string, maxRunes int) string {
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

// spreadsheetText prevents cells controlled by upstream metadata from being
// interpreted as formulas when a CSV is opened in Excel, Numbers, or Sheets.
func spreadsheetText(value string) string {
	value = terminalText(value, 0)
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
