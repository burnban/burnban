package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestContinueOnErrorCommandsTreatHelpAsSuccess(t *testing.T) {
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "doctor", run: func() error { return cmdDoctor([]string{"--help"}) }},
		{name: "pricing", run: func() error { return cmdPricing([]string{"--help"}) }},
		{name: "prune", run: func() error { return cmdPrune([]string{"--help"}) }},
		{name: "status", run: func() error {
			return cmdStatusTo([]string{"--help"}, &bytes.Buffer{})
		}},
		{name: "stop", run: func() error { return cmdStop([]string{"--help"}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); err != nil {
				t.Fatalf("--help returned error: %v", err)
			}
		})
	}
}

func TestCLIHelpSubprocessExitsZeroWithoutErrorFooter(t *testing.T) {
	if command := os.Getenv("BURNBAN_HELP_TEST_COMMAND"); command != "" {
		os.Args = []string{"burnban", command, "--help"}
		main()
		return
	}
	for _, command := range []string{"doctor", "pricing", "prune", "status", "stop"} {
		t.Run(command, func(t *testing.T) {
			child := exec.Command(os.Args[0], "-test.run=^TestCLIHelpSubprocessExitsZeroWithoutErrorFooter$")
			child.Env = append(os.Environ(), "BURNBAN_HELP_TEST_COMMAND="+command)
			output, err := child.CombinedOutput()
			if err != nil {
				t.Fatalf("%s --help exit: %v\n%s", command, err, output)
			}
			if strings.Contains(string(output), "flag: help requested") || strings.Contains(string(output), "burnban: flag:") {
				t.Fatalf("%s --help printed error footer:\n%s", command, output)
			}
		})
	}
}
