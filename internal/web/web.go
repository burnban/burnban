// Package web serves burnban's embedded dashboard: one self-contained HTML
// page and the JSON endpoint it polls. Everything ships inside the binary —
// no CDNs, no external assets, nothing leaves localhost.
package web

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/store"
)

//go:embed index.html
var indexHTML []byte

// Register mounts the dashboard at / (exact) and its feed at /api/summary.
// Provider proxy routes keep working because they match longer patterns.
func Register(mux *http.ServeMux, s *store.Store, version string) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/summary", func(w http.ResponseWriter, r *http.Request) {
		resp, err := build(s, version, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if err := writeMetrics(w, s, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /api/series", func(w http.ResponseWriter, r *http.Request) {
		hours := 24
		if v := r.URL.Query().Get("hours"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 24*14 {
				hours = n
			}
		}
		now := time.Now().UTC()
		since := now.Add(-time.Duration(hours-1) * time.Hour).Truncate(time.Hour)
		pts, err := s.HourlySeries(since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		byHour := make(map[string]float64, len(pts))
		for _, p := range pts {
			byHour[p.Hour] = p.Cost
		}
		type pt struct {
			T    string  `json:"t"`
			Cost float64 `json:"cost"`
		}
		out := make([]pt, 0, hours)
		for i := 0; i < hours; i++ {
			h := since.Add(time.Duration(i) * time.Hour)
			out = append(out, pt{T: h.Format(time.RFC3339), Cost: byHour[h.Format("2006-01-02T15")]})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// WithAuth guards every route with a shared token when one is configured.
// The token may arrive as Bearer auth, an x-burnban-token header, or a
// ?token= query param (so the dashboard can be opened in a browser). An
// empty token disables the check — that is the localhost-only default.
func WithAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("x-burnban-token")
		if got == "" {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				got = strings.TrimPrefix(h, "Bearer ")
			}
		}
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), want) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "burnban: missing or invalid token", http.StatusUnauthorized)
	})
}

// writeMetrics emits Prometheus text exposition so platform teams can
// scrape burnban into Grafana without any exporter in between. Per-model
// and per-agent series follow the store's top-20 cut to bound cardinality.
func writeMetrics(w http.ResponseWriter, s *store.Store, now time.Time) error {
	all, err := s.Summarize(time.Unix(0, 0))
	if err != nil {
		return err
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	today, err := s.SpentSince(midnight)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP burnban_requests_total Proxied inference requests since first run.\n# TYPE burnban_requests_total counter\nburnban_requests_total %d\n", all.Requests)
	fmt.Fprintf(w, "# HELP burnban_cost_usd_total Metered spend in USD since first run.\n# TYPE burnban_cost_usd_total counter\nburnban_cost_usd_total %g\n", all.Cost)
	fmt.Fprintf(w, "# TYPE burnban_model_cost_usd_total counter\n")
	for _, m := range all.Models {
		fmt.Fprintf(w, "burnban_model_cost_usd_total{model=%q} %g\n", m.Model, m.Cost)
	}
	fmt.Fprintf(w, "# TYPE burnban_model_requests_total counter\n")
	for _, m := range all.Models {
		fmt.Fprintf(w, "burnban_model_requests_total{model=%q} %d\n", m.Model, m.Requests)
	}
	fmt.Fprintf(w, "# TYPE burnban_agent_cost_usd_total counter\n")
	for _, a := range all.Agents {
		fmt.Fprintf(w, "burnban_agent_cost_usd_total{agent=%q} %g\n", a.Agent, a.Cost)
	}
	fmt.Fprintf(w, "# HELP burnban_spend_today_usd Spend since local midnight.\n# TYPE burnban_spend_today_usd gauge\nburnban_spend_today_usd %g\n", today)
	for _, win := range budget.Windows() {
		if win.Name == "daily" {
			continue // today's gauge above predates windows; keep its name stable
		}
		spent, err := s.SpentSince(win.Start(now))
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "# HELP burnban_spend_%s_usd Spend since the %s window opened.\n# TYPE burnban_spend_%s_usd gauge\nburnban_spend_%s_usd %g\n",
			win.Name, win.Name, win.Name, win.Name, spent)
	}
	for _, win := range budget.Windows() {
		capUSD := 0.0
		if capStr, err := s.GetSetting(win.Key); err == nil && capStr != "" {
			if v, perr := strconv.ParseFloat(capStr, 64); perr == nil {
				capUSD = v
			}
		}
		fmt.Fprintf(w, "# HELP burnban_cap_%s_usd Configured %s cap (0 = none).\n# TYPE burnban_cap_%s_usd gauge\nburnban_cap_%s_usd %g\n",
			win.Name, win.Name, win.Name, win.Name, capUSD)
	}
	ban := 0
	if b, err := s.GetSetting(budget.KeyBanActive); err == nil && b == "1" {
		ban = 1
	}
	fmt.Fprintf(w, "# HELP burnban_ban_active Whether the burn ban is engaged.\n# TYPE burnban_ban_active gauge\nburnban_ban_active %d\n", ban)
	return nil
}

type modelJSON struct {
	Model      string  `json:"model"`
	Requests   int64   `json:"requests"`
	In         int64   `json:"in_tokens"`
	Out        int64   `json:"out_tokens"`
	CacheRead  int64   `json:"cache_read_tokens"`
	CacheWrite int64   `json:"cache_write_tokens"`
	Cost       float64 `json:"cost_usd"`
}

type agentJSON struct {
	Agent    string  `json:"agent"`
	Requests int64   `json:"requests"`
	Cost     float64 `json:"cost_usd"`
}

type summaryJSON struct {
	Now           string      `json:"now"`
	Version       string      `json:"version"`
	TotalCost     float64     `json:"total_cost"`
	Requests      int64       `json:"requests"`
	In            int64       `json:"in_tokens"`
	Out           int64       `json:"out_tokens"`
	CacheRead     int64       `json:"cache_read_tokens"`
	CacheWrite    int64       `json:"cache_write_tokens"`
	CacheHitPct   float64     `json:"cache_hit_pct"`
	HasTraffic    bool        `json:"has_traffic"`
	LastHourCost  float64     `json:"last_hour_cost"`
	Estimated     int64       `json:"estimated"`
	Unpriced      int64       `json:"unpriced"`
	CapDailyUSD   float64     `json:"cap_daily_usd"`
	HasCap        bool        `json:"has_cap"`
	CapWeeklyUSD  float64     `json:"cap_weekly_usd"`
	WeekCost      float64     `json:"week_cost"`
	CapMonthlyUSD float64     `json:"cap_monthly_usd"`
	MonthCost     float64     `json:"month_cost"`
	BanActive     bool        `json:"ban_active"`
	OverrideToday bool        `json:"override_today"`
	Models        []modelJSON `json:"models"`
	Agents        []agentJSON `json:"agents"`
	DupGroups     int64       `json:"dup_groups"`
	DupWastedUSD  float64     `json:"dup_wasted_usd"`
}

func build(s *store.Store, version string, now time.Time) (*summaryJSON, error) {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	sum, err := s.Summarize(midnight)
	if err != nil {
		return nil, err
	}
	lastHour, err := s.SpentSince(now.Add(-time.Hour))
	if err != nil {
		return nil, err
	}

	resp := &summaryJSON{
		Now:          now.Format(time.RFC3339),
		Version:      version,
		TotalCost:    sum.Cost,
		Requests:     sum.Requests,
		In:           sum.In,
		Out:          sum.Out,
		CacheRead:    sum.CacheRead,
		CacheWrite:   sum.CacheWrite,
		LastHourCost: lastHour,
		Estimated:    sum.Estimated,
		Unpriced:     sum.Unpriced,
		Models:       []modelJSON{},
		Agents:       []agentJSON{},
		DupGroups:    sum.DupGroups,
		DupWastedUSD: sum.DupWastedUSD,
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
	for _, a := range sum.Agents {
		resp.Agents = append(resp.Agents, agentJSON{Agent: a.Agent, Requests: a.Requests, Cost: a.Cost})
	}

	if capStr, err := s.GetSetting(budget.KeyDailyCapUSD); err != nil {
		return nil, err
	} else if capStr != "" {
		if capUSD, perr := strconv.ParseFloat(capStr, 64); perr == nil {
			resp.HasCap = true
			resp.CapDailyUSD = capUSD
		}
	}
	if capStr, err := s.GetSetting(budget.KeyWeeklyCapUSD); err != nil {
		return nil, err
	} else if capStr != "" {
		if capUSD, perr := strconv.ParseFloat(capStr, 64); perr == nil && capUSD > 0 {
			resp.CapWeeklyUSD = capUSD
			if resp.WeekCost, err = s.SpentSince(budget.WeekStart(now)); err != nil {
				return nil, err
			}
		}
	}
	if capStr, err := s.GetSetting(budget.KeyMonthlyCapUSD); err != nil {
		return nil, err
	} else if capStr != "" {
		if capUSD, perr := strconv.ParseFloat(capStr, 64); perr == nil && capUSD > 0 {
			resp.CapMonthlyUSD = capUSD
			if resp.MonthCost, err = s.SpentSince(budget.MonthStart(now)); err != nil {
				return nil, err
			}
		}
	}
	if ban, err := s.GetSetting(budget.KeyBanActive); err != nil {
		return nil, err
	} else if ban == "1" {
		resp.BanActive = true
	}
	if ov, err := s.GetSetting(budget.KeyOverrideDay); err != nil {
		return nil, err
	} else if ov == now.Format("2006-01-02") {
		resp.OverrideToday = true
	}
	return resp, nil
}
