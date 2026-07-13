package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/burnban/burnban/internal/identity"
	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

type doctorResult struct {
	OK               bool                `json:"ok"`
	Version          string              `json:"version"`
	Database         string              `json:"database"`
	DatabaseOK       bool                `json:"database_ok"`
	Pricing          pricing.Diagnostics `json:"pricing"`
	PricingOK        bool                `json:"pricing_ok"`
	ServerURL        string              `json:"server_url,omitempty"`
	ServerOK         bool                `json:"server_ok"`
	LastRequestAt    string              `json:"last_request_at,omitempty"`
	RoutingRecent    bool                `json:"routing_recent"`
	ProviderRoutesOK bool                `json:"provider_routes_ok"`
	IdentityState    string              `json:"identity_state"`
	IdentityKind     string              `json:"identity_kind,omitempty"`
	IdentityValidTo  string              `json:"identity_valid_until,omitempty"`
	ConfiguredEnvs   map[string]string   `json:"configured_envs"`
	Issues           []string            `json:"issues"`
}

type providerBaseEnv struct {
	Key  string
	Path string
}

var providerBaseEnvs = []providerBaseEnv{
	{Key: "ANTHROPIC_BASE_URL", Path: "/anthropic"},
	{Key: "OPENAI_BASE_URL", Path: "/openai/v1"},
	{Key: "GOOGLE_GEMINI_BASE_URL", Path: "/gemini"},
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	serverURL := fs.String("url", "", "meter URL to verify (defaults to the running server state)")
	recent := fs.Duration("recent", 15*time.Minute, "how recent the last proxied request must be to verify routing")
	pricingMaxAge := fs.Duration("pricing-max-age", 45*24*time.Hour, "maximum acceptable age of the embedded pricing verification")
	sendToken := fs.Bool("send-token", false, "send BURNBAN_TOKEN to an explicit non-loopback --url")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if help, err := parseCommandFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if *recent <= 0 {
		return fmt.Errorf("--recent must be greater than zero")
	}
	if *pricingMaxAge <= 0 {
		return fmt.Errorf("--pricing-max-age must be greater than zero")
	}

	result := doctorResult{
		Version: version, Database: *dbPath, ProviderRoutesOK: true,
		IdentityState: "not_configured", ConfiguredEnvs: map[string]string{}, Issues: []string{},
	}
	prices, err := pricing.Load()
	if err != nil {
		result.Issues = append(result.Issues, "pricing table: "+err.Error())
	} else {
		result.Pricing = prices.Diagnostics()
		result.PricingOK = len(result.Pricing.ExpiredModels) == 0
		if !result.PricingOK {
			result.Issues = append(result.Issues, "expired pricing entries: "+strings.Join(result.Pricing.ExpiredModels, ", "))
		}
		if verified, parseErr := time.Parse("2006-01-02", result.Pricing.VerifiedDate); parseErr != nil {
			result.PricingOK = false
			result.Issues = append(result.Issues, "pricing verification date is invalid")
		} else if age := time.Since(verified); age > *pricingMaxAge {
			result.PricingOK = false
			result.Issues = append(result.Issues, fmt.Sprintf("pricing snapshot was last verified %d days ago", int(age.Hours()/24)))
		}
	}

	s, openErr := store.Open(*dbPath)
	if openErr != nil {
		result.Issues = append(result.Issues, "database: "+openErr.Error())
	} else {
		defer s.Close()
		if err := s.Probe(); err != nil {
			result.Issues = append(result.Issues, "database write probe: "+err.Error())
		} else {
			result.DatabaseOK = true
		}
		identitySettings, err := s.GetSettings(identity.KeyTrustGrant, identity.KeyTrustSource, identity.KeyPolicySource)
		if err != nil {
			result.IdentityState = "untrusted"
			result.Issues = append(result.Issues, "identity trust: "+err.Error())
		} else if identitySettings[identity.KeyTrustGrant] != "" || identitySettings[identity.KeyTrustSource] != "" {
			result.IdentityState = "untrusted"
			grant, err := identity.LoadTrustGrant(s, time.Now().UTC())
			if err != nil {
				result.Issues = append(result.Issues, "identity trust: "+err.Error())
			} else {
				result.IdentityState = "trusted"
				result.IdentityKind = grant.TenantKind
				result.IdentityValidTo = grant.ValidUntil
			}
		}
		if sum, err := s.LifetimeMetrics(); err == nil && !sum.LastRequestAt.IsZero() {
			result.LastRequestAt = sum.LastRequestAt.Format(time.RFC3339)
			result.RoutingRecent = time.Since(sum.LastRequestAt) <= *recent
			if !result.RoutingRecent {
				result.Issues = append(result.Issues, fmt.Sprintf("no proxied request recorded in the last %s", *recent))
			}
		} else if err != nil {
			result.Issues = append(result.Issues, "ledger summary: "+err.Error())
		} else {
			result.Issues = append(result.Issues, "no proxied requests recorded yet")
		}
	}

	rawEnvs := map[string]string{}
	for _, spec := range providerBaseEnvs {
		if value := os.Getenv(spec.Key); value != "" {
			rawEnvs[spec.Key] = value
		}
	}
	var discoveredState *serverState
	meterBase := ""
	if *serverURL == "" {
		if state, err := readServerState(serverStatePath(*dbPath)); err == nil {
			*serverURL = state.URL
			discoveredState = &state
		} else if !errors.Is(err, os.ErrNotExist) {
			result.Issues = append(result.Issues, "server state: "+err.Error())
		}
	}
	if *serverURL == "" {
		result.Issues = append(result.Issues, "meter is not running; start it with `burnban serve`")
	} else {
		meterBase = strings.TrimSuffix(*serverURL, "/")
		result.ServerURL = diagnosticServerURL(meterBase)
		var healthErr error
		if discoveredState != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			status, err := fetchControlStatus(ctx, *discoveredState)
			cancel()
			if err != nil {
				healthErr = err
			} else if !status.Health.OK || !status.Health.PersistenceOK {
				detail := terminalText(status.Health.Detail, 200)
				if detail == "" {
					detail = terminalText(status.Health.State, 80)
				}
				healthErr = fmt.Errorf("burnban health is unhealthy: %s", detail)
			}
		} else {
			healthErr = probeHealth(meterBase, *sendToken)
		}
		if healthErr != nil {
			result.Issues = append(result.Issues, "server health: "+healthErr.Error())
		} else {
			result.ServerOK = true
		}
	}
	for _, spec := range providerBaseEnvs {
		value, configured := rawEnvs[spec.Key]
		if !configured {
			continue
		}
		state, matches := endpointConfigurationState(value, meterBase, spec.Path)
		result.ConfiguredEnvs[spec.Key] = state
		if !matches {
			result.ProviderRoutesOK = false
			result.Issues = append(result.Issues, fmt.Sprintf("%s does not point to this meter's exact %s route", spec.Key, spec.Path))
		}
	}

	result.OK = result.DatabaseOK && result.PricingOK && result.ServerOK && result.RoutingRecent && result.ProviderRoutesOK && result.IdentityState != "untrusted"
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return err
		}
	} else {
		printDoctor(result)
	}
	if !result.OK {
		return fmt.Errorf("doctor found %d issue(s)", len(result.Issues))
	}
	return nil
}

func diagnosticServerURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "invalid URL (value redacted)"
	}
	origin := parsed.Scheme + "://" + parsed.Host
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.RawPath != "" || (parsed.Path != "" && parsed.Path != "/") {
		return origin + " (invalid suffix redacted)"
	}
	return origin
}

func probeHealth(base string, allowToken bool) error {
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("health URL must be an http(s) origin without credentials, query, or fragment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/health", nil)
	if err != nil {
		return err
	}
	if token := os.Getenv("BURNBAN_TOKEN"); token != "" && (allowToken || isLoopbackHost(parsed.Hostname())) {
		req.Header.Set("x-burnban-token", token)
	} else if os.Getenv("BURNBAN_TOKEN") != "" && !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("refusing to send BURNBAN_TOKEN to non-loopback %s without --send-token", parsed.Hostname())
	}
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read health response: %w", err)
	}
	var health struct {
		Service       string `json:"service"`
		OK            bool   `json:"ok"`
		State         string `json:"state"`
		Detail        string `json:"detail"`
		PersistenceOK bool   `json:"persistence_ok"`
	}
	if err := json.Unmarshal(body, &health); err != nil || health.Service != "burnban" {
		return fmt.Errorf("endpoint did not return a Burnban health document")
	}
	if resp.StatusCode != http.StatusOK || !health.OK || !health.PersistenceOK {
		detail := terminalText(health.Detail, 200)
		if detail == "" {
			detail = terminalText(health.State, 80)
		}
		return fmt.Errorf("burnban health is %s: %s", resp.Status, detail)
	}
	return nil
}

func endpointConfigurationState(raw, meterBase, expectedPath string) (string, bool) {
	endpoint, err := url.Parse(raw)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.RawPath != "" {
		return "set; invalid URL form (value redacted)", false
	}
	if meterBase == "" {
		return "set; meter route could not be checked", false
	}
	meter, err := url.Parse(meterBase)
	if err != nil || (meter.Scheme != "http" && meter.Scheme != "https") || meter.Host == "" ||
		meter.User != nil || meter.RawQuery != "" || meter.ForceQuery || meter.Fragment != "" ||
		(meter.Path != "" && meter.Path != "/") {
		return "set; meter route could not be checked", false
	}
	normalizedPath := strings.TrimSuffix(endpoint.EscapedPath(), "/")
	if normalizedPath == "" {
		normalizedPath = "/"
	}
	endpointOrigin, endpointOK := normalizedHTTPOrigin(endpoint)
	meterOrigin, meterOK := normalizedHTTPOrigin(meter)
	matches := endpointOK && meterOK && endpointOrigin == meterOrigin && normalizedPath == expectedPath
	if matches {
		return "set; points to this meter's " + expectedPath + " route", true
	}
	return "set; expected this meter's " + expectedPath + " route", false
}

func normalizedHTTPOrigin(parsed *url.URL) (string, bool) {
	if parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(parsed.Scheme) + "://" + net.JoinHostPort(host, port), true
}

func printDoctor(r doctorResult) {
	state := func(ok bool) string {
		if ok {
			return "ok"
		}
		return "needs attention"
	}
	fmt.Printf("BURNBAN DOCTOR\n\npricing   %s · snapshot %s · %d models\ndatabase  %s · %s\nserver    %s", state(r.PricingOK), terminalText(r.Pricing.Version, 100), r.Pricing.ModelCount, state(r.DatabaseOK), terminalText(r.Database, 240), state(r.ServerOK))
	if r.ServerURL != "" {
		fmt.Printf(" · %s", terminalText(r.ServerURL, 240))
	}
	fmt.Println()
	if r.LastRequestAt != "" {
		fmt.Printf("routing   %s · last request %s\n", state(r.RoutingRecent), r.LastRequestAt)
	} else {
		fmt.Printf("routing   %s\n", state(false))
	}
	fmt.Printf("routes    %s · %d provider base URL(s) configured\n", state(r.ProviderRoutesOK), len(r.ConfiguredEnvs))
	if r.IdentityState == "trusted" {
		fmt.Printf("identity  trusted · %s enrollment · valid until %s\n", terminalText(r.IdentityKind, 40), terminalText(r.IdentityValidTo, 40))
	} else {
		fmt.Printf("identity  %s\n", terminalText(r.IdentityState, 40))
	}
	if len(r.ConfiguredEnvs) > 0 {
		keys := make([]string, 0, len(r.ConfiguredEnvs))
		for key := range r.ConfiguredEnvs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Printf("env       %s=%s\n", key, terminalText(r.ConfiguredEnvs[key], 160))
		}
	}
	if len(r.Issues) > 0 {
		fmt.Println("\nACTION NEEDED")
		for _, issue := range r.Issues {
			fmt.Println("· " + terminalText(issue, 240))
		}
	} else {
		fmt.Println("\nAll checks passed; recent agent traffic is reaching the meter.")
	}
}

func cmdPricing(args []string) error {
	fs := flag.NewFlagSet("pricing", flag.ContinueOnError)
	model := fs.String("model", "", "show the effective price for one model ID")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if help, err := parseCommandFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	table, err := pricing.Load()
	if err != nil {
		return err
	}
	diagnostics := table.Diagnostics()
	if *model != "" {
		price, ok := table.Lookup(*model)
		if !ok {
			return fmt.Errorf("model %q is not in the effective pricing table", *model)
		}
		if *jsonOut {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"model": *model, "price": price, "diagnostics": diagnostics})
		}
		fmt.Printf("%s  input $%.4g/M · output $%.4g/M · cache read %.4gx · cache write %.4gx\n", terminalText(*model, 100), price.InputPerMTok, price.OutputPerMTok, price.CacheReadMult, price.CacheWriteMult)
		return nil
	}
	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"diagnostics": diagnostics, "models": table.Models})
	}
	fmt.Printf("Pricing snapshot %s · effective %s · verified %s · %d models\n", diagnostics.Version, diagnostics.EffectiveDate, diagnostics.VerifiedDate, diagnostics.ModelCount)
	if len(diagnostics.OverrideModels) > 0 {
		fmt.Printf("Overrides: %s (%s)\n", safeJoined(diagnostics.OverrideModels), terminalText(diagnostics.OverrideFile, 120))
	}
	if len(diagnostics.ExpiredModels) > 0 {
		fmt.Printf("WARNING: expired promotional prices: %s\n", safeJoined(diagnostics.ExpiredModels))
	}
	names := make([]string, 0, len(table.Models))
	for name := range table.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := table.Models[name]
		fmt.Printf("%-38s in $%7.4g/M  out $%7.4g/M  cache-r %.3gx  cache-w %.3gx\n", terminalText(name, 38), p.InputPerMTok, p.OutputPerMTok, p.CacheReadMult, p.CacheWriteMult)
	}
	return nil
}

func safeJoined(values []string) string {
	safe := make([]string, len(values))
	for i, value := range values {
		safe[i] = terminalText(value, 100)
	}
	return strings.Join(safe, ", ")
}

func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	olderThan := fs.String("older-than", "", "delete rows older than this window (for example 90d or 2160h)")
	before := fs.String("before", "", "delete rows before YYYY-MM-DD")
	yes := fs.Bool("yes", false, "confirm irreversible deletion")
	if help, err := parseCommandFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	if (*olderThan == "") == (*before == "") {
		return fmt.Errorf("provide exactly one of --older-than or --before")
	}
	var cutoff time.Time
	var err error
	if *olderThan != "" {
		cutoff, _, err = parseSince(*olderThan)
		if err != nil {
			return fmt.Errorf("bad --older-than: %w", err)
		}
	} else {
		cutoff, err = time.ParseInLocation("2006-01-02", *before, time.Local)
		if err != nil {
			return fmt.Errorf("bad --before %q: use YYYY-MM-DD", *before)
		}
	}
	if !cutoff.Before(time.Now()) {
		return fmt.Errorf("prune cutoff must be in the past")
	}
	if !*yes {
		return fmt.Errorf("refusing irreversible deletion without --yes (cutoff %s; settings and caps are preserved)", cutoff.Format(time.RFC3339))
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	lease, err := s.AcquireLease("serve", 30*time.Second)
	if errors.Is(err, store.ErrLeaseHeld) {
		return fmt.Errorf("refusing to prune while this ledger is served; run `burnban stop` with the same --db first")
	}
	if err != nil {
		return fmt.Errorf("acquire exclusive maintenance lease: %w", err)
	}
	released := false
	defer func() {
		if !released {
			_ = lease.Release()
		}
	}()
	var deleted int64
	for {
		if err := lease.Renew(); err != nil {
			return fmt.Errorf("renew exclusive maintenance lease: %w", err)
		}
		batch, err := s.PruneBatch(cutoff, 5000)
		if err != nil {
			return err
		}
		deleted += batch
		if batch == 0 {
			break
		}
	}
	if err := s.Checkpoint(); err != nil {
		return fmt.Errorf("checkpoint ledger after prune: %w", err)
	}
	if err := lease.Release(); err != nil {
		return fmt.Errorf("release exclusive maintenance lease: %w", err)
	}
	released = true
	fmt.Printf("deleted %d request/policy-decision record(s) before %s; policy documents, caps, and settings were preserved (logical retention does not necessarily shrink the database file)\n", deleted, cutoff.Format(time.RFC3339))
	return nil
}
