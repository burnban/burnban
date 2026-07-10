package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDemoRefusesExistingCustomDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "important.db")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := cmdDemo([]string{"--db", path})
	if err == nil || !strings.Contains(err.Error(), "without --force") {
		t.Fatalf("existing database was not protected: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "keep me" {
		t.Fatalf("existing database changed: data=%q err=%v", got, err)
	}
}
