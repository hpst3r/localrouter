package proxy

// Pooled-backend acceptance tests through the proxy with the real policy
// (docs/SPEC.md "Pooled backends (routes[].balance)").

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

// pooledRoute installs one pooled route "pool" (model gpt-x) over ids with
// the given caps (nil = uncapped) and unit weights.
func pooledRoute(h *harness, caps map[string]int, ids ...string) {
	w := map[string]int{}
	for _, id := range ids {
		w[id] = 1
	}
	h.routes = []core.Route{{Name: "pool", Models: []string{"gpt-x"}, Interactive: ids, Background: ids,
		BalanceStrategy: "least_inflight", BalanceIdentity: "x", BalanceWeight: w, BalanceMaxInflight: caps}}
}

func poolHarness(t *testing.T) (*harness, *stepClock) {
	h := newHarness(t)
	h.logHandler = jsonLogger(h.logs)
	clk := &stepClock{t: testNow}
	h.clock = clk
	return h, clk
}

// waitInflight polls the real policy until id has n open leases.
func waitInflight(t *testing.T, p *policy.Policy, id string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.Status(id).Inflight != n {
		if time.Now().After(deadline) {
			t.Fatalf("%s inflight = %d, want %d", id, p.Status(id).Inflight, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestP5PoolSaturatedIs429(t *testing.T) {
	h, clk := poolHarness(t)
	gate := make(chan struct{})
	var connA, connB atomic.Int32
	h.gates = append(h.gates, gate)
	for id, c := range map[string]*atomic.Int32{"a": &connA, "b": &connB} {
		h.upstream(id, core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
			c.Add(1)
			<-gate
			okJSON(w, r)
		})
	}
	pooledRoute(h, map[string]int{"a": 1, "b": 1}, "a", "b")
	pol := breakerPolicyFor(h, clk, policy.BreakerOptions{})
	h.start()
	r1 := h.async("/v1/responses", clientKey, respBody)
	waitInflight(t, pol, "a", 1)
	r2 := h.async("/v1/responses", clientKey, respBody)
	waitInflight(t, pol, "b", 1)
	for deadline := time.Now().Add(5 * time.Second); connA.Load()+connB.Load() < 2; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("blocked requests never reached the backends")
		}
	}

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	e := decodeErr(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" ||
		e.Error.Type != "pool_saturated" || e.Error.Message != "localrouter: pool saturated (every member at max_inflight): a, b" {
		t.Fatalf("third: %d Retry-After=%q %+v", resp.StatusCode, resp.Header.Get("Retry-After"), e)
	}
	h.waitHandled(1)
	if connA.Load() != 1 || connB.Load() != 1 {
		t.Fatalf("backend connections a=%d b=%d", connA.Load(), connB.Load())
	}
	if len(h.ledger.all()) != 0 || len(routingEvents(t, h.logs.String())) != 0 {
		t.Fatal("saturation wrote a ledger row or an attempt event")
	}
	if !strings.Contains(h.logs.String(), `"denial":"pool_saturated"`) {
		t.Fatalf("policy denied line lacks denial=pool_saturated: %s", h.logs.String())
	}
	unblock(gate)
	drainClose(t, r1)
	drainClose(t, r2)
	if connA.Load() != 1 || connB.Load() != 1 {
		t.Fatalf("backend connections after release a=%d b=%d", connA.Load(), connB.Load())
	}
}

func TestP6PoolCapRaceThroughProxy(t *testing.T) {
	h, clk := poolHarness(t)
	h.logHandler = nil
	var mu sync.Mutex
	cur, peak := map[string]int{}, map[string]int{}
	for _, id := range []string{"a", "b"} {
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
	breakerPolicyFor(h, clk, policy.BreakerOptions{})
	h.start()
	var wg sync.WaitGroup
	var ok, saturated atomic.Int32
	for range 200 {
		wg.Go(func() {
			resp, err := h.do("/v1/responses", clientKey, respBody)
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
	if peak["a"] > 2 || peak["b"] > 2 || ok.Load()+saturated.Load() != 200 || ok.Load() == 0 {
		t.Fatalf("peak %v, ok %d, saturated %d", peak, ok.Load(), saturated.Load())
	}
}

// TestP8PoolAllOpenFailsClosed: both members unreachable and open answer the
// breaker's 503; the cloud account on another route is never contacted.
func TestP8PoolAllOpenFailsClosed(t *testing.T) {
	h, clk, dials := breakerHarness(t)
	deadAccount(h, "d1")
	h.accounts["d2"] = core.Account{ID: "d2", Provider: core.ProviderOllama, BaseURL: "http://" + deadHost + ":81/v1"}
	var cloudHits atomic.Int32
	h.upstream("cloud", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		cloudHits.Add(1)
		okJSON(w, r)
	})
	pooledRoute(h, nil, "d1", "d2")
	h.routes = append(h.routes, core.Route{Name: "cloud", Models: []string{"m-cloud"}, Interactive: []string{"cloud"}, Background: []string{"cloud"}})
	breakerPolicyFor(h, clk, policy.BreakerOptions{Threshold: 1, Base: 5 * time.Second, Max: time.Minute})
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || dials.Load() != 2 {
		t.Fatalf("first: %d dials %d (both members tried, both opened)", resp.StatusCode, dials.Load())
	}
	h.waitHandled(1)
	clk.Add(2 * time.Second)
	resp = h.post("/v1/responses", bgKey, respBody, nil)
	e := decodeErr(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "3" ||
		e.Error.Type != "upstream_unavailable" || e.Error.Message != "localrouter: upstream unreachable: d1, d2" {
		t.Fatalf("all open: %d Retry-After=%q %+v", resp.StatusCode, resp.Header.Get("Retry-After"), e)
	}
	if cloudHits.Load() != 0 || dials.Load() != 2 {
		t.Fatalf("cloud hits %d, dials %d", cloudHits.Load(), dials.Load())
	}
}

// TestP8SaturatedPlusBreakerIs429: saturated + breaker members answer
// pool_saturated, not 503.
func TestP8SaturatedPlusBreakerIs429(t *testing.T) {
	h, clk, _ := breakerHarness(t)
	deadAccount(h, "d1")
	gate := make(chan struct{})
	h.blockingUpstream("b", gate, nil)
	pooledRoute(h, map[string]int{"b": 1}, "d1", "b")
	pol := breakerPolicyFor(h, clk, policy.BreakerOptions{Threshold: 1, Base: 5 * time.Second})
	h.start()
	r1 := h.async("/v1/responses", clientKey, respBody) // d1 fails and opens, fails over to b (held)
	waitInflight(t, pol, "b", 1)
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	e := decodeErr(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests || e.Error.Type != "pool_saturated" || e.Error.Message != "localrouter: pool saturated (every member at max_inflight): b" {
		t.Fatalf("saturated+breaker: %d %+v", resp.StatusCode, e)
	}
	unblock(gate)
	drainClose(t, r1)
}

func TestP9InPoolFailover(t *testing.T) {
	h, clk := poolHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, statusHandler(http.StatusBadGateway))
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	pooledRoute(h, nil, "a", "b")
	breakerPolicyFor(h, clk, policy.BreakerOptions{})
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows := h.waitRows(2)
	h.waitHandled(1)
	if rows[0].AccountID != "a" || rows[1].AccountID != "b" || rows[1].FailoverOf != rows[0].ID ||
		rows[0].Route != "pool" || rows[1].Route != "pool" {
		t.Fatalf("rows %+v", rows)
	}
	evs := routingEvents(t, h.logs.String())
	if len(evs) != 2 || evs[0]["account"] != "a" || evs[0]["failover_eligible"] != true || evs[1]["account"] != "b" {
		t.Fatalf("events %v", evs)
	}
}

func TestP10PoolAttemptCap(t *testing.T) {
	h, clk := poolHarness(t)
	var hits atomic.Int32
	fail := func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"down"}}`)
	}
	for _, id := range []string{"a", "b", "c"} {
		h.upstream(id, core.ProviderOpenAICompat, fail)
	}
	pooledRoute(h, nil, "a", "b", "c")
	h.opts.MaxFailovers = 1
	breakerPolicyFor(h, clk, policy.BreakerOptions{})
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "down") || hits.Load() != 2 {
		t.Fatalf("status %d hits %d body %s", resp.StatusCode, hits.Load(), body)
	}
}

func TestP18PoolCapabilityFilter(t *testing.T) {
	h, clk := poolHarness(t)
	var hits atomic.Int32
	for _, id := range []string{"a", "b"} {
		h.upstream(id, core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			okJSON(w, r)
		})
	}
	pooledRoute(h, nil, "a", "b")
	spec := core.UpstreamSpec{Protocols: []string{"responses", "chat"}, InputModalities: []string{"text", "image"}}
	h.routes[0].Upstreams = map[string]core.UpstreamSpec{"a": spec, "b": spec}
	breakerPolicyFor(h, clk, policy.BreakerOptions{})
	h.start()
	resp := h.post("/v1/responses", clientKey, imageBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("image: %d hits %d", resp.StatusCode, hits.Load())
	}
	resp = h.post("/v1/responses", clientKey, `{"model":"gpt-x","input":"hi","tools":[{"type":"function","name":"f"}]}`, nil)
	if e := decodeErr(t, resp); resp.StatusCode != http.StatusBadRequest || e.Error.Type != "capability_unsupported" || hits.Load() != 1 {
		t.Fatalf("tools: %d %+v hits %d", resp.StatusCode, e, hits.Load())
	}
}

// TestP19PoolEventAttributes: pooled attempts carry pool_pick,
// member_inflight and member_cap; unpooled attempts carry none of them.
func TestP19PoolEventAttributes(t *testing.T) {
	h, clk := poolHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	h.upstream("c", core.ProviderOpenAICompat, okJSON)
	pooledRoute(h, map[string]int{"a": 3}, "a", "b")
	h.routes[0].BalanceStickyTTL = time.Minute
	h.routes = append(h.routes, core.Route{Name: "plain", Models: []string{"m-plain"}, Interactive: []string{"c"}})
	breakerPolicyFor(h, clk, policy.BreakerOptions{})
	h.start()
	for i, hdr := range []map[string]string{nil, {"X-LocalRouter-Session": "S"}, {"X-LocalRouter-Session": "S"}} {
		resp := h.post("/v1/responses", clientKey, respBody, hdr)
		resp.Body.Close()
		h.waitHandled(i + 1)
	}
	resp := h.post("/v1/responses", clientKey, modelBody("m-plain"), nil)
	resp.Body.Close()
	h.waitHandled(4)
	evs := routingEvents(t, h.logs.String())
	if len(evs) != 4 {
		t.Fatalf("events %d", len(evs))
	}
	for i, want := range []string{"least_inflight", "least_inflight", "sticky"} {
		ev := evs[i]
		if ev["account"] != "a" || ev["pool_pick"] != want || ev["member_inflight"] != float64(1) || ev["member_cap"] != float64(3) {
			t.Fatalf("pooled event %d: %v", i, ev)
		}
	}
	for _, k := range []string{"pool_pick", "member_inflight", "member_cap"} {
		if _, ok := evs[3][k]; ok {
			t.Fatalf("unpooled event carries %s: %v", k, evs[3])
		}
	}
}

// TestP1LegacyPolicyKeepsPooledRouteOrdered: a policy without
// AcquireBalanced (any core.Policy) serves a pooled route on the unchanged
// ordered path rather than failing.
func TestP1LegacyPolicyKeepsPooledRouteOrdered(t *testing.T) {
	h, _ := poolHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	pooledRoute(h, map[string]int{"a": 1}, "a", "b")
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(h.policy.classes()) != 1 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h.waitHandled(1)
	if evs := routingEvents(t, h.logs.String()); len(evs) != 1 || evs[0]["pool_pick"] != nil {
		t.Fatalf("events %v", evs)
	}
}
