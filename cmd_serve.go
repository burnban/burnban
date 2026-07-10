package main

import (
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/pricing"
	"github.com/syft8/burnban/internal/proxy"
	"github.com/syft8/burnban/internal/store"
	"github.com/syft8/burnban/internal/web"
)

// reservedRoutes are path prefixes the dashboard and metrics own; a custom
// upstream may not shadow them.
var reservedRoutes = map[string]bool{"health": true, "api": true, "metrics": true}

// upstreamName is deliberately strict: route names become ServeMux
// patterns, where characters like '{' either panic at registration or
// register a wildcard that swallows arbitrary paths.
var upstreamName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// upstreamFlags collects repeated --upstream name=url pairs. A url may be
// prefixed with a usage shape ("anthropic:https://…", "gemini:…") when the
// endpoint is not OpenAI-compatible; unprefixed urls meter as OpenAI-shaped,
// which is what nearly every compatible provider emits.
type upstreamFlags map[string]proxy.Upstream

func (u upstreamFlags) String() string {
	pairs := make([]string, 0, len(u))
	for _, k := range slices.Sorted(maps.Keys(u)) {
		pairs = append(pairs, k+"="+redactURL(u[k].URL))
	}
	return strings.Join(pairs, ",")
}

func (u upstreamFlags) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return fmt.Errorf("want name=url, e.g. --upstream groq=https://api.groq.com/openai")
	}
	if !upstreamName.MatchString(name) {
		return fmt.Errorf("upstream name %q must match %s (it becomes a URL route)", name, upstreamName)
	}
	if reservedRoutes[name] {
		return fmt.Errorf("upstream name %q is reserved for burnban's own routes", name)
	}
	up := proxy.Upstream{URL: rest} // shape left empty = unspecified
	if shape, rawURL, ok := strings.Cut(rest, ":"); ok && slices.Contains(proxy.Shapes(), shape) {
		up = proxy.Upstream{URL: rawURL, Shape: shape}
	}
	if !strings.Contains(up.URL, "://") {
		return fmt.Errorf("upstream %s: url %q must include a scheme, e.g. https://", name, up.URL)
	}
	u[name] = up
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 4141, "listen port")
	host := fs.String("host", "127.0.0.1", "bind address; anything non-loopback requires BURNBAN_TOKEN (team mode)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate for non-loopback/team mode")
	tlsKey := fs.String("tls-key", "", "TLS private key for non-loopback/team mode")
	allowInsecure := fs.Bool("allow-insecure-http", false, "allow plaintext on a non-loopback bind (only behind a TLS reverse proxy)")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	custom := upstreamFlags{}
	fs.Var(custom, "upstream", "extra upstream as name=url (repeatable; OpenAI-shaped unless url is prefixed anthropic:/gemini:): groq, mistral, openrouter, ollama, vllm…")
	fs.Parse(args)

	if (*tlsCert == "") != (*tlsKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be provided together")
	}
	remote := !isLoopbackHost(*host)
	token := os.Getenv("BURNBAN_TOKEN")
	if remote && token == "" {
		return fmt.Errorf("refusing to bind %s without BURNBAN_TOKEN set — team mode fails closed", *host)
	}
	if remote && len(token) < 16 {
		return fmt.Errorf("BURNBAN_TOKEN must be at least 16 characters for a non-loopback bind")
	}
	if remote && *tlsCert == "" && !*allowInsecure {
		return fmt.Errorf("refusing plaintext non-loopback traffic (it carries provider keys): configure --tls-cert/--tls-key, or use --allow-insecure-http only behind a TLS reverse proxy")
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	prices, err := pricing.Load()
	if err != nil {
		return err
	}
	upstreams := proxy.DefaultUpstreams()
	for name, u := range custom {
		// Repointing a built-in keeps its native metering shape unless the
		// user overrode it with an explicit shape prefix.
		if base, isBuiltin := upstreams[name]; isBuiltin && u.Shape == "" {
			u.Shape = base.Shape
		}
		upstreams[name] = u
	}
	p, err := proxy.New(s, prices, upstreams)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(strings.Trim(*host, "[]"), strconv.Itoa(*port))
	scheme := "http"
	if *tlsCert != "" {
		scheme = "https"
	}
	base := scheme + "://" + addr
	authState := "open (localhost only)"
	if token != "" {
		authState = "token required (BURNBAN_TOKEN)"
	}

	capState := capBanner(s)
	banState := ""
	if local, external, _ := budget.BanStatus(s); local || external {
		if external {
			banState = "\n   🚫 ORGANIZATION BURN BAN IN EFFECT — external policy\n"
		} else {
			banState = "\n   🚫 BURN BAN IN EFFECT — lift with: burnban lift\n"
		}
	}

	customLines := ""
	for _, name := range slices.Sorted(maps.Keys(custom)) {
		shape := upstreams[name].Shape
		if shape == "" {
			shape = "openai"
		}
		customLines += fmt.Sprintf("     %-11s %s/%s → %s (%s-shaped metering)\n",
			name, base, name, redactURL(upstreams[name].URL), shape)
	}

	fmt.Printf(`🔥 burnban %s — the meter is running

   dashboard   %s

   point your agents here:
     anthropic   ANTHROPIC_BASE_URL=%s/anthropic
     openai      OPENAI_BASE_URL=%s/openai/v1
     gemini      GOOGLE_GEMINI_BASE_URL=%s/gemini
     xai         %s/xai/v1
%s
   db    %s
   cap   %s
   auth  %s
%s
   watch it live:  burnban top  (or open the dashboard)

`, version, base, base, base, base, base, customLines, *dbPath, capState, authState, banState)

	mux := http.NewServeMux()
	mux.Handle("/", p.Handler())
	web.Register(mux, s, version)
	srv := &http.Server{
		Addr: addr, Handler: web.WithAuth(token, mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	if *tlsCert != "" {
		return srv.ListenAndServeTLS(*tlsCert, *tlsKey)
	}
	return srv.ListenAndServe()
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// capBanner summarizes every configured budget window in one line.
func capBanner(s *store.Store) string {
	per := map[string]string{"daily": "day", "weekly": "week", "monthly": "month"}
	var parts []string
	if states, err := budget.Status(s, time.Now()); err == nil {
		for _, st := range states {
			if st.Set {
				parts = append(parts, fmt.Sprintf("$%.2f/%s (%s)", st.CapUSD, per[st.Name], st.Source))
			}
		}
	}
	if len(parts) == 0 {
		return "none — set one: burnban cap --daily 10"
	}
	return strings.Join(parts, " · ")
}
