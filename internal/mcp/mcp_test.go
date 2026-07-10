package mcp_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/mcp"
	"github.com/syft8/burnban/internal/store"
)

func TestServerSession(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"set_daily_cap","arguments":{"usd":5}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"burn_status","arguments":{}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	srv := &mcp.Server{S: s, Version: "test", In: strings.NewReader(in), Out: &out}
	if err := srv.Run(); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d responses, want 4 (notification must not be answered):\n%s", len(lines), out.String())
	}

	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &init); err != nil {
		t.Fatal(err)
	}
	if init.Result.ProtocolVersion == "" || init.Result.ServerInfo.Name != "burnban" {
		t.Fatalf("bad initialize response: %s", lines[0])
	}

	var tl struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &tl); err != nil {
		t.Fatal(err)
	}
	if len(tl.Result.Tools) != 5 {
		t.Fatalf("tools/list returned %d tools, want 5", len(tl.Result.Tools))
	}

	if v, _ := s.GetSetting(budget.KeyDailyCapUSD); v != "5.00" {
		t.Fatalf("cap setting = %q, want 5.00", v)
	}
	if !strings.Contains(lines[3], "cap_daily_usd") {
		t.Fatalf("burn_status missing cap: %s", lines[3])
	}
}

func TestUnknownToolIsInBandError(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}` + "\n"
	var out bytes.Buffer
	srv := &mcp.Server{S: s, Version: "test", In: strings.NewReader(in), Out: &out}
	if err := srv.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"isError":true`) {
		t.Fatalf("expected in-band tool error, got: %s", out.String())
	}
}
