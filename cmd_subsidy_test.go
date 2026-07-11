package main

import (
	"strings"
	"testing"
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
