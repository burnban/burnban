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
func DefaultUpstreams() map[string]string {
	return map[string]string{
		"anthropic": envOr("BURNBAN_ANTHROPIC_UPSTREAM", "https://api.anthropic.com"),
		"openai":    envOr("BURNBAN_OPENAI_UPSTREAM", "https://api.openai.com"),
		"xai":       envOr("BURNBAN_XAI_UPSTREAM", "https://api.x.ai"),
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

	if r.Method == http.MethodPost {
		denial, err := p.Guard.Check(time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if denial != nil {
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
		Agent:    agentFrom(r),
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
	}
}

// streamThrough copies the SSE stream to the client line by line, flushing
// as data arrives, while feeding a usage tracker. If the client goes away
// mid-stream we keep draining upstream so the spend still gets recorded.
func (p *Proxy) streamThrough(w http.ResponseWriter, body io.Reader, provider string) meter.Usage {
	var tracker meter.Tracker
	if provider == "anthropic" {
		tracker = &meter.AnthropicSSE{}
	} else {
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
	if provider == "anthropic" {
		return meter.ParseAnthropicJSON(body)
	}
	return meter.ParseOpenAIJSON(body)
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
