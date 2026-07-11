package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"time"
)

// cmdDesktop is the one-click desktop entry point. It deliberately reuses
// serve mode: there is no second daemon, mock database, or desktop-only state.
func cmdDesktop(args []string) error {
	return cmdServeMode(args, true)
}

func openDashboard(url string) error {
	name, args, err := dashboardCommand(runtime.GOOS, url)
	if err != nil {
		return err
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the short-lived platform opener without holding up the meter.
	go func() { _ = cmd.Wait() }()
	return nil
}

func dashboardURL(base, token string) string {
	// Never put the long-lived team token in a URL, browser history, reverse-
	// proxy log, or opener process arguments. The public dashboard shell asks
	// for it and keeps it in tab-scoped session storage.
	return base
}

// burnbanRunning distinguishes an already-running desktop meter from an
// unrelated process occupying the port. This makes repeated icon clicks open
// the existing dashboard instead of failing with "address already in use".
func burnbanRunning(base, token string) bool {
	req, err := http.NewRequest(http.MethodGet, base+"/api/summary", nil)
	if err != nil {
		return false
	}
	if token != "" {
		req.Header.Set("x-burnban-token", token)
	}
	client := &http.Client{
		Timeout:       time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var summary struct {
		Version string `json:"version"`
	}
	return json.NewDecoder(resp.Body).Decode(&summary) == nil && summary.Version != ""
}

func dashboardCommand(goos, url string) (string, []string, error) {
	switch goos {
	case "darwin":
		return "open", []string{url}, nil
	case "linux":
		return "xdg-open", []string{url}, nil
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}, nil
	default:
		return "", nil, fmt.Errorf("opening a browser is unsupported on %s", goos)
	}
}
