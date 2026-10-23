package proxy

// Acceptance tests B2, B8..B11 and B14 for the reachability breaker
// (docs/SPEC.md "Reachability breaker (policy.breaker)"). Dead upstreams are
// simulated with a custom dialer that fails DNS for deadHost; every other
// upstream is a loopback httptest server.

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/policy"
)

const deadHost = "llama.lab.invalid"

// deadClient returns an upstream client whose dialer fails DNS resolution for
// deadHost (like the real resolver: an OpError wrapping a DNSError) and
// counts those dial attempts in dials; other hosts dial normally.
func deadClient(dials *atomic.Int64) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	var d net.Dialer
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, _ := net.SplitHostPort(addr); host == deadHost {
			dials.Add(1)
			return nil, &net.OpError{Op: "dial", Net: network,
				Err: &net.DNSError{Err: "no such host", Name: deadHost, IsNotFound: true}}
		}
		return d.DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr}
}

// deadAccount registers an account whose host does not resolve.
func deadAccount(h *harness, id string) {
	h.accounts[id] = core.Account{ID: id, Provider: core.ProviderOllama, BaseURL: "http://" + deadHost + "/v1"}
}

// breakerPolicyFor installs a real policy over the harness accounts.
func breakerPolicyFor(h *harness, clock core.Clock, br policy.BreakerOptions) *policy.Policy {
	var accts []core.Account
	for _, a := range h.accounts {
		accts = append(accts, a)
	}
	p := policy.New(accts, h.quota, policy.Options{Clock: clock, Breaker: br})
	h.pol = p
	return p
}

// legacyPolicy hides AcquireWithBreaker, so the proxy takes the pre-breaker
// Acquire path.
type legacyPolicy struct{ core.Policy }

func modelBody(model string) string { return `{"model":"` + model + `","input":"hi","stream":false}` }

type b2Result struct {
	status     int
	retryAfter string
	body       string
}

type b2Row struct {
	Account, Model, Error, FailoverOf string
	Status                            int
	UsageKnown                        bool
	Usage                             core.Usage
}

// runB2 replays a fixed scenario (success, 429 failover, DNS-failure
// failover, repeated dead single-candidate, 500, cooled-down denial) through
// a real policy whose breaker is disabled, either directly or behind
// legacyPolicy, and returns everything a client or operator observes.
func runB2(t *testing.T, legacy bool) ([]b2Result, []b2Row, []map[string]any) {
	h := newHarness(t)
	h.logHandler = jsonLogger(h.logs)
	var dials atomic.Int64
	h.opts.HTTPClient = deadClient(&dials)
	h.upstream("ok", core.ProviderOpenAICompat, okJSON)
	h.upstream("busy", core.ProviderOpenAICompat, statusHandler(http.StatusTooManyRequests))
	h.upstream("boom", core.ProviderOpenAICompat, statusHandler(http.StatusInternalServerError))
	deadAccount(h, "dead")
	h.routes = []core.Route{
		{Name: "r-ok", Models: []string{"m-ok"}, Interactive: []string{"ok"}},
		{Name: "r-busy", Models: []string{"m-busy"}, Interactive: []string{"busy", "ok"}},
		{Name: "r-dead", Models: []string{"m-dead"}, Interactive: []string{"dead", "ok"}},
		{Name: "r-deadonly", Models: []string{"m-deadonly"}, Interactive: []string{"dead"}},
		{Name: "r-boom", Models: []string{"m-boom"}, Interactive: []string{"boom"}},
		{Name: "r-busyonly", Models: []string{"m-busyonly"}, Interactive: []string{"busy"}},
	}
	p := breakerPolicyFor(h, fixedClock{testNow}, policy.BreakerOptions{})
	if legacy {
		h.pol = legacyPolicy{p}
	}
	h.start()
	var results []b2Result
	models := []string{"m-ok", "m-busy", "m-dead", "m-boom", "m-busyonly"}
	for range 6 {
		models = append(models, "m-deadonly")
	}
	for i, m := range models {
		resp := h.post("/v1/responses", clientKey, modelBody(m), nil)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		h.waitHandled(i + 1)
		results = append(results, b2Result{resp.StatusCode, resp.Header.Get("Retry-After"), string(b)})
	}
	ids := map[string]string{"": ""}
	var rows []b2Row
	for i, r := range h.ledger.all() {
		ids[r.ID] = string(rune('A' + i))
		rows = append(rows, b2Row{r.AccountID, r.Model, r.Error, ids[r.FailoverOf], r.Status, r.UsageKnown, r.Usage})
	}
	evs := routingEvents(t, h.logs.String())
	for _, ev := range evs {
		for _, k := range []string{"time", "request_id", "attempt_id", "failover_of", "latency_ms"} {
			delete(ev, k)
		}
	}
	return results, rows, evs
}

func TestB2DisabledBreakerIsUnchanged(t *testing.T) {
	gotRes, gotRows, gotEvs := runB2(t, false)
	wantRes, wantRows, wantEvs := runB2(t, true)
	if !reflect.DeepEqual(gotRes, wantRes) {
		t.Fatalf("responses differ:\n got %+v\nwant %+v", gotRes, wantRes)
	}
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("ledger rows differ:\n got %+v\nwant %+v", gotRows, wantRows)
	}
	if !reflect.DeepEqual(gotEvs, wantEvs) {
		t.Fatalf("events differ:\n got %+v\nwant %+v", gotEvs, wantEvs)
	}
	// Sanity: the scenario really exercised the dead upstream six times as a
	// single candidate (no breaker, every request pays the dial) and denied
	// the cooled-down account with the legacy 429.
	dead502 := 0
	for _, r := range gotRes {
		if r.status == http.StatusBadGateway {
			dead502++
		}
	}
	if dead502 != 6 || gotRes[4].status != http.StatusTooManyRequests || !strings.Contains(gotRes[4].body, "quota_reserve") {
		t.Fatalf("unexpected scenario results: %+v", gotRes)
	}
}

// breakerHarness wires a real policy with an enabled breaker on a settable
// clock.
func breakerHarness(t *testing.T) (*harness, *stepClock, *atomic.Int64) {
	h := newHarness(t)
	h.logHandler = jsonLogger(h.logs)
	dials := &atomic.Int64{}
	h.opts.HTTPClient = deadClient(dials)
	clk := &stepClock{t: testNow}
	h.clock = clk
	return h, clk, dials
}

// TestB8LocalOnlyRouteFailsClosed: a route whose only candidate is open
// never spills to another account, even one other routes use.
func TestB8LocalOnlyRouteFailsClosed(t *testing.T) {
	h, clk, dials := breakerHarness(t)
	deadAccount(h, "local")
	var cloudHits atomic.Int32
	h.upstream("cloud", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		cloudHits.Add(1)
		okJSON(w, r)
	})
	h.routes = []core.Route{
		{Name: "local", Models: []string{"m-local"}, Interactive: []string{"local"}, Background: []string{"local"}},
		{Name: "cloud", Models: []string{"m-cloud"}, Interactive: []string{"cloud"}, Background: []string{"cloud"}},
	}
	breakerPolicyFor(h, clk, policy.BreakerOptions{Threshold: 1, Base: 5 * time.Second, Max: time.Minute})
	h.start()
	for i, key := range []string{clientKey, bgKey, clientKey, bgKey} {
		resp := h.post("/v1/responses", key, modelBody("m-local"), nil)
		resp.Body.Close()
		want := http.StatusServiceUnavailable
		if i == 0 {
			want = http.StatusBadGateway // the failure that opens the breaker
		}
		if resp.StatusCode != want {
			t.Fatalf("request %d: status %d, want %d", i, resp.StatusCode, want)
		}
	}
	if cloudHits.Load() != 0 || dials.Load() != 1 {
		t.Fatalf("cloud hits %d, dead dials %d; want 0 and 1", cloudHits.Load(), dials.Load())
	}
}

func TestB9AllBreakerRefusalIs503(t *testing.T) {
	h, clk, dials := breakerHarness(t)
	deadAccount(h, "d1")
	h.accounts["d2"] = core.Account{ID: "d2", Provider: core.ProviderOllama, BaseURL: "http://" + deadHost + ":81/v1"}
	h.routes = []core.Route{
		{Name: "r1", Models: []string{"m1"}, Interactive: []string{"d1"}},
		{Name: "r2", Models: []string{"m2"}, Interactive: []string{"d2"}},
		{Name: "both", Models: []string{"m-both"}, Interactive: []string{"d1", "d2"}},
	}
	breakerPolicyFor(h, clk, policy.BreakerOptions{Threshold: 1, Base: 5 * time.Second, Max: time.Minute})
	h.start()
	for i, m := range []string{"m1", "m2"} {
		resp := h.post("/v1/responses", clientKey, modelBody(m), nil)
		resp.Body.Close()
		h.waitHandled(i + 1)
		clk.Add(1500 * time.Millisecond)
	}
	// d1 opened at t0 (until t0+5s), d2 at t0+1.5s (until t0+6.5s); now t0+3s.
	rows, evs := len(h.ledger.all()), len(routingEvents(t, h.logs.String()))
	resp := h.post("/v1/responses", clientKey, modelBody("m-both"), nil)
	e := decodeErr(t, resp)
	h.waitHandled(3)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "2" ||
		e.Error.Type != "upstream_unavailable" || e.Error.Message != "localrouter: upstream unreachable: d1, d2" {
		t.Fatalf("refusal: %d Retry-After=%q %+v", resp.StatusCode, resp.Header.Get("Retry-After"), e)
	}
	// A policy denial: no upstream contact, no ledger row, no attempt event,
	// and the policy denied diagnostic carries the breaker denial name.
	if dials.Load() != 2 || len(h.ledger.all()) != rows || len(routingEvents(t, h.logs.String())) != evs {
		t.Fatalf("refusal touched upstream or wrote rows/events")
	}
	if !strings.Contains(h.logs.String(), `"msg":"policy denied"`) || !strings.Contains(h.logs.String(), `"denial":"breaker_open"`) {
		t.Fatalf("missing breaker denial log:\n%s", h.logs.String())
	}
}

func TestB9RetryAfterRoundsUpAndFloorsAtOne(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{{4*time.Second + time.Millisecond, "5"}, {4 * time.Second, "4"}, {0, "1"}, {-time.Second, "1"}, {time.Millisecond, "1"}} {
		rec := httptest.NewRecorder()
		writeUnreachable(rec, []string{"a"}, tc.in)
		if got := rec.Header().Get("Retry-After"); got != tc.want || rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%v: Retry-After %q code %d, want %q 503", tc.in, got, rec.Code, tc.want)
		}
	}
}

func TestB9MixedRefusalKeeps429(t *testing.T) {
	h, clk, _ := breakerHarness(t)
	deadAccount(h, "dead")
	h.upstream("busy", core.ProviderOpenAICompat, statusHandler(http.StatusTooManyRequests))
	h.routes = []core.Route{
		{Name: "r1", Models: []string{"m-dead"}, Interactive: []string{"dead"}},
		{Name: "r2", Models: []string{"m-busy"}, Interactive: []string{"busy"}},
		{Name: "mixed", Models: []string{"m-mixed"}, Interactive: []string{"busy", "dead"}},
	}
	h.opts.MaxFailovers = -1
	breakerPolicyFor(h, clk, policy.BreakerOptions{Threshold: 1})
	h.start()
	for i, m := range []string{"m-dead", "m-busy"} {
		resp := h.post("/v1/responses", clientKey, modelBody(m), nil)
		resp.Body.Close()
		h.waitHandled(i + 1)
	}
	resp := h.post("/v1/responses", clientKey, modelBody("m-mixed"), nil)
	e := decodeErr(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests || e.Error.Type != "quota_reserve" ||
		resp.Header.Get("Retry-After") != denyRetryAfter ||
		!strings.Contains(e.Error.Message, "busy: cooldown until") ||
		!strings.Contains(e.Error.Message, "dead: upstream unreachable (breaker open until") {
		t.Fatalf("mixed refusal: %d %+v", resp.StatusCode, e)
	}
}

// TestB10ProxyReportsUnreachable: the proxy sets Outcome.Unreachable only for
// a send that got no response headers and was not cancelled by the client.
func TestB10ProxyReportsUnreachable(t *testing.T) {
	h := newHarness(t)
	var dials atomic.Int64
	h.opts.HTTPClient = deadClient(&dials)
	h.opts.MaxFailovers = -1
	deadAccount(h, "dead")
	h.upstream("five", core.ProviderOpenAICompat, statusHandler(http.StatusInternalServerError))
	h.routes = []core.Route{
		{Name: "a", Models: []string{"m-dead"}, Interactive: []string{"dead"}},
		{Name: "b", Models: []string{"m-five"}, Interactive: []string{"five"}},
	}
	h.start()
	for i, tc := range []struct{ model, want string }{{"m-dead", "dns"}, {"m-five", ""}} {
		resp := h.post("/v1/responses", clientKey, modelBody(tc.model), nil)
		resp.Body.Close()
		h.waitHandled(i + 1)
		if o := h.policy.leases[i].outcome; o.Unreachable != tc.want {
			t.Fatalf("%s: Unreachable=%q, want %q (%+v)", tc.model, o.Unreachable, tc.want, o)
		}
	}
}

// A client that goes away while the upstream has not answered is not an
// unreachable upstream.
func TestB10ClientCancelDoesNotCount(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	h.gates = append(h.gates, gate)
	h.upstream("slow", core.ProviderOpenAICompat, func(http.ResponseWriter, *http.Request) { <-gate })
	singleRoute(h, "slow")
	h.start()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/responses", strings.NewReader(respBody))
	req.Header.Set("Authorization", "Bearer "+clientKey)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	select {
	case l := <-h.policy.releasedCh:
		if l.outcome.Unreachable != "" || l.outcome.Status != 0 {
			t.Fatalf("cancelled send released with %+v", l.outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease not released")
	}
}

func TestB10ClassifiesTransportErrors(t *testing.T) {
	send := func(c *http.Client, url string) error {
		resp, err := c.Get(url)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s: unexpected response", url)
		}
		return err
	}
	var dials atomic.Int64
	if got := unreachableClass(send(deadClient(&dials), "http://"+deadHost+"/")); got != "dns" {
		t.Errorf("dns: %q", got)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if got := unreachableClass(send(http.DefaultClient, closed.URL)); got != "dial" {
		t.Errorf("refused: %q", got)
	}
	tlsSrv := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsSrv.Close()
	if got := unreachableClass(send(&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{}}}, tlsSrv.URL)); got != "tls" {
		t.Errorf("tls: %q", got)
	}
	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	defer slow.Close()
	defer close(hang)
	if got := unreachableClass(send(&http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 50 * time.Millisecond}}, slow.URL)); got != "timeout" {
		t.Errorf("header timeout: %q", got)
	}
	reset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	}))
	defer reset.Close()
	if got := unreachableClass(send(&http.Client{Transport: &http.Transport{}}, reset.URL)); got != "reset" {
		t.Errorf("reset: %q", got)
	}
	if got := unreachableClass(errors.New("rewrite body: invalid JSON")); got != "" {
		t.Errorf("pre-send error counted: %q", got)
	}
}

// TestB14DeadUpstreamRequestCount drives 60 s of continuous client traffic
// (one request every 100 ms) at a dead single-candidate upstream with
// threshold 3, base 5s, max 60s, and counts upstream attempts: 3 to open,
// then one probe at each half-open (t=5.2s, 15.2s, 35.2s; the next would be
// at 75.2s).
func TestB14DeadUpstreamRequestCount(t *testing.T) {
	h, clk, dials := breakerHarness(t)
	deadAccount(h, "llama-lab")
	singleRoute(h, "llama-lab")
	breakerPolicyFor(h, clk, policy.BreakerOptions{Threshold: 3, Base: 5 * time.Second, Max: 60 * time.Second})
	h.start()
	statuses := map[int]int{}
	for i := range 600 {
		resp := h.post("/v1/responses", clientKey, respBody, nil)
		resp.Body.Close()
		h.waitHandled(i + 1)
		statuses[resp.StatusCode]++
		clk.Add(100 * time.Millisecond)
	}
	if dials.Load() != 6 || statuses[http.StatusBadGateway] != 6 || statuses[http.StatusServiceUnavailable] != 594 {
		t.Fatalf("upstream attempts %d, statuses %v; want 6 attempts, 6x502, 594x503", dials.Load(), statuses)
	}
	t.Logf("60s of traffic at 10 req/s: %d upstream attempts (without breaker: 600)", dials.Load())
}

// BR-2 (SPEC Amendment R1.3): reachability is observed when response headers
// arrive. A half-open probe that gets 200 headers from a recovered upstream
// closes the breaker at once, so other requests are served while the probe is
// still streaming, and the probe's end does not change the breaker again.
func TestBRFixBR2StreamingProbeClosesOnHeaders(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	h.gates = append(h.gates, gate)
	h.upstream("llm", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"stream":true`) {
			okJSON(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-gate // a long generation still in progress
	})
	h.routes = []core.Route{{Name: "r", Models: []string{"m"}, Interactive: []string{"llm"}}}
	clk := &stepClock{t: testNow}
	h.clock = clk
	p := breakerPolicyFor(h, clk, policy.BreakerOptions{Threshold: 1, Base: 5 * time.Second, Max: time.Minute})
	// The upstream was unreachable once: the breaker opens; 5s later it is
	// half-open and the upstream has recovered.
	l, d := p.Acquire(core.ClassInteractive, []string{"llm"}, nil)
	if !d.Allow {
		t.Fatalf("acquire: %s", d.Reason)
	}
	l.Release(core.Outcome{Unreachable: "dial"})
	clk.Add(5 * time.Second)
	h.start()

	probe := h.post("/v1/responses", clientKey, `{"model":"m","input":"hi","stream":true}`, nil)
	defer probe.Body.Close()
	if probe.StatusCode != http.StatusOK {
		t.Fatalf("probe status %d", probe.StatusCode)
	}
	// The probe has its response headers: the upstream is reachable.
	if state, _, _, _ := p.Breaker("llm"); state != policy.BreakerClosed {
		t.Errorf("breaker %q while the probe streams a 200, want closed", state)
	}
	resp := h.post("/v1/responses", clientKey, modelBody("m"), nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("request during a successful streaming probe: %d %s (want 200: reachability was already proven)", resp.StatusCode, b)
	}
	close(gate)
	_, _ = io.ReadAll(probe.Body)
	if state, fails, _, _ := p.Breaker("llm"); state != policy.BreakerClosed || fails != 0 {
		t.Errorf("breaker %s/%d after the probe stream ended, want closed/0", state, fails)
	}
}
