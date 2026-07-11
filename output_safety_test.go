package main

import (
	"strings"
	"testing"
)

func TestTerminalTextRemovesControlAndFormatCharacters(t *testing.T) {
	got := terminalText("safe\x1b]8;;https://evil\aBAD\u202emodel", 80)
	if strings.ContainsAny(got, "\x1b\a") || strings.ContainsRune(got, '\u202e') {
		t.Fatalf("unsafe terminal text %q", got)
	}
	if !strings.Contains(got, "BAD") || !strings.Contains(got, "model") {
		t.Fatalf("terminal text lost useful content: %q", got)
	}
}

func TestSpreadsheetTextNeutralizesFormulas(t *testing.T) {
	for _, value := range []string{"=HYPERLINK(\"x\")", "+cmd", "-1+2", "@SUM(A1)", "  =1+1"} {
		if got := spreadsheetText(value); !strings.HasPrefix(got, "'") {
			t.Errorf("spreadsheetText(%q) = %q, want apostrophe prefix", value, got)
		}
	}
	if got := spreadsheetText("claude-sonnet"); got != "claude-sonnet" {
		t.Fatalf("ordinary text changed: %q", got)
	}
}

func TestSafeResponseBodyIsBoundedAndTerminalSafe(t *testing.T) {
	body := strings.NewReader("bad\x1b]8;;https://evil\a" + strings.Repeat("x", 10_000))
	got := safeResponseBody(body)
	if strings.ContainsAny(got, "\x1b\a") {
		t.Fatalf("unsafe response text %q", got)
	}
	if len([]rune(got)) > 241 {
		t.Fatalf("response text was not bounded: %d runes", len([]rune(got)))
	}
}
