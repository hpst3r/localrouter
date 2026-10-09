package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/policy"
)

// --- PROXY-2: one case-sensitive parse of the request body ---

// countingUpstream registers an account that counts hits and captures bodies.
func countingUpstream(h *harness, id, provider string) (*atomic.Int32, chan []byte) {
	var hits atomic.Int32
	bodies := make(chan []byte, 16)
	h.upstream(id, provider, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		select {
		case bodies <- b:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	})
	return &hits, bodies
}

// The route is selected from the exact lowercase "model" key, so a
// case-variant key that encoding/json would have matched is rejected
// instead of letting the upstream run a different model than was routed.
func TestSecCaseFoldModelRejected(t *testing.T) {
	h := newHarness(t)
	hits, _ := countingUpstream(h, "o", core.ProviderOpenRouter)
	h.routes = []core.Route{{Name: "cheap", Models: []string{"cheap-model"}, Interactive: []string{"o"}, Background: []string{"o"}}}
	h.start()
	in := `{"model":"expensive/unrouted-model","MODEL":"cheap-model","messages":[]}`
	resp := h.post("/v1/chat/completions", bgKey, in, nil)
	e := decodeErr(t, resp)
	if resp.StatusCode != http.StatusBadRequest || e.Error.Type != "invalid_request_error" {
		t.Fatalf("status %d %+v", resp.StatusCode, e)
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream reached %d times", hits.Load())
	}
}

func TestSecCaseFoldStreamRejected(t *testing.T) {
	h := newHarness(t)
	hits, _ := countingUpstream(h, "o", core.ProviderOllama)
	singleRoute(h, "o")
	h.start()
	in := `{"model":"gpt-x","stream":true,"Stream":false,"messages":[]}`
	resp := h.post("/v1/chat/completions", clientKey, in, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || hits.Load() != 0 {
		t.Fatalf("status %d hits %d", resp.StatusCode, hits.Load())
	}
}

func TestSecAmbiguousBodiesRejected(t *testing.T) {
	h := newHarness(t)
	hits, _ := countingUpstream(h, "o", core.ProviderOllama)
	singleRoute(h, "o")
	h.start()
	cases := map[string]string{
		"duplicate model":         `{"model":"gpt-x","model":"gpt-x"}`,
		"duplicate escaped model": `{"model":"gpt-x","model":"other"}`,
		"duplicate other key":     `{"model":"gpt-x","messages":[],"messages":[]}`,
		"duplicate stream":        `{"model":"gpt-x","stream":false,"stream":true}`,
		"case-variant model":      `{"Model":"gpt-x"}`,
		"case-variant stream":     `{"model":"gpt-x","STREAM":true}`,
		"unicode-fold stream":     `{"model":"gpt-x","ſtream":true}`,
		"trailing object":         `{"model":"gpt-x"}{"model":"other"}`,
		"trailing garbage":        `{"model":"gpt-x"} x`,
		"model not string":        `{"model":["gpt-x"]}`,
		"model empty":             `{"model":""}`,
		"model missing":           `{"messages":[]}`,
		"stream string":           `{"model":"gpt-x","stream":"true"}`,
		"stream number":           `{"model":"gpt-x","stream":1}`,
		"stream null":             `{"model":"gpt-x","stream":null}`,
		"top-level array":         `[{"model":"gpt-x"}]`,
		"invalid utf-8":           "{\"model\":\"gpt-x\",\"x\xff\":1}",
		"unterminated":            `{"model":"gpt-x",`,
	}
	for name, in := range cases {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			resp := h.post(path, clientKey, in, nil)
			e := decodeErr(t, resp)
			if resp.StatusCode != http.StatusBadRequest || e.Error.Type != "invalid_request_error" {
				t.Errorf("%s %s: status %d %+v", name, path, resp.StatusCode, e)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream reached %d times", hits.Load())
	}
}

// Unambiguous bodies, including case-variant keys nested below the top
// level, still go upstream byte for byte when no edit is needed.
func TestSecValidBodiesForwardedVerbatim(t *testing.T) {
	h := newHarness(t)
	_, bodies := countingUpstream(h, "o", core.ProviderOllama)
	singleRoute(h, "o")
	h.start()
	for _, in := range []string{
		`{"model":"gpt-x",  "messages":[{"role":"user","content":"<b>&</b>","Model":"x","MODEL":"y","model":"z"}],"metadata":{"Stream":true,"stream":1}}`,
		"\n{\"model\":\"gpt-x\",\"stream\":false,\"Streaming\":1,\"models\":2}\r\n",
		`{"model":"gpt-x","x":"<"}`,
	} {
		resp := h.post("/v1/chat/completions", clientKey, in, nil)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d for %s", resp.StatusCode, in)
		}
		if got := string(<-bodies); got != in {
			t.Fatalf("body changed:\n got %q\nwant %q", got, in)
		}
	}
}

// --- PROXY-1: request-scoped failures do not cool accounts down ---

func fundedSnapshot(balance float64, fetched time.Time) core.Snapshot {
	return core.Snapshot{FetchedAt: fetched, Source: "usage_api",
		Credits: &core.Credits{TotalCreditsUSD: balance, BalanceUSD: balance, FetchedAt: fetched}}
}

func maxTokens402(hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"max_tokens":999999999`) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, `{"error":{"code":402,"message":"This request requires more credits, or fewer max_tokens."}}`)
			return
		}
		okJSON(w, r)
	}
}

// affordable402 answers like OpenRouter's real affordability check: the
// message states how many tokens the key can still afford.
func affordable402(hits *atomic.Int32, afford int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"max_tokens":999999999`) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = fmt.Fprintf(w, `{"error":{"code":402,"message":"This request requires more credits, or fewer max_tokens. You requested up to 999999999 tokens, but can only afford %d. To increase, visit https://openrouter.ai/settings/keys"}}`, afford)
			return
		}
		okJSON(w, r)
	}
}

// Accounts without a management key never learn their balance, which is the
// review PoC's setup. The 402 body itself proves funds ("can only afford N",
// N > 0), so the request is still relayed without failover or cooldown, and
// the relayed body is intact after the classifier peeked at it.
func TestSecRequestScoped402WithoutBalance(t *testing.T) {
	h := newHarness(t)
	var hits1, hits2 atomic.Int32
	h.upstream("or1", core.ProviderOpenRouter, affordable402(&hits1, 6937))
	h.upstream("or2", core.ProviderOpenRouter, affordable402(&hits2, 6937))
	singleRoute(h, "or1", "or2")
	h.pol = policy.New([]core.Account{h.accounts["or1"], h.accounts["or2"]}, h.quota, policy.Options{Clock: fixedClock{testNow}})
	h.start()

	resp := h.post("/v1/chat/completions", bgKey, `{"model":"gpt-x","max_tokens":999999999,"messages":[]}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(string(body), "can only afford 6937") ||
		!strings.HasSuffix(strings.TrimSpace(string(body)), "}}") {
		t.Fatalf("bg status %d body %s", resp.StatusCode, body)
	}
	if hits1.Load()+hits2.Load() != 1 {
		t.Fatalf("request replayed: or1=%d or2=%d", hits1.Load(), hits2.Load())
	}
	resp = h.post("/v1/chat/completions", clientKey, `{"model":"gpt-x","messages":[]}`, nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("interactive status %d: %s", resp.StatusCode, body)
	}
	if st := h.pol.Status("or1"); !st.CooldownUntil.IsZero() {
		t.Fatalf("or1 cooled down: %+v", st)
	}
}

// "can only afford 0" (or no affordable amount, or a known exhausted balance)
// is the account being out of credit: cooldown and failover as before.
func TestSecAffordZeroIsAccountLevel(t *testing.T) {
	for name, tc := range map[string]struct {
		afford int
		snap   *core.Snapshot
	}{
		"afford zero":            {afford: 0},
		"exhausted balance wins": {afford: 50, snap: ptrSnap(fundedSnapshot(0, testNow))},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			var hits, hitsB atomic.Int32
			h.upstream("or", core.ProviderOpenRouter, affordable402(&hits, tc.afford))
			h.upstream("b", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) { hitsB.Add(1); okJSON(w, r) })
			singleRoute(h, "or", "b")
			if tc.snap != nil {
				h.quota.setSnapshot("or", *tc.snap)
			}
			h.start()
			resp := h.post("/v1/chat/completions", clientKey, `{"model":"gpt-x","max_tokens":999999999,"messages":[]}`, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || hitsB.Load() != 1 {
				t.Fatalf("status %d, failover hits %d", resp.StatusCode, hitsB.Load())
			}
			if o := h.policy.leases[0].outcome; o.Status != 402 || o.RequestScoped {
				t.Fatalf("outcome %+v", o)
			}
		})
	}
}

func ptrSnap(s core.Snapshot) *core.Snapshot { return &s }

// Adapted from the review PoC: a 402 while the balance is known positive is
// the request's problem. It is relayed without failover and without a
// cooldown, so an interactive request on the same route still succeeds.
func TestSecRequestScoped402DoesNotLockOutInteractive(t *testing.T) {
	h := newHarness(t)
	var hits1, hits2 atomic.Int32
	h.upstream("or1", core.ProviderOpenRouter, maxTokens402(&hits1))
	h.upstream("or2", core.ProviderOpenRouter, maxTokens402(&hits2))
	singleRoute(h, "or1", "or2")
	h.quota.setSnapshot("or1", fundedSnapshot(25, testNow.Add(-time.Minute)))
	h.quota.setSnapshot("or2", fundedSnapshot(25, testNow.Add(-time.Minute)))
	h.pol = policy.New([]core.Account{h.accounts["or1"], h.accounts["or2"]}, h.quota, policy.Options{Clock: fixedClock{testNow}})
	h.start()

	resp := h.post("/v1/chat/completions", bgKey, `{"model":"gpt-x","max_tokens":999999999,"messages":[]}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(string(body), "fewer max_tokens") {
		t.Fatalf("bg status %d body %s", resp.StatusCode, body)
	}
	if hits1.Load()+hits2.Load() != 1 {
		t.Fatalf("request replayed: or1=%d or2=%d", hits1.Load(), hits2.Load())
	}

	resp = h.post("/v1/chat/completions", clientKey, `{"model":"gpt-x","messages":[]}`, nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("interactive status %d: %s", resp.StatusCode, body)
	}
	if st := h.pol.Status("or1"); !st.CooldownUntil.IsZero() {
		t.Fatalf("or1 cooled down: %+v", st)
	}
	if q := h.quota.refresh["or1"]; q == 0 {
		t.Errorf("no quota refresh requested")
	}
}

// Without proof of funds a 402 stays an account-level answer: cooldown and
// failover, so a genuinely empty account does not block the route.
func TestSecAccountLevel402StillFailsOver(t *testing.T) {
	cases := map[string]func(q *fakeQuota){
		"balance zero":  func(q *fakeQuota) { q.setSnapshot("or", fundedSnapshot(0, testNow)) },
		"balance stale": func(q *fakeQuota) { q.setSnapshot("or", fundedSnapshot(25, testNow.Add(-time.Hour))) },
		"key cap spent": func(q *fakeQuota) {
			s := fundedSnapshot(25, testNow)
			zero, cap := 0.0, 10.0
			s.Key = &core.KeyUsage{LimitUSD: &cap, LimitRemainingUSD: &zero, FetchedAt: testNow}
			q.setSnapshot("or", s)
		},
		"unknown": func(*fakeQuota) {},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.upstream("or", core.ProviderOpenRouter, payment402)
			h.upstream("b", core.ProviderOpenRouter, okJSON)
			orRoute(h, "or", "b")
			setup(h.quota)
			h.start()
			resp := h.post("/v1/chat/completions", clientKey, orChatBody, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if o := h.policy.leases[0].outcome; o.Status != 402 || o.RequestScoped {
				t.Fatalf("outcome %+v", o)
			}
		})
	}
}

// The real policy still cools a genuinely empty account down.
func TestSecExhausted402CoolsDown(t *testing.T) {
	h := newHarness(t)
	h.upstream("or", core.ProviderOpenRouter, payment402)
	orRoute(h, "or")
	h.pol = policy.New([]core.Account{h.accounts["or"]}, h.quota, policy.Options{Clock: fixedClock{testNow}})
	h.start()
	resp := h.post("/v1/chat/completions", clientKey, orChatBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if st := h.pol.Status("or"); st.CooldownUntil.IsZero() {
		t.Fatalf("no cooldown: %+v", st)
	}
}

// OpenRouter answers 403 for moderation-flagged input: final for this
// request, with no credential refresh, failover or cooldown.
func TestSecOpenRouter403RelayedWithoutRefreshOrFailover(t *testing.T) {
	h := newHarness(t)
	var orHits atomic.Int32
	h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		orHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":403,"message":"input was flagged"}}`)
	})
	bHits, _ := countingUpstream(h, "b", core.ProviderOpenRouter)
	orRoute(h, "or", "b")
	h.start()
	resp := h.post("/v1/chat/completions", clientKey, orChatBody, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "flagged") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if orHits.Load() != 1 || bHits.Load() != 0 || h.creds.invalidated["or"] != 0 {
		t.Fatalf("or=%d b=%d invalidated=%d", orHits.Load(), bHits.Load(), h.creds.invalidated["or"])
	}
	if o := h.policy.leases[0].outcome; o.Status != 403 || !o.RequestScoped {
		t.Fatalf("outcome %+v", o)
	}
	if rows := h.waitRows(1); rows[0].Status != 403 {
		t.Fatalf("row %+v", rows[0])
	}
}

// An OpenRouter 429 without exhaustion evidence is a per-model/upstream
// limit: no cooldown, but another account may still serve.
func TestSecOpenRouter429RequestScoped(t *testing.T) {
	limited := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":429,"message":"rate limited upstream"}}`)
	}
	t.Run("scoped", func(t *testing.T) {
		h := newHarness(t)
		h.upstream("or", core.ProviderOpenRouter, limited)
		h.upstream("b", core.ProviderOpenRouter, okJSON)
		orRoute(h, "or", "b")
		h.quota.setSnapshot("or", fundedSnapshot(25, testNow))
		h.start()
		resp := h.post("/v1/chat/completions", clientKey, orChatBody, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if o := h.policy.leases[0].outcome; o.Status != 429 || !o.RequestScoped {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("real policy keeps account open", func(t *testing.T) {
		h := newHarness(t)
		h.upstream("or", core.ProviderOpenRouter, limited)
		orRoute(h, "or")
		h.pol = policy.New([]core.Account{h.accounts["or"]}, h.quota, policy.Options{Clock: fixedClock{testNow}})
		h.start()
		resp := h.post("/v1/chat/completions", clientKey, orChatBody, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if st := h.pol.Status("or"); !st.CooldownUntil.IsZero() {
			t.Fatalf("cooled down: %+v", st)
		}
	})
	t.Run("exhausted", func(t *testing.T) {
		h := newHarness(t)
		h.upstream("or", core.ProviderOpenRouter, limited)
		h.upstream("b", core.ProviderOpenRouter, okJSON)
		orRoute(h, "or", "b")
		h.quota.setSnapshot("or", fundedSnapshot(0, testNow))
		h.start()
		resp := h.post("/v1/chat/completions", clientKey, orChatBody, nil)
		resp.Body.Close()
		if o := h.policy.leases[0].outcome; resp.StatusCode != 200 || o.Status != 429 || o.RequestScoped {
			t.Fatalf("status %d outcome %+v", resp.StatusCode, o)
		}
	})
	t.Run("other providers account-level", func(t *testing.T) {
		h := newHarness(t)
		h.upstream("a", core.ProviderOllama, limited)
		h.upstream("b", core.ProviderOllama, okJSON)
		singleRoute(h, "a", "b")
		h.start()
		resp := h.post("/v1/chat/completions", clientKey, `{"model":"gpt-x","messages":[]}`, nil)
		resp.Body.Close()
		if o := h.policy.leases[0].outcome; resp.StatusCode != 200 || o.Status != 429 || o.RequestScoped {
			t.Fatalf("status %d outcome %+v", resp.StatusCode, o)
		}
	})
}

// A client cannot force more than one credential refresh per account per
// minute; later auth failures inside the window are account-level failures.
func TestSecInvalidateRateLimited(t *testing.T) {
	h := newHarness(t)
	clk := &stepClock{t: testNow}
	h.clock = clk
	var hits atomic.Int32
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	singleRoute(h, "a")
	h.start()
	post := func() {
		resp := h.post("/v1/responses", clientKey, respBody, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	post()
	if h.creds.invalidated["a"] != 1 || hits.Load() != 2 {
		t.Fatalf("first: invalidated %d hits %d", h.creds.invalidated["a"], hits.Load())
	}
	clk.Add(10 * time.Second)
	post()
	if h.creds.invalidated["a"] != 1 || hits.Load() != 3 {
		t.Fatalf("second: invalidated %d hits %d", h.creds.invalidated["a"], hits.Load())
	}
	if o := h.policy.leases[1].outcome; o.Status != 401 || o.RequestScoped {
		t.Fatalf("second outcome %+v", o)
	}
	clk.Add(61 * time.Second)
	post()
	if h.creds.invalidated["a"] != 2 {
		t.Fatalf("third: invalidated %d", h.creds.invalidated["a"])
	}
}

// --- PROXY-3 / COL-1: upstream-reported numbers are range-checked ---

func TestSecUsageTokenRange(t *testing.T) {
	bad := []string{
		`{"usage":{"prompt_tokens":-1000000000,"completion_tokens":2}}`,
		`{"usage":{"prompt_tokens":5,"completion_tokens":1000000000001}}`,
		`{"usage":{"input_tokens":5,"output_tokens":2,"input_tokens_details":{"cached_tokens":-3}}}`,
		`{"usage":{"input_tokens":5,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":9223372036854775807}}}`,
	}
	for _, b := range bad {
		c := newUsageCapture("application/json")
		_, _ = c.Write([]byte(b))
		if u, ok := c.Result(); ok {
			t.Errorf("%s: usage known %+v", b, u)
		}
	}
	c := newUsageCapture("application/json")
	_, _ = c.Write([]byte(`{"usage":{"prompt_tokens":1000000000000,"completion_tokens":0}}`))
	if u, ok := c.Result(); !ok || u.InputTokens != core.MaxRecordTokens {
		t.Errorf("max value rejected: %+v %v", u, ok)
	}

	// A later out-of-range record makes the whole response's usage unknown
	// rather than leaving an earlier record in place.
	c = newUsageCapture("text/event-stream")
	feed(c, "data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\n"+
		"data: {\"usage\":{\"prompt_tokens\":-5,\"completion_tokens\":1}}\n\n", 7)
	if u, ok := c.Result(); ok {
		t.Errorf("sse: usage known %+v", u)
	}
}

func TestSecReportedCostUpperBound(t *testing.T) {
	for raw, want := range map[string]*float64{
		`1.5e308`:   nil,
		`1000000.5`: nil,
		`1e6`:       ptr(1e6),
		`0.25`:      ptr(0.25),
	} {
		u := &usageJSON{Cost: json.RawMessage(raw)}
		got := u.reportedCostUSD()
		if (got == nil) != (want == nil) || got != nil && *got != *want {
			t.Errorf("%s: got %v", raw, got)
		}
	}
}

func ptr(f float64) *float64 { return &f }

func TestSecLabelsSanitized(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, map[string]string{
		"X-LocalRouter-Session": "a\tb\xffc",
		"X-LocalRouter-Task":    strings.Repeat("€", 50),
	})
	resp.Body.Close()
	rows := h.waitRows(1)
	if rows[0].Session != "a�b�c" {
		t.Errorf("session %q", rows[0].Session)
	}
	if !utf8.ValidString(rows[0].Task) || len(rows[0].Task) > core.MaxLabelBytes || len(rows[0].Task) != 126 {
		t.Errorf("task %q (%d bytes)", rows[0].Task, len(rows[0].Task))
	}
}

func TestSecRecordErrorBounded(t *testing.T) {
	l := newFakeLedger()
	p := New(Deps{Ledger: l, Clock: fixedClock{testNow}}, Options{})
	p.record(context.Background(), core.RequestRecord{ID: "x", Error: "upstream stream error: " + strings.Repeat("é\x00", 300)})
	got := l.all()[0].Error
	if len(got) > core.MaxErrorBytes || !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
		t.Fatalf("error %q (%d bytes)", got, len(got))
	}
}
