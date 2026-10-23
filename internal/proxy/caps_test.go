package proxy

// Pooled backends Amendment R1.1 (PL-1) through the proxy: a member's
// max_inflight caps the account on every route, pooled or not.

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/policy"
)

// cappedPolicyFor installs a real policy whose per-account caps come from
// the harness routes, as the app wires it.
func cappedPolicyFor(h *harness, clock core.Clock) *policy.Policy {
	var accts []core.Account
	for _, a := range h.accounts {
		accts = append(accts, a)
	}
	p := policy.New(accts, h.quota, policy.Options{Clock: clock, MaxInflight: policy.AccountCaps(h.routes)})
	h.pol = p
	return p
}

func TestR1UnpooledRouteAtCapIs429(t *testing.T) {
	h, clk := poolHarness(t)
	gate := make(chan struct{})
	var conns atomic.Int32
	h.gates = append(h.gates, gate)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		conns.Add(1)
		<-gate
		okJSON(w, r)
	})
	pooledRoute(h, map[string]int{"a": 1}, "a")
	h.routes = append(h.routes, core.Route{Name: "plain", Models: []string{"m-plain"}, Interactive: []string{"a"}, Background: []string{"a"}})
	pol := cappedPolicyFor(h, clk)
	h.start()
	r1 := h.async("/v1/responses", clientKey, modelBody("m-plain"))
	waitInflight(t, pol, "a", 1)

	for _, model := range []string{"m-plain", "gpt-x"} {
		resp := h.post("/v1/responses", clientKey, modelBody(model), nil)
		e := decodeErr(t, resp)
		if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" ||
			e.Error.Type != "pool_saturated" || !strings.HasSuffix(e.Error.Message, ": a") {
			t.Fatalf("%s at cap: %d Retry-After=%q %+v", model, resp.StatusCode, resp.Header.Get("Retry-After"), e)
		}
	}
	h.waitHandled(2)
	if conns.Load() != 1 || len(h.ledger.all()) != 0 || len(routingEvents(t, h.logs.String())) != 0 {
		t.Fatalf("saturation reached the backend (%d) or wrote a row/event", conns.Load())
	}
	if strings.Count(h.logs.String(), `"denial":"pool_saturated"`) != 2 {
		t.Fatalf("policy denied lines: %s", h.logs.String())
	}
	unblock(gate)
	drainClose(t, r1)
}

// TestR1SharedCapRaceThroughProxy: 200 concurrent requests split between a
// pooled route and an unpooled route sharing account a never put more than
// a's cap on its backend.
func TestR1SharedCapRaceThroughProxy(t *testing.T) {
	h, clk := poolHarness(t)
	h.logHandler = nil
	var mu sync.Mutex
	cur, peak := map[string]int{}, map[string]int{}
	for _, id := range []string{"a", "b", "c"} {
		h.upstream(id, core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			cur[id]++
			peak[id] = max(peak[id], cur[id])
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			cur[id]--
			mu.Unlock()
			okJSON(w, r)
		})
	}
	pooledRoute(h, map[string]int{"a": 2, "b": 2}, "a", "b")
	h.routes = append(h.routes, core.Route{Name: "plain", Models: []string{"m-plain"}, Interactive: []string{"a", "c"}, Background: []string{"a", "c"}})
	cappedPolicyFor(h, clk)
	h.start()
	// c is uncapped, so more rows than the fake ledger buffers are recorded.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-h.ledger.ch:
			case <-done:
				return
			}
		}
	}()
	var wg sync.WaitGroup
	var ok, saturated atomic.Int32
	for i := range 200 {
		model := "gpt-x"
		if i%2 == 1 {
			model = "m-plain"
		}
		wg.Go(func() {
			resp, err := h.do("/v1/responses", clientKey, modelBody(model))
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusTooManyRequests:
				saturated.Add(1)
			}
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if peak["a"] > 2 || peak["b"] > 2 || peak["a"] == 0 || ok.Load()+saturated.Load() != 200 {
		t.Fatalf("peak %v, ok %d, saturated %d", peak, ok.Load(), saturated.Load())
	}
}
