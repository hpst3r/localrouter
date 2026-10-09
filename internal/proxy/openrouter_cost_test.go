package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// costPtr is a small helper so expectations read as literal USD amounts.
func costPtr(v float64) *float64 { return &v }

// equalCost compares two optional costs exactly (nil-aware).
func equalCost(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// reportedCost reads the capture's provider-reported cost after flushing.
func reportedCostOf(c *usageCapture) *float64 {
	_, _ = c.Result()
	return c.ReportedCost()
}

// A chat-completions final SSE chunk carries the provider cost at the top
// level, next to the usage token counts.
func TestReportedCostChatFinalChunk(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9,\"cost\":0.0003}}\n\n", 3)
	u, ok := c.Result()
	if !ok || u.InputTokens != 7 || u.OutputTokens != 2 {
		t.Fatalf("usage %+v ok=%v", u, ok)
	}
	if got := c.ReportedCost(); !equalCost(got, costPtr(0.0003)) {
		t.Fatalf("reported cost = %v, want 0.0003", got)
	}
}

// The non-SSE (JSON) path reads the top-level usage object.
func TestReportedCostNonStreamJSON(t *testing.T) {
	c := newUsageCapture("application/json")
	feed(c, `{"id":"x","usage":{"prompt_tokens":21,"completion_tokens":128,"cost":0.000123}}`, 5)
	u, ok := c.Result()
	if !ok || u.InputTokens != 21 || u.OutputTokens != 128 {
		t.Fatalf("usage %+v ok=%v", u, ok)
	}
	if got := c.ReportedCost(); !equalCost(got, costPtr(0.000123)) {
		t.Fatalf("reported cost = %v, want 0.000123", got)
	}
}

// Responses terminal events nest the usage (and cost) under "response".
func TestReportedCostResponsesTerminalNested(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	feed(c, "event: response.completed\n"+
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"cost\":0.5}}}\n\n", 4)
	if got := reportedCostOf(c); !equalCost(got, costPtr(0.5)) {
		t.Fatalf("terminal nested cost = %v, want 0.5", got)
	}

	// A non-terminal Responses event must not contribute cost.
	c = newUsageCapture("text/event-stream")
	feed(c, "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"cost\":9.99}}}\n\n", 4)
	if got := reportedCostOf(c); got != nil {
		t.Fatalf("in_progress cost = %v, want nil", got)
	}
}

// A terminal event that arrives without a trailing blank line is flushed by
// Result and must still yield its nested cost.
func TestReportedCostUnterminatedFinalFlush(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	feed(c, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":5,\"cost\":0.25}}}", 6)
	if got := reportedCostOf(c); !equalCost(got, costPtr(0.25)) {
		t.Fatalf("flushed cost = %v, want 0.25", got)
	}
}

// The last meaningful final usage record wins; costs are never summed per
// chunk. The most recent usage-bearing record is authoritative for cost, so
// a later record without a usable cost replaces (clears) an earlier one.
func TestReportedCostLastMeaningfulNoSum(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"cost\":0.1}}\n\n", 2)
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"cost\":0.2}}\n\n", 2)
	if got := reportedCostOf(c); !equalCost(got, costPtr(0.2)) {
		t.Fatalf("cost = %v, want 0.2 (last, not summed)", got)
	}

	// A final usage-bearing record without a usable cost is authoritative: it
	// clears the earlier reported cost rather than letting it go stale.
	c = newUsageCapture("text/event-stream")
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"cost\":0.4}}\n\n", 2)
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2}}\n\n", 2)
	if got := reportedCostOf(c); got != nil {
		t.Fatalf("cost = %v, want nil (final usage record without cost is authoritative)", got)
	}
}

// Cost knowledge is independent of token knowledge: a cost-only usage object
// reports a cost even though no token counts are available.
func TestReportedCostCostOnlyUsage(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	feed(c, "data: {\"choices\":[],\"usage\":{\"cost\":0.0007}}\n\n", 3)
	u, ok := c.Result()
	if ok {
		t.Fatalf("token knowledge should be false for cost-only usage: %+v", u)
	}
	if got := c.ReportedCost(); !equalCost(got, costPtr(0.0007)) {
		t.Fatalf("cost-only reported cost = %v, want 0.0007", got)
	}
}

// An explicit zero is a valid, meaningful provider-reported cost.
func TestReportedCostExplicitZeroValid(t *testing.T) {
	for _, body := range []string{
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":128,\"cost\":0}}\n\n",
		"data: {\"choices\":[],\"usage\":{\"cost\":0.0}}\n\n",
	} {
		c := newUsageCapture("text/event-stream")
		feed(c, body, 7)
		if got := reportedCostOf(c); !equalCost(got, costPtr(0)) {
			t.Fatalf("body %q: cost = %v, want explicit zero", body, got)
		}
	}
}

// Missing, null, or otherwise unusable cost values yield nil, and an
// unusable cost never discards valid token usage from the same record.
func TestReportedCostInvalidValuesIgnored(t *testing.T) {
	cases := []struct {
		name string
		frag string // raw JSON fragment appended inside the usage object
	}{
		{"missing", ""},
		{"null", `,"cost":null`},
		{"string", `,"cost":"0.1"`},
		{"negative", `,"cost":-1`},
		{"overflow", `,"cost":1e999`},
		{"bool", `,"cost":true`},
		{"object", `,"cost":{"amount":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":128" +
				tc.frag + "}}\n\n"
			c := newUsageCapture("text/event-stream")
			feed(c, body, 3)
			u, ok := c.Result()
			if !ok || u.InputTokens != 21 || u.OutputTokens != 128 {
				t.Fatalf("valid tokens lost: %+v ok=%v (body %q)", u, ok, body)
			}
			if got := c.ReportedCost(); got != nil {
				t.Fatalf("cost = %v, want nil for %s", got, tc.name)
			}
		})
	}
}

// A truncated / oversized final event is dropped, so no cost is recovered and
// the bounded buffers stay within their caps.
func TestReportedCostTruncatedBounds(t *testing.T) {
	// Oversized event carrying a cost is discarded entirely.
	c := newUsageCapture("text/event-stream")
	big := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"cost\":0.5},\"x\":\"" + strings.Repeat("a", maxEventBytes) + "\"}\n\n"
	feed(c, big, 1<<16)
	if got := reportedCostOf(c); got != nil {
		t.Fatalf("oversized event cost = %v, want nil", got)
	}
	if cap(c.line) > 2*maxEventBytes || cap(c.data) > 2*maxEventBytes {
		t.Fatal("buffers grew beyond cap")
	}

	// Truncated (unparseable) final event yields neither tokens nor cost.
	c = newUsageCapture("text/event-stream")
	feed(c, "data: {\"choices\":[],\"usage\":{\"cost\":0.5", 5)
	if got := reportedCostOf(c); got != nil {
		t.Fatalf("truncated event cost = %v, want nil", got)
	}
}

// cost_details must be ignored: only usage.cost counts.
func TestReportedCostIgnoresCostDetails(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"cost_details\":{\"upstream_inference_cost\":0.9}}}\n\n", 3)
	if got := reportedCostOf(c); got != nil {
		t.Fatalf("cost_details leaked into reported cost: %v", got)
	}
}

// A stale intermediate usage cost must not survive a later, authoritative
// final usage record that carries token counts but no usable cost. With
// continuous usage stats an intermediate record may report cost 0; the final
// record alone decides the price, so an unusable final cost must yield nil
// (letting the caller fall back to computed pricing) rather than keeping 0.
func TestReportedCostFinalCostUnusableClearsStale(t *testing.T) {
	cases := []struct {
		name string
		frag string // raw cost fragment for the FINAL usage object
	}{
		{"absent", ""},
		{"null", `,"cost":null`},
		{"string", `,"cost":"0.2"`},
		{"negative", `,"cost":-0.2`},
		{"overflow", `,"cost":1e999`},
		{"bool", `,"cost":true`},
		{"object", `,"cost":{"amount":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newUsageCapture("text/event-stream")
			// Intermediate record: token counts with an explicit zero cost.
			feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"cost\":0}}\n\n", 3)
			if got := c.ReportedCost(); !equalCost(got, costPtr(0)) {
				t.Fatalf("intermediate cost = %v, want 0", got)
			}
			// Final record: real token counts, but an unusable cost.
			feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":8"+tc.frag+"}}\n\n", 3)
			u, ok := c.Result()
			if !ok || u.InputTokens != 40 || u.OutputTokens != 8 {
				t.Fatalf("final tokens %+v ok=%v", u, ok)
			}
			if got := c.ReportedCost(); got != nil {
				t.Fatalf("stale intermediate cost survived: %v (want nil for %s)", got, tc.name)
			}
		})
	}
}

// The final record's usable cost overrides any earlier cost, including the
// explicit-zero intermediate used by continuous usage stats.
func TestReportedCostValidLatestOverrides(t *testing.T) {
	// Older zero replaced by a newer valid cost.
	c := newUsageCapture("text/event-stream")
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"cost\":0}}\n\n", 3)
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"cost\":0.0009}}\n\n", 3)
	if got := reportedCostOf(c); !equalCost(got, costPtr(0.0009)) {
		t.Fatalf("cost = %v, want 0.0009", got)
	}

	// Older valid replaced by a newer explicit zero (zero is authoritative).
	c = newUsageCapture("text/event-stream")
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"cost\":0.5}}\n\n", 3)
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"cost\":0}}\n\n", 3)
	if got := reportedCostOf(c); !equalCost(got, costPtr(0)) {
		t.Fatalf("cost = %v, want explicit zero", got)
	}
}

// Chunks that carry no usage object — content deltas, an explicit null usage,
// SSE comments, a bare [DONE] sentinel — must never clear the last reported
// usage cost.
func TestReportedCostUsageLessChunksPreserve(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	feed(c, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":5,\"cost\":0.0025}}\n\n", 3)
	feed(c, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", 3)
	feed(c, "data: {\"choices\":[],\"usage\":null}\n\n", 3)
	feed(c, ": OPENROUTER PROCESSING\n\n", 3)
	feed(c, "data: [DONE]\n\n", 3)
	if got := reportedCostOf(c); !equalCost(got, costPtr(0.0025)) {
		t.Fatalf("cost = %v, want 0.0025 preserved across usage-less chunks", got)
	}
}

// Within the Responses stream, a meaningful terminal usage record with an
// unusable cost replaces a previously valid cost with nil, while a terminal
// event that carries no usage object at all must not clear a valid cost.
func TestReportedCostResponsesTerminalInvalidReplacesValid(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	feed(c, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"cost\":0.5}}}\n\n", 3)
	if got := c.ReportedCost(); !equalCost(got, costPtr(0.5)) {
		t.Fatalf("first terminal cost = %v, want 0.5", got)
	}
	feed(c, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":70,\"output_tokens\":20,\"cost\":null}}}\n\n", 3)
	u, ok := c.Result()
	if !ok || u.InputTokens != 70 || u.OutputTokens != 20 {
		t.Fatalf("tokens %+v ok=%v", u, ok)
	}
	if got := c.ReportedCost(); got != nil {
		t.Fatalf("terminal unusable cost did not replace valid one: %v", got)
	}

	// A terminal event with no usage object is not a usage-bearing record.
	c = newUsageCapture("text/event-stream")
	feed(c, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"cost\":0.5}}}\n\n", 3)
	feed(c, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", 3)
	if got := reportedCostOf(c); !equalCost(got, costPtr(0.5)) {
		t.Fatalf("usage-less terminal cleared cost: %v, want 0.5", got)
	}
}

// Through the forwarding path, rec.ReportedCostUSD is populated for OpenRouter
// only, the response bytes are forwarded verbatim, and the final event is
// flushed before the accessor is read.
func TestForwardSetsReportedCostOnlyForOpenRouter(t *testing.T) {
	const sse = ": OPENROUTER PROCESSING\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":42,\"completion_tokens\":8,\"total_tokens\":50,\"cost\":0.0003,\"prompt_tokens_details\":{\"cached_tokens\":6},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"

	h := newHarness(t)
	h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	})
	orRoute(h, "or")
	h.start()
	resp := h.post("/v1/chat/completions", clientKey, `{"model":"anthropic/claude-sonnet-4.5","stream":true,"messages":[]}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("client body not forwarded: %q", body)
	}
	rows := h.waitRows(1)
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	r := rows[0]
	if r.Provider != core.ProviderOpenRouter {
		t.Fatalf("provider %q", r.Provider)
	}
	if !r.UsageKnown || r.Usage.InputTokens != 42 || r.Usage.OutputTokens != 8 {
		t.Fatalf("usage %+v known=%v", r.Usage, r.UsageKnown)
	}
	if !equalCost(r.ReportedCostUSD, costPtr(0.0003)) {
		t.Fatalf("reported cost = %v, want 0.0003", r.ReportedCostUSD)
	}
	if int64(len(body)) != r.BytesOut {
		t.Fatalf("bytes forwarded = %d, ledger bytes_out = %d", len(body), r.BytesOut)
	}
}

// A non-stream JSON response from OpenRouter also carries its cost.
func TestForwardSetsReportedCostNonStream(t *testing.T) {
	h := newHarness(t)
	h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"gen-1","usage":{"prompt_tokens":21,"completion_tokens":128,"cost":0.000123}}`)
	})
	orRoute(h, "or")
	h.start()
	resp := h.post("/v1/chat/completions", clientKey, orChatBody, nil)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	rows := h.waitRows(1)
	if !equalCost(rows[0].ReportedCostUSD, costPtr(0.000123)) {
		t.Fatalf("non-stream reported cost = %v, want 0.000123", rows[0].ReportedCostUSD)
	}
}

// Cost fields from any other provider are ignored.
func TestForwardIgnoresCostFromOtherProviders(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"cost\":0.42}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	orRoute(h, "a")
	h.start()
	resp := h.post("/v1/chat/completions", clientKey, `{"model":"anthropic/claude-sonnet-4.5","stream":true,"messages":[]}`, nil)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	rows := h.waitRows(1)
	if !rows[0].UsageKnown {
		t.Fatalf("usage not captured for compat provider: %+v", rows[0])
	}
	if rows[0].ReportedCostUSD != nil {
		t.Fatalf("non-OpenRouter cost must be ignored, got %v", *rows[0].ReportedCostUSD)
	}
}

// Regression coverage for the chosen independent-observation semantics: the
// provider-reported cost is an observation about price, not about tokens or
// transport. When a stream that already emitted a valid usage.cost ends in a
// transport failure — an upstream read error or a client abort — the ledger
// row must keep the observed cost while UsageKnown is forced false (the token
// counts are untrusted). This deliberately does NOT claim that the reported
// cost is the complete, audited final charge.
func TestReportedCostPreservedOnStreamFailure(t *testing.T) {
	// A final usage record carrying both token counts and a usable cost.
	const chunk = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3,\"cost\":0.00042}}\n\n"

	t.Run("upstream_read_error", func(t *testing.T) {
		h := newHarness(t)
		h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("upstream handler is not hijackable")
				return
			}
			conn, brw, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer conn.Close()
			// Advertise more bytes than are sent, then close without sending
			// the rest: the proxy observes the usage event, then its next read
			// of the upstream body fails.
			_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 100000\r\n\r\n")
			_, _ = brw.WriteString(chunk)
			_ = brw.Flush()
		})
		orRoute(h, "or")
		h.start()
		resp := h.post("/v1/chat/completions", clientKey, `{"model":"anthropic/claude-sonnet-4.5","stream":true,"messages":[]}`, nil)
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()

		r := h.waitRows(1)[0]
		if r.UsageKnown {
			t.Fatalf("UsageKnown = true, want false after upstream read error: %+v", r)
		}
		if !equalCost(r.ReportedCostUSD, costPtr(0.00042)) {
			t.Fatalf("reported cost = %v, want 0.00042 preserved across read error", r.ReportedCostUSD)
		}
		if !strings.Contains(r.Error, "upstream stream error") {
			t.Fatalf("error = %q, want upstream stream error", r.Error)
		}
	})

	t.Run("client_abort", func(t *testing.T) {
		h := newHarness(t)
		upstreamDone := make(chan struct{})
		h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
			defer close(upstreamDone)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chunk+"data: [DONE]\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		orRoute(h, "or")
		h.start()

		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"anthropic/claude-sonnet-4.5","stream":true,"messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+clientKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || !strings.HasPrefix(line, "data: ") {
			t.Fatalf("first line %q err %v", line, err)
		}
		cancel()
		resp.Body.Close()

		r := h.waitRows(1)[0]
		if r.UsageKnown {
			t.Fatalf("UsageKnown = true, want false after client abort: %+v", r)
		}
		if !equalCost(r.ReportedCostUSD, costPtr(0.00042)) {
			t.Fatalf("reported cost = %v, want 0.00042 preserved across abort", r.ReportedCostUSD)
		}
		if r.Error != "client disconnected" {
			t.Fatalf("error = %q, want client disconnected", r.Error)
		}
		select {
		case <-upstreamDone:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream request not canceled")
		}
	})
}
