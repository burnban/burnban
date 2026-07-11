// Package proxy is the request path: a pass-through HTTP proxy that meters
// every inference call, prices it, and refuses to forward when the budget
// guard says spend is paused. Bytes are forwarded unmodified — burnban
// observes traffic, it never rewrites it.
package proxy

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/meter"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

// maxBodyBytes caps buffered bodies; prompts and non-streamed replies fit
// comfortably, and streams are never buffered at all.
const maxBodyBytes = 32 << 20

const maxMeterLineBytes = 4 << 20

// Upstream is one forwarding target. Shape names the usage dialect its
// responses speak — "anthropic", "gemini", or "openai" (the default and the
// de-facto standard that Groq, Mistral, DeepSeek, OpenRouter, Ollama, vLLM
// and friends all emit) — so metering follows the wire format, not the
// route name.
type Upstream struct {
	URL   string
	Shape string
}

type parsedUpstream struct {
	url   *url.URL
	shape string
}

type Proxy struct {
	Store  *store.Store
	Prices *pricing.Table
	Guard  *budget.Guard
	Logf   func(format string, v ...any)

	upstreams map[string]parsedUpstream
	client    *http.Client
	webhooks  *http.Client

	fingerprintKey []byte
	warnPending    atomic.Bool
	alertMu        sync.Mutex
	alertsInFlight map[string]bool

	healthMu       sync.RWMutex
	probeMu        sync.Mutex
	persistenceErr string
	lastFailure    time.Time
	lastProbe      time.Time
}

// HealthSnapshot is the proxy's runtime safety state. Persistence failures
// latch fail-closed; in-flight reservation fields make bounded overshoot
// visible to health/doctor/dashboard consumers.
type HealthSnapshot struct {
	Service       string     `json:"service"`
	OK            bool       `json:"ok"`
	State         string     `json:"state"`
	Detail        string     `json:"detail,omitempty"`
	PersistenceOK bool       `json:"persistence_ok"`
	InFlight      int        `json:"in_flight"`
	ReservedUSD   float64    `json:"reserved_usd"`
	LastFailure   *time.Time `json:"last_failure,omitempty"`
}

// Shapes lists the usage dialects the meter can parse.
func Shapes() []string { return []string{"openai", "anthropic", "gemini"} }

func validShape(s string) bool {
	for _, v := range Shapes() {
		if s == v {
			return true
		}
	}
	return false
}

func New(s *store.Store, t *pricing.Table, upstreams map[string]Upstream) (*Proxy, error) {
	us := make(map[string]parsedUpstream, len(upstreams))
	for name, up := range upstreams {
		u, err := url.Parse(up.URL)
		if err != nil {
			return nil, fmt.Errorf("upstream %s: %w", name, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("upstream %s: URL must use http or https and include a host", name)
		}
		shape := up.Shape
		if shape == "" {
			shape = "openai"
		}
		if !validShape(shape) {
			return nil, fmt.Errorf("upstream %s: unknown shape %q (want openai, anthropic, or gemini)", name, shape)
		}
		us[name] = parsedUpstream{url: u, shape: shape}
	}
	fingerprintKey, err := loadFingerprintKey(s)
	if err != nil {
		return nil, fmt.Errorf("fingerprint key: %w", err)
	}
	transport := cloneDefaultTransport()
	// Inference endpoints may legitimately take several minutes before their
	// first response header. Request cancellation still bounds abandoned calls.
	transport.ResponseHeaderTimeout = 0
	transport.MaxIdleConnsPerHost = 32
	p := &Proxy{
		Store:          s,
		Prices:         t,
		Guard:          &budget.Guard{S: s},
		Logf:           log.Printf,
		upstreams:      us,
		fingerprintKey: fingerprintKey,
		alertsInFlight: map[string]bool{},
		client: &http.Client{
			// A proxy must relay redirects, not follow them with credentials.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     transport,
		},
		webhooks: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	return p, nil
}

func cloneDefaultTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
}

const fingerprintKeySetting = "_fingerprint_key"

func loadFingerprintKey(s *store.Store) ([]byte, error) {
	value, err := s.GetSetting(fingerprintKeySetting)
	if err != nil {
		return nil, err
	}
	if value != "" {
		key, err := hex.DecodeString(value)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("stored key is invalid")
		}
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := s.SetSetting(fingerprintKeySetting, hex.EncodeToString(key)); err != nil {
		return nil, err
	}
	return key, nil
}

// DefaultUpstreams maps URL path prefixes to provider APIs. Each can be
// overridden by env var, which is also how tests point burnban at fakes.
func DefaultUpstreams() map[string]Upstream {
	return map[string]Upstream{
		"anthropic":  {envOr("BURNBAN_ANTHROPIC_UPSTREAM", "https://api.anthropic.com"), "anthropic"},
		"openai":     {envOr("BURNBAN_OPENAI_UPSTREAM", "https://api.openai.com"), "openai"},
		"xai":        {envOr("BURNBAN_XAI_UPSTREAM", "https://api.x.ai"), "openai"},
		"gemini":     {envOr("BURNBAN_GEMINI_UPSTREAM", "https://generativelanguage.googleapis.com"), "gemini"},
		"openrouter": {envOr("BURNBAN_OPENROUTER_UPSTREAM", "https://openrouter.ai/api"), "openai"},
		"groq":       {envOr("BURNBAN_GROQ_UPSTREAM", "https://api.groq.com/openai"), "openai"},
		"mistral":    {envOr("BURNBAN_MISTRAL_UPSTREAM", "https://api.mistral.ai"), "openai"},
		"deepseek":   {envOr("BURNBAN_DEEPSEEK_UPSTREAM", "https://api.deepseek.com"), "openai"},
		"ollama":     {envOr("BURNBAN_OLLAMA_UPSTREAM", "http://127.0.0.1:11434"), "openai"},
		"vllm":       {envOr("BURNBAN_VLLM_UPSTREAM", "http://127.0.0.1:8000"), "openai"},
	}
}

func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		health := p.Health()
		if !health.OK {
			health = p.ProbeHealth()
		}
		w.Header().Set("Content-Type", "application/json")
		if !health.OK {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(health)
	})
	for name := range p.upstreams {
		name := name
		mux.Handle("/"+name+"/", http.StripPrefix("/"+name,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p.forward(w, r, name)
			})))
	}
	return mux
}

// Health returns a non-mutating snapshot of the proxy's persistence and
// admission state.
func (p *Proxy) Health() HealthSnapshot {
	p.healthMu.RLock()
	detail, lastFailure := p.persistenceErr, p.lastFailure
	p.healthMu.RUnlock()
	reservations := p.Guard.Reservations()
	ok := detail == ""
	state := "healthy"
	if !ok {
		state = "fail_closed"
	}
	var failure *time.Time
	if !lastFailure.IsZero() {
		value := lastFailure
		failure = &value
	}
	return HealthSnapshot{
		Service: "burnban", OK: ok, State: state, Detail: detail, PersistenceOK: ok,
		InFlight: reservations.InFlight, ReservedUSD: reservations.ReservedUSD,
		LastFailure: failure,
	}
}

// ProbeHealth performs a durable store write and clears a persistence latch
// only after that write commits successfully.
func (p *Proxy) ProbeHealth() HealthSnapshot {
	return p.probeHealth(true)
}

func (p *Proxy) probeHealth(force bool) HealthSnapshot {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	if !force {
		p.healthMu.RLock()
		detail, lastProbe := p.persistenceErr, p.lastProbe
		p.healthMu.RUnlock()
		if detail == "" || time.Since(lastProbe) < time.Second {
			return p.Health()
		}
	}
	p.healthMu.Lock()
	p.lastProbe = time.Now()
	p.healthMu.Unlock()
	err := p.Store.Probe()
	p.healthMu.Lock()
	if err == nil {
		p.persistenceErr = ""
	} else {
		p.persistenceErr = err.Error()
		p.lastFailure = time.Now()
	}
	p.healthMu.Unlock()
	return p.Health()
}

func (p *Proxy) markPersistenceFailure(err error) {
	if err == nil {
		return
	}
	p.healthMu.Lock()
	p.persistenceErr = err.Error()
	p.lastFailure = time.Now()
	p.healthMu.Unlock()
}

func (p *Proxy) ensurePersistence() error {
	p.healthMu.RLock()
	detail, lastProbe := p.persistenceErr, p.lastProbe
	p.healthMu.RUnlock()
	if detail == "" {
		return nil
	}
	// Bound durable recovery probes under a request storm. /health can still
	// trigger an immediate explicit probe.
	if time.Since(lastProbe) >= time.Second {
		if health := p.probeHealth(false); health.OK {
			return nil
		}
		detail = p.Health().Detail
	}
	return fmt.Errorf("ledger persistence unavailable; proxy is fail-closed: %s", detail)
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, provider string) {
	start := time.Now()
	agent := agentFrom(r)
	session := r.Header.Get("x-burnban-session")
	shape := p.upstreams[provider].shape

	if r.Method == http.MethodPost {
		if err := p.ensurePersistence(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}

	var reqBody []byte
	if r.Body != nil {
		// Read one byte past the cap so truncation is detectable: a body
		// we couldn't hold intact must be refused, never forwarded corrupt.
		b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			http.Error(w, "reading request body: "+err.Error(), http.StatusBadGateway)
			return
		}
		if len(b) > maxBodyBytes {
			http.Error(w, fmt.Sprintf("request body exceeds burnban's %dMB buffer", maxBodyBytes>>20),
				http.StatusRequestEntityTooLarge)
			return
		}
		reqBody = b
	}

	requestInfo := p.estimateRequest(r.URL.Path, reqBody)
	var reservation *budget.Reservation
	if r.Method == http.MethodPost {
		var denial *budget.Denial
		var err error
		reservation, denial, err = p.Guard.Admit(start, agent, requestInfo.admission)
		if err != nil {
			// Distinguish an invalid cap/configuration value from an unavailable
			// ledger. A failed durable probe latches health and turns the proxy
			// explicitly fail-closed before any upstream request is sent.
			if probeErr := p.Store.Probe(); probeErr != nil {
				p.markPersistenceFailure(probeErr)
				http.Error(w, "ledger persistence unavailable; proxy is fail-closed", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if denial != nil {
			p.alertCapReached(denial)
			writeDenial(w, denial)
			return
		}
		defer reservation.Release()
	}

	up := p.upstreams[provider].url
	outURL := *up
	outURL.Path = strings.TrimRight(up.Path, "/") + r.URL.Path
	switch {
	case up.RawQuery == "":
		outURL.RawQuery = r.URL.RawQuery
	case r.URL.RawQuery == "":
		outURL.RawQuery = up.RawQuery
	default:
		outURL.RawQuery = up.RawQuery + "&" + r.URL.RawQuery
	}

	out, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(out.Header, r.Header)
	rec := store.Request{
		Ts: start, Provider: provider, Agent: agent,
		Session: session, Route: r.URL.EscapedPath(),
	}
	if r.Method == http.MethodPost {
		rec.BodyHash = p.fingerprint(provider, r.Method, r.URL.EscapedPath(), r.URL.Query().Encode(), agent, session, start, reqBody)
	}

	resp, err := p.client.Do(out)
	if err != nil {
		// Once a request may have reached the provider, a transport failure is
		// an accounting ambiguity: the provider might have completed and billed
		// work even though no response usage reached Burnban. Persist that gap so
		// an active dollar cap fails closed instead of silently undercounting it.
		if r.Method == http.MethodPost {
			rec.LatencyMs = time.Since(start).Milliseconds()
			rec.UsageState = store.UsageMissing
			rec.PricingState = store.PricingUnmetered
			rec.Incomplete = true
			rec.EnforcementUnsafe = reservation != nil && reservation.CapActive()
			if insertErr := p.Store.Insert(rec); insertErr != nil {
				p.markPersistenceFailure(insertErr)
				p.Logf("burnban: store after upstream transport failure: %v", insertErr)
				http.Error(w, "ledger persistence unavailable; proxy is fail-closed", http.StatusServiceUnavailable)
				return
			}
		}
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	rec.Status = resp.StatusCode

	isSSE := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")

	responseHeaders := resp.Header.Clone()
	stripHopHeaders(responseHeaders)
	responseHeaders.Del("Content-Length")
	if resp.Uncompressed {
		responseHeaders.Del("Content-Encoding")
	}
	for k, vv := range responseHeaders {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	var usage meter.Usage
	responseIncomplete := false
	if isSSE {
		rec.Streamed = true
		streamed := p.streamThrough(w, resp.Body, shape)
		usage = streamed.Usage
		responseIncomplete = streamed.ReadErr != nil || (streamed.TrackingLimited && !streamed.Exact)
		if streamed.ReadErr != nil {
			p.Logf("burnban: streaming upstream body: %v", streamed.ReadErr)
		}
	} else {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
		if readErr != nil {
			responseIncomplete = true
			p.Logf("burnban: reading upstream body: %v", readErr)
		}
		_, writeErr := w.Write(body)
		if len(body) > maxBodyBytes {
			// Large non-streamed responses still pass through in full. They are
			// not parsed because keeping an unbounded copy would be a memory DoS.
			if writeErr == nil {
				if _, err := io.Copy(w, resp.Body); err != nil {
					responseIncomplete = true
					p.Logf("burnban: forwarding oversized upstream body: %v", err)
				}
			} else {
				responseIncomplete = true
			}
			p.Logf("burnban: upstream response exceeded %dMB; forwarded without metering", maxBodyBytes>>20)
		} else {
			usage = parseJSON(shape, body)
		}
	}

	rec.LatencyMs = time.Since(start).Milliseconds()
	if responseIncomplete {
		usage.Incomplete = true
		usage.Exact = false
		usage.Estimated = true
	}
	if resp.StatusCode < 300 {
		// A successful response without a provider usage frame still needs an
		// honest row. Request-side counts are estimates, never presented as exact.
		if !usage.Found {
			usage.Found = requestInfo.model != ""
			usage.Model = requestInfo.model
			usage.In = requestInfo.inputEstimate
			usage.Estimated = true
			usage.Incomplete = true
		} else {
			if usage.Model == "" {
				usage.Model = requestInfo.model
			}
			if !usage.Exact {
				if usage.In == 0 {
					usage.In = requestInfo.inputEstimate
				}
				usage.Estimated = true
				usage.Incomplete = true
			}
		}
	}
	if usage.Found {
		if usage.ServiceTier == "" {
			usage.ServiceTier = requestInfo.serviceTier
		}
		if usage.InferenceGeo == "" {
			usage.InferenceGeo = requestInfo.inferenceGeo
		}
		_, geoKnown := inferenceGeoPriceMultiplier(usage.InferenceGeo)
		usage.FeeUnknown = usage.FeeUnknown || serviceTierFeeUnpriced(usage.ServiceTier) || !geoKnown
		rec.Model = usage.Model
		rec.InTokens, rec.OutTokens = usage.In, usage.Out
		rec.CacheReadTokens, rec.CacheWriteTokens = usage.CacheRead, usage.CacheWrite
		rec.CacheWrite1hTokens = usage.CacheWrite1h
		rec.ServiceTier, rec.InferenceGeo = usage.ServiceTier, usage.InferenceGeo
		rec.ServerToolCalls, rec.FeeUnpriced = usage.ServerToolCalls, usage.FeeUnknown
		switch {
		case usage.Incomplete:
			rec.UsageState = store.UsagePartial
		case usage.Estimated:
			rec.UsageState = store.UsageEstimated
		default:
			rec.UsageState = store.UsageExact
		}
		rec.Incomplete = usage.Incomplete
		if price, ok := p.Prices.Lookup(usage.Model); ok {
			rec.CostUSD = costUsage(price, usage)
			rec.PricingState = store.PricingPriced
		} else {
			rec.PricingState = store.PricingUnknown
		}
	} else {
		rec.UsageState = store.UsageMissing
		rec.PricingState = store.PricingUnmetered
	}
	// Failed provider responses usually carry no billable usage. When a failed
	// response does include usage, however, treat it as billing evidence and
	// require complete known pricing just like a success.
	unsafeAccounting := false
	if resp.StatusCode < 300 {
		unsafeAccounting = rec.UsageState == store.UsageMissing || rec.UsageState == store.UsagePartial ||
			rec.PricingState != store.PricingPriced || rec.FeeUnpriced
	} else if rec.UsageState != store.UsageMissing {
		unsafeAccounting = rec.UsageState == store.UsagePartial ||
			rec.PricingState != store.PricingPriced || rec.FeeUnpriced
	}
	rec.EnforcementUnsafe = reservation != nil && reservation.CapActive() && unsafeAccounting
	if r.Method == http.MethodPost {
		if err := p.Store.Insert(rec); err != nil {
			p.markPersistenceFailure(err)
			p.Logf("burnban: store: %v", err)
			return
		}
		p.scheduleWarn(time.Now())
	}
}

func costUsage(price pricing.Price, usage meter.Usage) float64 {
	cost := pricing.Cost(price, usage.In, usage.Out, usage.CacheRead, usage.CacheWrite)
	oneHour := min(max(usage.CacheWrite1h, 0), max(usage.CacheWrite, 0))
	if oneHour > 0 {
		inputMult := 1.0
		totalInput := usage.In + usage.CacheRead + usage.CacheWrite
		if price.LongContextThreshold > 0 && totalInput > price.LongContextThreshold && price.LongInputMult > 0 {
			inputMult = price.LongInputMult
		}
		// Anthropic's 1-hour write tier is 2x input. pricing.Cost applied the
		// table's ordinary write multiplier to every write, so adjust this subset.
		cost += float64(oneHour) * price.InputPerMTok * (2 - price.CacheWriteMult) * inputMult / 1e6
	}
	geoMultiplier, _ := inferenceGeoPriceMultiplier(usage.InferenceGeo)
	return max(0, cost) * geoMultiplier
}

func serviceTierFeeUnpriced(tier string) bool {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "", "default", "standard", "standard_only":
		return false
	default:
		return true
	}
}

// US-only Anthropic inference carries a documented 10% token-price premium.
// Unknown future geo values remain unpriced so capped operation fails closed.
func inferenceGeoPriceMultiplier(geo string) (multiplier float64, known bool) {
	switch strings.ToLower(strings.TrimSpace(geo)) {
	case "", "global":
		return 1, true
	case "us":
		return 1.1, true
	default:
		return 1, false
	}
}

type requestEstimate struct {
	model         string
	inputEstimate int64
	serviceTier   string
	inferenceGeo  string
	admission     budget.AdmissionEstimate
}

func (p *Proxy) estimateRequest(path string, body []byte) requestEstimate {
	var request struct {
		Model               string `json:"model"`
		MaxTokens           int64  `json:"max_tokens"`
		MaxOutputTokens     int64  `json:"max_output_tokens"`
		MaxCompletionTokens int64  `json:"max_completion_tokens"`
		ServiceTier         string `json:"service_tier"`
		InferenceGeo        string `json:"inference_geo"`
		GenerationConfig    struct {
			MaxOutputTokens int64 `json:"maxOutputTokens"`
		} `json:"generationConfig"`
		Tools []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &request)
	model := request.Model
	if model == "" {
		model = modelFromPath(path)
	}
	maxOutput := max(request.MaxTokens, request.MaxOutputTokens, request.MaxCompletionTokens, request.GenerationConfig.MaxOutputTokens)
	inputEstimate := int64((len(body) + 3) / 4)
	info := requestEstimate{
		model: model, inputEstimate: inputEstimate,
		serviceTier: request.ServiceTier, inferenceGeo: request.InferenceGeo,
	}
	_, geoKnown := inferenceGeoPriceMultiplier(request.InferenceGeo)
	providerFeesUnbounded := hasUnboundedProviderTools(request.Tools) ||
		serviceTierFeeUnpriced(request.ServiceTier) || !geoKnown
	info.admission = budget.AdmissionEstimate{
		Bounded:     maxOutput > 0 && !providerFeesUnbounded,
		Description: "request model",
	}
	if model != "" {
		info.admission.Description = fmt.Sprintf("model %q", model)
	}
	price, ok := p.Prices.Lookup(model)
	if !ok {
		return info
	}
	// One token per request byte is a conservative input upper bound for
	// byte-backed tokenizers. Cache-write pricing can exceed ordinary input,
	// so reserve the more expensive interpretation.
	inputUpper := int64(len(body))
	normal := costUsage(price, meter.Usage{
		In: inputUpper, Out: maxOutput, InferenceGeo: request.InferenceGeo,
	})
	cacheWrite := costUsage(price, meter.Usage{
		Out: maxOutput, CacheWrite: inputUpper, CacheWrite1h: inputUpper,
		InferenceGeo: request.InferenceGeo,
	})
	info.admission.USD = max(normal, cacheWrite)
	info.admission.Priced = true
	return info
}

// hasUnboundedProviderTools recognizes provider-hosted tools whose request,
// container, retrieval, or generated-asset fees are not represented in the
// model token price table. Even when a token ceiling is present, admission
// must be exclusive under an active cap unless those fees can be bounded.
// Ordinary client-executed function tools remain bounded by max output.
func hasUnboundedProviderTools(tools []json.RawMessage) bool {
	for _, raw := range tools {
		var descriptor struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &descriptor) != nil {
			// The upstream may still accept an extension we cannot classify.
			return true
		}
		kind := strings.ToLower(strings.TrimSpace(descriptor.Type))
		for _, needle := range []string{
			"web_search", "web_fetch", "code_execution", "code_interpreter",
			"file_search", "image_generation", "computer_use_preview",
		} {
			if strings.Contains(kind, needle) {
				return true
			}
		}
		if kind == "mcp" || strings.HasPrefix(kind, "mcp_") {
			return true
		}

		// Gemini provider tools use object keys rather than a type discriminator.
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return true
		}
		for _, key := range []string{
			"googleSearch", "googleSearchRetrieval", "googleMaps", "urlContext",
			"codeExecution", "retrieval",
		} {
			if _, ok := fields[key]; ok {
				return true
			}
		}
	}
	return false
}

func modelFromPath(path string) string {
	const marker = "/models/"
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	model := path[i+len(marker):]
	if j := strings.IndexAny(model, ":/"); j >= 0 {
		model = model[:j]
	}
	return model
}

func (p *Proxy) fingerprint(provider, method, path, query, agent, session string, at time.Time, body []byte) string {
	mac := hmac.New(sha256.New, p.fingerprintKey)
	// Five-minute buckets catch accidental retries without labeling an
	// intentional repeated call hours later as waste.
	bucket := at.UTC().Truncate(5 * time.Minute).Format(time.RFC3339)
	for _, part := range []string{provider, method, path, query, agent, session, bucket} {
		_, _ = mac.Write([]byte(part))
		_, _ = mac.Write([]byte{0})
	}
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

type streamResult struct {
	meter.Usage
	ReadErr         error
	ClientGone      bool
	TrackingLimited bool
}

// streamThrough copies SSE fragments immediately while retaining at most a
// bounded line for metering. The outbound request shares the client context,
// so cancellation stops provider spend; any resulting partial accounting is
// marked incomplete rather than presented as exact.
func (p *Proxy) streamThrough(w http.ResponseWriter, body io.Reader, shape string) streamResult {
	var tracker meter.Tracker
	switch shape {
	case "anthropic":
		tracker = &meter.AnthropicSSE{}
	case "gemini":
		tracker = &meter.GeminiSSE{}
	default:
		tracker = &meter.OpenAISSE{}
	}

	flusher, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(body, 64<<10)
	result := streamResult{}
	var meterLine []byte
	lineLimited := false
	for {
		fragment, err := br.ReadSlice('\n')
		if len(fragment) > 0 {
			if !result.ClientGone {
				if _, werr := w.Write(fragment); werr != nil {
					result.ClientGone = true
				} else if flusher != nil {
					flusher.Flush()
				}
			}
			if !lineLimited {
				if len(meterLine)+len(fragment) <= maxMeterLineBytes {
					meterLine = append(meterLine, fragment...)
				} else {
					meterLine = nil
					lineLimited = true
					result.TrackingLimited = true
				}
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if !lineLimited && len(meterLine) > 0 {
			tracker.Feed(meterLine)
		}
		meterLine = meterLine[:0]
		lineLimited = false
		if err != nil {
			if err != io.EOF {
				result.ReadErr = err
			}
			break
		}
	}
	result.Usage = tracker.Usage()
	if (result.ReadErr != nil || result.TrackingLimited) && !result.Exact {
		result.Incomplete = true
		result.Estimated = true
	}
	return result
}

func parseJSON(shape string, body []byte) meter.Usage {
	switch shape {
	case "anthropic":
		return meter.ParseAnthropicJSON(body)
	case "gemini":
		return meter.ParseGeminiJSON(body)
	default:
		return meter.ParseOpenAIJSON(body)
	}
}

// hopHeaders are stripped before forwarding. Accept-Encoding is included
// so Go's transport negotiates compression itself and hands back a body we
// can always parse.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Trailers", "Transfer-Encoding", "Upgrade", "Accept-Encoding",
	// Burnban control and attribution metadata is consumed locally. Forwarding
	// any of it would disclose the gateway token and internal agent names.
	"X-Burnban-Token", "X-Burnban-Agent", "X-Burnban-Session",
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	stripHopHeaders(dst)
}

// stripHopHeaders removes both the fixed HTTP hop-by-hop headers and every
// extension header nominated by Connection. RFC 9110 requires intermediaries
// to consume those fields in each direction rather than forwarding them.
func stripHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				header.Del(token)
			}
		}
	}
	for _, name := range hopHeaders {
		header.Del(name)
	}
}

// agentFrom attributes a request to a client: an explicit x-burnban-agent
// header wins, else the User-Agent product token (e.g. "claude-cli").
func agentFrom(r *http.Request) string {
	if v := r.Header.Get("x-burnban-agent"); v != "" {
		return v
	}
	if v := r.Header.Get("x-client-name"); v != "" {
		return v
	}
	ua := r.Header.Get("User-Agent")
	if ua == "" {
		return "unknown"
	}
	lower := strings.ToLower(ua)
	// SDK user agents often hide the calling app, but popular agent clients
	// that identify themselves should remain stable across version upgrades.
	known := []struct {
		name    string
		needles []string
	}{
		{"claude-code", []string{"claude-code", "claude_cli", "claude-cli"}},
		{"codex", []string{"codex_cli", "codex-cli", "codex/"}},
		{"hermes", []string{"hermes-agent", "hermes_agent", "hermes/"}},
		{"openclaw", []string{"openclaw", "clawdbot", "moltbot"}},
		{"aider", []string{"aider"}},
		{"goose", []string{"goose-ai", "goose/", "goose-cli", "goose-desktop"}},
		{"cline", []string{"cline"}},
		{"roo-code", []string{"roo-code", "roocode", "roo code"}},
		{"continue", []string{"continue.dev", "continue/"}},
		{"cursor", []string{"cursor/", "cursor-agent"}},
		{"windsurf", []string{"windsurf"}},
		{"opencode", []string{"opencode"}},
	}
	for _, client := range known {
		for _, needle := range client.needles {
			if strings.Contains(lower, needle) {
				return client.name
			}
		}
	}
	if i := strings.IndexAny(ua, " ("); i > 0 {
		ua = ua[:i]
	}
	return ua
}

// alertCapReached fires the configured webhook (Slack-compatible JSON) the
// first time each window instance trips — a daily and a weekly cap hitting
// on the same day are two distinct alerts, not one. Fire-and-forget: a
// slow or dead webhook must never sit in the request path.
func (p *Proxy) alertCapReached(d *budget.Denial) {
	mark := d.AlertMark()
	if mark == "" {
		return
	}
	urlStr, err := p.Store.GetSetting(budget.KeyWebhookURL)
	if err != nil || urlStr == "" {
		return
	}
	p.queueWebhook(mark, urlStr, "🔥🚫 burnban: "+d.Message)
}

func (p *Proxy) scheduleWarn(now time.Time) {
	if !p.warnPending.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer p.warnPending.Store(false)
		p.maybeWarn(now)
	}()
}

// maybeWarn posts the early warning when a budget window crosses the warn
// threshold — once per window instance, and only when a webhook is set. It
// runs after the response is already on the wire, never in front of it.
func (p *Proxy) maybeWarn(now time.Time) {
	urlStr, err := p.Store.GetSetting(budget.KeyWebhookURL)
	if err != nil || urlStr == "" {
		return
	}
	warn, err := p.Guard.WarnStatus(now)
	if err != nil {
		p.Logf("burnban: warn check: %v", err)
		return
	}
	if warn == nil {
		return
	}
	p.queueWebhook(warn.MarkKey, urlStr, fmt.Sprintf(
		"⚠️ burnban: %.0f%% of the %s cap burned — $%.2f of $%.2f (resets %s)",
		warn.Pct, warn.Window, warn.Spent, warn.Cap, warn.Reset))
}

func (p *Proxy) queueWebhook(mark, urlStr, message string) {
	if delivered, err := p.Store.GetSetting(mark); err != nil {
		p.Logf("burnban: webhook delivery state: %v", err)
		return
	} else if delivered == "1" {
		return
	}
	p.alertMu.Lock()
	if p.alertsInFlight[mark] {
		p.alertMu.Unlock()
		return
	}
	p.alertsInFlight[mark] = true
	p.alertMu.Unlock()
	go func() {
		defer func() {
			p.alertMu.Lock()
			delete(p.alertsInFlight, mark)
			p.alertMu.Unlock()
		}()
		if err := p.deliverWebhook(urlStr, message); err != nil {
			p.Logf("burnban: webhook %s: %v", redactEndpoint(urlStr), err)
			return
		}
		if err := p.Store.SetSetting(mark, "1"); err != nil {
			p.markPersistenceFailure(err)
			p.Logf("burnban: recording webhook delivery: %v", err)
		}
	}()
}

func (p *Proxy) deliverWebhook(urlStr, message string) error {
	body, _ := json.Marshal(map[string]string{"text": message})
	delays := []time.Duration{0, 250 * time.Millisecond, time.Second}
	var lastErr error
	for _, delay := range delays {
		if delay > 0 {
			time.Sleep(delay)
		}
		resp, err := p.webhooks.Post(urlStr, "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = safeWebhookError(err)
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		closeErr := resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && closeErr == nil {
			return nil
		}
		if closeErr != nil {
			lastErr = closeErr
		} else {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
	return lastErr
}

func safeWebhookError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// url.Error.Error includes the complete webhook URL, whose path/query
		// commonly contains the credential. Preserve the useful transport cause
		// without ever returning the secret-bearing endpoint.
		return fmt.Errorf("%s request failed: %v", urlErr.Op, urlErr.Err)
	}
	return err
}

func redactEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<redacted>"
	}
	return u.Scheme + "://" + u.Host + "/<redacted>"
}

func writeDenial(w http.ResponseWriter, d *budget.Denial) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"type": d.Type, "message": d.Message},
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
