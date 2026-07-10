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
			"description": "Set a USD spend cap enforced by the proxy (requests past it get a 402). Pass 0 to remove the cap. With agent set, caps only that agent (daily window only). Window may be daily (default), weekly (resets Monday), or monthly (resets the 1st).",
			"inputSchema": obj(map[string]any{
				"usd":    map[string]any{"type": "number", "description": "cap in USD; 0 removes it"},
				"window": map[string]any{"type": "string", "enum": []string{"daily", "weekly", "monthly"}, "description": "budget window (default daily)"},
				"agent":  map[string]any{"type": "string", "description": "optional: cap a single agent by its reported name (e.g. claude-cli)"},
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
				"today_override": map[string]any{"type": "boolean", "description": "also override ALL budget caps (daily, weekly, monthly, per-agent) for the rest of today"},
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
			USD    float64 `json:"usd"`
			Window string  `json:"window"`
			Agent  string  `json:"agent"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if a.USD < 0 {
			return "", fmt.Errorf("cap must be >= 0")
		}
		if a.USD != 0 && a.USD < 0.01 {
			return "", fmt.Errorf("caps below $0.01 are not enforceable — use burn_ban to stop all spend")
		}
		win, ok := budget.WindowByName(a.Window)
		if !ok {
			return "", fmt.Errorf("window must be daily, weekly, or monthly")
		}
		key, scope := win.Key, win.Name+" cap"
		if a.Agent != "" {
			if win.Name != "daily" {
				return "", fmt.Errorf("per-agent caps are daily-only for now")
			}
			key, scope = budget.KeyAgentCapPrefix+a.Agent, fmt.Sprintf("daily cap for agent %q", a.Agent)
		}
		if a.USD == 0 {
			if err := s.S.DeleteSetting(key); err != nil {
				return "", err
			}
			if a.Agent == "" {
				if err := budget.ClearMarks(s.S, win.Name); err != nil {
					return "", err
				}
			}
			return scope + " removed", nil
		}
		if err := s.S.SetSetting(key, strconv.FormatFloat(a.USD, 'f', -1, 64)); err != nil {
			return "", err
		}
		if a.Agent == "" {
			// New threshold: re-arm this window's warning and alert.
			if err := budget.ClearMarks(s.S, win.Name); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("%s set to $%.2f — the proxy refuses spend past it with a 402", scope, a.USD), nil
	case "burn_ban":
		if err := s.S.SetSetting(budget.KeyBanActive, "1"); err != nil {
			return "", err
		}
		return "🚫 local burn ban in effect — all agent spend is paused until lifted", nil
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
		msg := "local burn ban lifted — spend can resume unless external policy blocks it"
		if a.TodayOverride {
			if err := s.S.SetSetting(budget.KeyOverrideDay, time.Now().Format("2006-01-02")); err != nil {
				return "", err
			}
			msg += " (local caps overridden for the rest of today)"
		}
		if _, external, err := budget.BanStatus(s.S); err != nil {
			return "", err
		} else if external {
			msg += "; organization burn ban remains active"
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
	today, err := s.S.SpentSince(budget.DayStart(now))
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
	localBan, externalBan, err := budget.BanStatus(s.S)
	if err != nil {
		return "", err
	} else if localBan || externalBan {
		out["ban_active"] = true
	}
	if externalBan {
		out["external_ban_active"] = true
	}
	// Per-window state, so an agent can pace itself against what's left.
	states, err := budget.Status(s.S, now)
	if err != nil {
		return "", err
	}
	windows := map[string]any{}
	for _, st := range states {
		if !st.Set {
			continue
		}
		windows[st.Name] = map[string]any{
			"cap_usd":       st.CapUSD,
			"spent_usd":     st.Spent,
			"remaining_usd": max(0, st.CapUSD-st.Spent),
			"resets":        st.Reset,
			"source":        st.Source,
		}
		out["has_cap"] = true
		if st.Name == "daily" {
			out["cap_daily_usd"] = st.CapUSD
		}
	}
	if len(windows) > 0 {
		out["budget_windows"] = windows
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
