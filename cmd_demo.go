package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/syft8/burnban/internal/budget"
	"github.com/syft8/burnban/internal/pricing"
	"github.com/syft8/burnban/internal/store"
)

// cmdDemo seeds a throwaway database with a realistic day of agent traffic
// and serves the dashboard on it — so anyone can see burnban alive in five
// seconds, before pointing real agents at it.
func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	port := fs.Int("port", 4242, "port for the demo server")
	dbPath := fs.String("db", filepath.Join(os.TempDir(), "burnban-demo.db"), "demo database path")
	force := fs.Bool("force", false, "replace an existing custom demo database")
	fs.Parse(args)

	customDB := false
	fs.Visit(func(f *flag.Flag) { customDB = customDB || f.Name == "db" })
	if customDB && !*force {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if _, err := os.Lstat(*dbPath + suffix); err == nil {
				return fmt.Errorf("refusing to replace existing demo database %s without --force", *dbPath)
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(*dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	if err := seedDemo(s); err != nil {
		s.Close()
		return err
	}
	for key, cap := range map[string]string{
		budget.KeyDailyCapUSD:   "40.00",
		budget.KeyWeeklyCapUSD:  "200.00",
		budget.KeyMonthlyCapUSD: "600.00",
	} {
		if err := s.SetSetting(key, cap); err != nil {
			s.Close()
			return err
		}
	}
	if err := s.Close(); err != nil {
		return err
	}

	fmt.Printf("🔥 demo traffic seeded (fake data, fresh every run) → %s\n\n", *dbPath)
	return cmdServe([]string{"--db", *dbPath, "--port", fmt.Sprint(*port)})
}

func seedDemo(s *store.Store) error {
	prices, err := pricing.Load()
	if err != nil {
		return err
	}
	// Fixed seed: the demo tells the same good story every time.
	rng := rand.New(rand.NewSource(41))

	type mix struct {
		model, agent string
		weight       int
		inLo, inHi   int64
		outLo, outHi int64
		cacheHot     bool
	}
	mixes := []mix{
		{"claude-fable-5", "claude-cli", 5, 2000, 9000, 200, 2500, true},
		{"claude-opus-4-7", "claude-cli", 4, 1500, 6000, 150, 1800, true},
		{"gpt-5.6-luna", "codex", 6, 800, 4000, 100, 1200, false},
		{"grok-4.5", "hermes", 4, 1000, 5000, 120, 1500, false},
		{"claude-haiku-4-5", "openclaw", 3, 400, 2000, 60, 700, true},
	}
	totalWeight := 0
	for _, m := range mixes {
		totalWeight += m.weight
	}
	pick := func() mix {
		n := rng.Intn(totalWeight)
		for _, m := range mixes {
			if n < m.weight {
				return m
			}
			n -= m.weight
		}
		return mixes[0]
	}

	now := time.Now()
	// A week of history so report/whatif/weekly budgets have something to
	// chew on; today gets the full diurnal curve the dashboard shows.
	for daysAgo := 6; daysAgo >= 0; daysAgo-- {
		for hoursAgo := 23; hoursAgo >= 0; hoursAgo-- {
			// A gentle diurnal curve: each demo day ramps up, peaks, cools off.
			phase := float64(23-hoursAgo) / 23
			busy := 3 + int(10*phase*(1.3-phase))
			if daysAgo > 0 {
				busy = busy * (5 + rng.Intn(4)) / 12 // past days: lighter, uneven
			}
			for i := 0; i < busy; i++ {
				m := pick()
				ts := now.AddDate(0, 0, -daysAgo).
					Add(-time.Duration(hoursAgo)*time.Hour - time.Duration(rng.Intn(3500))*time.Second)
				in := m.inLo + rng.Int63n(m.inHi-m.inLo)
				out := m.outLo + rng.Int63n(m.outHi-m.outLo)
				var cacheRead, cacheWrite int64
				if m.cacheHot {
					if rng.Float64() < 0.75 {
						cacheRead = 20000 + rng.Int63n(45000)
					} else {
						cacheWrite = 20000 + rng.Int63n(45000)
					}
				}
				price, ok := prices.Lookup(m.model)
				if !ok {
					return fmt.Errorf("demo model %q missing from pricing table", m.model)
				}
				req := store.Request{
					Ts:       ts,
					Provider: providerOf(m.model),
					Model:    m.model,
					Agent:    m.agent,
					Session:  fmt.Sprintf("%s-%d", m.agent, rng.Intn(4)),
					InTokens: in, OutTokens: out,
					CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
					CostUSD:   pricing.Cost(price, in, out, cacheRead, cacheWrite),
					LatencyMs: 400 + int64(rng.Intn(9000)),
					Status:    200,
					Streamed:  true,
					Priced:    true,
					// A narrow-ish hash space, so a handful of "duplicate
					// request" waste receipts show up — the point of the demo.
					BodyHash: fmt.Sprintf("demo%04d", rng.Intn(2000)),
				}
				if err := s.Insert(req); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func providerOf(model string) string {
	switch {
	case strings.HasPrefix(model, "claude"):
		return "anthropic"
	case strings.HasPrefix(model, "grok"):
		return "xai"
	default:
		return "openai"
	}
}
