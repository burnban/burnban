package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/burnban/burnban/internal/budget"
	"github.com/burnban/burnban/internal/localusage"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/proxy"
	"github.com/burnban/burnban/internal/store"
	"github.com/burnban/burnban/internal/telemetry"
	"github.com/burnban/burnban/internal/web"
)

// reservedRoutes are path prefixes the dashboard and metrics own; a custom
// upstream may not shadow them.
var reservedRoutes = map[string]bool{"health": true, "api": true, "metrics": true}

// upstreamName is deliberately strict: route names become ServeMux
// patterns, where characters like '{' either panic at registration or
// register a wildcard that swallows arbitrary paths.
var upstreamName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

const gracefulShutdownTimeout = 10 * time.Second

// Provider API keys, trace baggage, and custom attribution headers normally
// fit comfortably below 64 KiB in aggregate. Capping the public listener here
// prevents the previous 1 MiB header allocation from becoming a cheap memory
// abuse path while retaining substantially more room than common 8-16 KiB
// reverse-proxy defaults.
const providerMaxHeaderBytes = 64 << 10

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
	return cmdServeWithOptions(args, false, false)
}

// cmdServeMode runs the real metering proxy. Desktop mode uses the exact same
// server and database, then opens the dashboard once the listener is ready.
func cmdServeMode(args []string, launchDashboard bool) error {
	return cmdServeWithOptions(args, launchDashboard, false)
}

func cmdServeWithOptions(args []string, launchDashboard, demoMode bool) error {
	commandName := "serve"
	if launchDashboard {
		commandName = "desktop"
	}
	if demoMode {
		commandName = "demo-server"
	}
	fs := flag.NewFlagSet(commandName, flag.ExitOnError)
	port := fs.Int("port", 4141, "listen port")
	host := fs.String("host", "127.0.0.1", "bind address; anything non-loopback requires BURNBAN_TOKEN (team mode)")
	publicURL := fs.String("public-url", "", "public dashboard/proxy base URL advertised to clients (recommended for team mode)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate for non-loopback/team mode")
	tlsKey := fs.String("tls-key", "", "TLS private key for non-loopback/team mode")
	allowInsecure := fs.Bool("allow-insecure-http", false, "allow plaintext on a non-loopback bind (only behind a TLS reverse proxy)")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	otlpEndpoint := fs.String("otlp-endpoint", os.Getenv("BURNBAN_OTLP_ENDPOINT"), "opt-in OTLP/HTTP base endpoint (or BURNBAN_OTLP_ENDPOINT)")
	otlpAuthEnv := fs.String("otlp-auth-env", "BURNBAN_OTLP_AUTHORIZATION", "environment variable containing the complete OTLP Authorization header")
	otlpAllowPrivate := fs.Bool("otlp-allow-private-network", false, "allow a TLS OTLP collector on an RFC 1918/ULA address")
	otlpBatch := fs.Int("otlp-batch", 128, "maximum prompt-free ledger rows per asynchronous OTLP batch")
	otlpMaxBacklog := fs.Int64("otlp-max-backlog", 10_000, "maximum pending OTLP ledger rows before oldest rows are recorded as dropped")
	otlpInterval := fs.Duration("otlp-interval", 2*time.Second, "interval between asynchronous OTLP ledger polls")
	localUsageMaxScanMB := fs.Int64("local-usage-max-scan-mb", 512, "maximum local log MiB scanned per source for the dashboard")
	localUsageScanTimeout := fs.Duration("local-usage-scan-timeout", 10*time.Second, "maximum scan time per local source for the dashboard")
	allowRemoteAdmin := fs.Bool("allow-remote-admin", false, "enable dashboard control actions (caps, fuses, ban/lift, alerts) on a team/network gateway; loopback listeners always have them")
	custom := upstreamFlags{}
	fs.Var(custom, "upstream", "extra upstream as name=url (repeatable; OpenAI-shaped unless url is prefixed anthropic:/gemini:): groq, mistral, openrouter, ollama, vllm…")
	fs.Parse(args)
	if err := requireNoArgs(fs); err != nil {
		return err
	}

	if (*tlsCert == "") != (*tlsKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be provided together")
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("--port must be between 0 and 65535")
	}
	if *localUsageMaxScanMB <= 0 || *localUsageScanTimeout <= 0 {
		return fmt.Errorf("local usage scan limits must be greater than zero")
	}
	if *localUsageMaxScanMB > int64(^uint64(0)>>1)>>20 {
		return fmt.Errorf("--local-usage-max-scan-mb is too large for this platform")
	}
	var telemetrySink *telemetry.HTTPExporter
	if *otlpEndpoint != "" {
		sink, telemetryErr := telemetry.NewHTTPExporter(telemetry.HTTPConfig{
			Endpoint: *otlpEndpoint, AuthorizationEnv: *otlpAuthEnv,
			AllowPrivateNetwork: *otlpAllowPrivate, ServiceName: "burnban",
			ServiceVersion: version,
		})
		if telemetryErr != nil {
			return fmt.Errorf("configure optional OTLP export: %w", telemetryErr)
		}
		telemetrySink = sink
	}
	var parsedPublic *url.URL
	if *publicURL != "" {
		parsed, parseErr := url.Parse(*publicURL)
		if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			return fmt.Errorf("--public-url must be an absolute http(s) origin with no path, query, credentials, or fragment")
		}
		parsedPublic = parsed
		if parsedPublic.Scheme != "http" && parsedPublic.Scheme != "https" {
			return fmt.Errorf("--public-url scheme must be http or https")
		}
		if *tlsCert != "" && parsedPublic.Scheme != "https" {
			return fmt.Errorf("--public-url must use https when --tls-cert/--tls-key are configured")
		}
		*publicURL = strings.TrimSuffix(*publicURL, "/")
	}
	remote := !isLoopbackHost(*host)
	publicRemote := parsedPublic != nil && !isLoopbackHost(parsedPublic.Hostname())
	exposed := remote || publicRemote
	if publicRemote && parsedPublic.Scheme != "https" {
		return fmt.Errorf("non-loopback --public-url must use https")
	}
	token := os.Getenv("BURNBAN_TOKEN")
	if token != "" {
		if err := validateBurnbanToken(token, exposed); err != nil {
			return err
		}
	}
	if exposed && token == "" {
		return fmt.Errorf("refusing network exposure without BURNBAN_TOKEN set — team mode fails closed")
	}
	if remote && *tlsCert == "" && !*allowInsecure {
		return fmt.Errorf("refusing plaintext non-loopback traffic (it carries provider keys): configure --tls-cert/--tls-key, or use --allow-insecure-http only behind a TLS reverse proxy")
	}
	var serverCertificate *tls.Certificate
	if *tlsCert != "" {
		publicHost := ""
		if parsedPublic != nil {
			publicHost = parsedPublic.Hostname()
		}
		certificate, err := loadServerCertificate(*tlsCert, *tlsKey, publicHost, time.Now())
		if err != nil {
			return err
		}
		serverCertificate = &certificate
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	var telemetryWorker *telemetry.Worker
	if telemetrySink != nil {
		telemetryWorker, err = telemetry.NewWorker(s, telemetrySink, telemetry.WorkerConfig{
			BatchSize: *otlpBatch, MaxBacklog: *otlpMaxBacklog,
			PollInterval: *otlpInterval,
			Logf:         func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
		})
		if err != nil {
			return fmt.Errorf("configure optional OTLP worker: %w", err)
		}
	}
	lease, err := s.AcquireLease("serve", 15*time.Second)
	if err != nil {
		if errors.Is(err, store.ErrLeaseHeld) {
			if launchDashboard {
				if state, stateErr := readServerState(serverStatePath(*dbPath)); stateErr == nil && serverStateAlive(state) {
					if openErr := openDashboard(dashboardURL(state.URL, token)); openErr != nil {
						return openErr
					}
					fmt.Printf("burnban is already running — opened %s\n", state.URL)
					return nil
				}
			}
			return fmt.Errorf("database %s is already served by another live Burnban process; use a different --db or run `burnban status`", *dbPath)
		}
		return fmt.Errorf("acquire single-server database lease: %w", err)
	}
	defer func() {
		if err := lease.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "burnban: release database lease: %v\n", err)
		}
	}()

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
	displayHost := strings.Trim(*host, "[]")
	if ip := net.ParseIP(displayHost); ip != nil && ip.IsUnspecified() {
		displayHost = "localhost"
	}
	base := scheme + "://" + net.JoinHostPort(displayHost, strconv.Itoa(*port))
	if *publicURL != "" {
		base = *publicURL
	}
	authState := "open (localhost only)"
	if token != "" {
		authState = "token required (BURNBAN_TOKEN)"
	}
	panelState := "full controls (caps, fuses, ban/lift, alerts)"
	if exposed && !*allowRemoteAdmin {
		panelState = "read-only on this gateway (--allow-remote-admin enables controls)"
	}
	telemetryState := "off (no outbound telemetry)"
	if telemetryWorker != nil {
		telemetryState = "OTLP metadata export enabled (content-free, asynchronous)"
	}

	capState := capBanner(s)
	banState := ""
	if local, external, _ := budget.BanStatus(s); local || external {
		if external {
			banState = "\n   EXTERNAL BURN BAN IN EFFECT — controlled by external policy\n"
		} else {
			banState = "\n   BURN BAN IN EFFECT — lift with: burnban lift\n"
		}
	}
	if fuse, fuseErr := budget.FuseStatus(s, time.Now()); fuseErr == nil && fuse.Tripped {
		banState += "\n   SPEND-VELOCITY FUSE TRIPPED — inspect/reset with: burnban fuse\n"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if launchDashboard && burnbanRunning(base, token) {
			if openErr := openDashboard(dashboardURL(base, token)); openErr != nil {
				return openErr
			}
			fmt.Printf("burnban is already running — opened %s\n", base)
			return nil
		}
		return err
	}
	defer ln.Close()
	controlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open private loopback control listener: %w", err)
	}
	defer controlLn.Close()

	// Port 0 is useful for tests and embedded launchers. Always report/open the
	// address the OS actually assigned rather than an unusable :0 URL.
	if *port == 0 {
		boundPort := ln.Addr().(*net.TCPAddr).Port
		if *publicURL == "" {
			base = scheme + "://" + net.JoinHostPort(displayHost, strconv.Itoa(boundPort))
		}
	}
	controlBase := "http://" + controlLn.Addr().String()
	controlToken, err := newControlToken()
	if err != nil {
		return err
	}
	statePath := serverStatePath(*dbPath)
	state := serverState{
		Version: version, PID: os.Getpid(), URL: base, ControlURL: controlBase,
		DBPath: *dbPath, StartedAt: time.Now().UTC(), ControlToken: controlToken,
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

	fmt.Printf(`burnban %s — the meter is running

   dashboard   %s

   point your agents here:
     anthropic   ANTHROPIC_BASE_URL=%s/anthropic
     openai      OPENAI_BASE_URL=%s/openai/v1
     gemini      GOOGLE_GEMINI_BASE_URL=%s/gemini
     xai         %s/xai/v1

   compatible routes:
     openrouter  %s/openrouter/v1
     groq        %s/groq/v1
     mistral     %s/mistral/v1
     deepseek    %s/deepseek/v1
     ollama      %s/ollama/v1
     vllm        %s/vllm/v1
%s
   db    %s
   cap   %s
   auth  %s
   panel %s
   otlp  %s
%s
   watch it live:  burnban top  (or open the dashboard)

`, version, base, base, base, base, base, base, base, base, base, base, base, customLines, *dbPath, capState, authState, panelState, telemetryState, banState)

	mux := http.NewServeMux()
	mux.Handle("/", p.Handler())
	if demoMode {
		registerDemoProviderBlocks(mux, upstreams)
	}
	web.RegisterWithConfig(mux, s, web.Config{
		Version: version, Prices: prices, Demo: demoMode,
		Exposure:          map[bool]string{true: "team/network", false: "localhost"}[exposed],
		AuthRequired:      token != "",
		AllowAdmin:        !exposed || *allowRemoteAdmin,
		DisableLocalUsage: exposed,
		LocalUsageScanLimits: localusage.ScanLimits{
			MaxBytes:    *localUsageMaxScanMB << 20,
			MaxDuration: *localUsageScanTimeout,
		},
		Health: func() web.HealthStatus {
			h := p.Health()
			return web.HealthStatus{
				OK: h.OK, State: h.State, Detail: h.Detail, PersistenceOK: h.PersistenceOK,
				InFlight: h.InFlight, ReservedUSD: h.ReservedUSD, LastFailure: h.LastFailure,
			}
		},
	})
	// Invalid or missing local-control credentials must never fall through to
	// an upstream route.
	mux.HandleFunc("/api/control/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "burnban: invalid local control token", http.StatusUnauthorized)
	})
	shutdown := make(chan struct{}, 1)
	controlMux := http.NewServeMux()
	controlMux.HandleFunc("GET /api/control/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		status := map[string]any{
			"ok": true, "pid": os.Getpid(), "version": version,
			"started_at": state.StartedAt, "health": p.Health(),
		}
		if telemetryWorker != nil {
			status["telemetry"] = telemetryWorker.Stats()
		}
		_ = json.NewEncoder(w).Encode(status)
	})
	controlMux.HandleFunc("POST /api/control/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("burnban is shutting down\n"))
		select {
		case shutdown <- struct{}{}:
		default:
		}
	})
	controlDenied := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "burnban: invalid local control token", http.StatusUnauthorized)
	})
	controlSrv := &http.Server{
		Addr:              controlLn.Addr().String(),
		Handler:           web.LocalSafety("127.0.0.1", true, withControlToken(controlToken, controlMux, controlDenied)),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	defer controlSrv.Close()
	publicOrigin := ""
	if parsedPublic != nil {
		publicOrigin = parsedPublic.Scheme + "://" + parsedPublic.Host
	}
	srv := &http.Server{
		Addr: addr, Handler: web.LocalSafetyWithPublicOrigin(*host, token == "", publicOrigin, web.WithAuth(token, mux)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    providerMaxHeaderBytes,
	}
	if serverCertificate != nil {
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*serverCertificate}, MinVersion: tls.VersionTLS12}
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if telemetryWorker != nil {
		telemetryWorker.Start(ctx)
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := telemetryWorker.Stop(stopCtx); err != nil {
				fmt.Fprintf(os.Stderr, "burnban: stop optional telemetry worker: %v\n", err)
			}
		}()
	}
	leaseCtx, cancelLease := context.WithCancel(context.Background())
	defer cancelLease()
	leaseLost := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				if err := lease.Renew(); err != nil {
					select {
					case leaseLost <- err:
					default:
					}
					return
				}
			}
		}
	}()
	fatalServeErr := make(chan error, 1)
	controlServeErr := make(chan error, 1)
	go func() {
		if err := controlSrv.Serve(controlLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			controlServeErr <- err
		}
	}()
	// Publish lifecycle state only after the private control listener is ready
	// to accept connections. Status/stop readers may act as soon as this atomic
	// state file appears.
	if err := writeServerState(statePath, state); err != nil {
		return fmt.Errorf("write private lifecycle state: %w", err)
	}
	defer removeServerState(statePath, controlToken)
	shutdownDone := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
		case <-shutdown:
		case err := <-leaseLost:
			fatalServeErr <- fmt.Errorf("database lease renewal failed; meter stopped fail-closed: %w", err)
		case err := <-controlServeErr:
			fatalServeErr <- fmt.Errorf("private control listener failed: %w", err)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer cancel()
		shutdownErrors := make(chan error, 2)
		go func() { shutdownErrors <- srv.Shutdown(shutdownCtx) }()
		go func() { shutdownErrors <- controlSrv.Shutdown(shutdownCtx) }()
		shutdownDone <- errors.Join(<-shutdownErrors, <-shutdownErrors)
	}()
	if launchDashboard {
		if err := openDashboard(dashboardURL(base, token)); err != nil {
			fmt.Fprintf(os.Stderr, "burnban: dashboard is live at %s (could not open the browser: %v)\n", base, err)
		}
	}
	if *tlsCert != "" {
		err = srv.ServeTLS(ln, "", "")
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		if shutdownErr := <-shutdownDone; shutdownErr != nil {
			return fmt.Errorf("graceful shutdown: %w", shutdownErr)
		}
		select {
		case fatalErr := <-fatalServeErr:
			return fatalErr
		default:
			return nil
		}
	}
	return err
}

func validateBurnbanToken(token string, requireStrong bool) error {
	// Keep the shared secret portable across browsers, SDK header libraries,
	// proxies, and shells. Visible ASCII without spaces is the conservative
	// intersection; generated hex/base64url secrets fit it naturally.
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e {
			return fmt.Errorf("BURNBAN_TOKEN must contain only visible ASCII characters without spaces")
		}
	}
	if !requireStrong {
		return nil
	}
	if len(token) < 16 {
		return fmt.Errorf("BURNBAN_TOKEN must be at least 16 characters for network exposure")
	}
	distinct := map[byte]struct{}{}
	for i := 0; i < len(token); i++ {
		distinct[token[i]] = struct{}{}
	}
	if len(distinct) < 4 {
		return fmt.Errorf("BURNBAN_TOKEN must be a randomly generated secret for network exposure")
	}
	return nil
}

func loadServerCertificate(certPath, keyPath, publicHost string, now time.Time) (tls.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load TLS certificate/key: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return tls.Certificate{}, fmt.Errorf("TLS certificate file contains no certificates")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse TLS leaf certificate: %w", err)
	}
	if now.Before(leaf.NotBefore) {
		return tls.Certificate{}, fmt.Errorf("TLS certificate is not valid until %s", leaf.NotBefore.Format(time.RFC3339))
	}
	if !now.Before(leaf.NotAfter) {
		return tls.Certificate{}, fmt.Errorf("TLS certificate expired at %s", leaf.NotAfter.Format(time.RFC3339))
	}
	if publicHost != "" {
		if err := leaf.VerifyHostname(publicHost); err != nil {
			return tls.Certificate{}, fmt.Errorf("TLS certificate does not cover --public-url host %q: %w", publicHost, err)
		}
	}
	pair.Leaf = leaf
	return pair, nil
}

func registerDemoProviderBlocks(mux *http.ServeMux, upstreams map[string]proxy.Upstream) {
	for name := range upstreams {
		mux.HandleFunc("/"+name+"/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"type":    "burnban_demo_network_disabled",
					"message": "demo mode uses isolated fixtures and never forwards provider traffic; run `burnban serve` for real agents",
				},
			})
		})
	}
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
	if fuses, err := budget.FuseStatus(s, time.Now()); err == nil {
		for _, fuse := range fuses.Rules {
			parts = append(parts, fmt.Sprintf("$%.2f/rolling %s (%s fuse)", fuse.CapUSD, budget.FormatFuseDuration(fuse.Window), fuse.Name))
		}
		if fuses.Fanout != nil {
			parts = append(parts, fmt.Sprintf("%d requests/rolling %s (fanout fuse)",
				fuses.Fanout.LimitRequests, budget.FormatFuseDuration(fuses.Fanout.Window)))
		}
	}
	if len(parts) == 0 {
		return "none — set one: burnban cap --daily 10 or burnban fuse --burst 5m:4"
	}
	return strings.Join(parts, " · ")
}
