// Package web serves burnban's embedded dashboard: one self-contained HTML
// page and the JSON endpoint it polls. Everything ships inside the binary —
// no CDNs, no external assets, nothing leaves localhost.
package web

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
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
