// Feature-parity JSON API for the embedded dashboard. Every endpoint here
// mirrors a CLI command and calls the same internal functions, so the browser
// and the terminal can never disagree: report, requests/export, guardrails
// (cap/fuse/ban/lift/alert), whatif, optimize, downshift, policy, reconcile,
// diagnostics, and pricing.
//
// Mutating endpoints live under /api/admin/ and honor Config.AllowAdmin.
// Reads stay available everywhere the summary feed is. Cross-origin browser
// calls are rejected by LocalSafety before reaching any handler, and team
// gateways additionally require the shared token via WithAuth.
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/export"
	"github.com/burnban/burnban/internal/optimize"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
	"github.com/burnban/burnban/internal/whatif"
)

const (
	maxAdminBodyBytes  = 8 << 10
	maxReportWindow    = 366 * 24 * time.Hour
	maxRecentRequests  = 500
	maxAgentNameRunes  = 200
	policyEventsShown  = 20
	reconcileMaxLabel  = 100
	adminDisabledError = "burnban: dashboard admin controls are disabled on this listener (start with --allow-remote-admin to enable them on a team gateway)"
)

func registerParityAPI(mux *http.ServeMux, s *store.Store, cfg Config) {
	mux.HandleFunc("GET /api/report", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		now := time.Now()
		from, label, err := parseWindow(r.URL.Query().Get("window"), "today", maxReportWindow, now)
		if err != nil {
			http.Error(w, "burnban: "+err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := buildReport(s, from, label, now)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, resp)
	})

	mux.HandleFunc("GET /api/requests", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > maxRecentRequests {
				http.Error(w, fmt.Sprintf("burnban: limit must be an integer from 1 through %d", maxRecentRequests), http.StatusBadRequest)
				return
			}
			limit = n
		}
		rows, err := s.RecentRequests(limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"now":      time.Now().Format(time.RFC3339),
			"requests": rows,
		})
	})

	mux.HandleFunc("GET /api/guardrails", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		resp, err := buildGuardrailsFromStore(s, cfg, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, resp)
	})

	mux.HandleFunc("GET /api/whatif", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		now := time.Now()
		from, label, err := parseWindow(r.URL.Query().Get("window"), "7d", maxReportWindow, now)
		if err != nil {
			http.Error(w, "burnban: "+err.Error(), http.StatusBadRequest)
			return
		}
		requests, err := s.TokenRows(from)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		totals := whatif.Totals(requests)
		resp := map[string]any{
			"window": label, "now": now.Format(time.RFC3339),
			"actual_cost_usd": totals.CostUSD, "priced_requests": totals.Requests,
			"unpriced": totals.Unpriced, "unmetered": totals.Unmetered,
			"incomplete": totals.Incomplete, "fee_unpriced": totals.FeeUnpriced,
			"rows": []whatif.Row{},
		}
		if totals.Requests > 0 {
			only := r.URL.Query().Get("model")
			rows, ok := whatif.Rows(cfg.Prices, requests, only)
			if !ok {
				http.Error(w, fmt.Sprintf("burnban: no pricing for %q — add it to ~/.burnban/pricing.json", only), http.StatusBadRequest)
				return
			}
			resp["rows"] = rows
		}
		writeJSON(w, resp)
	})

	mux.HandleFunc("GET /api/optimize", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		now := time.Now().UTC().Truncate(time.Second)
		from, _, err := parseWindow(r.URL.Query().Get("window"), "30d", store.MaxOptimizationRange, now)
		if err != nil {
			http.Error(w, "burnban: "+err.Error(), http.StatusBadRequest)
			return
		}
		from = from.UTC().Truncate(time.Second)
		if !now.After(from) {
			http.Error(w, "burnban: optimization window must span at least one second", http.StatusBadRequest)
			return
		}
		dimension := r.URL.Query().Get("dimension")
		if dimension == "" {
			dimension = "agent"
		}
		days := 30
		if raw := r.URL.Query().Get("days"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 7 || n > 90 {
				http.Error(w, "burnban: days must be between 7 and 90", http.StatusBadRequest)
				return
			}
			days = n
		}
		cacheSample, err := s.OptimizationRows(from, now, 50_000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cacheReport, err := optimize.AnalyzeCache(cacheSample, from, now, optimize.DefaultCacheOptions())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		allocThrough := time.Now().UTC().Truncate(24 * time.Hour)
		allocFrom := allocThrough.Add(-time.Duration(days) * 24 * time.Hour)
		allocSample, err := s.OptimizationRows(allocFrom, allocThrough, 50_000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		allocReport, err := optimize.RecommendAllocations(allocSample, allocFrom, allocThrough, optimize.DefaultAllocationOptions(dimension, days))
		if err != nil {
			http.Error(w, "burnban: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{
			"now":        time.Now().Format(time.RFC3339),
			"cache":      cacheReport,
			"allocation": allocReport,
		})
	})

	mux.HandleFunc("GET /api/downshift", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		record, err := s.ActiveDownshiftDocument()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := map[string]any{"active": record != nil}
		if record != nil {
			resp["revision"] = record.Revision
			resp["mode"] = record.Mode
			resp["digest"] = record.Digest
			resp["api_version"] = record.APIVersion
			resp["applied_at"] = record.AppliedAt.Format(time.RFC3339)
			resp["forced"] = record.Forced
			if record.ForceReason != "" {
				resp["force_reason"] = record.ForceReason
			}
			if json.Valid([]byte(record.DocumentJSON)) {
				resp["document"] = json.RawMessage(record.DocumentJSON)
			}
			if sim, simErr := s.LatestDownshiftSimulation(record.Digest); simErr == nil && sim != nil {
				resp["simulation"] = map[string]any{
					"created_at": sim.CreatedAt.Format(time.RFC3339), "total_requests": sim.TotalRequests,
					"matched_requests": sim.MatchedRequests, "eligible_requests": sim.EligibleRequests,
					"indeterminate_requests": sim.IndeterminateRequests,
					"source_cost_usd":        sim.SourceCostUSD, "target_cost_usd": sim.TargetCostUSD,
				}
			}
		}
		writeJSON(w, resp)
	})

	mux.HandleFunc("GET /api/policy", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		now := time.Now()
		since, label, err := parseWindow(r.URL.Query().Get("window"), "7d", maxReportWindow, now)
		if err != nil {
			http.Error(w, "burnban: "+err.Error(), http.StatusBadRequest)
			return
		}
		record, err := s.ActivePolicyDocument()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		coverage, err := s.PolicyCoverageSince(since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		decisions, err := s.PolicyDecisionsSince(since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		events, err := s.PolicyLineageEvents(policyEventsShown)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := map[string]any{
			"active": record != nil, "window": label,
			"coverage": coverage, "decisions": decisions, "events": events,
		}
		if record != nil {
			resp["name"] = record.Name
			resp["namespace"] = record.Namespace
			resp["revision"] = record.Revision
			resp["digest"] = record.Digest
			resp["source"] = record.Source
			resp["api_version"] = record.APIVersion
			resp["applied_at"] = record.AppliedAt.Format(time.RFC3339)
			if json.Valid([]byte(record.DocumentJSON)) {
				resp["document"] = json.RawMessage(record.DocumentJSON)
			}
		}
		writeJSON(w, resp)
	})

	mux.HandleFunc("GET /api/reconcile", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		now := time.Now()
		from, label, err := parseWindow(r.URL.Query().Get("window"), "30d", maxReportWindow, now)
		if err != nil {
			http.Error(w, "burnban: "+err.Error(), http.StatusBadRequest)
			return
		}
		provider := strings.TrimSpace(r.URL.Query().Get("provider"))
		if len(provider) > reconcileMaxLabel || provider != export.TerminalText(provider, reconcileMaxLabel) {
			http.Error(w, "burnban: invalid provider filter", http.StatusBadRequest)
			return
		}
		report, err := s.Reconcile(from, now, provider)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"window": label, "report": report})
	})

	mux.HandleFunc("GET /api/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		resp := map[string]any{
			"now":      time.Now().Format(time.RFC3339),
			"version":  cfg.Version,
			"exposure": cfg.Exposure,
			"demo":     cfg.Demo, "auth_required": cfg.AuthRequired,
			"local_usage_enabled": !cfg.DisableLocalUsage,
			"admin_enabled":       cfg.AllowAdmin,
			"db_ok":               true,
		}
		if err := s.Probe(); err != nil {
			resp["db_ok"] = false
			resp["db_error"] = err.Error()
		}
		if cfg.Health != nil {
			health := cfg.Health()
			resp["health"] = &health
		}
		diag := cfg.Prices.Diagnostics()
		resp["pricing"] = diag
		if age, ok := pricingAgeDays(diag, time.Now()); ok {
			resp["pricing_age_days"] = age
			resp["pricing_stale"] = age > 45
		}
		var lifetime *store.MetricsSummary
		err := s.ReadSnapshot(func(snapshot *store.ReadSnapshot) error {
			var err error
			lifetime, err = snapshot.LifetimeMetrics()
			return err
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ledger := map[string]any{
			"requests": lifetime.Requests, "cost_usd": lifetime.Cost,
			"unknown_pricing": lifetime.UnknownPricing, "unmetered": lifetime.Unmetered,
			"incomplete": lifetime.Incomplete, "enforcement_gaps": lifetime.EnforcementGaps,
			"fee_unpriced": lifetime.FeeUnpriced,
		}
		if !lifetime.LastRequestAt.IsZero() {
			ledger["last_request_at"] = lifetime.LastRequestAt.Format(time.RFC3339)
		}
		resp["ledger"] = ledger
		writeJSON(w, resp)
	})

	mux.HandleFunc("GET /api/pricing", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		diag := cfg.Prices.Diagnostics()
		overrides := make(map[string]bool, len(diag.OverrideModels))
		for _, name := range diag.OverrideModels {
			overrides[name] = true
		}
		if only := r.URL.Query().Get("model"); only != "" {
			price, ok := cfg.Prices.Lookup(only)
			if !ok {
				http.Error(w, fmt.Sprintf("burnban: no pricing for %q", only), http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"diagnostics": diag, "model": pricingModelJSON(only, price, overrides[only])})
			return
		}
		names := make([]string, 0, len(cfg.Prices.Models))
		for name := range cfg.Prices.Models {
			names = append(names, name)
		}
		slices.Sort(names)
		models := make([]map[string]any, 0, len(names))
		for _, name := range names {
			models = append(models, pricingModelJSON(name, cfg.Prices.Models[name], overrides[name]))
		}
		writeJSON(w, map[string]any{"diagnostics": diag, "models": models})
	})

	mux.HandleFunc("GET /api/export", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		now := time.Now()
		from, _, err := parseWindow(r.URL.Query().Get("window"), "7d", maxReportWindow, now)
		if err != nil {
			http.Error(w, "burnban: "+err.Error(), http.StatusBadRequest)
			return
		}
		format := r.URL.Query().Get("format")
		if format == "" {
			format = "csv"
		}
		stamp := now.UTC().Format("20060102-150405")
		switch format {
		case "csv":
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="burnban-export-`+stamp+`.csv"`)
			if err := export.WriteCSV(w, s, from); err != nil {
				// Headers are already sent; the truncated body is the only signal left.
				return
			}
		case "json":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="burnban-export-`+stamp+`.json"`)
			if err := export.WriteJSON(w, s, from); err != nil {
				return
			}
		default:
			http.Error(w, "burnban: bad format: use csv or json", http.StatusBadRequest)
		}
	})

	registerAdminAPI(mux, s, cfg)
}

// ── admin (mutating) endpoints ─────────────────────────────────────────────

func registerAdminAPI(mux *http.ServeMux, s *store.Store, cfg Config) {
	admin := func(path string, handle func(w http.ResponseWriter, r *http.Request) (string, error)) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			secureHeaders(w)
			if !cfg.AllowAdmin {
				http.Error(w, adminDisabledError, http.StatusForbidden)
				return
			}
			message, err := handle(w, r)
			if err != nil {
				var invalid *invalidAdminRequest
				if errors.As(err, &invalid) {
					http.Error(w, "burnban: "+invalid.reason, http.StatusBadRequest)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			resp, err := buildGuardrailsFromStore(s, cfg, time.Now())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"ok": true, "message": message, "guardrails": resp})
		})
	}

	admin("/api/admin/cap", func(_ http.ResponseWriter, r *http.Request) (string, error) {
		var body struct {
			Window string  `json:"window"`
			Agent  string  `json:"agent"`
			CapUSD float64 `json:"cap_usd"`
		}
		if err := readAdminBody(r, &body); err != nil {
			return "", err
		}
		return applyCap(s, body.Window, body.Agent, body.CapUSD, time.Now())
	})

	admin("/api/admin/warn", func(_ http.ResponseWriter, r *http.Request) (string, error) {
		var body struct {
			Pct float64 `json:"pct"`
		}
		if err := readAdminBody(r, &body); err != nil {
			return "", err
		}
		if math.IsNaN(body.Pct) || math.IsInf(body.Pct, 0) || body.Pct < 0 || body.Pct > 100 {
			return "", badAdminRequest("warn threshold is a percentage of the cap: use 1-100, or 0 to disable")
		}
		if err := s.SetSetting(budget.KeyWarnPct, strconv.FormatFloat(body.Pct, 'f', -1, 64)); err != nil {
			return "", err
		}
		if body.Pct == 0 {
			return "early warnings disabled", nil
		}
		return fmt.Sprintf("early warning at %.4g%% of any cap (needs a webhook)", body.Pct), nil
	})

	admin("/api/admin/fuse", func(_ http.ResponseWriter, r *http.Request) (string, error) {
		var body fuseAdminRequest
		if err := readAdminBody(r, &body); err != nil {
			return "", err
		}
		return applyFuse(s, body, time.Now())
	})

	admin("/api/admin/ban", func(_ http.ResponseWriter, r *http.Request) (string, error) {
		if err := readAdminBody(r, &struct{}{}); err != nil {
			return "", err
		}
		if err := s.SetSetting(budget.KeyBanActive, "1"); err != nil {
			return "", err
		}
		message := "local burn ban in effect — all agent spend is paused until it is lifted"
		// The emergency stop also re-arms caps, so ban-then-lift returns to
		// enforced budgets instead of resurrecting an earlier today-override.
		if cleared, err := budget.ClearOverride(s, time.Now()); err != nil {
			return "", err
		} else if cleared {
			message += "; today's cap override cleared"
		}
		return message, nil
	})

	admin("/api/admin/lift", func(_ http.ResponseWriter, r *http.Request) (string, error) {
		var body struct {
			Today bool `json:"today"`
		}
		if err := readAdminBody(r, &body); err != nil {
			return "", err
		}
		now := time.Now()
		if err := s.DeleteSetting(budget.KeyBanActive); err != nil {
			return "", err
		}
		message := "local burn ban lifted"
		if body.Today {
			if err := s.SetSetting(budget.KeyOverrideDay, now.Format("2006-01-02")); err != nil {
				return "", err
			}
			message += " — local caps overridden for the rest of today"
		}
		if _, external, err := budget.BanStatus(s); err != nil {
			return "", err
		} else if external {
			message += "; external burn ban remains in effect"
		}
		if fuse, err := budget.FuseStatus(s, now); err != nil {
			return "", err
		} else if fuse.Tripped {
			message += "; spend-velocity fuse remains tripped — reset it from the fuse panel"
		}
		return message, nil
	})

	admin("/api/admin/webhook", func(_ http.ResponseWriter, r *http.Request) (string, error) {
		var body struct {
			URL string `json:"url"`
			Off bool   `json:"off"`
		}
		if err := readAdminBody(r, &body); err != nil {
			return "", err
		}
		switch {
		case body.Off:
			if err := s.DeleteSetting(budget.KeyWebhookURL); err != nil {
				return "", err
			}
			// Re-arm every window's sent-notification marks alongside.
			for _, w := range budget.Windows() {
				if err := budget.ClearMarks(s, w.Name); err != nil {
					return "", err
				}
			}
			if err := s.DeleteSettingsWithPrefix(budget.KeyFuseAlertedPrefix); err != nil {
				return "", err
			}
			return "webhook removed", nil
		case body.URL != "":
			u, err := url.Parse(body.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return "", badAdminRequest("webhook must be an http or https URL with a host")
			}
			if err := s.SetSetting(budget.KeyWebhookURL, body.URL); err != nil {
				return "", err
			}
			return "webhook set — burnban will POST warnings, cap trips, and each velocity-fuse incident", nil
		default:
			return "", badAdminRequest("provide url or off")
		}
	})
}

// applyCap mirrors `burnban cap` semantics for one window or one agent.
// cap_usd 0 removes the cap; "all" with 0 removes every global window.
func applyCap(s *store.Store, window, agent string, capUSD float64, now time.Time) (string, error) {
	if math.IsNaN(capUSD) || math.IsInf(capUSD, 0) || capUSD < 0 {
		return "", badAdminRequest("caps must be finite non-negative dollar amounts")
	}
	// Sub-cent caps would round to $0.00 in display and read as
	// "cap everything"; refuse them instead of storing a footgun.
	if capUSD != 0 && capUSD < 0.01 {
		return "", badAdminRequest("caps below $0.01 are not enforceable — use the burn ban to stop all spend")
	}
	if agent != "" {
		if window != "" && window != "daily" {
			return "", badAdminRequest("per-agent caps are daily-only for now")
		}
		agent = strings.TrimSpace(agent)
		if agent == "" || len([]rune(agent)) > maxAgentNameRunes || agent != export.TerminalText(agent, maxAgentNameRunes) {
			return "", badAdminRequest("agent name must be printable text up to 200 characters")
		}
		key := budget.KeyAgentCapPrefix + agent
		if capUSD == 0 {
			if err := s.DeleteSetting(key); err != nil {
				return "", err
			}
			return fmt.Sprintf("daily cap for agent %q removed", agent), nil
		}
		if err := s.SetSetting(key, strconv.FormatFloat(capUSD, 'f', -1, 64)); err != nil {
			return "", err
		}
		message := fmt.Sprintf("daily cap for agent %q set: $%.2f — the proxy returns 402 once it is reached", agent, capUSD)
		if cleared, err := budget.ClearOverride(s, now); err != nil {
			return "", err
		} else if cleared {
			message += "; today's cap override cleared"
		}
		return message, nil
	}
	if window == "all" {
		if capUSD != 0 {
			return "", badAdminRequest(`window "all" only supports cap_usd 0 (remove every global cap)`)
		}
		for _, w := range budget.Windows() {
			if err := s.DeleteSetting(w.Key); err != nil {
				return "", err
			}
			if err := budget.ClearMarks(s, w.Name); err != nil {
				return "", err
			}
		}
		return "all local global caps removed (external policy, per-agent caps, and warn threshold kept)", nil
	}
	win, ok := budget.WindowByName(window)
	if !ok {
		return "", badAdminRequest(`window must be "daily", "weekly", "monthly", or "all"`)
	}
	if capUSD == 0 {
		if err := s.DeleteSetting(win.Key); err != nil {
			return "", err
		}
		if err := budget.ClearMarks(s, win.Name); err != nil {
			return "", err
		}
		return fmt.Sprintf("local %s cap removed", win.Name), nil
	}
	if err := s.SetSetting(win.Key, strconv.FormatFloat(capUSD, 'f', -1, 64)); err != nil {
		return "", err
	}
	// A new threshold means the old "already warned/alerted" marks no
	// longer describe anything — re-arm both for this window.
	if err := budget.ClearMarks(s, win.Name); err != nil {
		return "", err
	}
	message := fmt.Sprintf("local %s cap set: $%.2f — the proxy returns 402 once it is reached", win.Name, capUSD)
	// A freshly set cap must actually enforce: drop any today-override,
	// which would otherwise silently suspend it until midnight.
	if cleared, err := budget.ClearOverride(s, now); err != nil {
		return "", err
	} else if cleared {
		message += "; today's cap override cleared — caps enforce again"
	}
	return message, nil
}

type fuseAdminRequest struct {
	Action    string             `json:"action"` // "set", "reset", or "off"
	HourlyUSD *float64           `json:"hourly_usd,omitempty"`
	Burst     *string            `json:"burst,omitempty"`  // "5m:4" or "off"
	Fanout    *string            `json:"fanout,omitempty"` // "1m:120" or "off"
	Baseline  *baselineAdminBody `json:"baseline,omitempty"`
	Cooldown  *string            `json:"cooldown,omitempty"` // "15m"
}

type baselineAdminBody struct {
	Off          bool    `json:"off,omitempty"`
	Multiplier   float64 `json:"multiplier,omitempty"`
	Window       string  `json:"window,omitempty"`
	LookbackDays int     `json:"lookback_days,omitempty"`
	MinimumUSD   float64 `json:"minimum_usd,omitempty"`
}

// applyFuse mirrors `burnban fuse` semantics, including validation bounds.
func applyFuse(s *store.Store, body fuseAdminRequest, now time.Time) (string, error) {
	switch body.Action {
	case "off":
		for _, key := range []string{budget.KeyFuseHourlyUSD, budget.KeyFuseBurst, budget.KeyFuseFanout, budget.KeyFuseBaseline, budget.KeyFuseCooldown, budget.KeyFuseTrip} {
			if err := s.DeleteSetting(key); err != nil {
				return "", err
			}
		}
		if err := s.DeleteSettingsWithPrefix(budget.KeyFuseAlertedPrefix); err != nil {
			return "", err
		}
		return "spend-velocity fuse removed", nil
	case "reset":
		if err := s.DeleteSetting(budget.KeyFuseTrip); err != nil {
			return "", err
		}
		return "fuse cooldown reset — new spend is eligible, but the fuse will retrip if rolling velocity is still above its limit", nil
	case "set":
	default:
		return "", badAdminRequest(`action must be "set", "reset", or "off"`)
	}
	if body.HourlyUSD == nil && body.Burst == nil && body.Fanout == nil && body.Baseline == nil && body.Cooldown == nil {
		return "", badAdminRequest("set requires at least one fuse field")
	}
	var messages []string
	if body.HourlyUSD != nil {
		usd := *body.HourlyUSD
		if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
			return "", badAdminRequest("hourly fuse must be a finite non-negative dollar amount")
		}
		if usd == 0 {
			if err := s.DeleteSetting(budget.KeyFuseHourlyUSD); err != nil {
				return "", err
			}
			messages = append(messages, "rolling hourly fuse removed")
		} else {
			if usd < 0.01 {
				return "", badAdminRequest("hourly fuse limits below $0.01 are not enforceable")
			}
			if err := s.SetSetting(budget.KeyFuseHourlyUSD, strconv.FormatFloat(usd, 'f', -1, 64)); err != nil {
				return "", err
			}
			messages = append(messages, fmt.Sprintf("rolling hourly fuse set: $%.2f", usd))
		}
	}
	if body.Burst != nil {
		raw := strings.ToLower(strings.TrimSpace(*body.Burst))
		if raw == "off" || raw == "0" {
			if err := s.DeleteSetting(budget.KeyFuseBurst); err != nil {
				return "", err
			}
			messages = append(messages, "rolling burst fuse removed")
		} else {
			window, usd, err := budget.ParseFuseBurst(raw)
			if err != nil {
				return "", badAdminRequest(err.Error())
			}
			if err := s.SetSetting(budget.KeyFuseBurst, budget.FormatFuseBurst(window, usd)); err != nil {
				return "", err
			}
			messages = append(messages, fmt.Sprintf("rolling %s burst fuse set: $%.2f", budget.FormatFuseDuration(window), usd))
		}
	}
	if body.Fanout != nil {
		raw := strings.ToLower(strings.TrimSpace(*body.Fanout))
		if raw == "off" || raw == "0" {
			if err := s.DeleteSetting(budget.KeyFuseFanout); err != nil {
				return "", err
			}
			messages = append(messages, "request fan-out fuse removed")
		} else {
			window, requests, err := budget.ParseFuseFanout(raw)
			if err != nil {
				return "", badAdminRequest(err.Error())
			}
			if err := s.SetSetting(budget.KeyFuseFanout, budget.FormatFuseFanout(window, requests)); err != nil {
				return "", err
			}
			messages = append(messages, fmt.Sprintf("rolling %s fan-out fuse set: %d requests", budget.FormatFuseDuration(window), requests))
		}
	}
	if body.Baseline != nil {
		if body.Baseline.Off {
			if err := s.DeleteSetting(budget.KeyFuseBaseline); err != nil {
				return "", err
			}
			messages = append(messages, "same-time baseline fuse removed")
		} else {
			window := time.Hour
			if body.Baseline.Window != "" {
				parsed, err := time.ParseDuration(body.Baseline.Window)
				if err != nil {
					return "", badAdminRequest("baseline window must be a duration such as 1h")
				}
				window = parsed
			}
			days := body.Baseline.LookbackDays
			if days == 0 {
				days = 14
			}
			minimum := body.Baseline.MinimumUSD
			if minimum == 0 {
				minimum = 0.25
			}
			raw, err := budget.EncodeFuseBaseline(budget.FuseBaselinePolicy{
				Version: 1, Window: window, Multiplier: body.Baseline.Multiplier,
				LookbackDays: days, MinimumUSD: minimum,
			})
			if err != nil {
				return "", badAdminRequest(err.Error())
			}
			if err := s.SetSetting(budget.KeyFuseBaseline, raw); err != nil {
				return "", err
			}
			messages = append(messages, fmt.Sprintf("same-time baseline fuse set: %s slot, %d-day lookback, minimum $%.2f",
				budget.FormatFuseDuration(window), days, minimum))
		}
	}
	if body.Cooldown != nil {
		cooldown, err := time.ParseDuration(strings.TrimSpace(*body.Cooldown))
		if err != nil {
			return "", badAdminRequest("cooldown must be a duration such as 15m")
		}
		if err := budget.ValidateFuseCooldown(cooldown); err != nil {
			return "", badAdminRequest(err.Error())
		}
		if err := s.SetSetting(budget.KeyFuseCooldown, budget.FormatFuseDuration(cooldown)); err != nil {
			return "", err
		}
		messages = append(messages, fmt.Sprintf("fuse cooldown set: %s", budget.FormatFuseDuration(cooldown)))
	}
	snapshot, err := budget.FuseStatus(s, now)
	if err != nil {
		return "", err
	}
	switch {
	case snapshot.Tripped:
		messages = append(messages, fmt.Sprintf("fuse remains tripped until %s", snapshot.TrippedUntil.In(now.Location()).Format("15:04:05")))
	case len(snapshot.Rules) == 0 && snapshot.Fanout == nil:
		messages = append(messages, "no active velocity-fuse rules remain")
	default:
		messages = append(messages, fmt.Sprintf("fuse armed — a trip pauses new spend for %s", budget.FormatFuseDuration(snapshot.Cooldown)))
	}
	return strings.Join(messages, "; "), nil
}

// ── shared builders and helpers ────────────────────────────────────────────

type reportJSON struct {
	Window          string      `json:"window"`
	Since           string      `json:"since"`
	Now             string      `json:"now"`
	TotalCost       float64     `json:"total_cost"`
	Requests        int64       `json:"requests"`
	In              int64       `json:"in_tokens"`
	Out             int64       `json:"out_tokens"`
	CacheRead       int64       `json:"cache_read_tokens"`
	CacheWrite      int64       `json:"cache_write_tokens"`
	CacheHitPct     float64     `json:"cache_hit_pct"`
	HasTraffic      bool        `json:"has_traffic"`
	LastHourCost    float64     `json:"last_hour_cost"`
	Estimated       int64       `json:"estimated"`
	Unpriced        int64       `json:"unpriced"`
	UnknownPricing  int64       `json:"unknown_pricing"`
	Unmetered       int64       `json:"unmetered"`
	Incomplete      int64       `json:"incomplete"`
	EnforcementGaps int64       `json:"enforcement_gaps"`
	FeeUnpriced     int64       `json:"fee_unpriced"`
	DupGroups       int64       `json:"dup_groups"`
	DupWastedUSD    float64     `json:"dup_wasted_usd"`
	LastRequestAt   string      `json:"last_request_at,omitempty"`
	Models          []modelJSON `json:"models"`
	Agents          []agentJSON `json:"agents"`
}

func buildReport(s *store.Store, from time.Time, label string, now time.Time) (*reportJSON, error) {
	var sum *store.Summary
	var lastHour float64
	err := s.ReadSnapshot(func(snapshot *store.ReadSnapshot) error {
		var err error
		sum, err = snapshot.Summarize(from)
		if err != nil {
			return err
		}
		lastHour, err = snapshot.SpentSince(now.Add(-time.Hour))
		return err
	})
	if err != nil {
		return nil, err
	}
	resp := &reportJSON{
		Window: label, Since: from.Format(time.RFC3339), Now: now.Format(time.RFC3339),
		TotalCost: sum.Cost, Requests: sum.Requests, In: sum.In, Out: sum.Out,
		CacheRead: sum.CacheRead, CacheWrite: sum.CacheWrite,
		LastHourCost: lastHour, Estimated: sum.Estimated, Unpriced: sum.Unpriced,
		UnknownPricing: sum.UnknownPricing, Unmetered: sum.Unmetered,
		Incomplete: sum.Incomplete, EnforcementGaps: sum.EnforcementGaps,
		FeeUnpriced: sum.FeeUnpriced, DupGroups: sum.DupGroups, DupWastedUSD: sum.DupWastedUSD,
		Models: []modelJSON{}, Agents: []agentJSON{},
	}
	if !sum.LastRequestAt.IsZero() {
		resp.LastRequestAt = sum.LastRequestAt.Format(time.RFC3339)
	}
	if total := sum.CacheRead + sum.In; total > 0 {
		resp.HasTraffic = true
		resp.CacheHitPct = float64(sum.CacheRead) / float64(total) * 100
	}
	for _, m := range sum.Models {
		resp.Models = append(resp.Models, modelJSON{
			Model: m.Model, Requests: m.Requests, In: m.In, Out: m.Out,
			CacheRead: m.CacheRead, CacheWrite: m.CacheWrite, Cost: m.Cost,
		})
	}
	if m := sum.ModelOther; m != nil {
		resp.Models = append(resp.Models, modelJSON{
			Model: "Other models (outside top 20)", Requests: m.Requests, In: m.In, Out: m.Out,
			CacheRead: m.CacheRead, CacheWrite: m.CacheWrite, Cost: m.Cost, IsOther: true,
		})
	}
	for _, a := range sum.Agents {
		resp.Agents = append(resp.Agents, agentJSON{Agent: a.Agent, Requests: a.Requests, Cost: a.Cost})
	}
	if a := sum.AgentOther; a != nil {
		resp.Agents = append(resp.Agents, agentJSON{
			Agent: "Other agents (outside top 20)", Requests: a.Requests, Cost: a.Cost, IsOther: true,
		})
	}
	return resp, nil
}

type guardrailWindowJSON struct {
	Name           string  `json:"name"`
	Set            bool    `json:"set"`
	CapUSD         float64 `json:"cap_usd"`
	LocalCapUSD    float64 `json:"local_cap_usd"`
	ExternalCapUSD float64 `json:"external_cap_usd"`
	SpentUSD       float64 `json:"spent_usd"`
	RemainingUSD   float64 `json:"remaining_usd"`
	Pct            float64 `json:"pct"`
	Source         string  `json:"source"`
	Reset          string  `json:"reset"`
}

type guardrailAgentJSON struct {
	Agent        string  `json:"agent"`
	CapUSD       float64 `json:"cap_usd"`
	SpentUSD     float64 `json:"spent_usd"`
	Requests     int64   `json:"requests"`
	RemainingUSD float64 `json:"remaining_usd"`
}

type guardrailFuseRuleJSON struct {
	Name               string  `json:"name"`
	Window             string  `json:"window"`
	CapUSD             float64 `json:"cap_usd"`
	SpentUSD           float64 `json:"spent_usd"`
	RemainingUSD       float64 `json:"remaining_usd"`
	Pct                float64 `json:"pct"`
	BaselineMedianUSD  float64 `json:"baseline_median_usd,omitempty"`
	BaselineMultiplier float64 `json:"baseline_multiplier,omitempty"`
	ProjectedToLimit   string  `json:"projected_time_to_limit,omitempty"`
}

type guardrailFanoutJSON struct {
	Window            string `json:"window"`
	LimitRequests     int64  `json:"limit_requests"`
	Requests          int64  `json:"requests"`
	RemainingRequests int64  `json:"remaining_requests"`
}

type guardrailFuseJSON struct {
	Rules         []guardrailFuseRuleJSON `json:"rules"`
	Fanout        *guardrailFanoutJSON    `json:"fanout,omitempty"`
	Cooldown      string                  `json:"cooldown"`
	Tripped       bool                    `json:"tripped"`
	TripRule      string                  `json:"trip_rule,omitempty"`
	TrippedUntil  string                  `json:"tripped_until,omitempty"`
	DenialMessage string                  `json:"denial_message,omitempty"`
}

type externalPolicyStateJSON struct {
	Version   string `json:"version,omitempty"`
	Source    string `json:"source,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type guardrailsJSON struct {
	Now            string                   `json:"now"`
	AllowAdmin     bool                     `json:"allow_admin"`
	Demo           bool                     `json:"demo"`
	BanActive      bool                     `json:"ban_active"`
	ExternalBan    bool                     `json:"external_ban"`
	OverrideToday  bool                     `json:"override_today"`
	Windows        []guardrailWindowJSON    `json:"windows"`
	AgentCaps      []guardrailAgentJSON     `json:"agent_caps"`
	WarnPct        float64                  `json:"warn_pct"`
	WarnConfigured bool                     `json:"warn_configured"`
	WebhookSet     bool                     `json:"webhook_set"`
	Webhook        string                   `json:"webhook,omitempty"`
	Fuse           guardrailFuseJSON        `json:"fuse"`
	ExternalPolicy *externalPolicyStateJSON `json:"external_policy,omitempty"`
}

type guardrailsReader interface {
	GetSettings(keys ...string) (map[string]string, error)
	GetSetting(key string) (string, error)
	SettingsWithPrefix(prefix string) (map[string]string, error)
	SpentSinceMulti(since []time.Time) ([]float64, error)
	BudgetUsageSinceMulti(since []time.Time) ([]store.BudgetUsage, error)
	BudgetUsageWindows(starts []time.Time, window time.Duration) ([]store.BudgetUsage, error)
	UsageSinceForAgents(since time.Time, agents []string) (map[string]store.AgentRow, error)
}

func buildGuardrailsFromStore(s *store.Store, cfg Config, now time.Time) (*guardrailsJSON, error) {
	var resp *guardrailsJSON
	err := s.ReadSnapshot(func(snapshot *store.ReadSnapshot) error {
		var err error
		resp, err = buildGuardrails(snapshot, cfg, now)
		return err
	})
	return resp, err
}

func buildGuardrails(s guardrailsReader, cfg Config, now time.Time) (*guardrailsJSON, error) {
	resp := &guardrailsJSON{
		Now: now.Format(time.RFC3339), AllowAdmin: cfg.AllowAdmin, Demo: cfg.Demo,
		Windows: []guardrailWindowJSON{}, AgentCaps: []guardrailAgentJSON{},
		Fuse: guardrailFuseJSON{Rules: []guardrailFuseRuleJSON{}},
	}
	localBan, externalBan, err := budget.BanStatus(s)
	if err != nil {
		return nil, err
	}
	resp.BanActive = localBan || externalBan
	resp.ExternalBan = externalBan
	states, err := budget.Status(s, now)
	if err != nil {
		return nil, err
	}
	for _, st := range states {
		resp.Windows = append(resp.Windows, guardrailWindowJSON{
			Name: st.Name, Set: st.Set, CapUSD: st.CapUSD,
			LocalCapUSD: st.LocalCapUSD, ExternalCapUSD: st.ExternalCapUSD,
			SpentUSD: st.Spent, RemainingUSD: max(0, st.CapUSD-st.Spent),
			Pct: st.Pct(), Source: st.Source, Reset: st.Reset,
		})
	}
	fuses, err := budget.FuseStatus(s, now)
	if err != nil {
		return nil, err
	}
	for _, rule := range fuses.Rules {
		out := guardrailFuseRuleJSON{
			Name: rule.Name, Window: budget.FormatFuseDuration(rule.Window),
			CapUSD: rule.CapUSD, SpentUSD: rule.SpentUSD, RemainingUSD: rule.Remaining,
			Pct: rule.Pct(), BaselineMedianUSD: rule.BaselineMedianUSD,
			BaselineMultiplier: rule.BaselineMultiplier,
		}
		if rule.ProjectedTimeToLimit > 0 {
			out.ProjectedToLimit = budget.FormatFuseDuration(rule.ProjectedTimeToLimit)
		}
		resp.Fuse.Rules = append(resp.Fuse.Rules, out)
	}
	if fuses.Fanout != nil {
		resp.Fuse.Fanout = &guardrailFanoutJSON{
			Window: budget.FormatFuseDuration(fuses.Fanout.Window), LimitRequests: fuses.Fanout.LimitRequests,
			Requests: fuses.Fanout.Requests, RemainingRequests: fuses.Fanout.RemainingRequests,
		}
	}
	resp.Fuse.Cooldown = budget.FormatFuseDuration(fuses.Cooldown)
	if fuses.Tripped {
		resp.Fuse.Tripped = true
		resp.Fuse.TripRule = fuses.TripRule
		resp.Fuse.TrippedUntil = fuses.TrippedUntil.Format(time.RFC3339)
		resp.Fuse.DenialMessage = fuses.DenialMessage
	}
	override, err := budget.OverrideActive(s, now)
	if err != nil {
		return nil, err
	}
	resp.OverrideToday = override

	settings, err := s.GetSettings(budget.KeyWarnPct, budget.KeyWebhookURL,
		budget.KeyExternalPolicyVersion, budget.KeyExternalPolicySource, budget.KeyExternalPolicyAt)
	if err != nil {
		return nil, err
	}
	resp.WarnPct = budget.DefaultWarnPct
	if raw := settings[budget.KeyWarnPct]; raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 100 {
			resp.WarnPct = v
			resp.WarnConfigured = true
		}
	}
	if hook := settings[budget.KeyWebhookURL]; hook != "" {
		resp.WebhookSet = true
		resp.Webhook = redactWebhook(hook)
	}
	if settings[budget.KeyExternalPolicyVersion] != "" || settings[budget.KeyExternalPolicySource] != "" {
		resp.ExternalPolicy = &externalPolicyStateJSON{
			Version:   safeLabel(settings[budget.KeyExternalPolicyVersion]),
			Source:    safeLabel(settings[budget.KeyExternalPolicySource]),
			UpdatedAt: safeLabel(settings[budget.KeyExternalPolicyAt]),
		}
	}

	configured, err := s.SettingsWithPrefix(budget.KeyAgentCapPrefix)
	if err != nil {
		return nil, err
	}
	agents := make([]string, 0, len(configured))
	caps := make(map[string]float64, len(configured))
	for agent, raw := range configured {
		if strings.TrimSpace(agent) == "" {
			continue
		}
		if capUSD, parseErr := strconv.ParseFloat(raw, 64); parseErr == nil && capUSD > 0 {
			caps[agent] = capUSD
			agents = append(agents, agent)
		}
	}
	slices.Sort(agents)
	usage, err := s.UsageSinceForAgents(budget.DayStart(now), agents)
	if err != nil {
		return nil, err
	}
	for _, agent := range agents {
		row := usage[agent]
		resp.AgentCaps = append(resp.AgentCaps, guardrailAgentJSON{
			Agent: agent, CapUSD: caps[agent], SpentUSD: row.Cost, Requests: row.Requests,
			RemainingUSD: max(0, caps[agent]-row.Cost),
		})
	}
	return resp, nil
}

func pricingModelJSON(name string, p pricing.Price, override bool) map[string]any {
	out := map[string]any{
		"model": name, "input_per_mtok": p.InputPerMTok, "output_per_mtok": p.OutputPerMTok,
		"cache_read_mult": p.CacheReadMult, "cache_write_mult": p.CacheWriteMult,
	}
	if p.LongContextThreshold > 0 {
		out["long_context_threshold"] = p.LongContextThreshold
		out["long_input_mult"] = p.LongInputMult
		out["long_output_mult"] = p.LongOutputMult
	}
	if p.Free {
		out["free"] = true
	}
	if p.Source != "" {
		out["source"] = p.Source
	}
	if p.VerifiedDate != "" {
		out["verified_date"] = p.VerifiedDate
	}
	if override {
		out["override"] = true
	}
	return out
}

func pricingAgeDays(diag pricing.Diagnostics, now time.Time) (int, bool) {
	stamp := diag.VerifiedDate
	if stamp == "" {
		stamp = diag.EffectiveDate
	}
	parsed, err := time.Parse("2006-01-02", stamp)
	if err != nil {
		return 0, false
	}
	return int(now.Sub(parsed) / (24 * time.Hour)), true
}

// parseWindow mirrors the CLI's --since grammar ("today", "24h", "7d", any Go
// duration) with an explicit upper bound so a browser cannot request an
// unbounded ledger scan.
func parseWindow(raw, fallback string, maxWindow time.Duration, now time.Time) (time.Time, string, error) {
	if raw == "" {
		raw = fallback
	}
	if raw == "today" {
		return budget.DayStart(now), "today (" + now.Format("2006-01-02") + ")", nil
	}
	if strings.HasSuffix(raw, "d") {
		n, err := strconv.ParseInt(strings.TrimSuffix(raw, "d"), 10, 64)
		if err != nil || n <= 0 || time.Duration(n)*24*time.Hour > maxWindow {
			return time.Time{}, "", fmt.Errorf("bad window %q: day windows go up to %dd", raw, int64(maxWindow/(24*time.Hour)))
		}
		if n == 1 {
			return now.Add(-24 * time.Hour), "last 1 day", nil
		}
		return now.Add(-time.Duration(n) * 24 * time.Hour), fmt.Sprintf("last %d days", n), nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > maxWindow {
		return time.Time{}, "", fmt.Errorf("bad window %q: use today, 24h, 7d, 30d, or a positive duration", raw)
	}
	return now.Add(-d), "last " + raw, nil
}

type invalidAdminRequest struct{ reason string }

func (e *invalidAdminRequest) Error() string { return e.reason }

func badAdminRequest(reason string) error { return &invalidAdminRequest{reason: reason} }

func readAdminBody(r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return badAdminRequest("Content-Type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxAdminBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badAdminRequest("invalid JSON body: " + err.Error())
	}
	if dec.More() {
		return badAdminRequest("invalid JSON body: trailing content")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func redactWebhook(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<redacted>"
	}
	return u.Scheme + "://" + u.Host + "/<redacted>"
}

// safeLabel bounds and sanitizes sidecar-written metadata before it reaches
// the browser payload; the dashboard also escapes at render time.
func safeLabel(value string) string {
	value = strings.ToValidUTF8(value, "�")
	var b strings.Builder
	for i, r := range value {
		if i >= 200 {
			b.WriteRune('…')
			break
		}
		if unicode.IsControl(r) {
			r = ' '
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
