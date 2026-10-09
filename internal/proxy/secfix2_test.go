package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/policy"
)

// --- PROXY-1 (wave 2): affordability 402 is request-scoped but retryable ---

func realPolicy(h *harness, ids ...string) {
	var accts []core.Account
	for _, id := range ids {
		accts = append(accts, h.accounts[id])
	}
	h.pol = policy.New(accts, h.quota, policy.Options{Clock: fixedClock{testNow}})
}

func requireNoCooldown(t *testing.T, h *harness, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if st := h.pol.Status(id); !st.CooldownUntil.IsZero() {
			t.Fatalf("%s cooled down: %+v", id, st)
		}
	}
}

// OpenRouter's affordability preflight depends on the request (max_tokens,
// prompt size, model price), including "can only afford 0" for a huge
// prompt. It must not cool the account down, but another account may have
// more credit, so the request still fails over.
func TestSecAffordability402FailsOverWithoutCooldown(t *testing.T) {
	for name, afford := range map[string]int{"positive": 6937, "zero": 0} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			var hitsA, hitsB atomic.Int32
			h.upstream("a", core.ProviderOpenRouter, affordable402(&hitsA, afford))
			h.upstream("b", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) { hitsB.Add(1); okJSON(w, r) })
			singleRoute(h, "a", "b")
			realPolicy(h, "a", "b")
			h.start()

			resp := h.post("/v1/chat/completions", bgKey, `{"model":"gpt-x","max_tokens":999999999,"messages":[]}`, nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || hitsA.Load() != 1 || hitsB.Load() != 1 {
				t.Fatalf("status %d hits a=%d b=%d body %s", resp.StatusCode, hitsA.Load(), hitsB.Load(), body)
			}
			requireNoCooldown(t, h, "a", "b")

			resp = h.post("/v1/chat/completions", clientKey, `{"model":"gpt-x","messages":[]}`, nil)
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || hitsA.Load() != 2 {
				t.Fatalf("interactive status %d hits a=%d: %s", resp.StatusCode, hitsA.Load(), body)
			}
			rows := h.waitRows(3)
			if rows[0].Status != 402 || !strings.Contains(rows[0].Error, "failing over") {
				t.Fatalf("row %+v", rows[0])
			}
		})
	}
}

// When every candidate answers with the affordability 402, the client gets
// the last upstream 402 intact and no account is cooled down.
func TestSecAffordability402AllCandidatesRelayed(t *testing.T) {
	h := newHarness(t)
	var hitsA, hitsB atomic.Int32
	h.upstream("a", core.ProviderOpenRouter, affordable402(&hitsA, 0))
	h.upstream("b", core.ProviderOpenRouter, maxTokens402(&hitsB))
	singleRoute(h, "a", "b")
	realPolicy(h, "a", "b")
	h.start()

	resp := h.post("/v1/chat/completions", bgKey, `{"model":"gpt-x","max_tokens":999999999,"messages":[]}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(string(body), "fewer max_tokens") ||
		!strings.HasSuffix(strings.TrimSpace(string(body)), "}}") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("hits a=%d b=%d", hitsA.Load(), hitsB.Load())
	}
	requireNoCooldown(t, h, "a", "b")
}

// A recent positive balance does not prove current funds: an "Insufficient
// credits" 402 is account-level (cooldown, failover) on both endpoints.
func TestSecInsufficientCreditsFreshPositiveSnapshotFailsOver(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t)
			var hitsA, hitsB atomic.Int32
			h.upstream("a", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
				hitsA.Add(1)
				w.WriteHeader(http.StatusPaymentRequired)
				_, _ = io.WriteString(w, `{"error":{"code":402,"message":"Insufficient credits. Your account balance is exhausted."}}`)
			})
			h.upstream("b", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) { hitsB.Add(1); okJSON(w, r) })
			singleRoute(h, "a", "b")
			h.quota.setSnapshot("a", fundedSnapshot(1, testNow.Add(-time.Minute)))
			realPolicy(h, "a", "b")
			h.start()

			resp := h.post(path, clientKey, `{"model":"gpt-x","messages":[]}`, nil)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || hitsA.Load() != 1 || hitsB.Load() != 1 {
				t.Fatalf("status %d hits a=%d b=%d", resp.StatusCode, hitsA.Load(), hitsB.Load())
			}
			if st := h.pol.Status("a"); st.CooldownUntil.IsZero() {
				t.Fatalf("a not cooled down: %+v", st)
			}
		})
	}
}

// A known exhausted balance makes even an affordability 402 account-level.
func TestSecAffordabilityWithExhaustedSnapshotIsAccountLevel(t *testing.T) {
	h := newHarness(t)
	var hitsA, hitsB atomic.Int32
	h.upstream("a", core.ProviderOpenRouter, affordable402(&hitsA, 50))
	h.upstream("b", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) { hitsB.Add(1); okJSON(w, r) })
	singleRoute(h, "a", "b")
	h.quota.setSnapshot("a", fundedSnapshot(0, testNow))
	h.start()

	resp := h.post("/v1/chat/completions", clientKey, `{"model":"gpt-x","max_tokens":999999999,"messages":[]}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("status %d hits a=%d b=%d", resp.StatusCode, hitsA.Load(), hitsB.Load())
	}
	if o := h.policy.leases[0].outcome; o.Status != 402 || o.RequestScoped {
		t.Fatalf("outcome %+v", o)
	}
}

// --- Pre-stream upstream body reads honor StreamIdleTimeout ---

// stalledHandler sends status and prefix, flushes, then stalls until the
// request is canceled; canceled is closed when that happens.
func stalledHandler(status int, prefix string, canceled chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, prefix)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(5 * time.Second):
		}
	}
}

func postWithin(t *testing.T, h *harness, path string, d time.Duration) (*http.Response, []byte, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+path, bytes.NewBufferString(`{"model":"gpt-x","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+clientKey)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed after %v: %v", time.Since(start), err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("body read failed after %v: %v", time.Since(start), err)
	}
	return resp, body, time.Since(start)
}

func waitClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s", what)
	}
}

// Adapted from Sol's PoC: the final attempt's 402 body is read for
// classification before streaming; a stalled body must end after the idle
// timeout, not the client deadline, and abort the upstream.
func TestSecFinal402ClassificationReadIdleTimeout(t *testing.T) {
	h := newHarness(t)
	h.opts.MaxFailovers = 1
	h.opts.StreamIdleTimeout = 30 * time.Millisecond
	h.upstream("a", core.ProviderOpenRouter, payment402)
	canceled := make(chan struct{})
	h.upstream("b", core.ProviderOpenRouter, stalledHandler(http.StatusPaymentRequired, `{"error":`, canceled))
	singleRoute(h, "a", "b")
	h.start()

	resp, body, elapsed := postWithin(t, h, "/v1/chat/completions", 3*time.Second)
	if elapsed > time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if resp.StatusCode != http.StatusPaymentRequired || string(body) != `{"error":` {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	waitClosed(t, canceled, "stalled upstream not canceled")
	rows := h.waitRows(2)
	if rows[1].AccountID != "b" || rows[1].Status != 402 || rows[1].Error != "upstream idle timeout reading error body" {
		t.Fatalf("row %+v", rows[1])
	}
	if o := h.policy.leases[1].outcome; o.Status != 402 || o.RequestScoped {
		t.Fatalf("outcome %+v", o)
	}
}

// The failover buffering of a retryable error body (and the 402
// classification read on a non-final attempt) is idle-bounded too.
func TestSecFailoverBufferReadIdleTimeout(t *testing.T) {
	for name, tc := range map[string]struct {
		provider string
		status   int
	}{
		"5xx":            {core.ProviderOpenAICompat, http.StatusBadGateway},
		"openrouter 402": {core.ProviderOpenRouter, http.StatusPaymentRequired},
		"openrouter 429": {core.ProviderOpenRouter, http.StatusTooManyRequests},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.opts.StreamIdleTimeout = 30 * time.Millisecond
			canceled := make(chan struct{})
			h.upstream("a", tc.provider, stalledHandler(tc.status, `{"error":"can only afford 5`, canceled))
			h.upstream("b", tc.provider, okJSON)
			singleRoute(h, "a", "b")
			h.start()

			resp, _, elapsed := postWithin(t, h, "/v1/chat/completions", 3*time.Second)
			if elapsed > time.Second || resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d after %v", resp.StatusCode, elapsed)
			}
			waitClosed(t, canceled, "stalled upstream not canceled")
			rows := h.waitRows(2)
			if rows[0].Status != tc.status || rows[0].Error != "upstream idle timeout reading error body" {
				t.Fatalf("row %+v", rows[0])
			}
			// An unreadable 402 body is classified account-level.
			if o := h.policy.leases[0].outcome; tc.status == 402 && o.RequestScoped {
				t.Fatalf("outcome %+v", o)
			}
		})
	}
}

// The body drained before an auth retry is idle-bounded, and the timeout
// does not cancel the retry on the same account.
func TestSecAuthRetryDrainIdleTimeout(t *testing.T) {
	h := newHarness(t)
	h.opts.StreamIdleTimeout = 30 * time.Millisecond
	canceled := make(chan struct{})
	stalled := stalledHandler(http.StatusUnauthorized, "x", canceled)
	var hits atomic.Int32
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			stalled(w, r)
			return
		}
		okJSON(w, r)
	})
	singleRoute(h, "a")
	h.start()
	resp, _, elapsed := postWithin(t, h, "/v1/responses", 3*time.Second)
	if elapsed > time.Second || resp.StatusCode != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("status %d hits %d after %v", resp.StatusCode, hits.Load(), elapsed)
	}
	waitClosed(t, canceled, "stalled upstream not canceled")
}

// --- Nullable stream ---

// SDKs send an optional stream as null; it means "not streaming".
func TestSecNullableStreamAccepted(t *testing.T) {
	h := newHarness(t)
	hits, bodies := countingUpstream(h, "a", core.ProviderOpenAICompat)
	singleRoute(h, "a")
	h.start()
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		in := `{"model":"gpt-x","stream":null,"messages":[]}`
		resp := h.post(path, clientKey, in, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d %s", path, resp.StatusCode, body)
		}
		if got := string(<-bodies); got != in {
			t.Fatalf("%s: body changed: %q", path, got)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("hits %d", hits.Load())
	}
	if hd, err := parseHead([]byte(`{"model":"m","stream":null}`)); err != nil || hd.Stream {
		t.Fatalf("parseHead %+v %v", hd, err)
	}
}

// --- Usage subsets ---

func TestSecUsageSubsetsClamped(t *testing.T) {
	chat := `{"usage":{"prompt_tokens":1,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":999999999999},"completion_tokens_details":{"reasoning_tokens":999999999999}}}`
	responses := `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":7}}}}`
	want := core.Usage{InputTokens: 1, CachedInputTokens: 1, OutputTokens: 2, ReasoningTokens: 2}
	for name, tc := range map[string]struct{ ct, body string }{
		"json chat":      {"application/json", chat},
		"json responses": {"application/json", `{"usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":7}}}`},
		"sse chat":       {"text/event-stream", "data: " + chat + "\n\ndata: [DONE]\n\n"},
		"sse responses":  {"text/event-stream", "event: response.completed\ndata: " + responses + "\n\n"},
	} {
		c := newUsageCapture(tc.ct)
		_, _ = c.Write([]byte(tc.body))
		if u, ok := c.Result(); !ok || u != want {
			t.Errorf("%s: usage %+v known %v", name, u, ok)
		}
	}

	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chat)
	})
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/chat/completions", clientKey, `{"model":"gpt-x","messages":[]}`, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if rows := h.waitRows(1); !rows[0].UsageKnown || rows[0].Usage != want {
		t.Fatalf("row %+v", rows[0])
	}
}
