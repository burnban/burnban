package proxy_test

import (
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

func TestProxyPersistsLayeredCostProvenance(t *testing.T) {
	response := `{"model":"claude-opus-4-7","usage":{"input_tokens":1000,"output_tokens":500}}`
	baseTable := func() *pricing.Table {
		return &pricing.Table{
			Models: map[string]pricing.Price{
				"claude-opus-4-7": {InputPerMTok: 5, OutputPerMTok: 25},
			},
			Contracts: []pricing.ContractPrice{{
				ID: "msa-us-priority", Provider: "anthropic", Model: "claude-opus-4-7",
				Region: "us", ServiceTier: "priority", EffectiveFrom: "2000-01-01",
				Price: pricing.Price{InputPerMTok: 1, OutputPerMTok: 2},
			}},
		}
	}
	request := func(t *testing.T, base string) {
		t.Helper()
		resp, err := http.Post(base+"/anthropic/v1/messages", "application/json", strings.NewReader(
			`{"model":"claude-opus-4-7","max_tokens":500,"service_tier":"priority","inference_geo":"us","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	row := func(t *testing.T, s *store.Store) store.Request {
		t.Helper()
		rows, err := s.Export(time.Time{})
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows=%+v err=%v", rows, err)
		}
		return rows[0]
	}

	t.Run("contract beats public list", func(t *testing.T) {
		srv, ledger := newProxyFor(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, response)
		}), baseTable())
		request(t, srv.URL)
		got := row(t, ledger)
		if got.CostSource != store.CostContract || got.CostSourceRef != "msa-us-priority" ||
			got.CostEffectiveFrom != "2000-01-01" || got.CostConfidence != store.ConfidenceContract ||
			math.Abs(got.CostUSD-.002) > 1e-12 {
			t.Fatalf("contract row = %+v", got)
		}
	})

	t.Run("provider final beats contract", func(t *testing.T) {
		srv, ledger := newProxyFor(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Burnban-Provider-Final-Cost-USD", "0.123456")
			_, _ = io.WriteString(w, response)
		}), baseTable())
		request(t, srv.URL)
		got := row(t, ledger)
		if got.CostSource != store.CostProviderFinal || got.CostConfidence != store.ConfidenceProviderFinal || got.CostUSD != .123456 {
			t.Fatalf("provider-final row = %+v", got)
		}
	})

	for name, headers := range map[string][]string{
		"malformed": {"NaN"},
		"duplicate": {"0.1", "0.2"},
	} {
		t.Run(name+" final fails closed", func(t *testing.T) {
			srv, ledger := newProxyFor(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				for _, value := range headers {
					w.Header().Add("X-Burnban-Provider-Final-Cost-USD", value)
				}
				_, _ = io.WriteString(w, response)
			}), baseTable())
			request(t, srv.URL)
			got := row(t, ledger)
			if got.PricingState != store.PricingUnknown || got.CostSource != store.CostUnknown || got.CostUSD != 0 {
				t.Fatalf("invalid final row = %+v", got)
			}
		})
	}
}
