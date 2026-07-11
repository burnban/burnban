// Package web serves burnban's embedded dashboard: one self-contained HTML
// page and the JSON endpoint it polls. Everything ships inside the binary;
// the page loads no CDNs or other third-party assets.
package web

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
	"github.com/burnban/burnban/internal/subsidy"
)

//go:embed index.html
var indexHTML []byte

// Register mounts the dashboard at / (exact) and its feed at /api/summary.
// Provider proxy routes keep working because they match longer patterns.
func Register(mux *http.ServeMux, s *store.Store, version string, prices *pricing.Table) {
	RegisterWithConfig(mux, s, Config{Version: version, Prices: prices, Exposure: "localhost"})
}

// Config describes runtime facts the embedded page cannot safely infer from
// window.location (notably whether it is a fake demo or a network service).
type Config struct {
	Version      string
	Prices       *pricing.Table
	Demo         bool
	Exposure     string
	AuthRequired bool
	// DisableLocalUsage prevents the server from reading host-user agent logs.
	// Network/team exposure always forces this true below, regardless of input.
	DisableLocalUsage bool
	Health            func() HealthStatus
}

type HealthStatus struct {
	OK            bool       `json:"ok"`
	State         string     `json:"state"`
	Detail        string     `json:"detail,omitempty"`
	PersistenceOK bool       `json:"persistence_ok"`
	InFlight      int        `json:"in_flight"`
	ReservedUSD   float64    `json:"reserved_usd"`
	LastFailure   *time.Time `json:"last_failure,omitempty"`
}

// RegisterWithConfig mounts the dashboard and JSON feeds with explicit
// runtime state. Register remains as a compatibility wrapper for embedders.
func RegisterWithConfig(mux *http.ServeMux, s *store.Store, cfg Config) {
	if cfg.Prices == nil {
		panic("web: Config.Prices is required")
	}
	if cfg.Exposure == "" {
		cfg.Exposure = "localhost"
	}
	if cfg.Exposure == "team/network" {
		cfg.DisableLocalUsage = true
	}
	subscriptions := newSubscriptionFeed(cfg.Prices)
	summaries := newSummaryFeed(s, cfg)
	metrics := newMetricsFeed(s)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/summary", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		resp, err := summaries.get(time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		body, err := metrics.get(time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(body))
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
		if cfg.DisableLocalUsage {
			http.Error(w, "burnban: host-local usage scanning is disabled on a team/network gateway", http.StatusForbidden)
			return
		}
		window := r.URL.Query().Get("window")
		if window == "" {
			window = "today"
		}
		var resp *subscriptionResponse
		var err error
		if cfg.Demo {
			resp, err = demoSubscription(window, time.Now())
		} else {
			resp, err = subscriptions.get(window, time.Now())
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

type metricsFeed struct {
	mu      sync.Mutex
	store   *store.Store
	created time.Time
	value   string
	ttl     time.Duration
}

func newMetricsFeed(s *store.Store) *metricsFeed {
	return &metricsFeed{store: s, ttl: 5 * time.Second}
}

func (f *metricsFeed) get(now time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.value != "" && now.Sub(f.created) >= 0 && now.Sub(f.created) < f.ttl {
		return f.value, nil
	}
	var body strings.Builder
	if err := writeMetrics(&body, f.store, now); err != nil {
		return "", err
	}
	f.value = body.String()
	f.created = time.Now()
	return f.value, nil
}

type summaryFeed struct {
	mu      sync.Mutex
	store   *store.Store
	cfg     Config
	created time.Time
	value   *summaryJSON
	ttl     time.Duration
}

func newSummaryFeed(s *store.Store, cfg Config) *summaryFeed {
	return &summaryFeed{store: s, cfg: cfg, ttl: time.Second}
}

func (f *summaryFeed) get(now time.Time) (*summaryJSON, error) {
	// Serialize cache misses so several dashboard tabs cannot launch the same
	// aggregate and duplicate-receipt scan concurrently. A one-second cache is
	// short relative to the two-second UI poll but absorbs synchronized tabs.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.value != nil && now.Sub(f.created) >= 0 && now.Sub(f.created) < f.ttl {
		return f.value, nil
	}
	value, err := build(f.store, f.cfg, now)
	if err != nil {
		return nil, err
	}
	f.value = value
	f.created = time.Now()
	return value, nil
}

// LocalSafety blocks DNS rebinding and cross-site browser requests before
// they can reach either dashboard data or a provider route carrying a key.
// Team mode still rejects browser cross-site requests, while allowing reverse
// proxies to choose the public Host value.
func LocalSafety(bindHost string, tokenlessLoopback bool, next http.Handler) http.Handler {
	return LocalSafetyWithPublicOrigin(bindHost, tokenlessLoopback, "", next)
}

// LocalSafetyWithPublicOrigin additionally trusts browser Origin checks that
// exactly match the configured public origin. This supports TLS-terminating
// reverse proxies that rewrite Host to the loopback backend while still
// rejecting every unconfigured cross-origin browser request.
func LocalSafetyWithPublicOrigin(bindHost string, tokenlessLoopback bool, publicOrigin string, next http.Handler) http.Handler {
	var public *url.URL
	if publicOrigin != "" {
		parsed, err := url.Parse(publicOrigin)
		if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" &&
			parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && (parsed.Path == "" || parsed.Path == "/") {
			public = parsed
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tokenlessLoopback && !safeLoopbackHost(r.Host, bindHost) {
			http.Error(w, "burnban: unexpected Host on localhost listener", http.StatusMisdirectedRequest)
			return
		}
		if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
			http.Error(w, "burnban: cross-site browser requests are not allowed", http.StatusForbidden)
			return
		}
		if rawOrigin := r.Header.Get("Origin"); rawOrigin != "" {
			origin, err := url.Parse(rawOrigin)
			validOrigin := err == nil && origin.Scheme != "" && origin.Host != "" && origin.User == nil &&
				origin.Path == "" && origin.RawPath == "" && origin.RawQuery == "" && origin.Fragment == ""
			matchesRequest := validOrigin && sameAuthority(origin.Host, r.Host)
			matchesPublic := validOrigin && public != nil && strings.EqualFold(origin.Scheme, public.Scheme) && sameAuthority(origin.Host, public.Host)
			if !matchesRequest && !matchesPublic {
				http.Error(w, "burnban: cross-origin browser requests are not allowed", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func safeLoopbackHost(requestHost, bindHost string) bool {
	host := hostOnly(requestHost)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	bind := hostOnly(bindHost)
	return bind != "" && strings.EqualFold(host, bind) && (strings.EqualFold(bind, "localhost") || (net.ParseIP(bind) != nil && net.ParseIP(bind).IsLoopback()))
}

func hostOnly(authority string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(authority, "[]")
}

func sameAuthority(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
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

func demoSubscription(window string, now time.Time) (*subscriptionResponse, error) {
	var since time.Time
	var label string
	var multiplier int64
	switch window {
	case "today":
		since, label, multiplier = budget.DayStart(now), "today", 1
	case "7d":
		since, label, multiplier = now.Add(-7*24*time.Hour), "last 7 days", 6
	case "30d":
		since, label, multiplier = now.Add(-30*24*time.Hour), "last 30 days", 24
	default:
		return nil, fmt.Errorf("bad window %q: use today, 7d, or 30d", window)
	}
	claude := subsidy.ModelUsage{Model: "claude-sonnet-5", Priced: true, PricingSource: "table", Totals: subsidy.Totals{
		Calls: 47 * multiplier, In: 38200 * multiplier, Out: 12400 * multiplier,
		CacheRead: 680000 * multiplier, CacheWrite: 42000 * multiplier,
		CacheWrite5m: 42000 * multiplier, APIUSD: 1.284 * float64(multiplier),
	}}
	codex := subsidy.ModelUsage{Model: "gpt-5.6-luna", Priced: true, PricingSource: "table", Totals: subsidy.Totals{
		Calls: 31 * multiplier, In: 26400 * multiplier, Out: 8900 * multiplier,
		CacheRead: 240000 * multiplier, APIUSD: 0.736 * float64(multiplier),
	}}
	providers := []subsidy.ProviderUsage{
		{Provider: "claude-code", Detected: true, Sessions: int(4 * multiplier), Models: []subsidy.ModelUsage{claude}, Days: []subsidy.DayUsage{}, Totals: claude.Totals},
		{Provider: "codex", Detected: true, Sessions: int(3 * multiplier), Models: []subsidy.ModelUsage{codex}, Days: []subsidy.DayUsage{}, Totals: codex.Totals},
		{Provider: "hermes", Models: []subsidy.ModelUsage{}, Days: []subsidy.DayUsage{}},
		{Provider: "openclaw", Models: []subsidy.ModelUsage{}, Days: []subsidy.DayUsage{}},
		{Provider: "goose", Models: []subsidy.ModelUsage{}, Days: []subsidy.DayUsage{}},
	}
	totals := subsidy.Totals{
		Calls: claude.Calls + codex.Calls, In: claude.In + codex.In, Out: claude.Out + codex.Out,
		CacheRead: claude.CacheRead + codex.CacheRead, CacheWrite: claude.CacheWrite + codex.CacheWrite,
		CacheWrite5m: claude.CacheWrite5m + codex.CacheWrite5m, APIUSD: claude.APIUSD + codex.APIUSD,
	}
	return &subscriptionResponse{Window: window, Label: label, Report: subsidy.Report{
		Since: since, Until: now, HasUsage: true, UnpricedModels: []string{}, Providers: providers, Totals: totals,
	}}, nil
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
// The token may arrive as an x-burnban-token header. Bearer auth is accepted
// on burnban-owned routes only because provider routes need that header for
// the provider API key. Credentials consumed here are removed before forwarding.
func WithAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The embedded shell contains no user data and must load before a browser
		// can send the token as a header. Every JSON/metrics/provider route stays
		// protected; this avoids putting a long-lived token in a URL.
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("x-burnban-token")
		usedBearer := false
		if got == "" && burnbanRoute(r.URL.Path) {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				got = strings.TrimPrefix(h, "Bearer ")
				usedBearer = true
			}
		}
		if subtle.ConstantTimeCompare([]byte(got), want) == 1 {
			r.Header.Del("x-burnban-token")
			if usedBearer {
				r.Header.Del("Authorization")
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
func writeMetrics(w *strings.Builder, s *store.Store, now time.Time) error {
	all, err := s.LifetimeMetrics()
	if err != nil {
		return err
	}
	states, err := budget.Status(s, now)
	if err != nil {
		return err
	}
	localBan, externalBan, err := budget.BanStatus(s)
	if err != nil {
		return err
	}
	// These are retained-ledger gauges, not monotonic process counters: an
	// explicit `burnban prune` intentionally makes them decrease.
	fmt.Fprintf(w, "# HELP burnban_ledger_requests Proxied inference requests retained in the ledger.\n# TYPE burnban_ledger_requests gauge\nburnban_ledger_requests %d\n", all.Requests)
	fmt.Fprintf(w, "# HELP burnban_ledger_cost_usd Metered spend retained in the ledger.\n# TYPE burnban_ledger_cost_usd gauge\nburnban_ledger_cost_usd %g\n", all.Cost)
	fmt.Fprintf(w, "# HELP burnban_ledger_unknown_pricing Retained successful calls with usage but unknown model pricing.\n# TYPE burnban_ledger_unknown_pricing gauge\nburnban_ledger_unknown_pricing %d\n", all.UnknownPricing)
	fmt.Fprintf(w, "# HELP burnban_ledger_unmetered Retained calls without usable token accounting.\n# TYPE burnban_ledger_unmetered gauge\nburnban_ledger_unmetered %d\n", all.Unmetered)
	fmt.Fprintf(w, "# HELP burnban_ledger_incomplete Retained partial or cancelled responses.\n# TYPE burnban_ledger_incomplete gauge\nburnban_ledger_incomplete %d\n", all.Incomplete)
	fmt.Fprintf(w, "# HELP burnban_ledger_enforcement_gaps Retained successful calls that made active-cap accounting unsafe.\n# TYPE burnban_ledger_enforcement_gaps gauge\nburnban_ledger_enforcement_gaps %d\n", all.EnforcementGaps)
	fmt.Fprintf(w, "# HELP burnban_ledger_unpriced_fees Retained calls with provider-side fee dimensions that could not be priced.\n# TYPE burnban_ledger_unpriced_fees gauge\nburnban_ledger_unpriced_fees %d\n", all.FeeUnpriced)
	fmt.Fprintf(w, "# TYPE burnban_ledger_model_cost_usd gauge\n")
	for _, m := range all.Models {
		fmt.Fprintf(w, "burnban_ledger_model_cost_usd{model=\"%s\"} %g\n", prometheusLabel(m.Model), m.Cost)
	}
	fmt.Fprintf(w, "# TYPE burnban_ledger_model_requests gauge\n")
	for _, m := range all.Models {
		fmt.Fprintf(w, "burnban_ledger_model_requests{model=\"%s\"} %d\n", prometheusLabel(m.Model), m.Requests)
	}
	fmt.Fprintf(w, "# TYPE burnban_ledger_agent_cost_usd gauge\n")
	for _, a := range all.Agents {
		fmt.Fprintf(w, "burnban_ledger_agent_cost_usd{agent=\"%s\"} %g\n", prometheusLabel(a.Agent), a.Cost)
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

func prometheusLabel(value string) string {
	value = strings.ToValidUTF8(value, "�")
	var b strings.Builder
	count := 0
	for _, r := range value {
		if count >= 200 {
			b.WriteRune('…')
			break
		}
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteRune(' ')
			} else {
				b.WriteRune(r)
			}
		}
		count++
	}
	return b.String()
}

type modelJSON struct {
	Model      string  `json:"model"`
	Requests   int64   `json:"requests"`
	In         int64   `json:"in_tokens"`
	Out        int64   `json:"out_tokens"`
	CacheRead  int64   `json:"cache_read_tokens"`
	CacheWrite int64   `json:"cache_write_tokens"`
	Cost       float64 `json:"cost_usd"`
	IsOther    bool    `json:"is_other,omitempty"`
}

type agentJSON struct {
	Agent        string  `json:"agent"`
	Requests     int64   `json:"requests"`
	Cost         float64 `json:"cost_usd"`
	CapUSD       float64 `json:"cap_usd"`
	RemainingUSD float64 `json:"remaining_usd"`
	IsOther      bool    `json:"is_other,omitempty"`
}

type budgetJSON struct {
	Name      string  `json:"name"`
	SpentUSD  float64 `json:"spent_usd"`
	CapUSD    float64 `json:"cap_usd"`
	Remaining float64 `json:"remaining_usd"`
	Pct       float64 `json:"pct"`
	Source    string  `json:"source"`
	Reset     string  `json:"reset"`
}

type summaryJSON struct {
	Now             string        `json:"now"`
	Version         string        `json:"version"`
	Demo            bool          `json:"demo"`
	Exposure        string        `json:"exposure"`
	AuthRequired    bool          `json:"auth_required"`
	LocalUsage      bool          `json:"local_usage_enabled"`
	Health          *HealthStatus `json:"health,omitempty"`
	LastRequestAt   string        `json:"last_request_at,omitempty"`
	TotalCost       float64       `json:"total_cost"`
	Requests        int64         `json:"requests"`
	In              int64         `json:"in_tokens"`
	Out             int64         `json:"out_tokens"`
	CacheRead       int64         `json:"cache_read_tokens"`
	CacheWrite      int64         `json:"cache_write_tokens"`
	CacheWrite1h    int64         `json:"cache_write_1h_tokens"`
	CacheHitPct     float64       `json:"cache_hit_pct"`
	HasTraffic      bool          `json:"has_traffic"`
	LastHourCost    float64       `json:"last_hour_cost"`
	Estimated       int64         `json:"estimated"`
	Unpriced        int64         `json:"unpriced"`
	UnknownPricing  int64         `json:"unknown_pricing"`
	Unmetered       int64         `json:"unmetered"`
	Incomplete      int64         `json:"incomplete"`
	EnforcementGaps int64         `json:"enforcement_gaps"`
	FeeUnpriced     int64         `json:"fee_unpriced"`
	CapDailyUSD     float64       `json:"cap_daily_usd"`
	CapDailyCost    float64       `json:"cap_daily_cost"`
	HasCap          bool          `json:"has_cap"`
	CapWeeklyUSD    float64       `json:"cap_weekly_usd"`
	WeekCost        float64       `json:"week_cost"`
	CapMonthlyUSD   float64       `json:"cap_monthly_usd"`
	MonthCost       float64       `json:"month_cost"`
	BanActive       bool          `json:"ban_active"`
	ExternalBan     bool          `json:"external_ban"`
	OverrideToday   bool          `json:"override_today"`
	ExternalPolicy  bool          `json:"external_policy"`
	Models          []modelJSON   `json:"models"`
	Agents          []agentJSON   `json:"agents"`
	Budgets         []budgetJSON  `json:"budgets"`
	DupGroups       int64         `json:"dup_groups"`
	DupWastedUSD    float64       `json:"dup_wasted_usd"`
}

func build(s *store.Store, cfg Config, now time.Time) (*summaryJSON, error) {
	sum, err := s.Summarize(budget.DayStart(now))
	if err != nil {
		return nil, err
	}
	lastHour, err := s.SpentSince(now.Add(-time.Hour))
	if err != nil {
		return nil, err
	}

	resp := &summaryJSON{
		Now:             now.Format(time.RFC3339),
		Version:         cfg.Version,
		Demo:            cfg.Demo,
		Exposure:        cfg.Exposure,
		AuthRequired:    cfg.AuthRequired,
		LocalUsage:      !cfg.DisableLocalUsage,
		TotalCost:       sum.Cost,
		Requests:        sum.Requests,
		In:              sum.In,
		Out:             sum.Out,
		CacheRead:       sum.CacheRead,
		CacheWrite:      sum.CacheWrite,
		CacheWrite1h:    sum.CacheWrite1h,
		LastHourCost:    lastHour,
		Estimated:       sum.Estimated,
		Unpriced:        sum.Unpriced,
		UnknownPricing:  sum.UnknownPricing,
		Unmetered:       sum.Unmetered,
		Incomplete:      sum.Incomplete,
		EnforcementGaps: sum.EnforcementGaps,
		FeeUnpriced:     sum.FeeUnpriced,
		Models:          []modelJSON{},
		Agents:          []agentJSON{},
		Budgets:         []budgetJSON{},
		DupGroups:       sum.DupGroups,
		DupWastedUSD:    sum.DupWastedUSD,
	}
	if cfg.Health != nil {
		health := cfg.Health()
		resp.Health = &health
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
			Model: "Other models (outside top 20)", Requests: m.Requests,
			In: m.In, Out: m.Out, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite,
			Cost: m.Cost, IsOther: true,
		})
	}
	configuredAgentCaps, err := s.SettingsWithPrefix(budget.KeyAgentCapPrefix)
	if err != nil {
		return nil, err
	}
	agentCaps := make(map[string]float64, len(configuredAgentCaps))
	capAgents := make([]string, 0, len(configuredAgentCaps))
	for agent, raw := range configuredAgentCaps {
		if strings.TrimSpace(agent) == "" {
			continue
		}
		if capUSD, parseErr := strconv.ParseFloat(raw, 64); parseErr == nil && capUSD > 0 {
			agentCaps[agent] = capUSD
			capAgents = append(capAgents, agent)
		}
	}
	slices.Sort(capAgents)
	cappedUsage, err := s.UsageSinceForAgents(budget.DayStart(now), capAgents)
	if err != nil {
		return nil, err
	}
	seenAgents := make(map[string]bool, len(sum.Agents))
	otherAgentRequests, otherAgentCost := int64(0), float64(0)
	if sum.AgentOther != nil {
		otherAgentRequests = sum.AgentOther.Requests
		otherAgentCost = sum.AgentOther.Cost
	}
	for _, a := range sum.Agents {
		row := agentJSON{Agent: a.Agent, Requests: a.Requests, Cost: a.Cost}
		if capUSD, capped := agentCaps[a.Agent]; capped {
			usage := cappedUsage[a.Agent]
			row.Requests = usage.Requests
			row.Cost = usage.Cost
			row.CapUSD = capUSD
			row.RemainingUSD = max(0, capUSD-usage.Cost)
		}
		seenAgents[a.Agent] = true
		resp.Agents = append(resp.Agents, row)
	}
	for _, agent := range capAgents {
		if seenAgents[agent] {
			continue
		}
		usage := cappedUsage[agent]
		capUSD := agentCaps[agent]
		resp.Agents = append(resp.Agents, agentJSON{
			Agent: agent, Requests: usage.Requests, Cost: usage.Cost,
			CapUSD: capUSD, RemainingUSD: max(0, capUSD-usage.Cost),
		})
		// This named capped agent was part of the aggregate outside-top-20
		// remainder. Pull it out so the displayed rows still reconcile exactly.
		otherAgentRequests = max(0, otherAgentRequests-usage.Requests)
		otherAgentCost = max(0, otherAgentCost-usage.Cost)
	}
	if otherAgentRequests > 0 {
		resp.Agents = append(resp.Agents, agentJSON{
			Agent: "Other agents (outside top 20)", Requests: otherAgentRequests,
			Cost: otherAgentCost, IsOther: true,
		})
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
		if st.Set {
			resp.Budgets = append(resp.Budgets, budgetJSON{
				Name: st.Name, SpentUSD: st.Spent, CapUSD: st.CapUSD,
				Remaining: max(0, st.CapUSD-st.Spent), Pct: st.Pct(), Source: st.Source, Reset: st.Reset,
			})
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
		resp.ExternalBan = externalBan
	}
	if ov, err := s.GetSetting(budget.KeyOverrideDay); err != nil {
		return nil, err
	} else if ov == now.Format("2006-01-02") {
		resp.OverrideToday = true
	}
	return resp, nil
}
