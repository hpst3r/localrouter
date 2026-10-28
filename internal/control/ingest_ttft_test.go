package control

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// P13: ingested ttft_ms must satisfy 0 <= ttft_ms <= latency_ms; valid timing
// fields reach the ledger unchanged.
func TestIngestTTFT(t *testing.T) {
	post := func(t *testing.T, ttft string) (*ingestFixture, int, string) {
		t.Helper()
		f := newIngestFixture(t, true, true)
		body := `{"schema_version":1,"host":"vm1","records":[{"id":"r1","account_id":"claude-max",` +
			`"model":"claude-opus","started_at":"` + t0.Add(-time.Minute).Format(time.RFC3339) + `",` +
			`"usage_known":true,"usage":{"input_tokens":100,"output_tokens":10},"latency_ms":500,` +
			`"streamed":true,"first_token_reasoning":true` + ttft + `}]}`
		rec := f.post(t, ingestKey, body)
		return f, rec.Code, rec.Body.String()
	}

	for _, tc := range []struct {
		name, ttft string
		want       int64
	}{{"zero", `,"ttft_ms":0`, 0}, {"mid", `,"ttft_ms":120`, 120}, {"equal latency", `,"ttft_ms":500`, 500}} {
		t.Run(tc.name, func(t *testing.T) {
			f, code, body := post(t, tc.ttft)
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, body)
			}
			row := f.rows()["r1"]
			if row.TTFTMS == nil || *row.TTFTMS != tc.want || !row.Streamed || !row.FirstTokenReasoning {
				t.Fatalf("row timing = %v streamed=%v reasoning=%v, want %d true true",
					row.TTFTMS, row.Streamed, row.FirstTokenReasoning, tc.want)
			}
		})
	}

	t.Run("absent", func(t *testing.T) {
		f, code, body := post(t, "")
		if code != http.StatusOK {
			t.Fatalf("status %d: %s", code, body)
		}
		if row := f.rows()["r1"]; row.TTFTMS != nil {
			t.Fatalf("ttft_ms = %d, want nil", *row.TTFTMS)
		}
	})

	for _, tc := range []struct{ name, ttft string }{
		{"negative", `,"ttft_ms":-1`}, {"above latency", `,"ttft_ms":501`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, code, body := post(t, tc.ttft)
			if code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", code, body)
			}
			if !strings.Contains(body, "records[0]: ttft_ms must be between 0 and latency_ms") {
				t.Fatalf("body = %s", body)
			}
			if n := len(f.rows()); n != 0 {
				t.Fatalf("rows written: %d", n)
			}
		})
	}
}
