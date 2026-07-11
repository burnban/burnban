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
	"sync"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/pricing"
	"github.com/syft8/burnban/internal/store"
	"github.com/syft8/burnban/internal/subsidy"
)

//go:embed index.html
var indexHTML []byte

// Register mounts the dashboard at / (exact) and its feed at /api/summary.
// Provider proxy routes keep working because they match longer patterns.
func Register(mux *http.ServeMux, s *store.Store, version string, prices *pricing.Table) {
	subscriptions := newSubscriptionFeed(prices)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/summary", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		resp, err := build(s, version, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		if err := writeMetrics(w, s, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /api/series", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
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
	mux.HandleFunc("GET /api/subsidy", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		window := r.URL.Query().Get("window")
		if window == "" {
			window = "today"
		}
		resp, err := subscriptions.get(window, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

type subscriptionResponse struct {
	Window string `json:"window"`
	Label  string `json:"label"`
	subsidy.Report
}

type cachedSubscription struct {
	created time.Time
	since   time.Time
	value   *subscriptionResponse
}

type subscriptionFeed struct {
	mu      sync.Mutex
	prices  *pricing.Table
	entries map[string]cachedSubscription
	ttl     time.Duration
}

func newSubscriptionFeed(prices *pricing.Table) *subscriptionFeed {
	return &subscriptionFeed{prices: prices, entries: map[string]cachedSubscription{}, ttl: time.Minute}
}

func (f *subscriptionFeed) get(window string, now time.Time) (*subscriptionResponse, error) {
	var since time.Time
	var label string
	switch window {
	case "today":
		since, label = budget.DayStart(now), "today"
	case "7d":
		since, label = now.Add(-7*24*time.Hour), "last 7 days"
	case "30d":
		since, label = now.Add(-30*24*time.Hour), "last 30 days"
	default:
		return nil, fmt.Errorf("bad window %q: use today, 7d, or 30d", window)
	}

	// Scans are read-only but can walk many logs. Serialize cache misses so a
	// dashboard refresh cannot start duplicate scans of the same local data.
	f.mu.Lock()
	defer f.mu.Unlock()
	if cached, ok := f.entries[window]; ok && time.Since(cached.created) < f.ttl &&
		(window != "today" || cached.since.Equal(since)) {
		return cached.value, nil
	}
	report, err := subsidy.BuildReport(f.prices, subsidy.ReportOptions{Since: since, Until: now})
	if err != nil {
		return nil, err
	}
	resp := &subscriptionResponse{Window: window, Label: label, Report: report}
	// Stamp the cache after the scan. Large histories can take longer than the
	// TTL itself on cold or network-backed storage; using the request start time
	// would make the freshly completed result immediately stale and let browser
	// polling create an endless queue of full rescans.
	f.entries[window] = cachedSubscription{created: time.Now(), since: since, value: resp}
	return resp, nil
}

func secureHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

// WithAuth guards every route with a shared token when one is configured.
// The token may arrive as an x-burnban-token header or a ?token= query param
// (so the dashboard can be opened in a browser). Bearer auth is accepted on
// burnban-owned routes only because provider routes need that header for the
// provider API key. Credentials consumed here are removed before forwarding.
func WithAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("x-burnban-token")
		usedBearer := false
		if got == "" && burnbanRoute(r.URL.Path) {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				got = strings.TrimPrefix(h, "Bearer ")
				usedBearer = true
			}
		}
		usedQuery := false
		if got == "" && burnbanRoute(r.URL.Path) {
			got = r.URL.Query().Get("token")
			usedQuery = got != ""
		}
		if subtle.ConstantTimeCompare([]byte(got), want) == 1 {
			r.Header.Del("x-burnban-token")
			if usedBearer {
				r.Header.Del("Authorization")
			}
			if usedQuery {
				q := r.URL.Query()
				q.Del("token")
				r.URL.RawQuery = q.Encode()
			}
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "burnban: missing or invalid token", http.StatusUnauthorized)
	})
}

func burnbanRoute(path string) bool {
	return path == "/" || path == "/health" || path == "/metrics" || strings.HasPrefix(path, "/api/")
}

// writeMetrics emits Prometheus text exposition so platform teams can
// scrape burnban into Grafana without any exporter in between. Per-model
// and per-agent series follow the store's top-20 cut to bound cardinality.
func writeMetrics(w http.ResponseWriter, s *store.Store, now time.Time) error {
	all, err := s.Summarize(time.Unix(0, 0))
	if err != nil {
		return err
	}
	states, err := budget.Status(s, now)
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
	for _, st := range states {
		fmt.Fprintf(w, "# HELP burnban_spend_%s_usd Spend since the %s window opened.\n# TYPE burnban_spend_%s_usd gauge\nburnban_spend_%s_usd %g\n",
			st.Name, st.Name, st.Name, st.Name, st.Spent)
		fmt.Fprintf(w, "# HELP burnban_cap_%s_usd Configured %s cap (0 = none).\n# TYPE burnban_cap_%s_usd gauge\nburnban_cap_%s_usd %g\n",
			st.Name, st.Name, st.Name, st.Name, st.CapUSD)
		if st.Name == "daily" {
			// Legacy alias from before budget windows existed.
			fmt.Fprintf(w, "# HELP burnban_spend_today_usd Spend since local midnight (alias of burnban_spend_daily_usd).\n# TYPE burnban_spend_today_usd gauge\nburnban_spend_today_usd %g\n", st.Spent)
		}
	}
	ban := 0
	localBan, externalBan, err := budget.BanStatus(s)
	if err != nil {
		return err
	}
	if localBan || externalBan {
		ban = 1
	}
	fmt.Fprintf(w, "# HELP burnban_ban_active Whether the burn ban is engaged.\n# TYPE burnban_ban_active gauge\nburnban_ban_active %d\n", ban)
	external := 0
	if externalBan {
		external = 1
	}
	fmt.Fprintf(w, "# HELP burnban_external_ban_active Whether an external policy has engaged the burn ban.\n# TYPE burnban_external_ban_active gauge\nburnban_external_ban_active %d\n", external)
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
	Now            string      `json:"now"`
	Version        string      `json:"version"`
	TotalCost      float64     `json:"total_cost"`
	Requests       int64       `json:"requests"`
	In             int64       `json:"in_tokens"`
	Out            int64       `json:"out_tokens"`
	CacheRead      int64       `json:"cache_read_tokens"`
	CacheWrite     int64       `json:"cache_write_tokens"`
	CacheHitPct    float64     `json:"cache_hit_pct"`
	HasTraffic     bool        `json:"has_traffic"`
	LastHourCost   float64     `json:"last_hour_cost"`
	Estimated      int64       `json:"estimated"`
	Unpriced       int64       `json:"unpriced"`
	CapDailyUSD    float64     `json:"cap_daily_usd"`
	CapDailyCost   float64     `json:"cap_daily_cost"`
	HasCap         bool        `json:"has_cap"`
	CapWeeklyUSD   float64     `json:"cap_weekly_usd"`
	WeekCost       float64     `json:"week_cost"`
	CapMonthlyUSD  float64     `json:"cap_monthly_usd"`
	MonthCost      float64     `json:"month_cost"`
	BanActive      bool        `json:"ban_active"`
	OverrideToday  bool        `json:"override_today"`
	ExternalPolicy bool        `json:"external_policy"`
	Models         []modelJSON `json:"models"`
	Agents         []agentJSON `json:"agents"`
	DupGroups      int64       `json:"dup_groups"`
	DupWastedUSD   float64     `json:"dup_wasted_usd"`
}

func build(s *store.Store, version string, now time.Time) (*summaryJSON, error) {
	sum, err := s.Summarize(budget.DayStart(now))
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

	// One settings query + one ledger scan covers every budget window; the
	// dashboard polls this endpoint every two seconds, so it must stay cheap.
	states, err := budget.Status(s, now)
	if err != nil {
		return nil, err
	}
	for _, st := range states {
		if st.Set {
			resp.HasCap = true // any enforcing window counts as "capped"
		}
		if st.ExternalCapUSD > 0 {
			resp.ExternalPolicy = true
		}
		switch st.Name {
		case "daily":
			if st.Set {
				resp.CapDailyUSD = st.CapUSD
				resp.CapDailyCost = st.Spent
			}
		case "weekly":
			if st.Set {
				resp.CapWeeklyUSD = st.CapUSD
				resp.WeekCost = st.Spent
			}
		case "monthly":
			if st.Set {
				resp.CapMonthlyUSD = st.CapUSD
				resp.MonthCost = st.Spent
			}
		}
	}
	localBan, externalBan, err := budget.BanStatus(s)
	if err != nil {
		return nil, err
	} else if localBan || externalBan {
		resp.BanActive = true
	}
	if ov, err := s.GetSetting(budget.KeyOverrideDay); err != nil {
		return nil, err
	} else if ov == now.Format("2006-01-02") {
		resp.OverrideToday = true
	}
	return resp, nil
}
