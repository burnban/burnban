// Package mcp exposes burnban over the Model Context Protocol (stdio), so
// Claude Code, Claude Desktop, Cursor — anything that speaks MCP — can ask
// about spend. Budget mutations are available only when the operator starts
// the server with the explicit --allow-budget-admin capability.
//
//	claude mcp add burnban -- burnban mcp
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/burnban/burnban/internal/approvalclient"
	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/policy"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

const protocolVersion = "2025-06-18"

type Server struct {
	S                   *store.Store
	Prices              *pricing.Table
	Version             string
	In                  io.Reader
	Out                 io.Writer
	AllowBudgetAdmin    bool
	AllowBudgetRequests bool
	ApprovalRequester   approvalclient.Requester
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
		resp.Result = map[string]any{"tools": toolDefs(s.AllowBudgetAdmin, s.AllowBudgetRequests)}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
			Meta      json.RawMessage `json:"_meta,omitempty"`
		}
		if err := decodeObject(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "bad params: " + err.Error()}
			break
		}
		if strings.TrimSpace(p.Name) == "" {
			resp.Error = &rpcError{Code: -32602, Message: "bad params: name is required"}
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

func toolDefs(allowBudgetAdmin, allowBudgetRequests bool) []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	tools := []map[string]any{
		{
			"name":        "spend_summary",
			"description": "Spend across all metered AI agents: totals, per-model and per-agent breakdown, cache economics, and waste receipts.",
			"inputSchema": obj(map[string]any{
				"since": map[string]any{"type": "string", "description": `window: "today" (default), "24h", "7d", or a Go duration like "90m"`},
			}),
		},
		{
			"name":        "burn_status",
			"description": "Current global, per-agent, and rolling velocity-fuse state, including spent, cap, remaining, overrides, cooldowns, and burn bans.",
			"inputSchema": obj(map[string]any{
				"agent": map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "optional: return status for one reported agent name"},
			}),
		},
		{
			"name":        "pricing_diagnostics",
			"description": "Pricing-table version, verification date, provenance, overrides, and any entries past their verified validity window.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "policy_status",
			"description": "Read-only summary of the active local v2 admission policy: revision, digest, mode, rule count, and apply time.",
			"inputSchema": obj(map[string]any{}),
		},
	}
	if allowBudgetRequests {
		tools = append(tools, map[string]any{
			"name":        "request_budget_exception",
			"description": "Request temporary additional runway for this enrolled meter. This creates a pending request only; an authenticated human must approve it.",
			"inputSchema": obj(map[string]any{
				"window":          map[string]any{"type": "string", "enum": []string{"daily", "weekly", "monthly"}},
				"increase_usd":    map[string]any{"type": "number", "minimum": 0.01, "maximum": 1e9},
				"reason":          map[string]any{"type": "string", "minLength": 1, "maxLength": 1000},
				"ticket":          map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
				"expires_minutes": map[string]any{"type": "integer", "minimum": 5, "maximum": 43200},
			}, "window", "increase_usd", "reason", "ticket", "expires_minutes"),
		})
	}
	if !allowBudgetAdmin {
		return tools
	}
	return append(tools,
		map[string]any{
			"name":        "set_daily_cap",
			"description": "Set the USD threshold after which new proxy requests get a 402. Pass 0 to remove it. In-flight requests can finish above the threshold. With agent set, applies to that agent's daily window only.",
			"inputSchema": obj(map[string]any{
				"usd":    map[string]any{"type": "number", "description": "cap in USD; 0 removes it"},
				"window": map[string]any{"type": "string", "enum": []string{"daily", "weekly", "monthly"}, "description": "budget window (default daily)"},
				"agent":  map[string]any{"type": "string", "maxLength": 128, "description": "optional: cap a single agent by its reported name (e.g. claude-cli)"},
			}, "usd"),
		},
		map[string]any{
			"name":        "burn_ban",
			"description": "Emergency stop: immediately pause ALL agent spend until the ban is lifted.",
			"inputSchema": obj(map[string]any{}),
		},
		map[string]any{
			"name":        "lift_burn_ban",
			"description": "Lift the burn ban so spend can resume.",
			"inputSchema": obj(map[string]any{
				"today_override": map[string]any{"type": "boolean", "description": "also override ALL budget caps (daily, weekly, monthly, per-agent) for the rest of today"},
			}),
		},
	)
}

func (s *Server) call(name string, args json.RawMessage) (string, error) {
	switch name {
	case "spend_summary":
		var a struct {
			Since string `json:"since"`
		}
		if err := decodeObject(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		return s.spendSummary(a.Since)
	case "burn_status":
		var a struct {
			Agent string `json:"agent"`
		}
		if err := decodeObject(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if a.Agent != "" {
			if err := validateAgentSelector(a.Agent); err != nil {
				return "", err
			}
		}
		return s.burnStatus(a.Agent)
	case "pricing_diagnostics":
		if err := decodeObject(args, &struct{}{}); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		return s.pricingDiagnostics()
	case "policy_status":
		if err := decodeObject(args, &struct{}{}); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		summary, err := policy.LoadActiveSummary(s.S)
		if err != nil {
			return "", err
		}
		encoded, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	case "request_budget_exception":
		if !s.AllowBudgetRequests || s.ApprovalRequester == nil {
			return "", fmt.Errorf("budget requests disabled: restart burnban mcp with --allow-budget-requests and enrolled Team meter credentials")
		}
		var a struct {
			Window         string   `json:"window"`
			IncreaseUSD    *float64 `json:"increase_usd"`
			Reason         string   `json:"reason"`
			Ticket         string   `json:"ticket"`
			ExpiresMinutes *int     `json:"expires_minutes"`
		}
		if err := decodeObject(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if a.IncreaseUSD == nil || a.ExpiresMinutes == nil {
			return "", fmt.Errorf("bad arguments: increase_usd and expires_minutes are required")
		}
		if *a.ExpiresMinutes < 5 || *a.ExpiresMinutes > 43200 {
			return "", fmt.Errorf("bad arguments: expires_minutes must be between 5 and 43200")
		}
		receipt, err := s.ApprovalRequester.Request(context.Background(), approvalclient.Request{
			Window: a.Window, IncreaseUSD: *a.IncreaseUSD, Reason: a.Reason, Ticket: a.Ticket,
			ExpiresIn: time.Duration(*a.ExpiresMinutes) * time.Minute,
		})
		if err != nil {
			return "", err
		}
		encoded, err := json.MarshalIndent(map[string]any{
			"request_id": receipt.ID, "status": receipt.Status, "scope_type": receipt.ScopeType,
			"scope_value": receipt.ScopeValue, "window": receipt.Window, "increase_usd": receipt.IncreaseUSD,
			"valid_until": receipt.ValidUntil, "human_authorization_required": true,
		}, "", "  ")
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	case "set_daily_cap":
		if err := s.requireBudgetAdmin(); err != nil {
			return "", err
		}
		var a struct {
			USD    *float64 `json:"usd"`
			Window string   `json:"window"`
			Agent  string   `json:"agent"`
		}
		if err := decodeObject(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if a.USD == nil {
			return "", fmt.Errorf("bad arguments: usd is required")
		}
		usd := *a.USD
		if math.IsNaN(usd) || math.IsInf(usd, 0) {
			return "", fmt.Errorf("cap must be finite")
		}
		if usd < 0 {
			return "", fmt.Errorf("cap must be >= 0")
		}
		if usd != 0 && usd < 0.01 {
			return "", fmt.Errorf("caps below $0.01 are not enforceable — use burn_ban to stop all spend")
		}
		win, ok := budget.WindowByName(a.Window)
		if !ok {
			return "", fmt.Errorf("window must be daily, weekly, or monthly")
		}
		key, scope := win.Key, win.Name+" cap"
		if a.Agent != "" {
			if err := validateAgentSelector(a.Agent); err != nil {
				return "", err
			}
			if win.Name != "daily" {
				return "", fmt.Errorf("per-agent caps are daily-only for now")
			}
			key, scope = budget.KeyAgentCapPrefix+a.Agent, fmt.Sprintf("daily cap for agent %q", a.Agent)
		}
		if usd == 0 {
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
		if err := s.S.SetSetting(key, strconv.FormatFloat(usd, 'f', -1, 64)); err != nil {
			return "", err
		}
		if a.Agent == "" {
			// New threshold: re-arm this window's warning and alert.
			if err := budget.ClearMarks(s.S, win.Name); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("%s set to $%.2f — new requests get a 402 after recorded spend reaches it", scope, usd), nil
	case "burn_ban":
		if err := s.requireBudgetAdmin(); err != nil {
			return "", err
		}
		if err := decodeObject(args, &struct{}{}); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if err := s.S.SetSetting(budget.KeyBanActive, "1"); err != nil {
			return "", err
		}
		return "local burn ban in effect — all agent spend is paused until lifted", nil
	case "lift_burn_ban":
		if err := s.requireBudgetAdmin(); err != nil {
			return "", err
		}
		var a struct {
			TodayOverride bool `json:"today_override"`
		}
		if err := decodeObject(args, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
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
			msg += "; external burn ban remains active"
		}
		if fuse, err := budget.FuseStatus(s.S, time.Now()); err != nil {
			return "", err
		} else if fuse.Tripped {
			msg += "; spend-velocity fuse remains tripped and requires `burnban fuse --reset`"
		}
		return msg, nil
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func validateAgentSelector(value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 256 ||
		utf8.RuneCountInString(value) > 128 || !utf8.ValidString(value) {
		return fmt.Errorf("agent must be trimmed valid UTF-8 within 128 characters and 256 bytes")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) {
			return fmt.Errorf("agent contains an unsafe character")
		}
	}
	return nil
}

func decodeObject(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("arguments must be a JSON object")
	}
	if err := rejectDuplicateKeys(trimmed); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return scanJSONValue(dec)
}

func scanJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key must be a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func (s *Server) requireBudgetAdmin() error {
	if s.AllowBudgetAdmin {
		return nil
	}
	return fmt.Errorf("budget mutation disabled: restart burnban mcp with --allow-budget-admin to enable it")
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

func (s *Server) burnStatus(agentFilter string) (string, error) {
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
	fuses, err := budget.FuseStatus(s.S, now)
	if err != nil {
		return "", err
	}
	if len(fuses.Rules) > 0 || fuses.Fanout != nil || fuses.Tripped {
		fuseOut := map[string]any{
			"tripped":          fuses.Tripped,
			"cooldown":         budget.FormatFuseDuration(fuses.Cooldown),
			"cooldown_seconds": fuses.Cooldown.Seconds(),
		}
		rules := map[string]any{}
		for _, rule := range fuses.Rules {
			ruleOut := map[string]any{
				"window": budget.FormatFuseDuration(rule.Window), "cap_usd": rule.CapUSD,
				"spent_usd": rule.SpentUSD, "remaining_usd": rule.Remaining,
			}
			if rule.ProjectedTimeToLimit > 0 {
				ruleOut["projected_seconds_to_limit"] = rule.ProjectedTimeToLimit.Seconds()
			}
			if rule.Name == "baseline" {
				ruleOut["baseline_median_usd"] = rule.BaselineMedianUSD
				ruleOut["baseline_multiplier"] = rule.BaselineMultiplier
			}
			rules[rule.Name] = ruleOut
		}
		if len(rules) > 0 {
			fuseOut["rules"] = rules
			out["has_cap"] = true
			out["has_velocity_fuse"] = true
		}
		if fuses.Fanout != nil {
			fuseOut["fanout"] = map[string]any{
				"window":   budget.FormatFuseDuration(fuses.Fanout.Window),
				"requests": fuses.Fanout.Requests, "limit_requests": fuses.Fanout.LimitRequests,
				"remaining_requests": fuses.Fanout.RemainingRequests,
			}
			out["has_velocity_fuse"] = true
		}
		if fuses.Tripped {
			fuseOut["trip_rule"] = fuses.TripRule
			fuseOut["projected_usd"] = fuses.TripProjected
			fuseOut["limit_usd"] = fuses.TripLimitUSD
			if fuses.TripLimitRequests > 0 {
				fuseOut["projected_requests"] = fuses.TripProjectedRequests
				fuseOut["limit_requests"] = fuses.TripLimitRequests
			}
			fuseOut["until"] = fuses.TrippedUntil.Format(time.RFC3339)
			fuseOut["reason"] = fuses.DenialMessage
		}
		out["velocity_fuse"] = fuseOut
	}
	overridden := false
	if ov, err := s.S.GetSetting(budget.KeyOverrideDay); err != nil {
		return "", err
	} else if ov == now.Format("2006-01-02") {
		out["cap_overridden_today"] = true
		overridden = true
	}
	agentCaps, err := s.S.SettingsWithPrefix(budget.KeyAgentCapPrefix)
	if err != nil {
		return "", err
	}
	if agentFilter != "" {
		if _, ok := agentCaps[agentFilter]; !ok {
			agentCaps[agentFilter] = ""
		}
	}
	agentNames := make([]string, 0, len(agentCaps))
	for name := range agentCaps {
		if agentFilter == "" || name == agentFilter {
			agentNames = append(agentNames, name)
		}
	}
	sort.Strings(agentNames)
	agentStates := map[string]any{}
	for _, name := range agentNames {
		spent, err := s.S.SpentSinceForAgent(budget.DayStart(now), name)
		if err != nil {
			return "", err
		}
		state := map[string]any{"spent_usd": spent, "cap_set": false}
		rawCap := agentCaps[name]
		if rawCap != "" {
			capUSD, err := strconv.ParseFloat(rawCap, 64)
			if err != nil || math.IsNaN(capUSD) || math.IsInf(capUSD, 0) || capUSD <= 0 {
				return "", fmt.Errorf("invalid stored cap for agent %q", name)
			}
			state["cap_set"] = true
			out["has_agent_cap"] = true
			state["cap_usd"] = capUSD
			state["remaining_usd"] = max(0, capUSD-spent)
			state["active"] = !overridden
			if overridden {
				state["overridden_today"] = true
			}
		}
		agentStates[name] = state
	}
	if len(agentStates) > 0 {
		out["agent_budgets"] = agentStates
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Server) pricingDiagnostics() (string, error) {
	if s.Prices == nil {
		return "", fmt.Errorf("pricing diagnostics unavailable")
	}
	b, err := json.MarshalIndent(s.Prices.Diagnostics(), "", "  ")
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
