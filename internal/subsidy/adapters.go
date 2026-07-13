package subsidy

import (
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/burnban/burnban/sourceadapter"
)

type builtinAdapter struct {
	manifest    sourceadapter.Manifest
	defaultPath func(home string) string
	scan        func(string, time.Time, ScanLimits, func(Event)) (ScanResult, error)
}

var _ sourceadapter.Adapter = builtinAdapter{}

func (a builtinAdapter) Manifest() sourceadapter.Manifest { return a.manifest }
func (a builtinAdapter) DefaultPath(home string) string   { return a.defaultPath(home) }
func (a builtinAdapter) Scan(path string, since time.Time, limits sourceadapter.ScanLimits, emit func(sourceadapter.Event)) (sourceadapter.ScanResult, error) {
	return a.scan(path, since, limits, emit)
}

func manifest(id, name, store string) sourceadapter.Manifest {
	return sourceadapter.Manifest{
		APIVersion: sourceadapter.APIVersion,
		ID:         id, DisplayName: name, Store: store,
		Privacy: sourceadapter.Privacy{ReadOnly: true},
	}
}

// BuiltinAdapters returns a fresh registry slice. The adapters themselves are
// stateless, and every manifest is validated again by BuildReport before use.
func BuiltinAdapters() []sourceadapter.Adapter {
	return []sourceadapter.Adapter{
		builtinAdapter{
			manifest:    manifest("claude-code", "Claude Code", "project JSONL session logs"),
			defaultPath: func(home string) string { return filepath.Join(home, ".claude", "projects") },
			scan:        scanClaude,
		},
		builtinAdapter{
			manifest:    manifest("codex", "Codex", "rollout JSONL session logs"),
			defaultPath: func(home string) string { return filepath.Join(home, ".codex", "sessions") },
			scan:        scanCodex,
		},
		builtinAdapter{
			manifest:    manifest("gemini-cli", "Gemini CLI", "project-scoped JSONL chat records"),
			defaultPath: DefaultGeminiDir,
			scan:        scanGemini,
		},
		builtinAdapter{
			manifest:    manifest("github-copilot-cli", "GitHub Copilot CLI", "per-session JSONL event logs"),
			defaultPath: DefaultCopilotDir,
			scan:        scanGitHubCopilotCLI,
		},
		builtinAdapter{
			manifest:    manifest("cursor", "Cursor", "read-only global composer metadata database"),
			defaultPath: DefaultCursorDB,
			scan:        scanCursor,
		},
		builtinAdapter{
			manifest:    manifest("opencode", "OpenCode", "read-only SQLite message metadata"),
			defaultPath: DefaultOpenCodeDB,
			scan:        scanOpenCode,
		},
		builtinAdapter{
			manifest:    manifest("hermes", "Hermes Agent", "read-only SQLite state database"),
			defaultPath: DefaultHermesDB,
			scan:        scanHermes,
		},
		builtinAdapter{
			manifest:    manifest("openclaw", "OpenClaw", "agent JSONL session transcripts"),
			defaultPath: DefaultOpenClawDir,
			scan:        scanOpenClaw,
		},
		builtinAdapter{
			manifest:    manifest("goose", "Goose", "read-only SQLite usage ledger"),
			defaultPath: DefaultGooseDB,
			scan:        scanGoose,
		},
	}
}

// DefaultGeminiDir returns the root containing Gemini CLI's project-scoped
// chat stores. GEMINI_CLI_HOME replaces the user's home before .gemini is
// appended, matching Gemini CLI's own storage contract.
func DefaultGeminiDir(home string) string {
	if configured := os.Getenv("GEMINI_CLI_HOME"); configured != "" {
		home = configured
	}
	return filepath.Join(home, ".gemini", "tmp")
}

// DefaultCopilotDir returns the managed session-state directory. COPILOT_HOME
// replaces the complete ~/.copilot configuration root, matching Copilot CLI's
// documented configuration-directory precedence.
func DefaultCopilotDir(home string) string {
	root := os.Getenv("COPILOT_HOME")
	if root == "" {
		root = filepath.Join(home, ".copilot")
	}
	return filepath.Join(root, "session-state")
}

func DefaultHermesDB(home string) string {
	hermesHome := os.Getenv("HERMES_HOME")
	if hermesHome == "" {
		hermesHome = filepath.Join(home, ".hermes")
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			native := filepath.Join(local, "hermes")
			if _, err := os.Stat(filepath.Join(native, "state.db")); err == nil {
				hermesHome = native
			}
		}
	}
	return filepath.Join(hermesHome, "state.db")
}

func DefaultOpenClawDir(home string) string {
	if value := os.Getenv("OPENCLAW_STATE_DIR"); value != "" {
		return value
	}
	return filepath.Join(home, ".openclaw")
}

// DefaultGooseDB returns Goose's current per-OS data path, honoring its
// documented GOOSE_PATH_ROOT override and legacy locations.
func DefaultGooseDB(home string) string {
	if root := os.Getenv("GOOSE_PATH_ROOT"); root != "" {
		return filepath.Join(root, "data", "sessions", "sessions.db")
	}
	var candidates []string
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		candidates = append(candidates,
			filepath.Join(data, "goose", "sessions", "sessions.db"),
			filepath.Join(data, "Block", "goose", "sessions", "sessions.db"))
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		candidates = append(candidates, filepath.Join(appData, "Block", "goose", "sessions", "sessions.db"))
	}
	candidates = append(candidates,
		filepath.Join(home, ".local", "share", "goose", "sessions", "sessions.db"),
		filepath.Join(home, ".local", "share", "Block", "goose", "sessions", "sessions.db"),
		filepath.Join(home, "Library", "Application Support", "Block", "goose", "sessions", "sessions.db"),
		filepath.Join(home, "Library", "Application Support", "goose", "sessions", "sessions.db"),
		filepath.Join(home, ".config", "goose", "sessions.db"),
	)
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Block", "goose", "sessions", "sessions.db")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Block", "goose", "sessions", "sessions.db")
		}
	}
	return filepath.Join(home, ".local", "share", "goose", "sessions", "sessions.db")
}
