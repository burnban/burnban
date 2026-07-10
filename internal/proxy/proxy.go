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

type Proxy struct {
	Store     *store.Store
	Prices    *pricing.Table
	Guard     *budget.Guard
	Upstreams map[string]*url.URL
	Logf      func(format string, v ...any)

	client *http.Client
}

func New(s *store.Store, t *pricing.Table, upstreams map[string]string) (*Proxy, error) {
	us := make(map[string]*url.URL, len(upstreams))
	for name, raw := range upstreams {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("upstream %s: %w", name, err)
		}
		us[name] = u
	}
	return &Proxy{
		Store:     s,
		Prices:    t,
		Guard:     &budget.Guard{S: s},
		Upstreams: us,
		Logf:      log.Printf,
		client: &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: 120 * time.Second,
			MaxIdleConnsPerHost:   32,
		}},
	}, nil
}

// DefaultUpstreams maps URL path prefixes to provider APIs. Each can be
// overridden by env var, which is also how tests point burnban at fakes.
// Any other name added to the map (serve --upstream name=url) is metered
// with OpenAI-shaped usage parsing — the de-facto standard that Groq,
// Mistral, DeepSeek, OpenRouter, Ollama, vLLM and friends all speak.
func DefaultUpstreams() map[string]string {
	return map[string]string{
		"anthropic": envOr("BURNBAN_ANTHROPIC_UPSTREAM", "https://api.anthropic.com"),
		"openai":    envOr("BURNBAN_OPENAI_UPSTREAM", "https://api.openai.com"),
		"xai":       envOr("BURNBAN_XAI_UPSTREAM", "https://api.x.ai"),
		"gemini":    envOr("BURNBAN_GEMINI_UPSTREAM", "https://generativelanguage.googleapis.com"),
	}
}

func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok":true}`)
	})
	for name := range p.Upstreams {
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

	if r.Method == http.MethodPost {
		denial, err := p.Guard.Check(time.Now(), agent)
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
		b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			http.Error(w, "reading request body: "+err.Error(), http.StatusBadGateway)
			return
		}
		reqBody = b
	}

	up := p.Upstreams[provider]
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
		usage = p.streamThrough(w, resp.Body, provider)
	} else {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		if err != nil {
			p.Logf("burnban: reading upstream body: %v", err)
		}
		_, _ = w.Write(body)
		if resp.StatusCode < 300 {
			usage = parseJSON(provider, body)
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
		p.maybeWarn(time.Now())
	}
}

// streamThrough copies the SSE stream to the client line by line, flushing
// as data arrives, while feeding a usage tracker. If the client goes away
// mid-stream we keep draining upstream so the spend still gets recorded.
func (p *Proxy) streamThrough(w http.ResponseWriter, body io.Reader, provider string) meter.Usage {
	var tracker meter.Tracker
	switch provider {
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

func parseJSON(provider string, body []byte) meter.Usage {
	switch provider {
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

// alertCapReached fires the configured webhook (Slack-compatible JSON)
// the first time a cap trips each day. Fire-and-forget: a slow or dead
// webhook must never sit in the request path.
func (p *Proxy) alertCapReached(d *budget.Denial) {
	if d.Type != "burnban_cap_reached" {
		return
	}
	urlStr, err := p.Store.GetSetting(budget.KeyWebhookURL)
	if err != nil || urlStr == "" {
		return
	}
	today := time.Now().Format("2006-01-02")
	if won, err := p.Store.SetSettingOnce(budget.KeyAlertedDay, today); err != nil || !won {
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
