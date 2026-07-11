package main

import (
	"fmt"
	"testing"

	"github.com/burnban/burnban/internal/pricing"
	"github.com/burnban/burnban/internal/store"
)

var benchmarkWhatifCost float64

func BenchmarkWhatif100KRowsAcross30Models(b *testing.B) {
	const rowCount = 100_000
	const modelCount = 30
	rows := make([]store.TokenRow, rowCount)
	for i := range rows {
		rows[i] = store.TokenRow{
			In: int64(100 + i%300_000), Out: int64(20 + i%2_000),
			CacheRead: int64(i % 10_000), CacheWrite: int64(i % 1_000),
			PricingState: store.PricingPriced, CostUSD: float64(i%1000) / 100_000,
		}
	}
	models := make([]struct {
		name  string
		price pricing.Price
	}, modelCount)
	for i := range models {
		models[i].name = fmt.Sprintf("target-%02d", i)
		models[i].price = pricing.Price{
			InputPerMTok: 1 + float64(i)/10, OutputPerMTok: 5 + float64(i)/5,
			CacheReadMult: .1, CacheWriteMult: 1.25,
			LongContextThreshold: 200_000 + int64(i%3)*36_000,
			LongInputMult:        2, LongOutputMult: 1.5,
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var total float64
		for _, model := range models {
			total += repriceRequests(model.name, model.price, rows)
		}
		benchmarkWhatifCost = total
	}
	b.ReportMetric(rowCount*modelCount, "request-models/op")
}
