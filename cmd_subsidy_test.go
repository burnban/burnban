package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/burnban/burnban/internal/subsidy"
)

func TestSubsidyFormattingSanitizesTerminalControlledNames(t *testing.T) {
	counts := formatCounts(map[string]int64{"standard\x1b]8;;https://evil\a": 2})
	names := safeNames([]string{"claude\u202e-model", "bad\nname"})
	for label, value := range map[string]string{"counts": counts, "names": names} {
		if strings.ContainsAny(value, "\x1b\a\n\r") || strings.ContainsRune(value, '\u202e') {
			t.Fatalf("%s retained terminal control data: %q", label, value)
		}
	}
}

func TestRenderSubsidyShareCardPlainText(t *testing.T) {
	card := subsidy.NewShareCard(subsidy.Report{
		Since:    time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		HasUsage: true,
		Totals:   subsidy.Totals{APIUSD: 4173.49},
	}, "last 30 days", 200)
	got := renderSubsidyShareCard(card, false)
	for _, want := range []string{
		"$4,173.49 API-EQUIVALENT",
		"20.9× a $200/mo plan",
		"LAST 30 DAYS",
		subsidy.ShareInstallCommand,
		subsidy.ShareWebsite,
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

func TestRenderSubsidyShareCardUsesANSIOnlyWhenEnabled(t *testing.T) {
	got := renderSubsidyShareCard(subsidy.ShareCard{
		HasUsage: true, APIEquivalentUSD: 1, MonthlyPaceUSD: 1,
		PlanCostUSD: 200, Multiplier: .005, Window: "today",
		InstallCommand: subsidy.ShareInstallCommand, Website: subsidy.ShareWebsite,
	}, true)
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("colored card has no ANSI styling: %q", got)
	}
}
