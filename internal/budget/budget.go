// Package budget decides whether a request may spend money right now:
// it enforces the manual burn ban and the daily/weekly/monthly dollar caps,
// and reports when spend crosses the early-warning threshold. It is on the
// request path, so reads are batched: one settings query, one ledger scan.
package budget

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/burnban/burnban/internal/store"
)

// Settings keys shared between the proxy and the CLI.
const (
	KeyDailyCapUSD   = "cap_daily_usd"
	KeyWeeklyCapUSD  = "cap_weekly_usd"
	KeyMonthlyCapUSD = "cap_monthly_usd"
	KeyBanActive     = "ban_active"
	KeyOverrideDay   = "cap_override_day"
	KeyWebhookURL    = "webhook_url"
	// External policy is a local SQLite contract for optional sidecars. The
	// MIT binary reads these keys but contains no sync client or remote URL.
	KeyExternalDailyCapUSD   = "external_cap_daily_usd"
	KeyExternalWeeklyCapUSD  = "external_cap_weekly_usd"
	KeyExternalMonthlyCapUSD = "external_cap_monthly_usd"
	KeyExternalBanActive     = "external_ban_active"
	KeyExternalPolicyVersion = "external_policy_version"
	KeyExternalPolicySource  = "external_policy_source"
	KeyExternalPolicyAt      = "external_policy_updated_at"
	// KeyExternalPolicyExceptions is a bounded, sidecar-written expiry
	// schedule. The stored external cap includes each active increment; after
	// its valid_until the MIT meter subtracts it locally even while offline.
	KeyExternalPolicyExceptions = "external_policy_exceptions_v1"
	// KeyWarnPct holds the early-warning threshold as a percentage of any
	// window's cap. Empty means DefaultWarnPct; "0" disables warnings.
	KeyWarnPct = "warn_pct"
	// KeyWarnedPrefix + "<window>:<window start date>" marks that window
	// instance as already warned about; KeyAlertedPrefix marks its
	// cap-reached alert as sent. Both are cleared when the cap changes.
	KeyWarnedPrefix  = "warned:"
	KeyAlertedPrefix = "alerted:"
	// KeyAgentCapPrefix + <agent name> holds that agent's own daily cap.
	KeyAgentCapPrefix = "cap_agent_daily_usd:"
)

// DefaultWarnPct is the warning threshold used when a webhook is configured
// but warn_pct was never set explicitly.
const DefaultWarnPct = 80.0

// Agent cap usage is cheap to reload from its covering index. Bounding the
// warm set prevents configured-agent churn from growing process memory
// forever; crossing the limit performs one amortized bulk eviction.
const maxAgentUsageCacheEntries = 4096

// Window is one budget enforcement window. Starts are computed in local
// time, matching how people read their bills.
type Window struct {
	Name        string // "daily", "weekly", "monthly" — also the cap flag name
	Key         string // settings key holding the user's local cap
	ExternalKey string // optional sidecar-managed allocation
	Start       func(now time.Time) time.Time
	Reset       string // when the window rolls over, for human-facing copy
}

// Windows lists the enforced budget windows, tightest first. It is the
// single source of truth: the CLI, MCP, dashboard, and metrics all derive
// their window vocabulary from it.
func Windows() []Window {
	return []Window{
		{"daily", KeyDailyCapUSD, KeyExternalDailyCapUSD, DayStart, "at midnight"},
		{"weekly", KeyWeeklyCapUSD, KeyExternalWeeklyCapUSD, WeekStart, "Monday"},
		{"monthly", KeyMonthlyCapUSD, KeyExternalMonthlyCapUSD, MonthStart, "on the 1st"},
	}
}

// WindowByName resolves a window name (empty means daily), so frontends
// never hand-copy the name→key mapping.
func WindowByName(name string) (Window, bool) {
	if name == "" {
		name = "daily"
	}
	for _, w := range Windows() {
		if w.Name == name {
			return w, true
		}
	}
	return Window{}, false
}

func DayStart(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// WeekStart is the most recent Monday at 00:00 — ISO weeks, like invoices.
func WeekStart(now time.Time) time.Time {
	d := DayStart(now)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

func MonthStart(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
}

type Guard struct {
	S *store.Store

	mu                sync.Mutex
	reservedUSD       float64
	reservedByAgent   map[string]float64
	inFlight          int
	cacheRevision     uint64
	usageCache        map[usageCacheKey]store.BudgetUsage
	activeGlobalKeys  map[usageCacheKey]struct{}
	agentCacheDay     int64
	agentCacheEntries int

	baselineCacheRaw      string
	baselineCacheStart    int64
	baselineCacheRevision uint64
	baselineCacheMedian   float64
}

// AdmissionEstimate is a conservative preflight cost bound derived from the
// request. Priced is false when the request's model cannot be priced. Bounded
// is false when the provider request omitted an output-token ceiling; such a
// call is admitted exclusively against any active dollar guardrail so
// concurrent calls cannot multiply an unknown overshoot.
type AdmissionEstimate struct {
	USD         float64
	Priced      bool
	Bounded     bool
	Description string
}

// Reservation accounts for admitted work that has not reached the durable
// ledger yet. Release must be called after the request row is inserted (or
// after the persistence failure has been latched fail-closed).
type Reservation struct {
	guard     *Guard
	agent     string
	amount    float64
	capActive bool
	agentDay  usageCacheKey
	done      bool // guarded by guard.mu
}

func (r *Reservation) Release() {
	if r == nil || r.guard == nil {
		return
	}
	g := r.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	r.releaseLocked()
}

func (r *Reservation) releaseLocked() {
	if r.done {
		return
	}
	g := r.guard
	g.reservedUSD = max(0, g.reservedUSD-r.amount)
	if r.agent != "" {
		left := max(0, g.reservedByAgent[r.agent]-r.amount)
		if left == 0 {
			delete(g.reservedByAgent, r.agent)
		} else {
			g.reservedByAgent[r.agent] = left
		}
	}
	if g.inFlight > 0 {
		g.inFlight--
	}
	r.done = true
}

// Settle durably inserts a completed request, updates every warm usage window,
// and releases its conservative reservation as one Guard-locked transition.
// On insert failure the reservation remains held until Release, so the proxy
// can latch persistence fail-closed without an undercount window.
func (r *Reservation) Settle(request store.Request) error {
	if r == nil || r.guard == nil {
		return fmt.Errorf("cannot settle a nil reservation")
	}
	if request.Agent != r.agent {
		return fmt.Errorf("settled request agent does not match its reservation")
	}
	if request.Ts.IsZero() || math.IsNaN(request.CostUSD) || math.IsInf(request.CostUSD, 0) || request.CostUSD < 0 {
		return fmt.Errorf("settled request has invalid timestamp or cost")
	}
	g := r.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.done {
		return nil
	}
	before := g.S.RequestRevision()
	if err := g.S.Insert(request); err != nil {
		return err
	}
	after := g.S.RequestRevision()
	if g.usageCache != nil && before%2 == 0 && g.cacheRevision == before && after == before+2 {
		for key := range g.activeGlobalKeys {
			g.addSettledUsageLocked(key, request)
		}
		if r.agent != "" {
			g.addSettledUsageLocked(r.agentDay, request)
		}
		g.cacheRevision = after
	} else {
		g.invalidateUsageCacheLocked(after)
	}
	if g.baselineCacheStart != 0 {
		switch {
		case before%2 == 0 && g.baselineCacheRevision == before && after == before+2 && request.Ts.UnixNano() >= g.baselineCacheStart:
			// Current-window settlements cannot alter historical baseline
			// samples, so advance the cache's sequence lock without rescanning.
			g.baselineCacheRevision = after
		default:
			g.clearBaselineCacheLocked()
		}
	}
	r.releaseLocked()
	return nil
}

func (g *Guard) addSettledUsageLocked(key usageCacheKey, request store.Request) {
	usage, ok := g.usageCache[key]
	if !ok || request.Ts.UnixNano() < key.startUnixNano || (key.agent != "" && key.agent != request.Agent) {
		return
	}
	usage.SpentUSD += request.CostUSD
	usage.Requests++
	if request.EnforcementUnsafe {
		usage.EnforcementGaps++
	}
	g.usageCache[key] = usage
}

func (r *Reservation) AmountUSD() float64 {
	if r == nil {
		return 0
	}
	return r.amount
}

func (r *Reservation) CapActive() bool {
	return r != nil && r.capActive
}

type ReservationSnapshot struct {
	InFlight    int
	ReservedUSD float64
}

func (g *Guard) Reservations() ReservationSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return ReservationSnapshot{InFlight: g.inFlight, ReservedUSD: g.reservedUSD}
}

// Denial explains why spend is paused, in words an agent's error surface
// will show the user verbatim. Window and WindowStart identify which cap
// tripped so alerting can dedup per window instance, not per day.
type Denial struct {
	Type        string
	Message     string
	Window      string
	WindowStart time.Time
	AlertKey    string
}

func (d *Denial) Error() string { return d.Message }

// AlertMark is the settings key that dedups this denial's webhook alert.
// Calendar windows derive it from their start; fuse incidents provide an
// explicit key. It is empty for denial types that do not alert.
func (d *Denial) AlertMark() string {
	if d.AlertKey != "" {
		return d.AlertKey
	}
	if d.Window == "" || d.WindowStart.IsZero() {
		return ""
	}
	return KeyAlertedPrefix + d.Window + ":" + d.WindowStart.Format("2006-01-02")
}

// WindowState is one window's live budget position, shared by the CLI
// status view, dashboard, metrics, and MCP so they can never disagree.
type WindowState struct {
	Window                   // embedded definition (Name, Key, Start, Reset)
	CapUSD         float64   // effective (stricter) cap; 0 when unset
	LocalCapUSD    float64   // user-managed local cap; 0 when unset
	ExternalCapUSD float64   // sidecar-managed allocation; 0 when unset
	LocalSpent     float64   // spend in the machine-local calendar window
	ExternalSpent  float64   // spend in the external policy's UTC calendar window
	Source         string    // "local", "external", or "both"
	Set            bool      // a valid effective cap is configured
	Spent          float64   // spend since the window opened
	StartAt        time.Time // when the window opened, at the queried instant
}

// Pct is spend as a percentage of the cap; 0 when no cap is set.
func (w WindowState) Pct() float64 {
	if !w.Set || w.CapUSD <= 0 {
		return 0
	}
	return w.Spent / w.CapUSD * 100
}

type statusReader interface {
	GetSettings(keys ...string) (map[string]string, error)
	SpentSinceMulti(ts []time.Time) ([]float64, error)
}

// Status reports every window's cap and live spend with one settings query
// and one ledger scan. The narrow reader contract also lets dashboard reads
// run against a single SQLite transaction without duplicating cap logic.
func Status(s statusReader, now time.Time) ([]WindowState, error) {
	wins := Windows()
	keys := make([]string, 0, len(wins)*2)
	starts := make([]time.Time, 0, len(wins)*2)
	for _, w := range wins {
		keys = append(keys, w.Key, w.ExternalKey)
		starts = append(starts, w.Start(now), externalStart(w, now))
	}
	keys = append(keys, KeyExternalPolicyExceptions)
	vals, err := s.GetSettings(keys...)
	if err != nil {
		return nil, err
	}
	expiredIncreases, err := expiredExternalIncreases(vals[KeyExternalPolicyExceptions], now)
	if err != nil {
		return nil, err
	}
	spents, err := s.SpentSinceMulti(starts)
	if err != nil {
		return nil, err
	}
	out := make([]WindowState, len(wins))
	for i, w := range wins {
		localStart, externalStartAt := starts[i*2], starts[i*2+1]
		st := WindowState{
			Window: w, LocalSpent: spents[i*2], ExternalSpent: spents[i*2+1],
			Spent: spents[i*2], StartAt: localStart,
		}
		local, localSet, err := parseCap(w.Key, vals[w.Key])
		if err != nil {
			return nil, err
		}
		external, externalSet, err := parseCap(w.ExternalKey, vals[w.ExternalKey])
		if err != nil {
			return nil, err
		}
		external, externalSet, err = subtractExpiredIncrease(w.Name, external, externalSet, expiredIncreases[w.Name])
		if err != nil {
			return nil, err
		}
		st.LocalCapUSD, st.ExternalCapUSD = local, external
		selectEffectiveState(&st, local, localSet, external, externalSet, localStart, externalStartAt)
		out[i] = st
	}
	return out, nil
}

type externalExceptionEnvelope struct {
	Version    int                         `json:"version"`
	Exceptions []externalExceptionSchedule `json:"exceptions"`
}

type externalExceptionSchedule struct {
	RequestID  string  `json:"request_id"`
	Window     string  `json:"window"`
	AmountUSD  float64 `json:"amount_usd"`
	ValidUntil string  `json:"valid_until"`
}

func expiredExternalIncreases(raw string, now time.Time) (map[string]float64, error) {
	out := map[string]float64{"daily": 0, "weekly": 0, "monthly": 0}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	if len(raw) > 256<<10 {
		return nil, fmt.Errorf("invalid %s: exceeds 256 KiB", KeyExternalPolicyExceptions)
	}
	if err := validateExternalExceptionJSON(raw); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", KeyExternalPolicyExceptions, err)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var envelope externalExceptionEnvelope
	if err := dec.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", KeyExternalPolicyExceptions, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("invalid %s: trailing JSON", KeyExternalPolicyExceptions)
	}
	if envelope.Version != 1 || len(envelope.Exceptions) > 1000 {
		return nil, fmt.Errorf("invalid %s envelope", KeyExternalPolicyExceptions)
	}
	seen := make(map[string]struct{}, len(envelope.Exceptions))
	for i, item := range envelope.Exceptions {
		if strings.TrimSpace(item.RequestID) != item.RequestID || item.RequestID == "" || len(item.RequestID) > 100 ||
			(item.Window != "daily" && item.Window != "weekly" && item.Window != "monthly") ||
			math.IsNaN(item.AmountUSD) || math.IsInf(item.AmountUSD, 0) || item.AmountUSD <= 0 || item.AmountUSD > 1e9 {
			return nil, fmt.Errorf("invalid %s item %d", KeyExternalPolicyExceptions, i)
		}
		if _, duplicate := seen[item.RequestID]; duplicate {
			return nil, fmt.Errorf("invalid %s: duplicate request %q", KeyExternalPolicyExceptions, item.RequestID)
		}
		seen[item.RequestID] = struct{}{}
		validUntil, err := time.Parse(time.RFC3339, item.ValidUntil)
		if err != nil || item.ValidUntil != validUntil.UTC().Format(time.RFC3339) {
			return nil, fmt.Errorf("invalid %s item %d expiry", KeyExternalPolicyExceptions, i)
		}
		if !validUntil.After(now) {
			next := out[item.Window] + item.AmountUSD
			if math.IsInf(next, 0) || next > 1e12 {
				return nil, fmt.Errorf("invalid %s expired total", KeyExternalPolicyExceptions)
			}
			out[item.Window] = next
		}
	}
	return out, nil
}

var canonicalExternalExceptionJSONFields = map[string]struct{}{
	"version": {}, "exceptions": {}, "request_id": {}, "window": {}, "amount_usd": {}, "valid_until": {},
}

func validateExternalExceptionJSON(raw string) error {
	if !utf8.ValidString(raw) {
		return fmt.Errorf("JSON is not valid UTF-8")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := scanExternalExceptionJSON(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func scanExternalExceptionJSON(dec *json.Decoder, depth int) error {
	if depth > 16 {
		return fmt.Errorf("JSON nesting exceeds 16 levels")
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, canonical := canonicalExternalExceptionJSONFields[key]; !canonical {
				return fmt.Errorf("unknown or non-canonical JSON field %q", key)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanExternalExceptionJSON(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := scanExternalExceptionJSON(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func subtractExpiredIncrease(window string, external float64, set bool, expired float64) (float64, bool, error) {
	if expired == 0 {
		return external, set, nil
	}
	if !set || expired > external {
		return 0, false, fmt.Errorf("invalid %s: expired %s increase exceeds the stored external cap", KeyExternalPolicyExceptions, window)
	}
	effective := external - expired
	if math.IsNaN(effective) || math.IsInf(effective, 0) || effective <= 0 {
		return 0, false, fmt.Errorf("invalid %s effective %s cap", KeyExternalPolicyExceptions, window)
	}
	return effective, true, nil
}

// Check returns a non-nil Denial when a ban/cooldown is active, a calendar or
// rolling window has reached its limit, or the calling agent has reached its
// daily cap. A nil, nil return means proceed.
func (g *Guard) Check(now time.Time, agent string) (*Denial, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	denial, _, err := g.checkLocked(now, agent)
	return denial, err
}

// Admit atomically checks durable spend plus every in-flight reservation and
// reserves room for this request. Admissions within one proxy process are
// serialized, closing the check-then-forward race that allowed a burst of
// concurrent requests to all observe the same stale ledger balance.
func (g *Guard) Admit(now time.Time, agent string, estimate AdmissionEstimate) (*Reservation, *Denial, error) {
	if math.IsNaN(estimate.USD) || math.IsInf(estimate.USD, 0) || estimate.USD < 0 {
		return nil, nil, fmt.Errorf("invalid admission estimate %v", estimate.USD)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	denial, state, err := g.checkLocked(now, agent)
	if err != nil || denial != nil {
		return nil, denial, err
	}

	amount := 0.0
	if state.active() {
		if !estimate.Priced {
			detail := estimate.Description
			if detail == "" {
				detail = "request model"
			}
			return nil, &Denial{
				Type: "burnban_unpriced_request",
				Message: fmt.Sprintf(
					"cannot safely enforce an active dollar guardrail because %s has no known price. Add pricing or disable the active guardrail before sending it.", detail),
			}, nil
		}
		if estimate.Bounded {
			amount = estimate.USD
			if limit, ok := state.firstCrossed(amount); ok {
				if limit.isFuse() {
					denial, err := g.tripFuseLocked(now, limit, amount)
					return nil, denial, err
				}
				return nil, &Denial{
					Type: "burnban_request_exceeds_remaining",
					Message: fmt.Sprintf(
						"request's conservative cost bound $%.4f exceeds the $%.4f remaining on the %s cap; lower max tokens, raise the cap, or override local caps for today.",
						amount, max(0, limit.remaining()), limit.name),
				}, nil
			}
		} else {
			// No declared output ceiling: allow one call, but reserve every active
			// window's remaining headroom so another call cannot race alongside it.
			amount = state.maxRemaining()
		}
	}
	if g.reservedByAgent == nil {
		g.reservedByAgent = map[string]float64{}
	}
	g.reservedUSD += amount
	if agent != "" {
		g.reservedByAgent[agent] += amount
	}
	g.inFlight++
	return &Reservation{
		guard: g, agent: agent, amount: amount, capActive: state.active(),
		agentDay: usageCacheKey{startUnixNano: DayStart(now).UnixNano(), agent: agent},
	}, nil, nil
}

type capPosition struct {
	name     string
	cap      float64
	spent    float64
	reserved float64
	unsafe   int64
	window   Window
	start    time.Time
	source   string
	agent    string

	fuseName     string
	fuseWindow   time.Duration
	fuseCooldown time.Duration
}

func (p capPosition) remaining() float64 { return p.cap - p.spent - p.reserved }
func (p capPosition) isFuse() bool       { return p.fuseName != "" }

type admissionState struct {
	positions []capPosition
}

type usageCacheKey struct {
	startUnixNano int64
	agent         string
}

type usageRequest struct {
	start time.Time
	agent string
}

func (g *Guard) invalidateUsageCacheLocked(revision uint64) {
	g.cacheRevision = revision
	g.usageCache = make(map[usageCacheKey]store.BudgetUsage)
	g.activeGlobalKeys = nil
	g.agentCacheDay = 0
	g.agentCacheEntries = 0
}

// reconcileUsageCacheLocked touches only the previous active global keys and
// the current agent during ordinary checks. A full agent sweep happens once on
// local-day rollover, plus an amortized sweep if same-day churn exceeds the
// bounded warm-agent set.
func (g *Guard) reconcileUsageCacheLocked(global []usageRequest, agent string, agentCapSet bool, agentStart time.Time) {
	desiredGlobal := make(map[usageCacheKey]struct{}, len(global))
	for _, request := range global {
		desiredGlobal[usageCacheKey{startUnixNano: request.start.UnixNano()}] = struct{}{}
	}
	for key := range g.activeGlobalKeys {
		if _, active := desiredGlobal[key]; !active {
			delete(g.usageCache, key)
		}
	}
	g.activeGlobalKeys = desiredGlobal

	currentDay := agentStart.UnixNano()
	if g.agentCacheDay != 0 && g.agentCacheDay != currentDay {
		currentEntries := 0
		for key := range g.usageCache {
			if key.agent != "" && key.startUnixNano != currentDay {
				delete(g.usageCache, key)
			} else if key.agent != "" {
				currentEntries++
			}
		}
		g.agentCacheEntries = currentEntries
	}
	g.agentCacheDay = currentDay
	currentAgentKey := usageCacheKey{startUnixNano: currentDay, agent: agent}
	if agent != "" && !agentCapSet {
		if _, exists := g.usageCache[currentAgentKey]; exists {
			delete(g.usageCache, currentAgentKey)
			g.agentCacheEntries = max(0, g.agentCacheEntries-1)
		}
	}
}

func (g *Guard) boundAgentUsageCacheLocked(keep map[usageCacheKey]struct{}) {
	if g.agentCacheEntries <= maxAgentUsageCacheEntries {
		return
	}
	for key := range g.usageCache {
		if key.agent != "" {
			if _, retained := keep[key]; !retained {
				delete(g.usageCache, key)
			}
		}
	}
	g.agentCacheEntries = 0
	for key := range keep {
		if _, exists := g.usageCache[key]; exists {
			g.agentCacheEntries++
		}
	}
}

// cachedUsagesLocked scans each previously unseen cutoff/agent once for a
// stable Store revision. Warm admissions are map lookups; direct inserts and
// pruning through this same Store change the revision and force a fresh
// durable snapshot. Production's serve lease supplies the one-writer contract.
func (g *Guard) cachedUsagesLocked(requests []usageRequest) ([]store.BudgetUsage, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		revision := g.S.RequestRevision()
		if revision%2 != 0 {
			return nil, fmt.Errorf("request ledger mutation is in progress; admission failed closed")
		}
		if g.usageCache == nil || g.cacheRevision != revision {
			g.invalidateUsageCacheLocked(revision)
		}

		missingGlobal := make([]time.Time, 0, len(requests))
		missingGlobalKeys := make([]usageCacheKey, 0, len(requests))
		missingAgents := make([]usageRequest, 0, 1)
		seen := make(map[usageCacheKey]bool, len(requests))
		for _, request := range requests {
			key := usageCacheKey{startUnixNano: request.start.UnixNano(), agent: request.agent}
			if _, ok := g.usageCache[key]; ok || seen[key] {
				continue
			}
			seen[key] = true
			if request.agent == "" {
				missingGlobal = append(missingGlobal, request.start)
				missingGlobalKeys = append(missingGlobalKeys, key)
			} else {
				missingAgents = append(missingAgents, request)
			}
		}

		loaded := make(map[usageCacheKey]store.BudgetUsage, len(missingGlobal)+len(missingAgents))
		if len(missingGlobal) > 0 {
			usages, err := g.S.BudgetUsageSinceMulti(missingGlobal)
			if err != nil {
				return nil, err
			}
			if err := validateFuseUsages(usages); err != nil {
				return nil, err
			}
			for i, usage := range usages {
				loaded[missingGlobalKeys[i]] = usage
			}
		}
		for _, request := range missingAgents {
			usage, err := g.S.BudgetUsageSinceForAgent(request.start, request.agent)
			if err != nil {
				return nil, err
			}
			if err := validateFuseUsages([]store.BudgetUsage{usage}); err != nil {
				return nil, err
			}
			loaded[usageCacheKey{startUnixNano: request.start.UnixNano(), agent: request.agent}] = usage
		}
		if current := g.S.RequestRevision(); current != revision || current%2 != 0 {
			g.invalidateUsageCacheLocked(current)
			continue
		}
		addedAgent := false
		var keepAgents map[usageCacheKey]struct{}
		for key, usage := range loaded {
			if _, exists := g.usageCache[key]; !exists && key.agent != "" {
				g.agentCacheEntries++
				addedAgent = true
				if keepAgents == nil {
					keepAgents = make(map[usageCacheKey]struct{}, len(missingAgents))
					for _, request := range requests {
						if request.agent != "" {
							keepAgents[usageCacheKey{startUnixNano: request.start.UnixNano(), agent: request.agent}] = struct{}{}
						}
					}
				}
			}
			g.usageCache[key] = usage
		}
		if addedAgent {
			g.boundAgentUsageCacheLocked(keepAgents)
		}
		out := make([]store.BudgetUsage, len(requests))
		for i, request := range requests {
			out[i] = g.usageCache[usageCacheKey{startUnixNano: request.start.UnixNano(), agent: request.agent}]
		}
		return out, nil
	}
	return nil, fmt.Errorf("request ledger changed repeatedly while loading budget state; admission failed closed")
}

func (s admissionState) active() bool { return len(s.positions) > 0 }

func (s admissionState) firstCrossed(amount float64) (capPosition, bool) {
	for _, p := range s.positions {
		if amount > p.remaining()+1e-12 {
			return p, true
		}
	}
	return capPosition{}, false
}

func (s admissionState) maxRemaining() float64 {
	var out float64
	for _, p := range s.positions {
		out = max(out, p.remaining())
	}
	return max(0, out)
}

func (g *Guard) checkLocked(now time.Time, agent string) (*Denial, admissionState, error) {
	state := admissionState{}
	keys := []string{KeyBanActive, KeyExternalBanActive, KeyOverrideDay, KeyExternalPolicyExceptions}
	agentKey := ""
	if agent != "" {
		agentKey = KeyAgentCapPrefix + agent
		keys = append(keys, agentKey)
	}
	for _, w := range Windows() {
		keys = append(keys, w.Key, w.ExternalKey)
	}
	keys = append(keys, fuseSettingKeys()...)
	vals, err := g.S.GetSettings(keys...)
	if err != nil {
		return nil, state, err
	}
	expiredIncreases, err := expiredExternalIncreases(vals[KeyExternalPolicyExceptions], now)
	if err != nil {
		return nil, state, err
	}

	if vals[KeyBanActive] == "1" {
		return &Denial{
			Type:    "burnban_banned",
			Message: "burn ban in effect: all agent spend is paused. Lift it with `burnban lift`.",
		}, state, nil
	}
	if vals[KeyExternalBanActive] == "1" {
		return &Denial{
			Type:    "burnban_external_ban",
			Message: "external burn ban in effect: spend is paused by a locally configured external policy. Contact the policy owner.",
		}, state, nil
	}
	trip, err := parseFuseTrip(vals[KeyFuseTrip])
	if err != nil {
		return nil, state, err
	}
	if trip != nil && now.Before(trip.Until) {
		return trip.denial(now), state, nil
	}
	overrideLocal := vals[KeyOverrideDay] == now.Format("2006-01-02")
	fuseRules, fuseCooldown, err := fuseRulesFromSettings(vals)
	if err != nil {
		return nil, state, err
	}
	baselinePolicy, err := ParseFuseBaseline(vals[KeyFuseBaseline])
	if err != nil {
		return nil, state, err
	}
	var fanoutWindow time.Duration
	var fanoutLimit int64
	if raw := strings.TrimSpace(vals[KeyFuseFanout]); raw != "" {
		fanoutWindow, fanoutLimit, err = ParseFuseFanout(raw)
		if err != nil {
			return nil, state, fmt.Errorf("invalid %s setting %q: %w", KeyFuseFanout, raw, err)
		}
	}

	// Local caps use the machine's calendar; external allocations use UTC so
	// every meter attached to a coordinator evaluates the same window. One scan
	// still covers every configured cutoff.
	type capCheck struct {
		window Window
		cap    float64
		start  time.Time
		source string
	}
	var checks []capCheck
	for _, w := range Windows() {
		local, localSet, err := parseCap(w.Key, vals[w.Key])
		if err != nil {
			return nil, state, err
		}
		if overrideLocal {
			local, localSet = 0, false
		}
		external, externalSet, err := parseCap(w.ExternalKey, vals[w.ExternalKey])
		if err != nil {
			return nil, state, err
		}
		external, externalSet, err = subtractExpiredIncrease(w.Name, external, externalSet, expiredIncreases[w.Name])
		if err != nil {
			return nil, state, err
		}
		if localSet {
			checks = append(checks, capCheck{window: w, cap: local, start: w.Start(now), source: "local"})
		}
		if externalSet {
			checks = append(checks, capCheck{window: w, cap: external, start: externalStart(w, now), source: "external"})
		}
	}
	agentCap, agentCapSet := 0.0, false
	if agentKey != "" && !overrideLocal {
		agentCap, agentCapSet, err = parseCap(agentKey, vals[agentKey])
		if err != nil {
			return nil, state, err
		}
	}
	usageRequests := make([]usageRequest, 0, len(checks)+len(fuseRules)+3)
	for _, check := range checks {
		usageRequests = append(usageRequests, usageRequest{start: check.start})
	}
	for _, rule := range fuseRules {
		usageRequests = append(usageRequests, usageRequest{start: rollingStart(now, rule.window)})
	}
	baselineUsageIndex := -1
	baselineStart := time.Time{}
	if baselinePolicy != nil {
		baselineStart = baselineWindowStart(now, baselinePolicy.Window)
		baselineUsageIndex = len(usageRequests)
		usageRequests = append(usageRequests, usageRequest{start: baselineStart})
	}
	fanoutUsageIndex := -1
	if fanoutLimit != 0 {
		fanoutUsageIndex = len(usageRequests)
		usageRequests = append(usageRequests, usageRequest{start: rollingStart(now, fanoutWindow)})
	}
	globalUsageCount := len(usageRequests)
	if agentCapSet {
		usageRequests = append(usageRequests, usageRequest{start: DayStart(now), agent: agent})
	}
	usages, err := g.cachedUsagesLocked(usageRequests)
	if err != nil {
		return nil, state, err
	}
	g.reconcileUsageCacheLocked(usageRequests[:globalUsageCount], agent, agentCapSet, DayStart(now))
	if len(checks) > 0 {
		for i, check := range checks {
			position := capPosition{
				name: check.window.Name, cap: check.cap, spent: usages[i].SpentUSD,
				reserved: g.reservedUSD, unsafe: usages[i].EnforcementGaps,
				window: check.window, start: check.start, source: check.source,
			}
			state.positions = append(state.positions, position)
			if position.unsafe > 0 {
				return &Denial{
					Type: "burnban_metering_gap",
					Message: fmt.Sprintf(
						"%s cap enforcement is paused fail-closed: %d successful request(s) in this window had incomplete usage or unknown pricing. Correct pricing/accounting or override/remove the local cap.",
						check.window.Name, position.unsafe),
				}, state, nil
			}
			if position.spent >= check.cap {
				spentText := fmt.Sprintf("$%.2f spent", position.spent)
				if check.source == "external" {
					return &Denial{
						Type: "burnban_external_cap_reached",
						Message: fmt.Sprintf(
							"external %s burn allocation reached: %s of $%.2f (resets on the UTC boundary). Contact the policy owner.",
							check.window.Name, spentText, check.cap),
						Window: check.window.Name, WindowStart: check.start,
					}, state, nil
				}
				return &Denial{
					Type: "burnban_cap_reached",
					Message: fmt.Sprintf(
						"%s burn cap reached: %s of $%.2f (resets %s). Raise it (`burnban cap --%s %.2f`) or override for today (`burnban lift --today`).",
						check.window.Name, spentText, check.cap, check.window.Reset, check.window.Name, check.cap*2),
					Window:      check.window.Name,
					WindowStart: check.start,
				}, state, nil
			}
			if position.spent+position.reserved >= check.cap {
				// Reservations are temporary and may settle far below their
				// conservative bound. Refuse the concurrent call without firing a
				// durable "cap reached" webhook for spend that has not happened.
				return &Denial{
					Type: "burnban_inflight_headroom",
					Message: fmt.Sprintf(
						"the %s cap's remaining headroom is reserved by in-flight work ($%.2f spent + $%.4f reserved of $%.2f); retry after it completes or lower max tokens.",
						check.window.Name, position.spent, position.reserved, check.cap),
				}, state, nil
			}
		}
	}
	for i, rule := range fuseRules {
		usage := usages[len(checks)+i]
		position := capPosition{
			name: "rolling " + FormatFuseDuration(rule.window) + " " + rule.name,
			cap:  rule.capUSD, spent: usage.SpentUSD, reserved: g.reservedUSD,
			unsafe: usage.EnforcementGaps, fuseName: rule.name,
			fuseWindow: rule.window, fuseCooldown: fuseCooldown,
		}
		state.positions = append(state.positions, position)
		if position.unsafe > 0 {
			return &Denial{
				Type: "burnban_metering_gap",
				Message: fmt.Sprintf(
					"the rolling %s spend-velocity fuse is paused fail-closed: %d successful request(s) in this window had incomplete usage or unknown pricing. Correct pricing/accounting or remove the fuse.",
					FormatFuseDuration(rule.window), position.unsafe),
			}, state, nil
		}
		if position.spent+position.reserved >= position.cap {
			denial, err := g.tripFuseLocked(now, position, 0)
			return denial, state, err
		}
	}
	if baselinePolicy != nil {
		usage := usages[baselineUsageIndex]
		median, err := g.baselineMedianLocked(vals[KeyFuseBaseline], *baselinePolicy, baselineStart)
		if err != nil {
			return nil, state, err
		}
		limit := max(baselinePolicy.MinimumUSD, median*baselinePolicy.Multiplier)
		position := capPosition{
			name: "same-slot baseline", cap: limit, spent: usage.SpentUSD,
			reserved: g.reservedUSD, unsafe: usage.EnforcementGaps,
			fuseName: "baseline", fuseWindow: baselinePolicy.Window, fuseCooldown: fuseCooldown,
		}
		state.positions = append(state.positions, position)
		if position.unsafe > 0 {
			return &Denial{
				Type: "burnban_metering_gap",
				Message: fmt.Sprintf(
					"the %s baseline spend fuse is paused fail-closed: %d successful request(s) in the current slot had incomplete usage or unknown pricing. Correct pricing/accounting or remove the fuse.",
					FormatFuseDuration(baselinePolicy.Window), position.unsafe),
			}, state, nil
		}
		if position.spent+position.reserved >= position.cap {
			denial, err := g.tripFuseLocked(now, position, 0)
			return denial, state, err
		}
	}
	if fanoutLimit != 0 {
		usage := usages[fanoutUsageIndex]
		current := usage.Requests + int64(g.inFlight)
		if current >= fanoutLimit {
			denial, err := g.tripFuseRequestsLocked(now, fanoutWindow, fuseCooldown, fanoutLimit, current+1)
			return denial, state, err
		}
	}

	if !agentCapSet {
		return nil, state, nil
	}
	usage := usages[globalUsageCount]
	position := capPosition{
		name: "daily agent", cap: agentCap, spent: usage.SpentUSD,
		reserved: g.reservedByAgent[agent], unsafe: usage.EnforcementGaps,
		start: DayStart(now), source: "local", agent: agent,
	}
	state.positions = append(state.positions, position)
	if position.unsafe > 0 {
		return &Denial{
			Type: "burnban_metering_gap",
			Message: fmt.Sprintf(
				"daily cap for agent %q is paused fail-closed: %d successful request(s) had incomplete usage or unknown pricing.", agent, position.unsafe),
		}, state, nil
	}
	if position.spent >= agentCap {
		return &Denial{
			Type: "burnban_agent_cap_reached",
			Message: fmt.Sprintf(
				"daily cap for agent %q reached: $%.2f spent of $%.2f. Raise it: `burnban cap --agent AGENT_NAME --daily %.2f`.",
				agent, position.spent, agentCap, agentCap*2),
		}, state, nil
	}
	if position.spent+position.reserved >= agentCap {
		return &Denial{
			Type: "burnban_inflight_headroom",
			Message: fmt.Sprintf(
				"the daily cap for agent %q is reserved by in-flight work ($%.2f spent + $%.4f reserved of $%.2f); retry after it completes or lower max tokens.",
				agent, position.spent, position.reserved, agentCap),
		}, state, nil
	}
	return nil, state, nil
}

type settingsReader interface {
	GetSettings(keys ...string) (map[string]string, error)
}

// BanStatus reports the independently managed local and external ban states.
func BanStatus(s settingsReader) (local, external bool, err error) {
	vals, err := s.GetSettings(KeyBanActive, KeyExternalBanActive)
	if err != nil {
		return false, false, err
	}
	return vals[KeyBanActive] == "1", vals[KeyExternalBanActive] == "1", nil
}

func selectEffectiveState(st *WindowState, local float64, localSet bool, external float64, externalSet bool, localStart, externalStartAt time.Time) {
	switch {
	case localSet && externalSet:
		localPct := st.LocalSpent / local
		externalPct := st.ExternalSpent / external
		switch {
		case localPct > externalPct:
			st.CapUSD, st.Spent, st.StartAt, st.Source = local, st.LocalSpent, localStart, "local"
		case externalPct > localPct:
			st.CapUSD, st.Spent, st.StartAt, st.Source = external, st.ExternalSpent, externalStartAt, "external"
		default:
			if local <= external {
				st.CapUSD, st.Spent, st.StartAt = local, st.LocalSpent, localStart
			} else {
				st.CapUSD, st.Spent, st.StartAt = external, st.ExternalSpent, externalStartAt
			}
			st.Source = "both"
		}
		st.Set = true
	case localSet:
		st.CapUSD, st.Spent, st.StartAt, st.Source, st.Set = local, st.LocalSpent, localStart, "local", true
	case externalSet:
		st.CapUSD, st.Spent, st.StartAt, st.Source, st.Set = external, st.ExternalSpent, externalStartAt, "external", true
	}
}

func externalStart(w Window, now time.Time) time.Time {
	utc := now.UTC()
	switch w.Name {
	case "daily":
		return DayStart(utc)
	case "weekly":
		return WeekStart(utc)
	default:
		return MonthStart(utc)
	}
}

// Warning reports a window at or past the early-warning threshold.
type Warning struct {
	Window     string
	Pct        float64
	Spent, Cap float64
	MarkKey    string // settings key that dedups this window instance
	Reset      string
}

// WarnStatus returns the most-burned window at or past the warning
// threshold that has not been warned about yet, or nil. The caller owns
// setting MarkKey and delivering the message — the guard only decides.
func (g *Guard) WarnStatus(now time.Time) (*Warning, error) {
	states, err := Status(g.S, now)
	if err != nil {
		return nil, err
	}
	markKeys := make([]string, 0, len(states))
	for _, st := range states {
		markKeys = append(markKeys, KeyWarnedPrefix+st.Name+":"+st.StartAt.Format("2006-01-02"))
	}
	vals, err := g.S.GetSettings(append(markKeys, KeyWarnPct)...)
	if err != nil {
		return nil, err
	}

	threshold := DefaultWarnPct
	if pctStr := vals[KeyWarnPct]; pctStr != "" {
		v, perr := strconv.ParseFloat(pctStr, 64)
		if perr != nil || math.IsNaN(v) || math.IsInf(v, 0) || v > 100 {
			return nil, fmt.Errorf("invalid %s setting %q", KeyWarnPct, pctStr)
		}
		threshold = v
	}
	if threshold <= 0 {
		return nil, nil
	}

	var worst *Warning
	for i, st := range states {
		if !st.Set || st.Pct() < threshold || vals[markKeys[i]] == "1" {
			continue
		}
		if worst == nil || st.Pct() > worst.Pct {
			worst = &Warning{
				Window: st.Name, Pct: st.Pct(), Spent: st.Spent, Cap: st.CapUSD,
				MarkKey: markKeys[i], Reset: st.Reset,
			}
		}
	}
	return worst, nil
}

// ClearMarks forgets a window's sent warning and alert, re-arming both.
// Called whenever that window's cap is set, raised, or removed.
func ClearMarks(s *store.Store, window string) error {
	if err := s.DeleteSettingsWithPrefix(KeyWarnedPrefix + window + ":"); err != nil {
		return err
	}
	return s.DeleteSettingsWithPrefix(KeyAlertedPrefix + window + ":")
}

// parseCap interprets a stored cap. Values ≤ 0 are treated as unset rather
// than as a cap that denies everything: a zero cap is never what a user
// meant, and it would otherwise poison percentage math downstream.
func parseCap(key, s string) (float64, bool, error) {
	if s == "" {
		return 0, false, nil
	}
	v, perr := strconv.ParseFloat(s, 64)
	if perr != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false, fmt.Errorf("invalid %s setting %q", key, s)
	}
	if v <= 0 {
		return 0, false, nil
	}
	return v, true, nil
}
