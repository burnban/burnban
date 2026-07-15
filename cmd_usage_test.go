package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/burnban/burnban/internal/localusage"
)

func TestUsageFormattingSanitizesTerminalControlledNames(t *testing.T) {
	counts := formatCounts(map[string]int64{"standard\x1b]8;;https://evil\a": 2})
	names := safeNames([]string{"claude\u202e-model", "bad\nname"})
	for label, value := range map[string]string{"counts": counts, "names": names} {
		if strings.ContainsAny(value, "\x1b\a\n\r") || strings.ContainsRune(value, '\u202e') {
			t.Fatalf("%s retained terminal control data: %q", label, value)
		}
	}
}

func TestRenderUsageShareCardPlainText(t *testing.T) {
	card := localusage.NewShareCard(localusage.Report{
		Since:    time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		HasUsage: true,
		Totals:   localusage.Totals{APIUSD: 4173.49},
	}, "last 30 days", 200)
	got := renderUsageShareCard(card, false)
	for _, want := range []string{
		"BURNBAN LOCAL USAGE",
		"$4,173.49 API-EQUIVALENT",
		"20.9× a $200/mo plan",
		"LAST 30 DAYS",
		localusage.ShareInstallCommand,
		localusage.ShareWebsite,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("card missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("plain card contains ANSI escapes: %q", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if width := utf8.RuneCountInString(line); width != shareCardInnerWidth+4 {
			t.Fatalf("line width = %d, want %d: %q", width, shareCardInnerWidth+4, line)
		}
	}
}

func TestRenderUsageShareCardUsesANSIOnlyWhenEnabled(t *testing.T) {
	got := renderUsageShareCard(localusage.ShareCard{
		HasUsage: true, APIEquivalentUSD: 1, MonthlyPaceUSD: 1,
		PlanCostUSD: 200, Multiplier: .005, Window: "today",
		InstallCommand: localusage.ShareInstallCommand, Website: localusage.ShareWebsite,
	}, true)
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("colored card has no ANSI styling: %q", got)
	}
}
