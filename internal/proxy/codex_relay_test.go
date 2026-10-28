package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

// An upstream Content-Type is relayed as is, even when the body looks like an
// event stream: only an absent one is ever filled in.
func TestRelayNeverOverridesUpstreamContentType(t *testing.T) {
	stream := codexLiveStream()
	for _, ct := range []string{"text/plain", "text/event-stream; charset=utf-8", "application/json"} {
		t.Run(ct, func(t *testing.T) {
			h := newHarness(t)
			h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ct)
				_, _ = io.WriteString(w, stream)
			})
			singleRoute(h, "a")
			h.start()
			resp := h.post("/v1/responses", clientKey, `{"model":"gpt-x","input":"hi","stream":true}`, nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != stream {
				t.Fatalf("relayed body differs (len %d vs %d)", len(body), len(stream))
			}
			if got := resp.Header.Values("Content-Type"); len(got) != 1 || got[0] != ct {
				t.Fatalf("client Content-Type %q, want %q", got, ct)
			}
			h.waitRows(1)
		})
	}
}

// A body that stays undecided past maxSniffBytes (leading whitespace) is
// relayed byte-for-byte as JSON once the cap decides it, with no
// Content-Type invented, and its usage still parses.
func TestRelayUntypedJSONPastSniffCap(t *testing.T) {
	payload := strings.Repeat(" ", maxSniffBytes+streamChunkSize) +
		`{"id":"r1","usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":1}}}`
	h := newHarness(t)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, _ *http.Request) {
		noContentType(w)
		f := w.(http.Flusher)
		for i := 0; i < len(payload); i += 1000 {
			_, _ = io.WriteString(w, payload[i:min(i+1000, len(payload))])
			f.Flush()
		}
	})
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != payload {
		t.Fatalf("relayed body differs (len %d vs %d)", len(body), len(payload))
	}
	if ct, ok := resp.Header["Content-Type"]; ok {
		t.Fatalf("client got Content-Type %q", ct)
	}
	row := h.waitRows(1)[0]
	want := core.Usage{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 3, ReasoningTokens: 1}
	if !row.UsageKnown || row.Usage != want {
		t.Fatalf("row usage %+v known=%v", row.Usage, row.UsageKnown)
	}
}

// The status line held while sniffing is bounded by the idle timeout: an
// upstream that stalls before its body is decided still gets the status, the
// bytes it sent and an idle-timeout row.
func TestRelayUndecidedBodyBoundedByIdleTimeout(t *testing.T) {
	h := newHarness(t)
	h.opts.StreamIdleTimeout = 50 * time.Millisecond
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		noContentType(w)
		_, _ = io.WriteString(w, "da") // a prefix of "data:": still undecided
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, `{"model":"gpt-x","input":"hi","stream":true}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "da" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	row := h.waitRows(1)[0]
	if row.Error != "stream idle timeout" || row.UsageKnown {
		t.Fatalf("row %+v", row)
	}
}

// Once Codex usage is known again, spend-control settlement books it exactly
// like the same stream from another provider that labels it text/event-stream:
// an estimated cost from the price table, the hold released.
func TestCodexUntypedStreamSettlesLikeTypedStream(t *testing.T) {
	stream := codexLiveStream()
	half := 0.5
	pricing := ledger.NewPricing(map[string]ledger.ModelPrice{
		"gpt-x": {Input: 2, CachedInput: &half, Output: 10},
	})
	cost, ok := pricing.Cost("gpt-x", core.Usage{InputTokens: 3521, CachedInputTokens: 3328, OutputTokens: 5, ReasoningTokens: 2})
	if !ok {
		t.Fatal("fixture model gpt-x is not priced")
	}
	estimated, err := budget.ReportedUSDToMicros(cost)
	if err != nil || estimated <= 0 {
		t.Fatalf("fixture estimate %d: %v", estimated, err)
	}
	for _, tc := range []struct {
		provider, ct string
	}{
		{core.ProviderCodex, ""},
		{core.ProviderOpenAICompat, "text/event-stream"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			h := newHarness(t)
			h.upstream("a", tc.provider, func(w http.ResponseWriter, _ *http.Request) {
				if tc.ct == "" {
					noContentType(w)
				} else {
					w.Header().Set("Content-Type", tc.ct)
				}
				_, _ = io.WriteString(w, stream)
			})
			singleRoute(h, "a")
			st, gate := openBudgetGate(t, nil, 250_000, pricing)
			h.startBudget(gate)
			resp := h.post("/v1/responses", clientKey, `{"model":"gpt-x","input":"hi","stream":true}`, nil)
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			row := h.waitRows(1)[0]
			if !row.UsageKnown || row.Error != "" {
				t.Fatalf("row %+v", row)
			}
			if c, basis := ledger.ResolveRecordCost(row, pricing); c == nil || *c != cost || basis != ledger.CostBasisEstimated {
				t.Fatalf("resolved cost %v basis %q, want %v estimated", c, basis, cost)
			}
			if got, want := snapshotOf(t, st, "alice"), (budget.Snapshot{Estimated: estimated}); got != want {
				t.Fatalf("snapshot = %+v, want %+v", got, want)
			}
		})
	}
}
