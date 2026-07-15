package main

import "github.com/burnban/burnban/internal/export"

// terminalText makes provider-controlled attribution safe to print in a
// terminal. Model, agent, and session names can originate in HTTP headers or
// upstream JSON, so control/format characters must never reach an ANSI-aware
// terminal verbatim.
func terminalText(value string, maxRunes int) string {
	return export.TerminalText(value, maxRunes)
}

// spreadsheetText prevents cells controlled by upstream metadata from being
// interpreted as formulas when a CSV is opened in Excel, Numbers, or Sheets.
func spreadsheetText(value string) string {
	return export.SpreadsheetText(value)
}
