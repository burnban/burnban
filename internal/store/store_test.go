package store_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
	_ "modernc.org/sqlite"
)

func TestOpenUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	dir := filepath.Join(t.TempDir(), "private", "burnban")
	path := filepath.Join(dir, "ledger.db")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode = %o, want 600", got)
	}
}

func TestRequestJSONUsesStableNamesAndHidesFingerprint(t *testing.T) {
	b, err := json.Marshal(store.Request{
		Ts: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC), Provider: "anthropic",
		CacheWrite1hTokens: 12, BodyHash: "private-fingerprint", ServiceTier: "priority",
		InferenceGeo: "us", ServerToolCalls: 2, FeeUnpriced: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(b, &value); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ts", "provider", "cache_write_1h_tokens", "service_tier", "inference_geo", "server_tool_calls", "fee_unpriced"} {
		if _, ok := value[key]; !ok {
			t.Errorf("JSON missing %q: %s", key, b)
		}
	}
	if _, ok := value["BodyHash"]; ok || value["body_hash"] != nil {
		t.Fatalf("fingerprint leaked in JSON: %s", b)
	}
}

func TestLegacySchemaMigratesAndClassifiesRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE requests (
			id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, provider TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '', agent TEXT NOT NULL DEFAULT '', session TEXT NOT NULL DEFAULT '',
			in_tokens INTEGER NOT NULL DEFAULT 0, out_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0, latency_ms INTEGER NOT NULL DEFAULT 0,
			status INTEGER NOT NULL DEFAULT 0, streamed INTEGER NOT NULL DEFAULT 0,
			estimated INTEGER NOT NULL DEFAULT 0, priced INTEGER NOT NULL DEFAULT 1,
			body_hash TEXT NOT NULL DEFAULT '');
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO requests(ts,provider,status,priced,body_hash) VALUES
			('2026-07-10T10:00:00Z','anthropic',500,0,'legacy-unsalted-hash');
		INSERT INTO requests(ts,provider,model,in_tokens,status,priced) VALUES
			('2026-07-10T11:00:00Z','openai','new-model',100,200,0);
		INSERT INTO requests(ts,provider,model,in_tokens,cost_usd,status,priced) VALUES
			('2026-07-10T12:00:00Z','openai','known-model',100,0.01,200,1);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sum, err := s.Summarize(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 3 || sum.UnknownPricing != 1 || sum.Unpriced != 1 || sum.Unmetered != 1 {
		t.Fatalf("migrated summary = %+v", sum)
	}
	if want := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC); !sum.LastRequestAt.Equal(want) {
		t.Fatalf("last request = %v, want %v", sum.LastRequestAt, want)
	}
	rows, err := s.Export(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].UsageState != store.UsageMissing || rows[0].PricingState != store.PricingUnmetered {
		t.Fatalf("failed row states = %s/%s", rows[0].UsageState, rows[0].PricingState)
	}
	if rows[1].UsageState != store.UsageExact || rows[1].PricingState != store.PricingUnknown {
		t.Fatalf("unknown row states = %s/%s", rows[1].UsageState, rows[1].PricingState)
	}
	if rows[0].BodyHash != "" || sum.DupGroups != 0 {
		t.Fatalf("legacy unkeyed fingerprint survived privacy migration: row=%+v summary=%+v", rows[0], sum)
	}
}

func TestUsageStatesProbeAndPrune(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "states.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := time.Now().Add(-48 * time.Hour)
	requests := []store.Request{
		{Ts: old, Provider: "openai", Status: 500},
		{Ts: time.Now(), Provider: "openai", Model: "unknown", InTokens: 10, Status: 200},
		{Ts: time.Now(), Provider: "openai", Model: "known", InTokens: 10, Status: 200,
			UsageState: store.UsagePartial, PricingState: store.PricingPriced, EnforcementUnsafe: true},
	}
	for _, request := range requests {
		if err := s.Insert(request); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Probe(); err != nil {
		t.Fatalf("durable probe: %v", err)
	}
	sum, err := s.Summarize(time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unmetered != 1 || sum.UnknownPricing != 1 || sum.Incomplete != 1 || sum.EnforcementGaps != 1 {
		t.Fatalf("state summary = %+v", sum)
	}
	deleted, err := s.Prune(time.Now().Add(-24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("pruned %d rows, want 1", deleted)
	}
	remaining, err := s.Summarize(time.Unix(0, 0))
	if err != nil || remaining.Requests != 2 {
		t.Fatalf("remaining summary = %+v, err=%v", remaining, err)
	}
}

func TestLifetimeMetricsUsesDurableStateFields(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	for _, request := range []store.Request{
		{Ts: now.Add(-time.Second), Provider: "openai", Model: "known", Agent: "build",
			InTokens: 10, CostUSD: 0.01, UsageState: store.UsageExact, PricingState: store.PricingPriced},
		{Ts: now, Provider: "anthropic", Model: "new", Agent: "build", InTokens: 20,
			UsageState: store.UsagePartial, PricingState: store.PricingUnknown,
			EnforcementUnsafe: true, FeeUnpriced: true},
		{Ts: now, Provider: "gemini", Status: 500,
			UsageState: store.UsageMissing, PricingState: store.PricingUnmetered},
	} {
		if err := s.Insert(request); err != nil {
			t.Fatal(err)
		}
	}
	metrics, err := s.LifetimeMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Requests != 3 || metrics.Cost != 0.01 || metrics.UnknownPricing != 1 ||
		metrics.Unmetered != 1 || metrics.Incomplete != 1 || metrics.EnforcementGaps != 1 ||
		metrics.FeeUnpriced != 1 || !metrics.LastRequestAt.Equal(now) {
		t.Fatalf("lifetime metrics = %+v", metrics)
	}
	if len(metrics.Models) != 2 || len(metrics.Agents) != 1 || metrics.Agents[0].Requests != 2 {
		t.Fatalf("lifetime dimensions = models=%+v agents=%+v", metrics.Models, metrics.Agents)
	}
}

func TestSpentSinceForAgentsIncludesMoreThanSummaryTopTwenty(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	agents := make([]string, 0, 31)
	for i := 0; i < 30; i++ {
		agent := fmt.Sprintf("agent-%02d", i)
		agents = append(agents, agent)
		if err := s.Insert(store.Request{
			Ts: now, Provider: "openai", Model: fmt.Sprintf("model-%02d", i), Agent: agent, CostUSD: float64(i+1) / 100,
			PricingState: store.PricingPriced,
		}); err != nil {
			t.Fatal(err)
		}
	}
	agents = append(agents, "configured-with-no-spend", "agent-05") // zero row + duplicate input
	for i := 30; i < 850; i++ {
		agents = append(agents, fmt.Sprintf("configured-%03d", i))
	}
	spend, err := s.SpentSinceForAgents(now.Add(-time.Minute), agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(spend) != 851 || spend["agent-00"] != 0.01 || spend["agent-29"] != 0.30 ||
		spend["configured-with-no-spend"] != 0 {
		t.Fatalf("batched agent spend = len=%d first=%v last=%v zero=%v", len(spend),
			spend["agent-00"], spend["agent-29"], spend["configured-with-no-spend"])
	}
	usage, err := s.UsageSinceForAgents(now.Add(-time.Minute), []string{"agent-05", "configured-with-no-spend"})
	if err != nil {
		t.Fatal(err)
	}
	if usage["agent-05"].Requests != 1 || usage["agent-05"].Cost != 0.06 ||
		usage["configured-with-no-spend"].Requests != 0 {
		t.Fatalf("batched agent usage = %+v", usage)
	}
	summary, err := s.Summarize(now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Models) != 20 || len(summary.Agents) != 20 || summary.ModelOther == nil || summary.AgentOther == nil ||
		summary.ModelOther.Requests != 10 || summary.AgentOther.Requests != 10 ||
		abs(summary.ModelOther.Cost-0.55) > 1e-9 || abs(summary.AgentOther.Cost-0.55) > 1e-9 {
		t.Fatalf("top-20 remainder = models=%d/%+v agents=%d/%+v", len(summary.Models), summary.ModelOther, len(summary.Agents), summary.AgentOther)
	}
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func TestLeaseAtomicAcquireRenewReleaseAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.db")
	stores := make([]*store.Store, 2)
	for i := range stores {
		var err error
		stores[i], err = store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer stores[i].Close()
	}

	type result struct {
		index int
		lease *store.Lease
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, len(stores))
	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Add(1)
		go func(index int, s *store.Store) {
			defer wg.Done()
			<-start
			lease, err := s.AcquireLease("serve", 200*time.Millisecond)
			results <- result{index: index, lease: lease, err: err}
		}(i, s)
	}
	close(start)
	wg.Wait()
	close(results)

	var winner result
	wins, held := 0, 0
	for got := range results {
		switch {
		case got.err == nil:
			winner, wins = got, wins+1
		case errors.Is(got.err, store.ErrLeaseHeld):
			held++
		default:
			t.Fatalf("acquire error = %v", got.err)
		}
	}
	if wins != 1 || held != 1 || winner.lease.Owner() == "" {
		t.Fatalf("atomic acquire: wins=%d held=%d winner=%+v", wins, held, winner)
	}

	oldExpiry := winner.lease.ExpiresAt()
	time.Sleep(2 * time.Millisecond)
	if err := winner.lease.Renew(); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !winner.lease.ExpiresAt().After(oldExpiry) {
		t.Fatalf("renewed expiry %v did not extend %v", winner.lease.ExpiresAt(), oldExpiry)
	}
	if err := winner.lease.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	other := stores[1-winner.index]
	reacquired, err := other.AcquireLease("serve", time.Second)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := reacquired.Release(); err != nil {
		t.Fatal(err)
	}

	stale, err := stores[0].AcquireLease("crash-recovery", 25*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stores[1].AcquireLease("crash-recovery", time.Second); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("unexpired lease acquire error = %v, want ErrLeaseHeld", err)
	}
	time.Sleep(75 * time.Millisecond)
	if err := stale.Renew(); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("expired owner renew error = %v, want ErrLeaseHeld", err)
	}
	recovered, err := stores[1].AcquireLease("crash-recovery", time.Second)
	if err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
	if recovered.Owner() == stale.Owner() {
		t.Fatal("expired lease was not assigned a new owner")
	}
	if err := recovered.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCreatesPrivateDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	dir := filepath.Join(t.TempDir(), "new", "burnban")
	s, err := store.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %o, want 700", got)
	}
}

func TestOpenPreservesFileURIQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uri.db")
	s, err := store.Open("file:" + path + "?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("URI database was not created at %s: %v", path, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("URI database mode = %o, want 600", got)
		}
	}
}

func TestFileURIMemoryModeDoesNotCreateDiskFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "named-memory")
	s, err := store.Open("file:" + path + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mode=memory created a disk file at %s: %v", path, err)
	}
}
