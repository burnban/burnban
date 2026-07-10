// Package mcp exposes burnban over the Model Context Protocol (stdio), so
// Claude Code, Claude Desktop, Cursor — anything that speaks MCP — can ask
// about spend and control budgets. Registration is one line:
//
//	claude mcp add burnban -- burnban mcp
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/store"
)

const protocolVersion = "2025-06-18"

type Server struct {
	S       *store.Store
	Version string
	In      io.Reader
	Out     io.Writer
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Run serves newline-delimited JSON-RPC until stdin closes.
func (s *Server) Run() error {
	sc := bufio.NewScanner(s.In)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	enc := json.NewEncoder(s.Out)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil || req.ID == nil {
			continue // malformed input or a notification: nothing to answer
		}
		if err := enc.Encode(s.handle(&req)); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(req *request) *response {
	resp := &response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "burnban", "version": s.Version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolDefs()}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "bad params: " + err.Error()}
			break
		}
		text, err := s.call(p.Name, p.Arguments)
		if err != nil {
			// Tool-level failures travel in-band so the model can react.
			resp.Result = result(err.Error(), true)
			break
		}
		resp.Result = result(text, false)
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func result(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

func toolDefs() []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	return []map[string]any{
		{
			"name":        "spend_summary",
			"description": "Spend across all metered AI agents: totals, per-model and per-agent breakdown, cache economics, and waste receipts.",
			"inputSchema": obj(map[string]any{
				"since": map[string]any{"type": "string", "description": `window: "today" (default), "24h", "7d", or a Go duration like "90m"`},
			}),
		},
		{
			"name":        "burn_status",
			"description": "Current budget state: today's spend, last-hour rate, daily cap, and whether a burn ban is in effect.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "set_daily_cap",
			"description": "Set the daily USD spend cap enforced by the proxy (requests past it get a 402). Pass 0 to remove the cap.",
			"inputSchema": obj(map[string]any{
				"usd": map[string]any{"type": "number", "description": "daily cap in USD; 0 removes it"},
			}, "usd"),
		},
		{
			"name":        "burn_ban",
			"description": "Emergency stop: immediately pause ALL agent spend until the ban is lifted.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "lift_burn_ban",
			"description": "Lift the burn ban so spend can resume.",
			"inputSchema": obj(map[string]any{
				"today_override": map[string]any{"type": "boolean", "description": "also override the daily cap for the rest of today"},
			}),
		},
	}
}

func (s *Server) call(name string, args json.RawMessage) (string, error) {
	switch name {
	case "spend_summary":
		var a struct {
			Since string `json:"since"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
		}
		return s.spendSummary(a.Since)
	case "burn_status":
		return s.burnStatus()
	case "set_daily_cap":
		var a struct {
			USD float64 `json:"usd"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if a.USD < 0 {
			return "", fmt.Errorf("cap must be >= 0")
		}
		if a.USD == 0 {
			if err := s.S.DeleteSetting(budget.KeyDailyCapUSD); err != nil {
				return "", err
			}
			return "daily cap removed — spend is uncapped", nil
		}
		if err := s.S.SetSetting(budget.KeyDailyCapUSD, strconv.FormatFloat(a.USD, 'f', 2, 64)); err != nil {
			return "", err
		}
		return fmt.Sprintf("daily cap set to $%.2f — the proxy refuses spend past it with a 402", a.USD), nil
	case "burn_ban":
		if err := s.S.SetSetting(budget.KeyBanActive, "1"); err != nil {
			return "", err
		}
		return "🚫 burn ban in effect — all agent spend is paused until lifted", nil
	case "lift_burn_ban":
		var a struct {
			TodayOverride bool `json:"today_override"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
		}
		if err := s.S.DeleteSetting(budget.KeyBanActive); err != nil {
			return "", err
		}
		msg := "burn ban lifted — spend can resume"
		if a.TodayOverride {
			if err := s.S.SetSetting(budget.KeyOverrideDay, time.Now().Format("2006-01-02")); err != nil {
				return "", err
			}
			msg += " (daily cap overridden for the rest of today)"
		}
		return msg, nil
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func (s *Server) spendSummary(since string) (string, error) {
	from, label, err := sinceTime(since)
	if err != nil {
		return "", err
	}
	sum, err := s.S.Summarize(from)
	if err != nil {
		return "", err
	}
	type modelOut struct {
		Model      string  `json:"model"`
		Requests   int64   `json:"requests"`
		In         int64   `json:"in_tokens"`
		Out        int64   `json:"out_tokens"`
		CacheRead  int64   `json:"cache_read_tokens"`
		CacheWrite int64   `json:"cache_write_tokens"`
		CostUSD    float64 `json:"cost_usd"`
	}
	type agentOut struct {
		Agent    string  `json:"agent"`
		Requests int64   `json:"requests"`
		CostUSD  float64 `json:"cost_usd"`
	}
	out := map[string]any{
		"window":         label,
		"total_cost_usd": sum.Cost,
		"requests":       sum.Requests,
		"tokens": map[string]int64{
			"in": sum.In, "out": sum.Out,
			"cache_read": sum.CacheRead, "cache_write": sum.CacheWrite,
		},
		"unpriced_requests":   sum.Unpriced,
		"estimated_responses": sum.Estimated,
		"waste": map[string]any{
			"duplicate_groups":     sum.DupGroups,
			"duplicate_wasted_usd": sum.DupWastedUSD,
		},
	}
	if total := sum.CacheRead + sum.In; total > 0 {
		out["cache_hit_pct"] = float64(sum.CacheRead) / float64(total) * 100
	}
	models := make([]modelOut, 0, len(sum.Models))
	for _, m := range sum.Models {
		models = append(models, modelOut{m.Model, m.Requests, m.In, m.Out, m.CacheRead, m.CacheWrite, m.Cost})
	}
	agents := make([]agentOut, 0, len(sum.Agents))
	for _, a := range sum.Agents {
		agents = append(agents, agentOut{a.Agent, a.Requests, a.Cost})
	}
	out["by_model"] = models
	out["by_agent"] = agents
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Server) burnStatus() (string, error) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	today, err := s.S.SpentSince(midnight)
	if err != nil {
		return "", err
	}
	lastHour, err := s.S.SpentSince(now.Add(-time.Hour))
	if err != nil {
		return "", err
	}
	out := map[string]any{
		"spent_today_usd":     today,
		"spent_last_hour_usd": lastHour,
		"ban_active":          false,
		"has_cap":             false,
	}
	if ban, err := s.S.GetSetting(budget.KeyBanActive); err != nil {
		return "", err
	} else if ban == "1" {
		out["ban_active"] = true
	}
	if capStr, err := s.S.GetSetting(budget.KeyDailyCapUSD); err != nil {
		return "", err
	} else if capStr != "" {
		if capUSD, perr := strconv.ParseFloat(capStr, 64); perr == nil {
			out["has_cap"] = true
			out["cap_daily_usd"] = capUSD
		}
	}
	if ov, err := s.S.GetSetting(budget.KeyOverrideDay); err != nil {
		return "", err
	} else if ov == now.Format("2006-01-02") {
		out["cap_overridden_today"] = true
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func sinceTime(s string) (time.Time, string, error) {
	now := time.Now()
	switch s {
	case "", "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()), "today", nil
	case "24h":
		return now.Add(-24 * time.Hour), "last 24h", nil
	case "7d":
		return now.Add(-7 * 24 * time.Hour), "last 7 days", nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("bad since %q: use today, 24h, 7d, or a duration like 90m", s)
	}
	return now.Add(-d), "last " + s, nil
}
