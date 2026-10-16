package ledger

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// BenchmarkPerformance answers a 30-day, day-bucket, group=model query over
// 1,000,000 eligible rows (SPEC target: < 2 s on a laptop).
func BenchmarkPerformance(b *testing.B) {
	l, err := Open(filepath.Join(b.TempDir(), "bench.db"), nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	end := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	start := end.Add(-30 * 24 * time.Hour)
	const n = 1_000_000
	step := end.Sub(start) / n
	models := []string{"m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10", "m11"}
	batch := make([]core.RequestRecord, 0, MaxBatch)
	for i := range n {
		lat := int64(200 + i%5000)
		ttft := lat / int64(2+i%7)
		batch = append(batch, core.RequestRecord{
			ID: fmt.Sprintf("b%07d", i), StartedAt: start.Add(time.Duration(i) * step),
			Model: models[i%len(models)], Status: 200, LatencyMS: lat, TTFTMS: &ttft,
			Streamed: i%10 != 0, FirstTokenReasoning: i%5 == 0, UsageKnown: i%20 != 0,
			Usage: core.Usage{InputTokens: int64(i % 1000), OutputTokens: int64(i % 997), ReasoningTokens: int64(i % 13)},
		})
		if len(batch) == MaxBatch {
			if err := l.RecordBatch(ctx, batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	q := core.AnalyticsQuery{From: start, To: end, Bucket: "day", Group: "model"}
	b.ResetTimer()
	for b.Loop() {
		res, err := l.Performance(ctx, q)
		if err != nil {
			b.Fatal(err)
		}
		if res.Totals.Requests != n {
			b.Fatalf("requests = %d", res.Totals.Requests)
		}
	}
}
