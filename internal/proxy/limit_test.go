package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// requestTimeout bounds every request a limit test makes, so a handler that is
// never released fails the test instead of hanging the binary until the
// package-level -timeout fires.
const requestTimeout = 2 * time.Second

// boundedWait bounds every wait for a limiter state transition. It is kept
// short (and well under the package -timeout) so that even a wholly regressed
// suite fails with per-test messages instead of tripping the 30s alarm: each
// failing limit test costs at most one boundedWait.
const boundedWait = 2 * time.Second

// fakeLimiter is a stand-in for internal/connlim.Controller. The proxy depends
// on the behaviour (admission, release, capacity reporting), not the package,
// so the proxy's own tests stay free of a second library dependency.
type fakeLimiter struct {
	mu        sync.Mutex
	global    int
	perClient map[string]int
	active    int
	byClient  map[string]int
	peak      int
	released  int
}

func newFakeLimiter(global int, perClient map[string]int) *fakeLimiter {
	return &fakeLimiter{global: global, perClient: perClient, byClient: map[string]int{}}
}

func (l *fakeLimiter) Acquire(client string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.global > 0 && l.active >= l.global {
		return nil, false
	}
	if lim, limited := l.perClient[client]; limited && l.byClient[client] >= lim {
		return nil, false
	}
	l.active++
	l.byClient[client]++
	if l.active > l.peak {
		l.peak = l.active
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.active--
			l.released++
			if n := l.byClient[client] - 1; n <= 0 {
				delete(l.byClient, client)
			} else {
				l.byClient[client] = n
			}
		})
	}, true
}

func (l *fakeLimiter) Stats() core.InflightStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return core.InflightStats{GlobalLimit: l.global, GlobalActive: l.active, GlobalPeak: l.peak}
}

func (l *fakeLimiter) snapshot() (active, peak, released int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.peak, l.released
}

// blockingUpstream registers account id with a handler that blocks until gate
// is closed, then answers with a minimal JSON body. The gate is remembered so
// harness teardown can always release it; closing the same channel twice is
// avoided by every closer (here and in teardown) going through unblock.
func (h *harness) blockingUpstream(id string, gate chan struct{}, hits *atomic.Int32) {
	h.gates = append(h.gates, gate)
	h.upstream(id, core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		<-gate
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r1","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
}

// unblock closes gate exactly once; it is safe to call after teardown may
// already have closed it, and safe to call twice.
func unblock(gate chan struct{}) {
	select {
	case <-gate:
	default:
		close(gate)
	}
}

// do performs one proxy request, bounded by a real deadline so no call can
// block forever (which would hang the test binary rather than fail). It never
// touches *testing.T, so it is safe to call from a helper goroutine (unlike
// h.post, whose t.Fatal would unwind a non-test goroutine).
func (h *harness) do(path, key, body string) (*http.Response, error) {
	return h.doCtx(context.Background(), path, key, body)
}

func (h *harness) doCtx(ctx context.Context, path, key, body string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+path, strings.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose releases the per-request context when the body is closed, so
// expect's deadline does not leak goroutines or hold connections.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// async starts a proxy request in its own goroutine and returns a channel that
// will carry the response (nil on transport error). The caller must drain it.
func (h *harness) async(path, key, body string) <-chan *http.Response {
	ch := make(chan *http.Response, 1)
	go func() {
		resp, err := h.do(path, key, body)
		if err != nil {
			ch <- nil
			return
		}
		ch <- resp
	}()
	return ch
}

func drainClose(t *testing.T, ch <-chan *http.Response) {
	t.Helper()
	resp := <-ch
	if resp == nil {
		t.Fatal("request failed")
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// A saturated global limit rejects with 429, an OpenAI-style error naming the
// concurrency limit (not quota), a Retry-After hint, and no upstream contact,
// no admission lease and no slot accounting change. The slot is held for the
// whole life of the earlier request and released exactly once at the end.
func TestConcurrencyGlobalLimitRejects(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	var hits atomic.Int32
	h.blockingUpstream("a", gate, &hits)
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a", "b")
	h.setLimiter(1, nil)
	h.start()

	held := h.async("/v1/responses", clientKey, respBody)
	waitActive(t, h.lim, 1)

	resp, err := h.do("/v1/responses", bgKey, respBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		resp.Body.Close()
		t.Fatal("missing Retry-After")
	}
	e := decodeErr(t, resp)
	if !strings.Contains(e.Error.Type, "concurrency") || !strings.Contains(e.Error.Message, "concurrency") {
		t.Fatalf("error %+v must identify the concurrency limit", e)
	}
	if strings.Contains(strings.ToLower(e.Error.Type+e.Error.Message), "quota") {
		t.Fatalf("error %+v must not blame quota", e)
	}
	if hits.Load() != 0 {
		t.Fatal("rejected request contacted upstream")
	}
	// Exactly one policy admission happened: the held request. The rejected
	// request must never have reached policy (and thus never taken a lease).
	if got := len(h.policy.classes()); got != 1 {
		t.Fatalf("rejected request reached policy: %d admissions, want 1", got)
	}
	if active, _, released := h.lim.snapshot(); active != 1 || released != 0 {
		t.Fatalf("rejected request changed slot accounting: active=%d released=%d", active, released)
	}
	if len(h.ledger.all()) != 0 {
		t.Fatal("rejected request wrote a ledger row")
	}

	unblock(gate)
	drainClose(t, held)
	waitReleased(t, h.lim, 1)
	if active, _, _ := h.lim.snapshot(); active != 0 {
		t.Fatalf("slot leaked after completion: active=%d", active)
	}
}

// A per-client limit saturates that client only: another client is admitted
// alongside it, and a released slot is reusable by the limited client.
func TestConcurrencyPerClientIsolation(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	var hits atomic.Int32
	h.blockingUpstream("a", gate, &hits)
	singleRoute(h, "a")
	h.setLimiter(0, map[string]int{"alice": 1})
	h.start()

	heldAlice := h.async("/v1/responses", clientKey, respBody)
	waitActive(t, h.lim, 1)

	// Same client over its own limit: rejected before any upstream work.
	if resp, err := h.do("/v1/responses", clientKey, respBody); err != nil {
		t.Fatal(err)
	} else if resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Fatalf("same client status %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// Another client has its own budget: admitted, so active climbs to 2.
	heldBatch := h.async("/v1/responses", bgKey, respBody)
	waitActive(t, h.lim, 2)
	if hits.Load() != 0 {
		t.Fatal("rejected request contacted upstream")
	}

	unblock(gate)
	drainClose(t, heldAlice)
	drainClose(t, heldBatch)
	waitReleased(t, h.lim, 2)
	if active, _, released := h.lim.snapshot(); active != 0 || released != 2 {
		t.Fatalf("active=%d released=%d", active, released)
	}

	// The limited client is admitted again once its slot frees.
	resp, err := h.do("/v1/responses", clientKey, respBody)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after release status %d", resp.StatusCode)
	}
}

// Unauthenticated requests and /v1/models never occupy an inference slot.
func TestModelsAndUnauthDoNotConsumeSlots(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	var hits atomic.Int32
	h.blockingUpstream("a", gate, &hits)
	singleRoute(h, "a")
	h.setLimiter(1, nil)
	h.start()

	held := h.async("/v1/responses", clientKey, respBody)
	waitActive(t, h.lim, 1)

	if resp, err := h.do("/v1/responses", "nope", respBody); err != nil {
		t.Fatal(err)
	} else if resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("unauth status %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+clientKey)
	modelsResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	modelsResp.Body.Close()
	if modelsResp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models must not need a slot: %d", modelsResp.StatusCode)
	}
	if active, _, released := h.lim.snapshot(); active != 1 || released != 0 {
		t.Fatalf("non-inference request touched slots: active=%d released=%d", active, released)
	}
	if len(h.policy.classes()) != 1 {
		t.Fatalf("non-inference request reached policy: %d admissions", len(h.policy.classes()))
	}

	unblock(gate)
	drainClose(t, held)
	waitReleased(t, h.lim, 1)
}

// A slot is held for the whole life of a streaming response, across failover,
// and is counted exactly once even when several upstreams are tried.
func TestSlotHeldAcrossStreamAndFailover(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.gates = append(h.gates, release)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // triggers failover
	})
	h.upstream("b", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-release
	})
	singleRoute(h, "a", "b")
	h.setLimiter(1, nil)
	h.start()

	streamResp := h.async("/v1/responses", clientKey, `{"model":"gpt-x","stream":true}`)

	// While the stream is open (after failover a->b) the single slot is busy,
	// so a second inference request is refused.
	waitActivePeak(t, h.lim, 1, 1)
	if resp, err := h.do("/v1/responses", bgKey, respBody); err != nil {
		t.Fatal(err)
	} else if resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Fatalf("streaming must hold the slot: status %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if peak, _, _ := h.lim.snapshot(); peak != 1 {
		t.Fatalf("failover double-counted the slot: peak=%d", peak)
	}

	unblock(release)
	drainClose(t, streamResp)
	waitReleased(t, h.lim, 1)
}

// A malformed body is rejected, but the slot taken for it must be released so
// it cannot leak — validated with the race detector across a real request.
func TestSlotReleasedOnMalformedBody(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	h.setLimiter(1, nil)
	h.start()

	for _, body := range []string{`{`, `{"no_model":true}`, `not json`} {
		resp, err := h.do("/v1/responses", clientKey, body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("body %q status %d, want 400", body, resp.StatusCode)
		}
		resp.Body.Close()
	}
	waitReleased(t, h.lim, len([]string{`{`, `{"no_model":true}`, `not json`}))
	if active, _, _ := h.lim.snapshot(); active != 0 {
		t.Fatalf("slot leaked on malformed body: active=%d", active)
	}
	// The single slot is usable again by a well-formed request.
	resp, err := h.do("/v1/responses", clientKey, respBody)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy request after 400s: status %d", resp.StatusCode)
	}
}

// A policy denial (quota reserve, no upstream) still releases the slot it took.
func TestSlotReleasedOnQuotaDenial(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("a", core.ProviderOpenAICompat, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	singleRoute(h, "a")
	h.policy.deny["a"] = true
	h.setLimiter(1, nil)
	h.start()

	resp, err := h.do("/v1/responses", clientKey, respBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp.Body.Close()
	if hits.Load() != 0 {
		t.Fatal("denied request contacted upstream")
	}
	waitReleased(t, h.lim, 1)
	if active, _, _ := h.lim.snapshot(); active != 0 {
		t.Fatalf("slot leaked on quota denial: active=%d", active)
	}
}

// A client cancelling an in-flight request must release the slot promptly, with
// a finite bound so the test cannot hang.
func TestSlotReleasedOnClientCancel(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	var hits atomic.Int32
	h.blockingUpstream("a", gate, &hits)
	singleRoute(h, "a")
	h.setLimiter(1, nil)
	h.start()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/responses", strings.NewReader(respBody))
	if err != nil {
		t.Fatal(err)
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
	waitActive(t, h.lim, 1)
	cancel()

	select {
	case resp := <-done:
		if resp != nil {
			resp.Body.Close()
		}
	case <-time.After(boundedWait):
		t.Fatal("cancelled request never returned")
	}
	waitReleased(t, h.lim, 1)
	if active, _, _ := h.lim.snapshot(); active != 0 {
		t.Fatalf("slot leaked after cancel: active=%d", active)
	}
}

// Failover must keep a single slot: a retryable first account (503) then a
// second account's success releases the slot exactly once.
func TestSlotReleasedAcrossFailover(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a", "b")
	h.setLimiter(1, nil)
	h.start()

	resp, err := h.do("/v1/responses", clientKey, respBody)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"r1"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if classes := h.policy.classes(); len(classes) != 2 {
		t.Fatalf("expected one admission per attempt, got %d", len(classes))
	}
	waitReleased(t, h.lim, 1)
	if active, peak, released := h.lim.snapshot(); active != 0 || peak != 1 || released != 1 {
		t.Fatalf("active=%d peak=%d released=%d; failover must use one slot", active, peak, released)
	}
}

func waitActive(t *testing.T, l *fakeLimiter, want int) {
	t.Helper()
	waitFor(t, func() bool { a, _, _ := l.snapshot(); return a == want },
		func() string { a, _, _ := l.snapshot(); return fmt.Sprintf("active=%d want %d", a, want) })
}

func waitActivePeak(t *testing.T, l *fakeLimiter, wantActive, wantPeak int) {
	t.Helper()
	waitFor(t, func() bool { a, p, _ := l.snapshot(); return a == wantActive && p == wantPeak },
		func() string {
			a, p, _ := l.snapshot()
			return fmt.Sprintf("active=%d peak=%d want %d/%d", a, p, wantActive, wantPeak)
		})
}

func waitReleased(t *testing.T, l *fakeLimiter, want int) {
	t.Helper()
	waitFor(t, func() bool { _, _, r := l.snapshot(); return r == want },
		func() string { _, _, r := l.snapshot(); return fmt.Sprintf("released=%d want %d", r, want) })
}

// waitFor polls cond every 5ms for up to 5s; it fails with msg() on timeout.
// The deadline is finite so a stuck release can never hang the run.
func waitFor(t *testing.T, cond func() bool, msg func() string) {
	t.Helper()
	deadline := time.After(boundedWait)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: %s", msg())
		case <-time.After(5 * time.Millisecond):
		}
	}
}
