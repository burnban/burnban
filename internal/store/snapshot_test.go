package store_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/store"
)

func TestReadSnapshotReconcilesWhileWALWriterCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.db")
	reader, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-time.Hour)
	if err := reader.Insert(store.Request{
		Ts: now.Add(-time.Minute), Provider: "openai", Model: "before", Agent: "alpha",
		CostUSD: 1, PricingState: store.PricingPriced,
	}); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"cap_daily_usd":             "10",
		"ban_active":                "0",
		"cap_override_day":          "yesterday",
		"cap_agent_daily_usd:alpha": "5",
		"external_cap_daily_usd":    "15",
		"external_cap_weekly_usd":   "30",
		"external_cap_monthly_usd":  "60",
	} {
		if err := reader.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}

	err = reader.ReadSnapshot(func(snapshot *store.ReadSnapshot) error {
		// The rich aggregate is the first read and therefore fixes the SQLite
		// snapshot used by every query below.
		sum, err := snapshot.Summarize(cutoff)
		if err != nil {
			return err
		}

		writeDone := make(chan error, 1)
		go func() {
			if err := writer.Insert(store.Request{
				Ts: now, Provider: "anthropic", Model: "after", Agent: "beta",
				CostUSD: 2, PricingState: store.PricingPriced,
			}); err != nil {
				writeDone <- err
				return
			}
			for key, value := range map[string]string{
				"cap_daily_usd":            "20",
				"ban_active":               "1",
				"cap_override_day":         now.Format("2006-01-02"),
				"cap_agent_daily_usd:beta": "7",
			} {
				if err := writer.SetSetting(key, value); err != nil {
					writeDone <- err
					return
				}
			}
			writeDone <- nil
		}()

		select {
		case err := <-writeDone:
			if err != nil {
				return fmt.Errorf("concurrent writer: %w", err)
			}
		case <-time.After(2 * time.Second):
			return fmt.Errorf("WAL writer was blocked by read snapshot")
		}

		lastHour, err := snapshot.SpentSince(cutoff)
		if err != nil {
			return err
		}
		windows, err := snapshot.SpentSinceMulti([]time.Time{cutoff, now.Add(-30 * time.Minute)})
		if err != nil {
			return err
		}
		usage, err := snapshot.UsageSinceForAgents(cutoff, []string{"alpha", "beta"})
		if err != nil {
			return err
		}
		caps, err := snapshot.SettingsWithPrefix("cap_agent_daily_usd:")
		if err != nil {
			return err
		}
		settings, err := snapshot.GetSettings("cap_daily_usd", "ban_active", "cap_override_day")
		if err != nil {
			return err
		}
		metrics, err := snapshot.LifetimeMetrics()
		if err != nil {
			return err
		}

		if sum.Requests != 1 || sum.Cost != 1 || lastHour != sum.Cost ||
			len(windows) != 2 || windows[0] != sum.Cost || windows[1] != sum.Cost {
			return fmt.Errorf("snapshot totals do not reconcile: sum=%+v lastHour=%v windows=%v", sum, lastHour, windows)
		}
		if len(sum.Models) != 1 || sum.Models[0].Cost != sum.Cost ||
			len(sum.Agents) != 1 || sum.Agents[0].Cost != sum.Cost {
			return fmt.Errorf("snapshot dimensions do not reconcile: sum=%+v", sum)
		}
		if metrics.Requests != sum.Requests || metrics.Cost != sum.Cost ||
			len(metrics.Models) != 1 || metrics.Models[0].Cost != metrics.Cost ||
			len(metrics.Agents) != 1 || metrics.Agents[0].Cost != metrics.Cost {
			return fmt.Errorf("snapshot metrics do not reconcile: summary=%+v metrics=%+v", sum, metrics)
		}
		if usage["alpha"].Requests != 1 || usage["alpha"].Cost != sum.Cost ||
			usage["beta"].Requests != 0 || usage["beta"].Cost != 0 {
			return fmt.Errorf("snapshot capped-agent usage=%+v", usage)
		}
		if len(caps) != 1 || caps["alpha"] != "5" {
			return fmt.Errorf("snapshot agent caps=%+v", caps)
		}
		if settings["cap_daily_usd"] != "10" || settings["ban_active"] != "0" ||
			settings["cap_override_day"] != "yesterday" {
			return fmt.Errorf("snapshot settings=%+v", settings)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The writes committed while the read transaction was open and become
	// visible immediately after it closes.
	after, err := reader.Summarize(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := reader.GetSettings("cap_daily_usd", "ban_active", "cap_override_day")
	if err != nil {
		t.Fatal(err)
	}
	if after.Requests != 2 || after.Cost != 3 || settings["cap_daily_usd"] != "20" ||
		settings["ban_active"] != "1" || settings["cap_override_day"] != now.Format("2006-01-02") {
		t.Fatalf("committed state missing after snapshot: sum=%+v settings=%+v", after, settings)
	}
}
