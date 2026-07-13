package main

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/burnban/burnban/internal/optimize"
)

func TestAllocationCSVNeutralizesFormulaMetadata(t *testing.T) {
	report := optimize.AllocationReport{
		Dimension: "agent",
		Recommendations: []optimize.AllocationRecommendation{{
			Scope: "=HYPERLINK(\"https://attacker\")", ApplyCommand: "+cmd", OperatorAction: "@action", Confidence: "-confidence",
		}},
	}
	var output bytes.Buffer
	if err := writeAllocationCSV(&output, report); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(output.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{11, 24, 25, 26} {
		if !strings.HasPrefix(rows[1][index], "'") {
			t.Errorf("column %d not formula-neutralized: %q", index, rows[1][index])
		}
	}
}

func TestOpenRegularQualityFileRejectsNonregularPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scores.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openRegularQualityFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := openRegularQualityFile(dir); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := openRegularQualityFile("-"); err == nil {
		t.Fatal("stdin accepted")
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "scores-link.json")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := openRegularQualityFile(link); err == nil {
			t.Fatal("symlink accepted")
		}
	}
}
