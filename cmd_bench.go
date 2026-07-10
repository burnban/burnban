package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/pricing"
	"github.com/syft8/burnban/internal/proxy"
	"github.com/syft8/burnban/internal/store"
)

// cmdBench measures what burnban adds to a request: it stands up a fake
// instant upstream on loopback, runs the same traffic direct and through a
// fully armed proxy (metering, pricing, cap checks against a set budget),
// and prints the difference. Loopback with a zero-latency upstream is the
// worst possible case for a proxy — real inference calls are seconds.
func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	n := fs.Int("requests", 500, "requests per pass")
	conc := fs.Int("concurrency", 4, "parallel clients")
	fs.Parse(args)

	if *n < 1 || *conc < 1 {
		return fmt.Errorf("requests and concurrency must be >= 1")
	}
	res, err := runBench(*n, *conc)
	if err != nil {
		return err
	}

	fmt.Printf("🔥 burnban bench — %d requests × %d clients, instant loopback upstream (worst case)\n\n", *n, *conc)
	w := func(label string, s latStats) {
		fmt.Printf("  %-9s %9s %9s %9s %9s\n", label, fmtDur(s.p50), fmtDur(s.p90), fmtDur(s.p99), fmtDur(s.mean))
	}
	fmt.Printf("  %-9s %9s %9s %9s %9s\n", "", "p50", "p90", "p99", "mean")
	w("direct", res.direct)
	w("burnban", res.proxied)
	fmt.Println("  " + strings.Repeat("─", 49))
	w("added", res.overhead())

	fmt.Printf(`
  every proxied request was metered, priced, and checked against a live
  budget cap; %d/%d landed in the ledger. Against a real inference call
  measured in seconds, ~%s is a rounding error.
`, res.recorded, *n, fmtDur(res.overhead().p50))
	return nil
}

type latStats struct {
	p50, p90, p99, mean time.Duration
}

type benchResult struct {
	direct, proxied latStats
	recorded        int64
}

func (r *benchResult) overhead() latStats {
	sub := func(a, b time.Duration) time.Duration {
		if a < b {
			return 0
		}
		return a - b
	}
	return latStats{
		p50:  sub(r.proxied.p50, r.direct.p50),
		p90:  sub(r.proxied.p90, r.direct.p90),
		p99:  sub(r.proxied.p99, r.direct.p99),
		mean: sub(r.proxied.mean, r.direct.mean),
	}
}

const benchUpstreamBody = `{"id":"msg_bench","type":"message","role":"assistant","model":"claude-bench-1","content":[{"type":"text","text":"All four engines are running and the meter shows a clean burn. This reply is padded to a realistic size so the benchmark moves response bytes that look like production traffic rather than a toy payload."}],"stop_reason":"end_turn","usage":{"input_tokens":1200,"output_tokens":350,"cache_read_input_tokens":8000,"cache_creation_input_tokens":0}}`

func runBench(n, conc int) (*benchResult, error) {
	upstream, upURL, err := serveLoopback(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, benchUpstreamBody)
	}))
	if err != nil {
		return nil, err
	}
	defer upstream.Close()

	dir, err := os.MkdirTemp("", "burnban-bench")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	s, err := store.Open(filepath.Join(dir, "bench.db"))
	if err != nil {
		return nil, err
	}
	defer s.Close()
	// Arm a cap that never trips: the point is to pay for the check.
	if err := s.SetSetting(budget.KeyDailyCapUSD, "1000000"); err != nil {
		return nil, err
	}

	prices := &pricing.Table{Models: map[string]pricing.Price{
		"claude-bench-1": {InputPerMTok: 5, OutputPerMTok: 25, CacheReadMult: 0.1, CacheWriteMult: 1.25},
	}}
	p, err := proxy.New(s, prices, map[string]proxy.Upstream{"anthropic": {URL: upURL, Shape: "anthropic"}})
	if err != nil {
		return nil, err
	}
	p.Logf = func(string, ...any) {}
	proxySrv, proxyURL, err := serveLoopback(p.Handler())
	if err != nil {
		return nil, err
	}
	defer proxySrv.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: conc * 2}}
	warmup := min(50, n)

	measure := func(url string) (latStats, error) {
		if err := runPass(client, url, warmup, conc, nil); err != nil {
			return latStats{}, fmt.Errorf("warmup: %w", err)
		}
		lats := make([]time.Duration, 0, n)
		if err := runPass(client, url, n, conc, &lats); err != nil {
			return latStats{}, err
		}
		return summarizeLats(lats), nil
	}

	direct, err := measure(upURL + "/v1/messages")
	if err != nil {
		return nil, err
	}
	// Count ledger rows around the measured pass, so `recorded` reports
	// exactly what the timed requests wrote — warmup rows excluded by
	// measurement, not by assumption.
	if err := runPass(client, proxyURL+"/anthropic/v1/messages", warmup, conc, nil); err != nil {
		return nil, fmt.Errorf("warmup: %w", err)
	}
	rowsBefore, err := ledgerRows(s)
	if err != nil {
		return nil, err
	}
	lats := make([]time.Duration, 0, n)
	if err := runPass(client, proxyURL+"/anthropic/v1/messages", n, conc, &lats); err != nil {
		return nil, err
	}
	proxied := summarizeLats(lats)
	rowsAfter, err := ledgerRows(s)
	if err != nil {
		return nil, err
	}
	return &benchResult{direct: direct, proxied: proxied, recorded: rowsAfter - rowsBefore}, nil
}

func ledgerRows(s *store.Store) (int64, error) {
	sum, err := s.Summarize(time.Unix(0, 0))
	if err != nil {
		return 0, err
	}
	return sum.Requests, nil
}

const benchRequestBody = `{"model":"claude-bench-1","max_tokens":512,"messages":[{"role":"user","content":"benchmark request with a plausible amount of prompt text attached to it"}]}`

// runPass fires total POSTs at url from conc workers; when lats is non-nil
// each request's wall time is appended to it. It always waits for every
// worker to finish before returning — an early error must not leave
// stragglers firing into the next measurement pass.
func runPass(client *http.Client, url string, total, conc int, lats *[]time.Duration) error {
	var (
		next    atomic.Int64
		wg      sync.WaitGroup
		mu      sync.Mutex
		firstEr error
	)
	perWorker := make([][]time.Duration, conc)
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for next.Add(1) <= int64(total) {
				start := time.Now()
				resp, err := client.Post(url, "application/json", strings.NewReader(benchRequestBody))
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						err = fmt.Errorf("status %d", resp.StatusCode)
					}
				}
				if err != nil {
					mu.Lock()
					if firstEr == nil {
						firstEr = err
					}
					mu.Unlock()
					return
				}
				perWorker[w] = append(perWorker[w], time.Since(start))
			}
		}(w)
	}
	wg.Wait()
	if firstEr != nil {
		return firstEr
	}
	if lats != nil {
		for _, ws := range perWorker {
			*lats = append(*lats, ws...)
		}
	}
	return nil
}

func summarizeLats(lats []time.Duration) latStats {
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	// Nearest-rank percentiles: ceil(p·n) — anything else drops the tail
	// sample the percentile exists to report.
	pct := func(p float64) time.Duration {
		if len(lats) == 0 {
			return 0
		}
		i := int(math.Ceil(p*float64(len(lats)))) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(lats) {
			i = len(lats) - 1
		}
		return lats[i]
	}
	var sum time.Duration
	for _, d := range lats {
		sum += d
	}
	mean := time.Duration(0)
	if len(lats) > 0 {
		mean = sum / time.Duration(len(lats))
	}
	return latStats{p50: pct(0.50), p90: pct(0.90), p99: pct(0.99), mean: mean}
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

// serveLoopback binds a throwaway HTTP server to 127.0.0.1:0.
func serveLoopback(h http.Handler) (*http.Server, string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	return srv, "http://" + l.Addr().String(), nil
}
