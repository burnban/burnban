package proxy

import (
	"net/http/httptest"
	"testing"
)

func TestAgentFromPopularClients(t *testing.T) {
	tests := map[string]string{
		"claude-cli/2.1":       "claude-code",
		"codex_cli_rs/0.115":   "codex",
		"Hermes-Agent/0.18":    "hermes",
		"openclaw/2026.7":      "openclaw",
		"aider/0.85":           "aider",
		"goose-ai/1.20":        "goose",
		"Cline/3.2":            "cline",
		"Roo-Code/4.1":         "roo-code",
		"continue.dev/1.0":     "continue",
		"cursor-agent/2026.06": "cursor",
		"OpenAI/Python 2.4.0":  "OpenAI/Python",
	}
	for userAgent, want := range tests {
		req := httptest.NewRequest("POST", "http://localhost", nil)
		req.Header.Set("User-Agent", userAgent)
		if got := agentFrom(req); got != want {
			t.Errorf("agentFrom(%q) = %q, want %q", userAgent, got, want)
		}
	}
	req := httptest.NewRequest("POST", "http://localhost", nil)
	req.Header.Set("User-Agent", "OpenAI/Python")
	req.Header.Set("x-client-name", "hermes-profile-work")
	if got := agentFrom(req); got != "hermes-profile-work" {
		t.Fatalf("x-client-name attribution = %q", got)
	}
	req.Header.Set("x-burnban-agent", "explicit")
	if got := agentFrom(req); got != "explicit" {
		t.Fatalf("explicit attribution = %q", got)
	}
}
