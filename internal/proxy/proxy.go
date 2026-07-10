// Package proxy is the request path: a pass-through HTTP proxy that meters
// every inference call, prices it, and refuses to forward when the budget
// guard says spend is paused. Bytes are forwarded unmodified — burnban
// observes traffic, it never rewrites it.
package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/meter"
	"github.com/syft8/burnban/internal/pricing"
	"github.com/syft8/burnban/internal/store"
)

// maxBodyBytes caps buffered bodies; prompts and non-streamed replies fit
// comfortably, and streams are never buffered at all.
const maxBodyBytes = 32 << 20

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
		shape := up.Shape
		if shape == "" {
			shape = "openai"
		}
		if !validShape(shape) {
			return nil, fmt.Errorf("upstream %s: unknown shape %q (want openai, anthropic, or gemini)", name, shape)
		}
		us[name] = parsedUpstream{url: u, shape: shape}
	}
	return &Proxy{
		Store:     s,
		Prices:    t,
		Guard:     &budget.Guard{S: s},
		Logf:      log.Printf,
		upstreams: us,
		client: &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: 120 * time.Second,
			MaxIdleConnsPerHost:   32,
		}},
	}, nil
}

// DefaultUpstreams maps URL path prefixes to provider APIs. Each can be
// overridden by env var, which is also how tests point burnban at fakes.
func DefaultUpstreams() map[string]Upstream {
	return map[string]Upstream{
		"anthropic": {envOr("BURNBAN_ANTHROPIC_UPSTREAM", "https://api.anthropic.com"), "anthropic"},
		"openai":    {envOr("BURNBAN_OPENAI_UPSTREAM", "https://api.openai.com"), "openai"},
		"xai":       {envOr("BURNBAN_XAI_UPSTREAM", "https://api.x.ai"), "openai"},
		"gemini":    {envOr("BURNBAN_GEMINI_UPSTREAM", "https://generativelanguage.googleapis.com"), "gemini"},
	}
}

func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok":true}`)
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

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, provider string) {
	start := time.Now()
	agent := agentFrom(r)
	shape := p.upstreams[provider].shape

	if r.Method == http.MethodPost {
		denial, err := p.Guard.Check(start, agent)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if denial != nil {
			p.alertCapReached(denial)
			writeDenial(w, denial)
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

	up := p.upstreams[provider].url
	outURL := *up
	outURL.Path = strings.TrimRight(up.Path, "/") + r.URL.Path
	outURL.RawQuery = r.URL.RawQuery

	out, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(out.Header, r.Header)

	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	rec := store.Request{
		Ts:       start,
		Provider: provider,
		Agent:    agent,
		Session:  r.Header.Get("x-burnban-session"),
		Status:   resp.StatusCode,
	}
	if r.Method == http.MethodPost && len(reqBody) > 0 {
		sum := sha256.Sum256(reqBody)
		rec.BodyHash = hex.EncodeToString(sum[:8])
	}

	isSSE := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")

	for k, vv := range resp.Header {
		if k == "Content-Length" {
			continue
		}
		if resp.Uncompressed && k == "Content-Encoding" {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	var usage meter.Usage
	if isSSE {
		rec.Streamed = true
		usage = p.streamThrough(w, resp.Body, shape)
	} else {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		if err != nil {
			p.Logf("burnban: reading upstream body: %v", err)
		}
		_, _ = w.Write(body)
		if resp.StatusCode < 300 {
			usage = parseJSON(shape, body)
		}
	}

	rec.LatencyMs = time.Since(start).Milliseconds()
	if usage.Found {
		rec.Model = usage.Model
		rec.InTokens, rec.OutTokens = usage.In, usage.Out
		rec.CacheReadTokens, rec.CacheWriteTokens = usage.CacheRead, usage.CacheWrite
		rec.Estimated = usage.Estimated
		if price, ok := p.Prices.Lookup(usage.Model); ok {
			rec.CostUSD = pricing.Cost(price, usage.In, usage.Out, usage.CacheRead, usage.CacheWrite)
			rec.Priced = true
		}
	}
	if r.Method == http.MethodPost {
		if err := p.Store.Insert(rec); err != nil {
			p.Logf("burnban: store: %v", err)
		}
		// Off the handler goroutine: the warn check reads the ledger and
		// must never hold up this connection's next keep-alive request.
		// SetSettingOnce makes concurrent duplicates race-safe.
		go p.maybeWarn(time.Now())
	}
}

// streamThrough copies the SSE stream to the client line by line, flushing
// as data arrives, while feeding a usage tracker. If the client goes away
// mid-stream we keep draining upstream so the spend still gets recorded.
func (p *Proxy) streamThrough(w http.ResponseWriter, body io.Reader, shape string) meter.Usage {
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
	clientGone := false
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if !clientGone {
				if _, werr := w.Write(line); werr != nil {
					clientGone = true
				} else if flusher != nil {
					flusher.Flush()
				}
			}
			tracker.Feed(line)
		}
		if err != nil {
			break
		}
	}
	return tracker.Usage()
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
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Trailers", "Transfer-Encoding", "Upgrade", "Accept-Encoding",
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		dst.Del(h)
	}
}

// agentFrom attributes a request to a client: an explicit x-burnban-agent
// header wins, else the User-Agent product token (e.g. "claude-cli").
func agentFrom(r *http.Request) string {
	if v := r.Header.Get("x-burnban-agent"); v != "" {
		return v
	}
	ua := r.Header.Get("User-Agent")
	if ua == "" {
		return "unknown"
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
	if won, err := p.Store.SetSettingOnce(mark, "1"); err != nil || !won {
		return
	}
	p.postWebhook(urlStr, "🔥🚫 burnban: "+d.Message)
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
	if won, err := p.Store.SetSettingOnce(warn.MarkKey, "1"); err != nil || !won {
		return
	}
	p.postWebhook(urlStr, fmt.Sprintf(
		"⚠️ burnban: %.0f%% of the %s cap burned — $%.2f of $%.2f (resets %s)",
		warn.Pct, warn.Window, warn.Spent, warn.Cap, warn.Reset))
}

func (p *Proxy) postWebhook(urlStr, text string) {
	body, _ := json.Marshal(map[string]string{"text": text})
	go func() {
		c := &http.Client{Timeout: 5 * time.Second}
		resp, err := c.Post(urlStr, "application/json", bytes.NewReader(body))
		if err != nil {
			p.Logf("burnban: webhook: %v", err)
			return
		}
		resp.Body.Close()
	}()
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
