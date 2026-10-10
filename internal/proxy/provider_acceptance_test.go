package proxy

// provider_acceptance_test.go is a maintained, deterministic provider acceptance
// suite for the forwarding edge (/v1/responses and /v1/chat/completions). It
// composes only the existing in-process harness (fakes_test.go): fake
// policy/creds/quota/ledger plus loopback httptest upstream servers, and
// synthetic credentials. It touches no production code, makes no external
// network call and reads no real secret.
//
// COVERAGE — named per (Provider, protocol) pair, not a claim about "all
// adapters":
//
//	openai_compat : /v1/responses (Responses protocol) and /v1/chat/completions
//	codex         : /v1/responses only (Responses protocol; the proxy drops
//	                codex accounts from /v1/chat/completions candidates, so
//	                there is no chat path to accept here)
//	openrouter    : /v1/chat/completions + /v1/responses, the provider-specific
//	                402 failover (account-level vs request-scoped
//	                affordability), the final moderation 403, the
//	                request-scoped 429, and usage.cost observation
//	ollama        : /v1/chat/completions (also the negative control whose
//	                usage.cost must be ignored)
//
// NOT covered: core.ProviderClaude is quota-only and never proxies inference
// (see core.ProviderClaude), so it exposes no forwarding behaviour to accept;
// no account/stream/cache/identity metadata beyond the above is exercised.
// Token-derived ("estimated") cost is resolved by internal/ledger from the
// recorded token usage, not by the proxy, so at this layer the suite asserts
// the provider-reported cost plumbing (populated for OpenRouter only) rather
// than a priced total; see TestProviderAcceptanceReportedCostParsing.
//
// DETERMINISM — every wait is either a harness channel (waitRows /
// policy.releasedCh) or a select bounded by the single hard deadline paBound.
// Every outbound test request is bounded by paBound too. There are no
// sleep-based gates and no wall-clock sequencing.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// paFailBody is the body a scripted failing upstream returns. It is a marker so
// the tests can prove it never reaches the client when another account serves.
const paFailBody = `{"error":{"message":"pa-fail-marker","type":"upstream_error"}}`

// paAffordBody is OpenRouter's affordability preflight 402, carrying the same
// marker as paFailBody.
const paAffordBody = `{"error":{"code":402,"message":"pa-fail-marker: This request requires more credits, or fewer max_tokens. You requested up to 64 tokens, but can only afford 0."}}`

// paTruncChunk is a complete SSE frame that the "no failover after bytes" fake
// upstream writes before dropping the connection mid-body.
const paTruncChunk = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"

// ---------------------------------------------------------------------------
// Non-stream acceptance table: successful chat per (Provider, protocol).
// ---------------------------------------------------------------------------

func paNonStreamProfiles() []paProfile {
	return []paProfile{
		{
			name: "codex_responses",
			paReqExpect: paReqExpect{
				provider: core.ProviderCodex, endpoint: pathResponses, model: "gpt-x",
				// The Codex normalization may only drop max_output_tokens and
				// metadata and force store=false; every unrelated field
				// (instructions, temperature, top_p, user) must survive
				// untouched. The expected body is spelled out by hand.
				request:  `{"model":"gpt-x","input":"hi","max_output_tokens":10,"metadata":{"a":1},"store":true,"instructions":"be <b>brief</b>","temperature":0.7,"top_p":0.9,"user":"u1"}`,
				upstream: `{"input":"hi","instructions":"be <b>brief</b>","model":"gpt-x","store":false,"temperature":0.7,"top_p":0.9,"user":"u1"}`,
			},
			upstream:       `{"id":"r1","usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":1}}}`,
			wantUsage:      core.Usage{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 3, ReasoningTokens: 1},
			wantUsageKnown: true,
		},
		{
			name: "openai_compat_responses_model_rewrite",
			paReqExpect: paReqExpect{
				provider: core.ProviderOpenAICompat, endpoint: pathResponses, model: "gpt-x",
				upstreamModel: "vendor/model-x",
				// Only model may change. The other fields must pass through,
				// including one whose value a naive re-encode would HTML-escape.
				request: `{"model":"gpt-x","input":"hi","temperature":0.2,"top_p":0.8,"user":"a<b>&c","stream":false}`,
			},
			upstream:       `{"id":"r2","usage":{"input_tokens":8,"output_tokens":2,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":1}}}`,
			wantUsage:      core.Usage{InputTokens: 8, CachedInputTokens: 3, OutputTokens: 2, ReasoningTokens: 1},
			wantUsageKnown: true,
		},
		{
			name: "openai_compat_chat",
			paReqExpect: paReqExpect{
				provider: core.ProviderOpenAICompat, endpoint: pathChat, model: "gpt-x",
				// A non-stream chat body triggers no rewrite at all, so it must
				// be forwarded byte-for-byte.
				request: `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"temperature":0.3,"n":2,"stream":false}`,
			},
			upstream:       `{"id":"c1","usage":{"prompt_tokens":9,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}}`,
			wantUsage:      core.Usage{InputTokens: 9, CachedInputTokens: 2, OutputTokens: 4, ReasoningTokens: 1},
			wantUsageKnown: true,
		},
		{
			name: "openrouter_chat_reported_cost",
			paReqExpect: paReqExpect{
				provider: core.ProviderOpenRouter, endpoint: pathChat, model: "anthropic/claude-sonnet-4.5",
				request: `{"model":"anthropic/claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}],"stream":false,"max_tokens":16}`,
			},
			upstream:         `{"id":"gen-1","usage":{"prompt_tokens":21,"completion_tokens":128,"cost":0.000123}}`,
			wantUsage:        core.Usage{InputTokens: 21, OutputTokens: 128},
			wantUsageKnown:   true,
			wantReportedCost: costPtr(0.000123),
		},
		{
			name: "ollama_chat_cost_ignored",
			paReqExpect: paReqExpect{
				provider: core.ProviderOllama, endpoint: pathChat, model: "gpt-x",
				request: `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			},
			// Negative control: a non-OpenRouter provider that reports a cost
			// must not have it recorded (estimation belongs to the ledger).
			upstream:         `{"id":"o1","usage":{"prompt_tokens":5,"completion_tokens":2,"cost":0.42}}`,
			wantUsage:        core.Usage{InputTokens: 5, OutputTokens: 2},
			wantUsageKnown:   true,
			wantReportedCost: nil,
		},
	}
}

func TestProviderAcceptanceNonStreamSuccess(t *testing.T) {
	for _, p := range paNonStreamProfiles() {
		t.Run(p.name, func(t *testing.T) {
			h := newHarness(t)
			var hits atomic.Int32
			reqCh := make(chan []byte, 1)
			h.upstream("a", p.provider, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.URL.Path != p.endpoint {
					t.Errorf("provider acceptance: upstream path = %s, want %s", r.URL.Path, p.endpoint)
				}
				body, _ := io.ReadAll(r.Body)
				reqCh <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, p.upstream)
			})
			h.routes = []core.Route{{Name: "main", Models: []string{p.model}, UpstreamModel: p.upstreamModel, Interactive: []string{"a"}, Background: []string{"a"}}}
			h.start()

			resp := paPost(t, h, p.endpoint, clientKey, p.request, nil)
			if resp.StatusCode != http.StatusOK {
				body := paReadBody(t, resp)
				t.Fatalf("provider acceptance: status %d, want 200 (body %s)", resp.StatusCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("provider acceptance: response content-type = %q, want application/json", ct)
			}
			body := paReadBody(t, resp)
			if string(body) != p.upstream {
				t.Fatalf("provider acceptance: relayed body altered:\n got %q\nwant %q", body, p.upstream)
			}
			paCheckRequest(t, p.paReqExpect, paRecvBytes(t, "upstream request body", reqCh))

			// The lease is released before the row is written, so the release
			// alone does not settle the row set; paWaitRows waits for the
			// proxy handler to finish before asserting the exact count.
			lease := paAwaitLease(t, "lease release", h.policy.releasedCh)
			if lease.id != "a" || lease.released != 1 || lease.outcome.Status != http.StatusOK || !lease.outcome.UsageKnown {
				t.Fatalf("provider acceptance: lease outcome = %+v released=%d", lease.outcome, lease.released)
			}
			rows := paWaitRows(t, h, 1)
			row := rows[0]
			if row.Provider != p.provider || row.AccountID != "a" || row.Status != http.StatusOK {
				t.Fatalf("provider acceptance: row = %+v", row)
			}
			if row.UsageKnown != p.wantUsageKnown || row.Usage != p.wantUsage {
				t.Fatalf("provider acceptance: usage = %+v known=%v, want %+v known=%v", row.Usage, row.UsageKnown, p.wantUsage, p.wantUsageKnown)
			}
			if !equalCost(row.ReportedCostUSD, p.wantReportedCost) {
				t.Fatalf("provider acceptance: reported cost = %s, want %s", paCostString(row.ReportedCostUSD), paCostString(p.wantReportedCost))
			}
			if got := int64(len(body)); row.BytesOut != got {
				t.Errorf("provider acceptance: bytes_out = %d, want %d", row.BytesOut, got)
			}
			if got := len(h.policy.classes()); got != 1 {
				t.Fatalf("provider acceptance: policy admissions = %d, want 1", got)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("provider acceptance: upstream hits = %d, want 1", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Streaming acceptance table: actually-flushed SSE terminal usage.
// ---------------------------------------------------------------------------

func paStreamProfiles() []paStreamProfile {
	// Responses protocol. The intermediate response.in_progress event carries a
	// decoy usage (9999) with the real usage nested one level deeper than the
	// terminal event expects: only the terminal response.completed frame may be
	// authoritative.
	responsesCodex := paEvent(
		"event: response.created",
		`data: {"type":"response.created","response":{"usage":null}}`,
	) + paEvent(
		"event: response.in_progress",
		`data: {"type":"response.in_progress","response":{"usage":{"input_tokens":9999,"output_tokens":9999}}}`,
	) + paEvent(
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
	) + paEvent(
		"event: response.completed",
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":100},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":30}}}}`,
	)

	// Same protocol, but the terminal frame is NOT terminated by a blank line,
	// so its usage is only recovered when Result flushes the unterminated final
	// event. This pins the "flushed SSE terminal usage" behaviour.
	responsesUnterminated := paEvent(
		"event: response.created",
		`data: {"type":"response.created","response":{"usage":null}}`,
	) + "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":7,"input_tokens_details":{"cached_tokens":3},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1}}}}`

	// Chat protocol: the final data frame carries top-level usage (and, for
	// OpenRouter only, the provider-reported cost).
	chatStream := func(cost *float64) string {
		final := `data: {"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":6},"completion_tokens_details":{"reasoning_tokens":2}`
		if cost != nil {
			final += `,"cost":` + paCostString(cost)
		}
		final += `}}`
		return paEvent(`data: {"choices":[{"delta":{"content":"hi"}}]}`) +
			paEvent(`data: {"choices":[{"delta":{"content":"there"}}],"usage":null}`) +
			paEvent(final) +
			paEvent("data: [DONE]")
	}

	// Responses protocol whose nested terminal usage carries an OpenRouter-style
	// cost. The non-terminal response.in_progress frame is a decoy (cost 123.5)
	// and must be ignored: only the terminal response.completed frame is
	// authoritative, both for tokens and for the provider-reported cost.
	responsesNestedTerminalCost := paEvent(
		"event: response.created",
		`data: {"type":"response.created","response":{"usage":null}}`,
	) + paEvent(
		"event: response.in_progress",
		`data: {"type":"response.in_progress","response":{"usage":{"input_tokens":9,"output_tokens":9,"cost":123.5}}}`,
	) + paEvent(
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
	) + paEvent(
		"event: response.completed",
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":88,"input_tokens_details":{"cached_tokens":7},"output_tokens":12,"output_tokens_details":{"reasoning_tokens":3},"cost":0.0007}}}`,
	)

	return []paStreamProfile{
		{
			name: "codex_responses_terminal_event",
			paReqExpect: paReqExpect{
				provider: core.ProviderCodex, endpoint: pathResponses, model: "gpt-x", stream: true,
				// Only max_output_tokens and a non-false store may change;
				// instructions/temperature must survive the normalization.
				request:  `{"model":"gpt-x","input":"hi","stream":true,"max_output_tokens":10,"store":true,"instructions":"be <b>brief</b>","temperature":0.5}`,
				upstream: `{"input":"hi","instructions":"be <b>brief</b>","model":"gpt-x","store":false,"stream":true,"temperature":0.5}`,
			},
			sse:            responsesCodex,
			wantUsage:      core.Usage{InputTokens: 120, CachedInputTokens: 100, OutputTokens: 50, ReasoningTokens: 30},
			wantUsageKnown: true,
		},
		{
			name: "openai_compat_responses_unterminated_terminal_event",
			paReqExpect: paReqExpect{
				provider: core.ProviderOpenAICompat, endpoint: pathResponses, model: "gpt-x", stream: true,
				// No rewrite applies to a Responses stream with no upstream
				// model, so the body must arrive byte-for-byte.
				request: `{"model":"gpt-x","stream":true,"input":"hi","temperature":0.1}`,
			},
			sse:            responsesUnterminated,
			wantUsage:      core.Usage{InputTokens: 7, CachedInputTokens: 3, OutputTokens: 2, ReasoningTokens: 1},
			wantUsageKnown: true,
		},
		{
			name: "openrouter_chat_reported_cost",
			paReqExpect: paReqExpect{
				provider: core.ProviderOpenRouter, endpoint: pathChat, model: "anthropic/claude-sonnet-4.5", stream: true,
				request: `{"model":"anthropic/claude-sonnet-4.5","stream":true,"messages":[]}`,
			},
			sse:              chatStream(costPtr(0.0003)),
			wantUsage:        core.Usage{InputTokens: 42, CachedInputTokens: 6, OutputTokens: 8, ReasoningTokens: 2},
			wantUsageKnown:   true,
			wantReportedCost: costPtr(0.0003),
		},
		{
			name: "openrouter_chat_stream_options_preserved",
			paReqExpect: paReqExpect{
				provider: core.ProviderOpenRouter, endpoint: pathChat, model: "anthropic/claude-sonnet-4.5", stream: true,
				// forceUsage merges include_usage into the existing
				// stream_options object; the unrelated keys must be preserved.
				request: `{"model":"anthropic/claude-sonnet-4.5","stream":true,"messages":[],"stream_options":{"foo":"bar","include_usage":false}}`,
			},
			sse:              chatStream(costPtr(0.0011)),
			wantUsage:        core.Usage{InputTokens: 42, CachedInputTokens: 6, OutputTokens: 8, ReasoningTokens: 2},
			wantUsageKnown:   true,
			wantReportedCost: costPtr(0.0011),
		},
		{
			name: "openrouter_responses_nested_terminal_reported_cost",
			paReqExpect: paReqExpect{
				provider: core.ProviderOpenRouter, endpoint: pathResponses, model: "anthropic/claude-sonnet-4.5", stream: true,
				request: `{"model":"anthropic/claude-sonnet-4.5","stream":true,"input":"hi"}`,
			},
			sse:              responsesNestedTerminalCost,
			wantUsage:        core.Usage{InputTokens: 88, CachedInputTokens: 7, OutputTokens: 12, ReasoningTokens: 3},
			wantUsageKnown:   true,
			wantReportedCost: costPtr(0.0007),
		},
		{
			name: "openai_compat_responses_nested_terminal_cost_dropped",
			paReqExpect: paReqExpect{
				provider: core.ProviderOpenAICompat, endpoint: pathResponses, model: "gpt-x", stream: true,
				request: `{"model":"gpt-x","stream":true,"input":"hi"}`,
			},
			// Negative control: the same nested terminal cost over the same
			// protocol, but a non-OpenRouter provider. The tokens are recorded;
			// the cost must be dropped.
			sse:              responsesNestedTerminalCost,
			wantUsage:        core.Usage{InputTokens: 88, CachedInputTokens: 7, OutputTokens: 12, ReasoningTokens: 3},
			wantUsageKnown:   true,
			wantReportedCost: nil,
		},
		{
			name: "ollama_chat_cost_ignored",
			paReqExpect: paReqExpect{
				provider: core.ProviderOllama, endpoint: pathChat, model: "gpt-x", stream: true,
				request: `{"model":"gpt-x","stream":true,"messages":[]}`,
			},
			sse:              chatStream(costPtr(0.42)),
			wantUsage:        core.Usage{InputTokens: 42, CachedInputTokens: 6, OutputTokens: 8, ReasoningTokens: 2},
			wantUsageKnown:   true,
			wantReportedCost: nil,
		},
	}
}

func TestProviderAcceptanceStreamTerminalUsage(t *testing.T) {
	for _, p := range paStreamProfiles() {
		t.Run(p.name, func(t *testing.T) {
			h := newHarness(t)
			reqCh := make(chan []byte, 1)
			h.upstream("a", p.provider, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				reqCh <- body
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				paFlushChunks(w, p.sse, 7)
			})
			h.routes = []core.Route{{Name: "main", Models: []string{p.model}, Interactive: []string{"a"}, Background: []string{"a"}}}
			h.start()

			resp := paPost(t, h, p.endpoint, clientKey, p.request, nil)
			if resp.StatusCode != http.StatusOK {
				body := paReadBody(t, resp)
				t.Fatalf("provider acceptance: status %d, want 200 (body %s)", resp.StatusCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
				t.Fatalf("provider acceptance: stream content-type = %q, want text/event-stream", ct)
			}
			body := paReadBody(t, resp)
			if string(body) != p.sse {
				t.Fatalf("provider acceptance: stream altered:\n got %q\nwant %q", body, p.sse)
			}
			paCheckRequest(t, p.paReqExpect, paRecvBytes(t, "upstream request body", reqCh))

			// The release precedes the record; paWaitRows settles the row set.
			lease := paAwaitLease(t, "lease release", h.policy.releasedCh)
			if lease.id != "a" || lease.released != 1 || lease.outcome.Status != http.StatusOK || !lease.outcome.UsageKnown {
				t.Fatalf("provider acceptance: lease outcome = %+v released=%d", lease.outcome, lease.released)
			}
			rows := paWaitRows(t, h, 1)
			row := rows[0]
			if row.Provider != p.provider || row.AccountID != "a" || row.Status != http.StatusOK {
				t.Fatalf("provider acceptance: row = %+v", row)
			}
			if row.UsageKnown != p.wantUsageKnown || row.Usage != p.wantUsage {
				t.Fatalf("provider acceptance: terminal usage = %+v known=%v, want %+v known=%v", row.Usage, row.UsageKnown, p.wantUsage, p.wantUsageKnown)
			}
			if row.BytesOut != int64(len(p.sse)) {
				t.Fatalf("provider acceptance: bytes_out = %d, want %d", row.BytesOut, len(p.sse))
			}
			if !equalCost(row.ReportedCostUSD, p.wantReportedCost) {
				t.Fatalf("provider acceptance: reported cost = %s, want %s", paCostString(row.ReportedCostUSD), paCostString(p.wantReportedCost))
			}
			if got := len(h.policy.classes()); got != 1 {
				t.Fatalf("provider acceptance: policy admissions = %d, want 1", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Reported cost: OpenRouter reports it, everything else is ignored here.
// ---------------------------------------------------------------------------

func TestProviderAcceptanceReportedCostParsing(t *testing.T) {
	// The proxy records RequestRecord.ReportedCostUSD only from OpenRouter and
	// never prices tokens itself (that is internal/ledger's job). These cases pin
	// the boundary: the same "cost" field is dropped for every other provider,
	// and an OpenRouter cost is recorded even when it arrives without token
	// counts (cost knowledge is independent of token knowledge).
	cases := []struct {
		name      string
		provider  string
		body      string
		wantKnown bool
		wantCost  *float64
	}{
		{
			name: "openrouter_reported", provider: core.ProviderOpenRouter,
			body:      `{"id":"g1","usage":{"prompt_tokens":11,"completion_tokens":5,"cost":0.00123}}`,
			wantKnown: true, wantCost: costPtr(0.00123),
		},
		{
			name: "openai_compat_dropped", provider: core.ProviderOpenAICompat,
			body:      `{"id":"g2","usage":{"prompt_tokens":11,"completion_tokens":5,"cost":0.00123}}`,
			wantKnown: true, wantCost: nil,
		},
		{
			name: "codex_dropped", provider: core.ProviderCodex,
			body:      `{"id":"g3","usage":{"input_tokens":11,"output_tokens":5,"cost":0.00123}}`,
			wantKnown: true, wantCost: nil,
		},
		{
			name: "ollama_dropped", provider: core.ProviderOllama,
			body:      `{"id":"g4","usage":{"prompt_tokens":11,"completion_tokens":5,"cost":0.00123}}`,
			wantKnown: true, wantCost: nil,
		},
		{
			name: "openrouter_cost_without_tokens", provider: core.ProviderOpenRouter,
			body:      `{"id":"g5","usage":{"cost":0.5}}`,
			wantKnown: false, wantCost: costPtr(0.5),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.upstream("a", c.provider, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, c.body)
			})
			h.routes = []core.Route{{Name: "main", Models: []string{"gpt-x"}, Interactive: []string{"a"}, Background: []string{"a"}}}
			h.start()

			resp := paPost(t, h, pathResponses, clientKey, `{"model":"gpt-x","input":"hi"}`, nil)
			if resp.StatusCode != http.StatusOK {
				body := paReadBody(t, resp)
				t.Fatalf("provider acceptance: status %d, want 200 (body %s)", resp.StatusCode, body)
			}
			paReadBody(t, resp)

			// The release precedes the record; paWaitRows settles the row set.
			lease := paAwaitLease(t, "lease release", h.policy.releasedCh)
			if lease.id != "a" || lease.released != 1 || lease.outcome.Status != http.StatusOK {
				t.Fatalf("provider acceptance: lease outcome = %+v released=%d", lease.outcome, lease.released)
			}
			row := paWaitRows(t, h, 1)[0]
			if row.UsageKnown != c.wantKnown {
				t.Fatalf("provider acceptance: usage_known = %v, want %v", row.UsageKnown, c.wantKnown)
			}
			if !equalCost(row.ReportedCostUSD, c.wantCost) {
				t.Fatalf("provider acceptance: reported cost = %s, want %s", paCostString(row.ReportedCostUSD), paCostString(c.wantCost))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Clean stream EOF with no usage anywhere: the counts stay unknown, not zero.
func TestProviderAcceptanceStreamCleanEOFWithoutUsage(t *testing.T) {
	// A stream that ends cleanly but never reports usage must be recorded with
	// UsageKnown=false (and no cost). This is the boundary against a zero Usage
	// value masquerading as a measured zero.
	cases := []struct {
		name     string
		provider string
		endpoint string
		model    string
		request  string
		sse      string
	}{
		{
			name: "openrouter_chat", provider: core.ProviderOpenRouter,
			endpoint: pathChat, model: "anthropic/claude-sonnet-4.5",
			request: `{"model":"anthropic/claude-sonnet-4.5","stream":true,"messages":[],"stream_options":{"include_usage":false}}`,
			sse:     paEvent(`data: {"choices":[{"delta":{"content":"hi"}}]}`) + paEvent("data: [DONE]"),
		},
		{
			name: "openai_compat_responses", provider: core.ProviderOpenAICompat,
			endpoint: pathResponses, model: "gpt-x",
			request: `{"model":"gpt-x","stream":true,"input":"hi"}`,
			sse: paEvent("event: response.created", `data: {"type":"response.created","response":{"id":"r1"}}`) +
				paEvent("event: response.output_text.delta", `data: {"type":"response.output_text.delta","delta":"hi"}`) +
				paEvent("event: response.completed", `data: {"type":"response.completed","response":{"id":"r1"}}`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			var hits atomic.Int32
			h.upstream("a", c.provider, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				paFlushChunks(w, c.sse, 7)
			})
			h.routes = []core.Route{{Name: "main", Models: []string{c.model}, Interactive: []string{"a"}, Background: []string{"a"}}}
			h.start()

			resp := paPost(t, h, c.endpoint, clientKey, c.request, nil)
			if resp.StatusCode != http.StatusOK {
				body := paReadBody(t, resp)
				t.Fatalf("provider acceptance: status %d, want 200 (body %s)", resp.StatusCode, body)
			}
			body := paReadBody(t, resp)
			if string(body) != c.sse {
				t.Fatalf("provider acceptance: stream altered:\n got %q\nwant %q", body, c.sse)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("provider acceptance: upstream hits = %d, want 1", got)
			}

			// The release precedes the record; paWaitRows settles the row set.
			lease := paAwaitLease(t, "lease release", h.policy.releasedCh)
			if lease.id != "a" || lease.released != 1 || lease.outcome.Status != http.StatusOK || lease.outcome.UsageKnown {
				t.Fatalf("provider acceptance: lease outcome = %+v released=%d", lease.outcome, lease.released)
			}
			row := paWaitRows(t, h, 1)[0]
			if row.AccountID != "a" || row.Provider != c.provider || row.Status != http.StatusOK {
				t.Fatalf("provider acceptance: row = %+v", row)
			}
			if row.Error != "" {
				t.Fatalf("provider acceptance: clean EOF recorded error %q, want none", row.Error)
			}
			if row.UsageKnown || row.Usage != (core.Usage{}) {
				t.Fatalf("provider acceptance: usage = %+v known=%v, want the unknown zero value", row.Usage, row.UsageKnown)
			}
			if row.ReportedCostUSD != nil {
				t.Fatalf("provider acceptance: reported cost = %s, want nil", paCostString(row.ReportedCostUSD))
			}
			if row.BytesOut != int64(len(c.sse)) {
				t.Fatalf("provider acceptance: bytes_out = %d, want %d", row.BytesOut, len(c.sse))
			}
			if got := len(h.policy.classes()); got != 1 {
				t.Fatalf("provider acceptance: policy acquisitions = %d, want 1", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Client cancellation propagates upstream.
// ---------------------------------------------------------------------------

// TestProviderAcceptanceClientCancelPropagatesUpstream pins that a client abort
// actually cancels the upstream request. The upstream handler reports two
// DISTINCT signals on two different channels:
//
//   - aborted  — closed only when the upstream request context ends, i.e. the
//     proxy really aborted the in-flight upstream request;
//   - fallback — closed only when the cleanup gate releases the handler, which
//     happens at test teardown and must never be mistaken for a cancellation.
//
// The handler never uses a timer, and every wait is bounded by paBound, so a
// proxy that fails to propagate the abort has no way to make aborted close: the
// await fails loudly and deterministically instead of winning a race with a
// shared timeout. The gate exists only so a failed assertion cannot leak the
// blocked handler.
func TestProviderAcceptanceClientCancelPropagatesUpstream(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		h := newHarness(t)
		gate := paCleanupGate(h)
		entered := make(chan struct{})
		aborted := make(chan struct{})
		fallback := make(chan struct{})
		h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
			// Draining the request body lets the upstream server arm its
			// disconnect detector, so r.Context() fires the moment the proxy
			// aborts the request instead of on a timer.
			_, _ = io.ReadAll(r.Body)
			close(entered)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				close(aborted)
			case <-gate:
				// Cleanup released us without a real abort. This is not a
				// cancellation and is deliberately reported on its own channel.
				close(fallback)
			}
		})
		h.routes = []core.Route{{Name: "main", Models: []string{"gpt-x"}, Interactive: []string{"a"}, Background: []string{"a"}}}
		h.start()

		// The deadline is only a safety net; cancel() is the client abort.
		ctx, cancel := context.WithTimeout(context.Background(), paBound)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+pathChat, strings.NewReader(`{"model":"gpt-x","stream":true,"messages":[]}`))
		if err != nil {
			t.Fatalf("provider acceptance: build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+clientKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("provider acceptance: request failed: %v", err)
		}
		// Reading the first flushed frame proves bytes reached the client, so a
		// disconnect here is an abort mid-stream, not a pre-write cancel.
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "data: ") {
			t.Fatalf("provider acceptance: first streamed line = %q err=%v", line, err)
		}
		paAwait(t, "upstream handler entry", entered)
		cancel()
		_ = resp.Body.Close()
		paAwait(t, "upstream request abort observed by the upstream handler", aborted)
		select {
		case <-fallback:
			t.Fatalf("provider acceptance: upstream handler was released by the cleanup gate, so no real upstream cancellation was observed")
		default:
		}

		// The release precedes the record; paWaitRows settles the row set.
		lease := paAwaitLease(t, "lease release", h.policy.releasedCh)
		if lease.released != 1 || lease.outcome.UsageKnown {
			t.Fatalf("provider acceptance: lease outcome = %+v released=%d", lease.outcome, lease.released)
		}
		row := paWaitRows(t, h, 1)[0]
		if row.Error != "client disconnected" || row.UsageKnown {
			t.Fatalf("provider acceptance: row error=%q usage_known=%v, want client disconnected / false", row.Error, row.UsageKnown)
		}
		if row.BytesOut == 0 {
			t.Fatalf("provider acceptance: expected bytes to have reached the client before the abort")
		}
	})

	t.Run("non_stream", func(t *testing.T) {
		h := newHarness(t)
		gate := paCleanupGate(h)
		entered := make(chan struct{})
		aborted := make(chan struct{})
		fallback := make(chan struct{})
		h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
			// See the stream case: drain so the upstream server can detect the
			// proxy's abort immediately.
			_, _ = io.ReadAll(r.Body)
			close(entered)
			select {
			case <-r.Context().Done():
				close(aborted)
			case <-gate:
				close(fallback)
			}
		})
		singleRoute(h, "a")
		h.start()

		ctx, cancel := context.WithTimeout(context.Background(), paBound)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+pathResponses, strings.NewReader(respBody))
		if err != nil {
			t.Fatalf("provider acceptance: build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+clientKey)
		done := make(chan *http.Response, 1)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- nil
				return
			}
			done <- resp
		}()
		paAwait(t, "upstream handler entry", entered)
		cancel()
		// The abort is asserted first and on its own channel, so a proxy that
		// never cancels the upstream request fails here rather than passing
		// because the request happened to return.
		paAwait(t, "upstream request abort observed by the upstream handler", aborted)
		select {
		case <-fallback:
			t.Fatalf("provider acceptance: upstream handler was released by the cleanup gate, so no real upstream cancellation was observed")
		default:
		}
		select {
		case resp := <-done:
			if resp != nil {
				_ = resp.Body.Close()
			}
		case <-time.After(paBound):
			t.Fatalf("provider acceptance: cancelled request never returned")
		}

		lease := paAwaitLease(t, "lease release", h.policy.releasedCh)
		if lease.released != 1 || lease.outcome.UsageKnown {
			t.Fatalf("provider acceptance: lease outcome = %+v released=%d", lease.outcome, lease.released)
		}
		row := paWaitRows(t, h, 1)[0]
		if row.Error != "client disconnected" || row.UsageKnown {
			t.Fatalf("provider acceptance: row error=%q usage_known=%v, want client disconnected / false", row.Error, row.UsageKnown)
		}
	})
}

// ---------------------------------------------------------------------------
// Inbound authentication: a rejected key never reaches the forwarding path.
// ---------------------------------------------------------------------------

func TestProviderAcceptanceBadAuthNeverReachesUpstream(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		hdr    map[string]string
		wantOK bool
	}{
		{name: "missing_header"},
		{name: "wrong_key", key: "sk-wrong-key"},
		{name: "basic_scheme", hdr: map[string]string{"Authorization": "Basic Zm9v"}},
		{name: "no_scheme", hdr: map[string]string{"Authorization": "sk-raw-token"}},
		{name: "bearer_without_token", hdr: map[string]string{"Authorization": "Bearer"}},
		{name: "accepted_bearer", key: clientKey, wantOK: true},
		{name: "accepted_scheme_case_insensitive", hdr: map[string]string{"Authorization": "BEARER " + clientKey}, wantOK: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			var hits atomic.Int32
			h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				okJSON(w, r)
			})
			singleRoute(h, "a")
			h.start()

			resp := paPost(t, h, pathResponses, c.key, respBody, c.hdr)

			if c.wantOK {
				if resp.StatusCode != http.StatusOK {
					body := paReadBody(t, resp)
					t.Fatalf("provider acceptance: status %d, want 200 (body %s)", resp.StatusCode, body)
				}
				paReadBody(t, resp)
				paWaitRows(t, h, 1)
				if hits.Load() != 1 {
					t.Fatalf("provider acceptance: upstream hits = %d, want 1", hits.Load())
				}
				return
			}

			if resp.StatusCode != http.StatusUnauthorized {
				body := paReadBody(t, resp)
				t.Fatalf("provider acceptance: status %d, want 401 (body %s)", resp.StatusCode, body)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("provider acceptance: 401 is missing WWW-Authenticate")
			}
			e := decodeErr(t, resp)
			if e.Error.Message == "" || e.Error.Type == "" {
				t.Fatalf("provider acceptance: error body = %+v", e)
			}
			if hits.Load() != 0 {
				t.Fatalf("provider acceptance: rejected request reached upstream (%d hits)", hits.Load())
			}
			if got := len(h.policy.classes()); got != 0 {
				t.Fatalf("provider acceptance: rejected request was admitted by policy (%d acquisitions)", got)
			}
			if got := len(h.ledger.all()); got != 0 {
				t.Fatalf("provider acceptance: rejected request wrote %d ledger rows", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Failover before response bytes: buffered upstream failures, chained rows,
// credential refresh, provider-specific retryable statuses, and whether each
// failure is request-scoped (Outcome.RequestScoped: no cooldown) or final.
// ---------------------------------------------------------------------------

type paRowSpec struct {
	acct   string
	status int
}

type paReleaseSpec struct {
	status     int
	resetAfter time.Duration // 0 = expect a zero ResetAt
	scoped     bool          // expected Outcome.RequestScoped (no cooldown)
}

type paFailoverScenario struct {
	name               string
	provider           string // provider of account "a"
	statuses           []int  // ordered statuses account "a" answers
	retryAfter         string
	failBody           string // account "a" failure body; "" = paFailBody
	second             bool   // account "b" (openai_compat, success) is a candidate
	relayed            bool   // exhausted: the buffered failure must reach the client
	final              bool   // a final answer: relayed from "a" without failover to "b"
	wantRows           []paRowSpec
	wantARelease       paReleaseSpec
	wantAInvalidations int
}

// paScripted is a fake upstream that answers each hit with the next status in
// its list (repeating the last), returning failBody (default paFailBody) for
// non-200 statuses.
type paScripted struct {
	statuses   []int
	retryAfter string
	failBody   string
	mu         sync.Mutex
	next       int
	hits       atomic.Int32
	auths      []string
}

func (s *paScripted) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		i := s.next
		if i >= len(s.statuses) {
			i = len(s.statuses) - 1
		}
		status := s.statuses[i]
		s.next++
		s.mu.Unlock()

		if status == http.StatusOK {
			okJSON(w, r)
			return
		}
		if s.retryAfter != "" {
			w.Header().Set("Retry-After", s.retryAfter)
		}
		w.WriteHeader(status)
		body := s.failBody
		if body == "" {
			body = paFailBody
		}
		_, _ = io.WriteString(w, body)
	}
}

func (s *paScripted) count() int { return int(s.hits.Load()) }

func (s *paScripted) authSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...)
}

func paFailoverScenarios() []paFailoverScenario {
	return []paFailoverScenario{
		{
			name: "openai_compat_401_refresh_same_account", provider: core.ProviderOpenAICompat,
			statuses: []int{401, 200}, wantAInvalidations: 1,
			wantRows:     []paRowSpec{{"a", 401}, {"a", 200}},
			wantARelease: paReleaseSpec{status: 200},
		},
		{
			name: "openai_compat_401_twice_then_failover", provider: core.ProviderOpenAICompat,
			statuses: []int{401, 401}, second: true, wantAInvalidations: 1,
			wantRows:     []paRowSpec{{"a", 401}, {"a", 401}, {"b", 200}},
			wantARelease: paReleaseSpec{status: 401},
		},
		{
			name: "openai_compat_401_twice_relayed_no_failover", provider: core.ProviderOpenAICompat,
			statuses: []int{401, 401}, wantAInvalidations: 1, relayed: true,
			wantRows:     []paRowSpec{{"a", 401}, {"a", 401}},
			wantARelease: paReleaseSpec{status: 401},
		},
		{
			name: "openai_compat_429_failover", provider: core.ProviderOpenAICompat,
			statuses: []int{429}, retryAfter: "120", second: true,
			wantRows:     []paRowSpec{{"a", 429}, {"b", 200}},
			wantARelease: paReleaseSpec{status: 429, resetAfter: 120 * time.Second},
		},
		{
			name: "codex_429_failover", provider: core.ProviderCodex,
			statuses: []int{429}, retryAfter: "60", second: true,
			wantRows:     []paRowSpec{{"a", 429}, {"b", 200}},
			wantARelease: paReleaseSpec{status: 429, resetAfter: 60 * time.Second},
		},
		{
			// A non-affordability 402 body is the account out of credit:
			// account-level, so policy may cool it down.
			name: "openrouter_402_failover", provider: core.ProviderOpenRouter,
			statuses: []int{402}, retryAfter: "90", second: true,
			wantRows:     []paRowSpec{{"a", 402}, {"b", 200}},
			wantARelease: paReleaseSpec{status: 402, resetAfter: 90 * time.Second},
		},
		{
			// The affordability preflight depends on this request: it still
			// fails over, but must not cool the account down.
			name: "openrouter_402_affordability_failover_request_scoped", provider: core.ProviderOpenRouter,
			statuses: []int{402}, retryAfter: "90", failBody: paAffordBody, second: true,
			wantRows:     []paRowSpec{{"a", 402}, {"b", 200}},
			wantARelease: paReleaseSpec{status: 402, resetAfter: 90 * time.Second, scoped: true},
		},
		{
			// OpenRouter answers moderation-flagged input with 403 (auth
			// failures are 401): no credential refresh, no failover.
			name: "openrouter_403_moderation_final", provider: core.ProviderOpenRouter,
			statuses: []int{403}, second: true, final: true,
			wantRows:     []paRowSpec{{"a", 403}},
			wantARelease: paReleaseSpec{status: 403, scoped: true},
		},
		{
			// Other providers' 403 stays an auth failure that refreshes.
			name: "openai_compat_403_refresh_same_account", provider: core.ProviderOpenAICompat,
			statuses: []int{403, 200}, wantAInvalidations: 1,
			wantRows:     []paRowSpec{{"a", 403}, {"a", 200}},
			wantARelease: paReleaseSpec{status: 200},
		},
		{
			// Without exhaustion evidence an OpenRouter 429 is a per-model or
			// upstream limit: fail over without cooling the account down.
			name: "openrouter_429_failover_request_scoped", provider: core.ProviderOpenRouter,
			statuses: []int{429}, retryAfter: "60", second: true,
			wantRows:     []paRowSpec{{"a", 429}, {"b", 200}},
			wantARelease: paReleaseSpec{status: 429, resetAfter: 60 * time.Second, scoped: true},
		},
		{
			name: "openai_compat_503_failover", provider: core.ProviderOpenAICompat,
			statuses: []int{503}, second: true,
			wantRows:     []paRowSpec{{"a", 503}, {"b", 200}},
			wantARelease: paReleaseSpec{status: 503},
		},
		{
			name: "openrouter_402_relayed_no_failover", provider: core.ProviderOpenRouter,
			statuses: []int{402}, retryAfter: "90", relayed: true,
			wantRows:     []paRowSpec{{"a", 402}},
			wantARelease: paReleaseSpec{status: 402, resetAfter: 90 * time.Second},
		},
	}
}

func TestProviderAcceptanceFailoverBeforeBytes(t *testing.T) {
	for _, s := range paFailoverScenarios() {
		t.Run(s.name, func(t *testing.T) {
			h := newHarness(t)
			a := &paScripted{statuses: s.statuses, retryAfter: s.retryAfter, failBody: s.failBody}
			b := &paScripted{statuses: []int{http.StatusOK}}
			h.upstream("a", s.provider, a.handler())
			h.upstream("b", core.ProviderOpenAICompat, b.handler())
			accts := []string{"a"}
			if s.second {
				accts = append(accts, "b")
			}
			singleRoute(h, accts...)
			h.start()

			resp := paPost(t, h, pathResponses, clientKey, respBody, nil)
			body := paReadBody(t, resp)

			if s.relayed || s.final {
				want := s.statuses[len(s.statuses)-1]
				if resp.StatusCode != want {
					t.Fatalf("provider acceptance: relayed status = %d, want %d (body %s)", resp.StatusCode, want, body)
				}
				if !strings.Contains(string(body), "pa-fail-marker") {
					t.Fatalf("provider acceptance: failure body not relayed: %s", body)
				}
				if got := resp.Header.Get("Retry-After"); got != s.retryAfter {
					t.Fatalf("provider acceptance: relayed Retry-After = %q, want %q", got, s.retryAfter)
				}
			} else {
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("provider acceptance: status = %d, want 200 (body %s)", resp.StatusCode, body)
				}
				if !strings.Contains(string(body), `"r1"`) {
					t.Fatalf("provider acceptance: success body missing from client response: %s", body)
				}
				if strings.Contains(string(body), "pa-fail-marker") {
					t.Fatalf("provider acceptance: a failed attempt's body reached the client: %s", body)
				}
			}

			// Every attempt releases its lease and then writes its record, so
			// the releases alone do not settle the row set; paWaitRows waits
			// for the proxy handler to finish before asserting the exact count.
			leaseA := paAwaitLease(t, "release of account a lease", h.policy.releasedCh)
			if leaseA.id != "a" || leaseA.released != 1 || leaseA.outcome.Status != s.wantARelease.status {
				t.Fatalf("provider acceptance: account a lease outcome = %+v released=%d, want status %d", leaseA.outcome, leaseA.released, s.wantARelease.status)
			}
			if s.wantARelease.resetAfter == 0 {
				if !leaseA.outcome.ResetAt.IsZero() {
					t.Fatalf("provider acceptance: account a ResetAt = %v, want zero", leaseA.outcome.ResetAt)
				}
			} else if want := testNow.Add(s.wantARelease.resetAfter); !leaseA.outcome.ResetAt.Equal(want) {
				t.Fatalf("provider acceptance: account a ResetAt = %v, want %v", leaseA.outcome.ResetAt, want)
			}
			if leaseA.outcome.RequestScoped != s.wantARelease.scoped {
				t.Fatalf("provider acceptance: account a RequestScoped = %v, want %v", leaseA.outcome.RequestScoped, s.wantARelease.scoped)
			}
			if s.second && !s.final {
				leaseB := paAwaitLease(t, "release of account b lease", h.policy.releasedCh)
				if leaseB.id != "b" || leaseB.released != 1 || leaseB.outcome.Status != http.StatusOK || !leaseB.outcome.UsageKnown {
					t.Fatalf("provider acceptance: account b lease outcome = %+v released=%d", leaseB.outcome, leaseB.released)
				}
			}
			rows := paWaitRows(t, h, len(s.wantRows))
			for i, want := range s.wantRows {
				if rows[i].AccountID != want.acct || rows[i].Status != want.status {
					t.Fatalf("provider acceptance: row %d = %s/%d, want %s/%d", i, rows[i].AccountID, rows[i].Status, want.acct, want.status)
				}
				if i == 0 {
					if rows[i].FailoverOf != "" {
						t.Fatalf("provider acceptance: row 0 has failover_of %q, want empty", rows[i].FailoverOf)
					}
				} else if rows[i].FailoverOf != rows[i-1].ID {
					t.Fatalf("provider acceptance: row %d failover_of = %q, want %q", i, rows[i].FailoverOf, rows[i-1].ID)
				}
			}

			var wantA, wantB int
			for _, r := range s.wantRows {
				if r.acct == "a" {
					wantA++
				} else {
					wantB++
				}
			}
			if got := a.count(); got != wantA {
				t.Fatalf("provider acceptance: account a hits = %d, want %d", got, wantA)
			}
			if got := b.count(); got != wantB {
				t.Fatalf("provider acceptance: account b hits = %d, want %d", got, wantB)
			}

			// One lease per distinct candidate account; a final acquisition with
			// no admissible account happens only on the exhausted path. A final
			// answer stops at the first account.
			wantAcquisitions, wantLeases := len(accts), len(accts)
			switch {
			case s.final:
				wantAcquisitions, wantLeases = 1, 1
			case s.relayed:
				wantAcquisitions++
			}
			if got := len(h.policy.classes()); got != wantAcquisitions {
				t.Fatalf("provider acceptance: policy acquisitions = %d, want %d", got, wantAcquisitions)
			}
			if got := len(h.policy.leases); got != wantLeases {
				t.Fatalf("provider acceptance: leases granted = %d, want %d", got, wantLeases)
			}

			if got := h.creds.invalidated["a"]; got != s.wantAInvalidations {
				t.Fatalf("provider acceptance: credential invalidations for a = %d, want %d", got, s.wantAInvalidations)
			}
			if s.wantAInvalidations > 0 {
				auths := a.authSnapshot()
				if len(auths) < 2 || auths[0] == auths[1] {
					t.Fatalf("provider acceptance: refreshed credential not used on retry: %v", auths)
				}
			}

		})
	}
}

// ---------------------------------------------------------------------------
// No failover after response bytes: once any byte reached the client, a broken
// stream must not be retried on another account.
// ---------------------------------------------------------------------------

func TestProviderAcceptanceNoFailoverAfterResponseBytes(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		endpoint string
		model    string
		request  string
	}{
		{"openai_compat_responses", core.ProviderOpenAICompat, pathResponses, "gpt-x", `{"model":"gpt-x","stream":true}`},
		{"codex_responses", core.ProviderCodex, pathResponses, "gpt-x", `{"model":"gpt-x","stream":true}`},
		{"openrouter_chat", core.ProviderOpenRouter, pathChat, "anthropic/claude-sonnet-4.5", `{"model":"anthropic/claude-sonnet-4.5","stream":true,"messages":[]}`},
		{"ollama_chat", core.ProviderOllama, pathChat, "gpt-x", `{"model":"gpt-x","stream":true,"messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			var bHits atomic.Int32
			// Account a advertises a body far larger than it sends, writes one
			// flushed SSE frame, then drops the connection: the proxy streams
			// the frame to the client and then hits a read error.
			h.upstream("a", c.provider, func(w http.ResponseWriter, _ *http.Request) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Errorf("provider acceptance: upstream handler is not hijackable")
					return
				}
				conn, brw, err := hj.Hijack()
				if err != nil {
					t.Errorf("provider acceptance: hijack: %v", err)
					return
				}
				defer conn.Close()
				_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 100000\r\n\r\n")
				_, _ = brw.WriteString(paTruncChunk)
				_ = brw.Flush()
			})
			h.upstream("b", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
				bHits.Add(1)
				okJSON(w, r)
			})
			h.routes = []core.Route{{Name: "main", Models: []string{c.model}, Interactive: []string{"a", "b"}, Background: []string{"a", "b"}}}
			h.start()

			resp := paPost(t, h, c.endpoint, clientKey, c.request, nil)
			if resp.StatusCode != http.StatusOK {
				body := paReadBody(t, resp)
				t.Fatalf("provider acceptance: status %d, want 200 (body %s)", resp.StatusCode, body)
			}
			body := paReadBody(t, resp)
			if string(body) != paTruncChunk {
				t.Fatalf("provider acceptance: client body = %q, want the pre-error bytes %q", body, paTruncChunk)
			}

			if got := bHits.Load(); got != 0 {
				t.Fatalf("provider acceptance: failover after response bytes reached account b (%d hits)", got)
			}
			// The release precedes the record; paWaitRows settles the row set.
			lease := paAwaitLease(t, "lease release", h.policy.releasedCh)
			if lease.id != "a" || lease.released != 1 || lease.outcome.UsageKnown {
				t.Fatalf("provider acceptance: lease outcome = %+v released=%d", lease.outcome, lease.released)
			}
			rows := paWaitRows(t, h, 1)
			row := rows[0]
			if row.AccountID != "a" || row.Provider != c.provider || row.Status != http.StatusOK {
				t.Fatalf("provider acceptance: row = %+v", row)
			}
			if row.UsageKnown || !strings.Contains(row.Error, "upstream stream error") {
				t.Fatalf("provider acceptance: row error = %q usage_known=%v, want an upstream stream error with unknown usage", row.Error, row.UsageKnown)
			}
			if row.BytesOut != int64(len(paTruncChunk)) {
				t.Fatalf("provider acceptance: bytes_out = %d, want %d", row.BytesOut, len(paTruncChunk))
			}
			if got := len(h.policy.classes()); got != 1 {
				t.Fatalf("provider acceptance: policy acquisitions = %d, want 1 (no failover)", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Suite self-check: the exact-count row helper must see a row that an
// in-flight handler writes after the first n rows were observed.
// ---------------------------------------------------------------------------

// paFatalRecorder captures a Fatalf from the helper under test instead of
// failing the enclosing test, then stops the calling goroutine like t.Fatalf.
type paFatalRecorder struct {
	testing.TB
	mu  sync.Mutex
	msg string
}

func (r *paFatalRecorder) Helper() {}

func (r *paFatalRecorder) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.msg = fmt.Sprintf(format, args...)
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *paFatalRecorder) message() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.msg
}

func TestProviderAcceptanceWaitRowsDetectsLateRow(t *testing.T) {
	h := newHarness(t)
	// Stand-in for the proxy: it records one row and completes the response,
	// then stays in flight until its client connection goes away and only then
	// writes a stray second row, the way a handler that is still running after
	// the client has its response could.
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_ = h.ledger.Record(context.Background(), core.RequestRecord{ID: "first"})
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		_ = h.ledger.Record(context.Background(), core.RequestRecord{ID: "late"})
	}))
	t.Cleanup(h.srv.Close)

	paReadBody(t, paPost(t, h, pathChat, clientKey, `{}`, nil))

	rec := &paFatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		paWaitRows(rec, h, 1)
	}()
	paAwait(t, "paWaitRows to return", done)
	if msg := rec.message(); !strings.Contains(msg, "want exactly 1") {
		t.Fatalf("provider acceptance: paWaitRows accepted a late stray row (failure message %q)", msg)
	}
}
