package subsidy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DetectMeteredProviders reports which subscription-style agents are actually
// running on pay-per-token API auth right now, so their usage is real spend
// rather than a flat-rate subsidy comparison. It reflects current auth state,
// not the billing mode of each historical session (the logs never record
// that), so a manual override still exists for overage and mixed windows.
func DetectMeteredProviders(home string) []string {
	var metered []string
	// Claude Code prioritizes ANTHROPIC_API_KEY over an authenticated
	// subscription, and the API account that owns the key owns billing.
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" {
		metered = append(metered, "claude-code")
	}
	// Codex records its auth mode explicitly: "apikey" bills every token at API
	// rates; "chatgpt" draws from the plan's included usage.
	if codexAPIKeyAuth(filepath.Join(home, ".codex", "auth.json")) {
		metered = append(metered, "codex")
	}
	return metered
}

func codexAPIKeyAuth(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var auth struct {
		AuthMode     string `json:"auth_mode"`
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	}
	if json.Unmarshal(b, &auth) != nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.AuthMode), "apikey") {
		return true
	}
	// Older stores may omit auth_mode but carry a key; a present key means the
	// token flows through the API account, not the ChatGPT plan.
	return auth.AuthMode == "" && strings.TrimSpace(auth.OpenAIAPIKey) != ""
}
