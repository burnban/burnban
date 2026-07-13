package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/burnban/burnban/internal/policy"
	"github.com/burnban/burnban/internal/store"
)

func TestPolicyTemplatesValidateAndApplyIdempotently(t *testing.T) {
	for _, name := range policyTemplateNames() {
		t.Run(name, func(t *testing.T) {
			document, ok := policyTemplate(name)
			if !ok {
				t.Fatal("template missing")
			}
			raw, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := policy.Parse(raw); err != nil {
				t.Fatalf("template does not validate: %v", err)
			}
		})
	}
	for _, required := range []string{
		"individual-coding-agent", "ci-review-bot", "autonomous-research-agent",
		"production-customer-app", "local-private-model", "unknown-price-sandbox",
	} {
		if _, ok := policyTemplate(required); !ok {
			t.Errorf("required fleet-health template %q is missing", required)
		}
	}

	document, _ := policyTemplate("starter")
	raw, _ := json.Marshal(document)
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	dbPath := filepath.Join(dir, "ledger.db")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := cmdPolicyApply([]string{"--db", dbPath, path}); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	active, err := s.ActivePolicyDocument()
	if err != nil || active == nil || active.Namespace != "starter" || active.Revision != 1 {
		t.Fatalf("active=%+v err=%v", active, err)
	}
}

func TestHistoricalPolicyContextIncludesAllCacheWriteTiersConservatively(t *testing.T) {
	context, known := historicalPolicyContext(store.Request{
		Provider: "anthropic", InTokens: 1, CacheReadTokens: 2, CacheWriteTokens: 3, CacheWrite1hTokens: 4,
	})
	if known || context.EstimatedInput != 10 {
		t.Fatalf("context=%+v known=%t", context, known)
	}
	context, _ = historicalPolicyContext(store.Request{InTokens: math.MaxInt64, CacheWrite1hTokens: 1})
	if context.EstimatedInput != math.MaxInt64 {
		t.Fatalf("overflowed historical input=%d", context.EstimatedInput)
	}
}

func TestPolicyResetCommandRequiresExactConfirmationsAndWritesAudit(t *testing.T) {
	document, _ := policyTemplate("starter")
	raw, _ := json.Marshal(document)
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	dbPath := filepath.Join(dir, "ledger.db")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdPolicyApply([]string{"--db", dbPath, path}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	active, err := s.ActivePolicyDocument()
	s.Close()
	if err != nil || active == nil {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	if err := cmdPolicyLineageChange([]string{"--db", dbPath, "--reason", "missing confirmations"}, false); err == nil {
		t.Fatal("reset without exact confirmations succeeded")
	}
	if err := cmdPolicyLineageChange([]string{
		"--db", dbPath, "--confirm-digest", active.Digest, "--confirm-source", active.Source,
		"--reason", "move to a new test namespace", "--actor", "test-operator",
	}, false); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if active, err := s.ActivePolicyDocument(); err != nil || active != nil {
		t.Fatalf("active after reset=%+v err=%v", active, err)
	}
	events, err := s.PolicyLineageEvents(10)
	if err != nil || len(events) != 1 || events[0].Actor != "test-operator" || events[0].Action != "reset" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}
