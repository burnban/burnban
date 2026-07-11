package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type serverState struct {
	Version      string    `json:"version"`
	PID          int       `json:"pid"`
	URL          string    `json:"url"`
	ControlURL   string    `json:"control_url"`
	DBPath       string    `json:"db_path"`
	StartedAt    time.Time `json:"started_at"`
	ControlToken string    `json:"control_token"`
}

type lifecycleHealth struct {
	Service       string  `json:"service"`
	OK            bool    `json:"ok"`
	State         string  `json:"state"`
	Detail        string  `json:"detail,omitempty"`
	PersistenceOK bool    `json:"persistence_ok"`
	InFlight      int     `json:"in_flight"`
	ReservedUSD   float64 `json:"reserved_usd"`
}

type controlStatusPayload struct {
	OK        bool            `json:"ok"`
	PID       int             `json:"pid"`
	Version   string          `json:"version"`
	StartedAt time.Time       `json:"started_at"`
	Health    lifecycleHealth `json:"health"`
}

type statusResult struct {
	OK        bool             `json:"ok"`
	Active    bool             `json:"active"`
	Healthy   bool             `json:"healthy"`
	Version   string           `json:"version,omitempty"`
	PID       int              `json:"pid,omitempty"`
	StartedAt string           `json:"started_at,omitempty"`
	URL       string           `json:"url,omitempty"`
	Database  string           `json:"database,omitempty"`
	Health    *lifecycleHealth `json:"health,omitempty"`
	Issue     string           `json:"issue,omitempty"`
}

func serverStatePath(dbPath string) string {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		abs = dbPath
	}
	return abs + ".server.json"
}

func newControlToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("create local control token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func writeServerState(path string, state serverState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create server-state directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".burnban-server-*")
	if err != nil {
		return fmt.Errorf("create server-state file: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish server-state file: %w", err)
	}
	ok = true
	return nil
}

func readServerState(path string) (serverState, error) {
	var state serverState
	info, err := os.Lstat(path)
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() {
		return state, fmt.Errorf("server-state path is not a regular file: %s", terminalText(path, 300))
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return state, fmt.Errorf("server-state file permissions are too broad (%#o); run chmod 600 %s", info.Mode().Perm(), terminalText(path, 300))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return state, fmt.Errorf("read server state: %w", err)
	}
	if state.PID <= 0 || state.ControlURL == "" || len(state.ControlToken) < 32 {
		return state, fmt.Errorf("server-state file is incomplete: %s", terminalText(path, 300))
	}
	controlURL, err := parseControlURL(state.ControlURL)
	if err != nil {
		return state, fmt.Errorf("server-state control URL is invalid: %s", terminalText(path, 300))
	}
	state.ControlURL = strings.TrimSuffix(controlURL.String(), "/")
	if state.StartedAt.IsZero() {
		return state, fmt.Errorf("server-state start time is missing: %s", terminalText(path, 300))
	}
	return state, nil
}

// parseControlURL is intentionally stricter than a general HTTP URL parser.
// The lifecycle token is only ever sent over plaintext to an explicit local
// listener, never through an environment proxy or to a public endpoint.
func parseControlURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("control URL must be a plain HTTP loopback origin")
	}
	if !isLoopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("control URL host must be loopback")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || host == "" || port == "" {
		return nil, fmt.Errorf("control URL must include a port")
	}
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return nil, fmt.Errorf("control URL port is invalid")
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("control URL port is invalid")
	}
	return parsed, nil
}

func removeServerState(path, token string) {
	state, err := readServerState(path)
	if err == nil && subtle.ConstantTimeCompare([]byte(state.ControlToken), []byte(token)) == 1 {
		_ = os.Remove(path)
	}
}

func withControlToken(token string, control, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(want) > 0 && r.URL.Path != "" && len(r.URL.Path) >= len("/api/control/") && r.URL.Path[:len("/api/control/")] == "/api/control/" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("x-burnban-control-token")), want) == 1 {
			control.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func cmdStatus(args []string) error {
	return cmdStatusTo(args, os.Stdout)
}

func cmdStatusTo(args []string, out io.Writer) error {
	fs := newCommandFlagSet("status")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	state, err := readServerState(serverStatePath(*dbPath))
	if errors.Is(err, os.ErrNotExist) {
		issue := "burnban is not running (no server state found)"
		if *jsonOut {
			if encodeErr := json.NewEncoder(out).Encode(statusResult{Issue: issue}); encodeErr != nil {
				return encodeErr
			}
		}
		return errors.New(issue)
	}
	if err != nil {
		if *jsonOut {
			if encodeErr := json.NewEncoder(out).Encode(statusResult{Issue: terminalText(err.Error(), 300)}); encodeErr != nil {
				return encodeErr
			}
		}
		return err
	}
	result := statusResult{
		Version: state.Version, PID: state.PID, StartedAt: state.StartedAt.Format(time.RFC3339),
		URL: state.URL, Database: state.DBPath,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := fetchControlStatus(ctx, state)
	if err != nil {
		result.Issue = terminalText(fmt.Sprintf("burnban state exists for pid %d, but the server is unreachable: %v", state.PID, err), 400)
		if *jsonOut {
			if encodeErr := json.NewEncoder(out).Encode(result); encodeErr != nil {
				return encodeErr
			}
		}
		return errors.New(result.Issue)
	}
	result.Active = true
	result.Health = &status.Health
	result.Healthy = status.Health.OK && status.Health.PersistenceOK
	result.OK = result.Active && result.Healthy
	health := terminalText(status.Health.State, 80)
	if health == "" {
		health = "unknown"
	}
	if !result.Healthy && status.Health.Detail != "" {
		health += " — " + terminalText(status.Health.Detail, 160)
	}
	if *jsonOut {
		if !result.Healthy {
			result.Issue = "burnban is running but unhealthy: " + health
		}
		if err := json.NewEncoder(out).Encode(result); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "burnban %s is running\npid      %d\nstarted  %s\nurl      %s\ndb       %s\nhealth   %s · %d in flight · $%.4f reserved\n", terminalText(state.Version, 80), state.PID, state.StartedAt.Local().Format(time.RFC3339), terminalText(state.URL, 200), terminalText(state.DBPath, 200), health, status.Health.InFlight, status.Health.ReservedUSD)
	}
	if !result.Healthy {
		return fmt.Errorf("burnban is running but unhealthy: %s", health)
	}
	return nil
}

func cmdStop(args []string) error {
	fs := newCommandFlagSet("stop")
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs(fs); err != nil {
		return err
	}
	path := serverStatePath(*dbPath)
	state, err := readServerState(path)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("burnban is not running")
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	resp, err := controlRequest(ctx, state, http.MethodPost)
	cancel()
	if err != nil {
		return fmt.Errorf("ask burnban pid %d to stop: %w", state.PID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("stop returned %s: %s", resp.Status, safeResponseBody(resp.Body))
	}
	deadline := time.Now().Add(gracefulShutdownTimeout + 2*time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			fmt.Println("burnban stopped")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("burnban accepted the stop request but did not exit within %s", gracefulShutdownTimeout+2*time.Second)
}

func controlRequest(ctx context.Context, state serverState, method string) (*http.Response, error) {
	parsed, err := parseControlURL(state.ControlURL)
	if err != nil {
		return nil, err
	}
	var path string
	switch method {
	case http.MethodGet:
		path = "/api/control/status"
	case http.MethodPost:
		path = "/api/control/stop"
	default:
		return nil, fmt.Errorf("unsupported control method %q", method)
	}
	parsed.Path = path
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-burnban-control-token", state.ControlToken)
	return controlHTTPClient.Do(req)
}

var controlHTTPClient = &http.Client{
	Transport: directControlTransport(),
	Timeout:   3 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func directControlTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		direct := transport.Clone()
		direct.Proxy = nil
		return direct
	}
	return &http.Transport{Proxy: nil}
}

func fetchControlStatus(ctx context.Context, state serverState) (controlStatusPayload, error) {
	var payload controlStatusPayload
	resp, err := controlRequest(ctx, state, http.MethodGet)
	if err != nil {
		return payload, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return payload, fmt.Errorf("server status returned %s: %s", resp.Status, safeResponseBody(resp.Body))
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := dec.Decode(&payload); err != nil {
		return payload, fmt.Errorf("decode server status: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return payload, fmt.Errorf("decode server status: trailing content")
	}
	if !payload.OK || payload.Health.Service != "burnban" {
		return payload, fmt.Errorf("control endpoint did not return a Burnban status document")
	}
	if payload.PID != 0 && payload.PID != state.PID {
		return payload, fmt.Errorf("control endpoint pid %d does not match server state pid %d", payload.PID, state.PID)
	}
	return payload, nil
}

func serverStateAlive(state serverState) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := fetchControlStatus(ctx, state)
	return err == nil
}

func safeResponseBody(body io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(body, 4096))
	text := terminalText(string(data), 240)
	if text == "" {
		return "empty response"
	}
	return text
}

// newCommandFlagSet keeps lifecycle helpers testable: parse errors are
// returned rather than terminating the process from inside a subcommand.
func newCommandFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}
