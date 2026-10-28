package proxy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

// Streamed is recorded from the capture's decided mode, so a Codex event
// stream sent without a Content-Type counts as streamed, with its TTFT and
// usage, and lands in the performance analytics' streamed/TTFT samples just
// like a typed stream. JSON bodies, typed or not, stay non-streamed.
func TestStreamedRecordedAfterSniff(t *testing.T) {
	const ms = time.Millisecond
	sse := []ttftStep{
		{10 * ms, sseEvent("response.created", `{"type":"response.created","response":{"id":"r"}}`)},
		{30 * ms, sseEvent("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hi"}`)},
		{100 * ms, sseEvent("response.completed",
			`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":7}}}`)},
	}
	json := []ttftStep{{10 * ms, `{"id":"r","usage":{"input_tokens":5,"output_tokens":7}}`}}
	for _, tc := range []struct {
		name, ctype  string
		steps        []ttftStep
		wantStreamed bool
		wantTTFT     int64 // 0 = nil
		wantLatency  int64
	}{
		{"codex untyped SSE", "", sse, true, 40, 140},
		{"codex typed SSE", "text/event-stream", sse, true, 40, 140},
		{"codex untyped JSON", "", json, false, 0, 10},
		{"codex typed JSON", "application/json", json, false, 0, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := relayTTFTAs(t, core.ProviderCodex, "/v1/responses", tc.ctype, tc.steps, 0)
			if rec.Streamed != tc.wantStreamed || rec.LatencyMS != tc.wantLatency || rec.Error != "" {
				t.Fatalf("streamed=%v latency=%d error=%q, want %v %d", rec.Streamed, rec.LatencyMS, rec.Error, tc.wantStreamed, tc.wantLatency)
			}
			if want := (core.Usage{InputTokens: 5, OutputTokens: 7}); !rec.UsageKnown || rec.Usage != want {
				t.Fatalf("usage %+v known=%v", rec.Usage, rec.UsageKnown)
			}
			if tc.wantTTFT == 0 {
				if rec.TTFTMS != nil {
					t.Fatalf("ttft=%d, want nil", *rec.TTFTMS)
				}
			} else if rec.TTFTMS == nil || *rec.TTFTMS != tc.wantTTFT {
				t.Fatalf("ttft=%v, want %d", rec.TTFTMS, tc.wantTTFT)
			}

			l, err := ledger.Open(filepath.Join(t.TempDir(), "l.db"), nil, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			if err := l.Record(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
			res, err := l.Performance(context.Background(), core.AnalyticsQuery{
				From: rec.StartedAt.Add(-time.Hour), To: rec.StartedAt.Add(time.Hour), Bucket: "hour", Group: "model"})
			if err != nil {
				t.Fatal(err)
			}
			tot := res.Totals
			if tot.Requests != 1 {
				t.Fatalf("perf requests = %d, want 1", tot.Requests)
			}
			if tc.wantStreamed {
				// D = 7-1 = 6 tokens over G = 140-40 = 100 ms.
				if tot.StreamedRequests != 1 || tot.TTFTMS == nil || tot.TTFTMS.N != 1 || tot.DecodeTPS == nil || tot.DecodeTPS.N != 1 {
					t.Fatalf("perf totals %+v ttft=%+v decode=%+v", tot, tot.TTFTMS, tot.DecodeTPS)
				}
			} else if tot.StreamedRequests != 0 || tot.TTFTMS != nil || tot.DecodeTPS != nil {
				t.Fatalf("perf totals %+v", tot)
			}
		})
	}
}
