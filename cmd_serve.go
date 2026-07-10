package main

import (
	"flag"
	"fmt"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

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
		pairs = append(pairs, k+"="+u[k].URL)
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
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	custom := upstreamFlags{}
	fs.Var(custom, "upstream", "extra upstream as name=url (repeatable; OpenAI-shaped unless url is prefixed anthropic:/gemini:): groq, mistral, openrouter, ollama, vllm…")
	fs.Parse(args)

	token := os.Getenv("BURNBAN_TOKEN")
	if *host != "127.0.0.1" && *host != "localhost" && *host != "::1" && token == "" {
		return fmt.Errorf("refusing to bind %s without BURNBAN_TOKEN set — team mode fails closed", *host)
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

	addr := fmt.Sprintf("%s:%d", *host, *port)
	base := "http://" + addr
	authState := "open (localhost only)"
	if token != "" {
		authState = "token required (BURNBAN_TOKEN)"
	}

	capState := capBanner(s)
	banState := ""
	if ban, _ := s.GetSetting(budget.KeyBanActive); ban == "1" {
		banState = "\n   🚫 BURN BAN IN EFFECT — lift with: burnban lift\n"
	}

	customLines := ""
	for _, name := range slices.Sorted(maps.Keys(custom)) {
		shape := upstreams[name].Shape
		if shape == "" {
			shape = "openai"
		}
		customLines += fmt.Sprintf("     %-11s %s/%s → %s (%s-shaped metering)\n",
			name, base, name, upstreams[name].URL, shape)
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
	srv := &http.Server{Addr: addr, Handler: web.WithAuth(token, mux)}
	return srv.ListenAndServe()
}

// capBanner summarizes every configured budget window in one line.
func capBanner(s *store.Store) string {
	per := map[string]string{"daily": "day", "weekly": "week", "monthly": "month"}
	var parts []string
	for _, w := range budget.Windows() {
		if v, _ := s.GetSetting(w.Key); v != "" {
			parts = append(parts, fmt.Sprintf("$%s/%s", v, per[w.Name]))
		}
	}
	if len(parts) == 0 {
		return "none — set one: burnban cap --daily 10"
	}
	return strings.Join(parts, " · ")
}
