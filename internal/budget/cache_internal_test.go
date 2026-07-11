package budget

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestUsageCacheEvictsInactiveAndRolledKeys(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	activeGlobal := usageCacheKey{startUnixNano: DayStart(now).UnixNano()}
	oldGlobal := usageCacheKey{startUnixNano: DayStart(now.AddDate(0, 0, -1)).UnixNano()}
	alphaCurrent := usageCacheKey{startUnixNano: DayStart(now).UnixNano(), agent: "alpha"}
	alphaOld := usageCacheKey{startUnixNano: DayStart(now.AddDate(0, 0, -1)).UnixNano(), agent: "alpha"}
	betaCurrent := usageCacheKey{startUnixNano: DayStart(now).UnixNano(), agent: "beta"}
	gammaOld := usageCacheKey{startUnixNano: DayStart(now.AddDate(0, 0, -1)).UnixNano(), agent: "gamma"}
	g := &Guard{
		agentCacheDay: DayStart(now.AddDate(0, 0, -1)).UnixNano(), agentCacheEntries: 4,
		usageCache: map[usageCacheKey]store.BudgetUsage{
			activeGlobal: {}, oldGlobal: {}, alphaCurrent: {}, alphaOld: {}, betaCurrent: {}, gammaOld: {},
		},
		activeGlobalKeys: map[usageCacheKey]struct{}{activeGlobal: {}, oldGlobal: {}},
	}
	g.reconcileUsageCacheLocked([]usageRequest{{start: DayStart(now)}}, "alpha", true, DayStart(now))
	for _, key := range []usageCacheKey{activeGlobal, alphaCurrent, betaCurrent} {
		if _, ok := g.usageCache[key]; !ok {
			t.Errorf("active/unrelated key was evicted: %+v", key)
		}
	}
	for _, key := range []usageCacheKey{oldGlobal, alphaOld, gammaOld} {
		if _, ok := g.usageCache[key]; ok {
			t.Errorf("inactive/rolled key survived: %+v", key)
		}
	}
	g.reconcileUsageCacheLocked(nil, "alpha", false, DayStart(now))
	if len(g.activeGlobalKeys) != 0 {
		t.Fatalf("inactive global keys=%+v", g.activeGlobalKeys)
	}
	if _, ok := g.usageCache[alphaCurrent]; ok {
		t.Fatal("inactive current-agent key survived")
	}
	if _, ok := g.usageCache[betaCurrent]; !ok {
		t.Fatal("checking alpha evicted another agent's warm key")
	}
}

func TestSettleTouchesOnlyActiveGlobalsAndReservationAgent(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "settle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	activeGlobal := usageCacheKey{startUnixNano: DayStart(now).UnixNano()}
	inactiveGlobal := usageCacheKey{startUnixNano: DayStart(now.AddDate(0, 0, -1)).UnixNano()}
	alpha := usageCacheKey{startUnixNano: DayStart(now).UnixNano(), agent: "alpha"}
	beta := usageCacheKey{startUnixNano: DayStart(now).UnixNano(), agent: "beta"}
	g := &Guard{
		S: s, cacheRevision: s.RequestRevision(), inFlight: 1,
		usageCache: map[usageCacheKey]store.BudgetUsage{
			activeGlobal: {SpentUSD: 1}, inactiveGlobal: {SpentUSD: 2},
			alpha: {SpentUSD: 3}, beta: {SpentUSD: 4},
		},
		activeGlobalKeys: map[usageCacheKey]struct{}{activeGlobal: {}},
	}
	r := &Reservation{guard: g, agent: "alpha", agentDay: alpha}
	if err := r.Settle(store.Request{
		Ts: now, Provider: "openai", Agent: "alpha", CostUSD: 0.5,
		UsageState: store.UsageExact, PricingState: store.PricingPriced,
	}); err != nil {
		t.Fatal(err)
	}
	if g.usageCache[activeGlobal].SpentUSD != 1.5 || g.usageCache[alpha].SpentUSD != 3.5 {
		t.Fatalf("active settlement totals=%+v", g.usageCache)
	}
	if g.usageCache[inactiveGlobal].SpentUSD != 2 || g.usageCache[beta].SpentUSD != 4 {
		t.Fatalf("settlement touched inactive/unrelated keys=%+v", g.usageCache)
	}
}

func TestAgentCacheSameDayCapChurnStaysBounded(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	g := &Guard{
		agentCacheDay: DayStart(now).UnixNano(),
		usageCache:    make(map[usageCacheKey]store.BudgetUsage),
	}
	for i := 0; i < maxAgentUsageCacheEntries+250; i++ {
		key := usageCacheKey{
			startUnixNano: DayStart(now).UnixNano(), agent: fmt.Sprintf("churn-%06d", i),
		}
		g.usageCache[key] = store.BudgetUsage{}
		g.agentCacheEntries++
		g.boundAgentUsageCacheLocked(map[usageCacheKey]struct{}{key: {}})
		if g.agentCacheEntries > maxAgentUsageCacheEntries {
			t.Fatalf("iteration %d entries=%d exceeds %d", i, g.agentCacheEntries, maxAgentUsageCacheEntries)
		}
	}
	agentEntries := 0
	for key := range g.usageCache {
		if key.agent != "" {
			agentEntries++
		}
	}
	if agentEntries != g.agentCacheEntries || agentEntries > maxAgentUsageCacheEntries {
		t.Fatalf("tracked=%d actual=%d max=%d", g.agentCacheEntries, agentEntries, maxAgentUsageCacheEntries)
	}
}

func BenchmarkReservationSettleCachedAgents(b *testing.B) {
	for _, agents := range []int{1, 100_000} {
		b.Run(fmt.Sprintf("agents_%d", agents), func(b *testing.B) {
			s, err := store.Open(filepath.Join(b.TempDir(), "settle.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = s.Close() })
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			global := usageCacheKey{startUnixNano: DayStart(now).UnixNano()}
			target := usageCacheKey{startUnixNano: DayStart(now).UnixNano(), agent: "agent-000000"}
			cache := make(map[usageCacheKey]store.BudgetUsage, agents+1)
			cache[global] = store.BudgetUsage{}
			for i := 0; i < agents; i++ {
				cache[usageCacheKey{
					startUnixNano: DayStart(now).UnixNano(), agent: fmt.Sprintf("agent-%06d", i),
				}] = store.BudgetUsage{}
			}
			g := &Guard{
				S: s, cacheRevision: s.RequestRevision(), usageCache: cache,
				activeGlobalKeys: map[usageCacheKey]struct{}{global: {}},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				g.inFlight++
				r := &Reservation{guard: g, agent: target.agent, agentDay: target}
				if err := r.Settle(store.Request{
					Ts: now, Provider: "openai", Agent: target.agent, CostUSD: 0.001,
					UsageState: store.UsageExact, PricingState: store.PricingPriced,
				}); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(agents), "cached_agents")
		})
	}
}

func BenchmarkGuardWarmCheckCachedAgents(b *testing.B) {
	for _, agents := range []int{1, 100_000} {
		b.Run(fmt.Sprintf("agents_%d", agents), func(b *testing.B) {
			s, err := store.Open(filepath.Join(b.TempDir(), "check.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = s.Close() })
			if err := s.SetSetting(KeyDailyCapUSD, "1000"); err != nil {
				b.Fatal(err)
			}
			if err := s.SetSetting(KeyAgentCapPrefix+"agent-000000", "1000"); err != nil {
				b.Fatal(err)
			}
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			global := usageCacheKey{startUnixNano: DayStart(now).UnixNano()}
			cache := make(map[usageCacheKey]store.BudgetUsage, agents+1)
			cache[global] = store.BudgetUsage{}
			for i := 0; i < agents; i++ {
				cache[usageCacheKey{
					startUnixNano: DayStart(now).UnixNano(), agent: fmt.Sprintf("agent-%06d", i),
				}] = store.BudgetUsage{}
			}
			g := &Guard{
				S: s, cacheRevision: s.RequestRevision(), usageCache: cache,
				activeGlobalKeys: map[usageCacheKey]struct{}{global: {}},
				agentCacheDay:    DayStart(now).UnixNano(), agentCacheEntries: agents,
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if denial, err := g.Check(now, "agent-000000"); err != nil || denial != nil {
					b.Fatalf("warm check denial=%+v err=%v", denial, err)
				}
			}
			b.ReportMetric(float64(agents), "cached_agents")
		})
	}
}
