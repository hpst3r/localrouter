package app_test

// B13: the reachability breaker end to end through the real App: config
// policy.breaker, a dead upstream (a closed loopback port: connection
// refused, no network), the 503 refusal, /control/v1/status, and breaker
// state surviving a reload that changes the breaker knobs.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
)

type brClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *brClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *brClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestB13BreakerEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	key := "br-client-key-0123456789"
	hrWriteFile(t, filepath.Join(dir, "a.key"), key)
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), hrPricingYAML(1.0))
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	cfgPath := filepath.Join(dir, "config.yaml")
	base := hrConfigYAML(deadURL, "a.key", hrPricedModel, 0)
	hrWriteFile(t, cfgPath, base+"policy: {breaker: {threshold: 2, base: 10s, max: 40s}}\n")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	clk := &brClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	a, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	e := &hrEnv{t: t, dir: dir, cfgPath: cfgPath, a: a, srv: httptest.NewServer(a.Handler),
		client: &http.Client{Timeout: 20 * time.Second}}
	t.Cleanup(e.srv.Close)

	for i := range 2 {
		if r := e.responses(key, hrPricedModel); r.status != http.StatusBadGateway {
			t.Fatalf("attempt %d: %d %s", i, r.status, r.body)
		}
	}
	r := e.responses(key, hrPricedModel)
	var body struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	_ = json.Unmarshal(r.body, &body)
	if r.status != http.StatusServiceUnavailable || r.header.Get("Retry-After") != "10" ||
		body.Error.Type != "upstream_unavailable" || body.Error.Message != "localrouter: upstream unreachable: acct" {
		t.Fatalf("refusal: %d Retry-After=%q %s", r.status, r.header.Get("Retry-After"), r.body)
	}
	breaker := func() map[string]any {
		t.Helper()
		st := e.do("GET", "/control/v1/status", key, "")
		var doc struct {
			Accounts []struct {
				ID      string         `json:"id"`
				Breaker map[string]any `json:"breaker"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(st.body, &doc); err != nil || len(doc.Accounts) != 1 {
			t.Fatalf("status: %v %s", err, st.body)
		}
		return doc.Accounts[0].Breaker
	}
	if b := breaker(); b["state"] != "open" || b["consecutive_failures"] != float64(2) || b["open_until"] != "2026-10-01T12:00:10Z" {
		t.Fatalf("status breaker = %v", b)
	}

	// Reload with new knobs: the open breaker survives the generation swap.
	hrWriteFile(t, cfgPath, base+"policy: {breaker: {threshold: 2, base: 10s, max: 15s}}\n")
	if st, err := a.Reload(cfgPath); err != nil || !st.OK {
		t.Fatalf("reload: %+v %v", st, err)
	}
	if b := breaker(); b["state"] != "open" || b["open_until"] != "2026-10-01T12:00:10Z" {
		t.Fatalf("breaker after reload = %v", b)
	}
	// The half-open probe fails and re-opens for min(2*10s, new max 15s).
	clk.add(10 * time.Second)
	if r := e.responses(key, hrPricedModel); r.status != http.StatusBadGateway {
		t.Fatalf("probe: %d %s", r.status, r.body)
	}
	if b := breaker(); b["state"] != "open" || b["open_until"] != "2026-10-01T12:00:25Z" {
		t.Fatalf("breaker after failed probe = %v", b)
	}
	if r := e.responses(key, hrPricedModel); r.status != http.StatusServiceUnavailable || r.header.Get("Retry-After") != "15" {
		t.Fatalf("refusal after probe: %d %v", r.status, r.header)
	}

	// Removing the block disables the breaker on reload: requests reach the
	// (still dead) upstream again and status omits the breaker.
	hrWriteFile(t, cfgPath, base)
	if st, err := a.Reload(cfgPath); err != nil || !st.OK {
		t.Fatalf("reload: %+v %v", st, err)
	}
	if r := e.responses(key, hrPricedModel); r.status != http.StatusBadGateway || strings.Contains(string(r.body), "upstream_unavailable") {
		t.Fatalf("disabled breaker: %d %s", r.status, r.body)
	}
	if b := breaker(); b != nil {
		t.Fatalf("disabled breaker in status: %v", b)
	}
}

// brStatus returns the single account's breaker object from
// /control/v1/status (nil when the breaker is disabled).
func brStatus(e *hrEnv, key string) map[string]any {
	e.t.Helper()
	st := e.do("GET", "/control/v1/status", key, "")
	var doc struct {
		Accounts []struct {
			Breaker map[string]any `json:"breaker"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(st.body, &doc); err != nil || len(doc.Accounts) != 1 {
		e.t.Fatalf("status: %v %s", err, st.body)
	}
	return doc.Accounts[0].Breaker
}

// brFlakyUpstream answers per its mode: "stream" sends 200 headers and part
// of the body, then holds until streamGate closes; "hangup" closes the
// connection before any headers; "hold" signals probeIn and hangs up once
// probeGate closes.
type brFlakyUpstream struct {
	mu                    sync.Mutex
	mode                  string
	hits                  int
	streamIn, probeIn     chan struct{}
	streamGate, probeGate chan struct{}
}

func (u *brFlakyUpstream) setMode(m string) { u.mu.Lock(); u.mode = m; u.mu.Unlock() }

func (u *brFlakyUpstream) hitCount() int { u.mu.Lock(); defer u.mu.Unlock(); return u.hits }

func (u *brFlakyUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	u.mu.Lock()
	mode := u.mode
	u.hits++
	u.mu.Unlock()
	hangup := func() {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	switch mode {
	case "stream":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"r","object":"response",`)
		w.(http.Flusher).Flush()
		close(u.streamIn)
		<-u.streamGate
		_, _ = io.WriteString(w, `"usage":{"input_tokens":1,"output_tokens":0}}`)
	case "hold":
		close(u.probeIn)
		<-u.probeGate
		hangup()
	default:
		hangup()
	}
}

// R1.5 (BR-1 end to end): a reload (the SIGHUP path) enables the breaker
// while a long response admitted under the disabled configuration is in
// flight. The breaker opens; when the old response ends during the half-open
// probe it changes nothing, and exactly one probe reaches the upstream.
func TestBRFixBR1ReloadEnablesBreakerDuringStream(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	key := "br-client-key-0123456789"
	hrWriteFile(t, filepath.Join(dir, "a.key"), key)
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), hrPricingYAML(1.0))
	up := &brFlakyUpstream{mode: "stream", streamIn: make(chan struct{}), probeIn: make(chan struct{}),
		streamGate: make(chan struct{}), probeGate: make(chan struct{})}
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	endStream := sync.OnceFunc(func() { close(up.streamGate) })
	endProbe := sync.OnceFunc(func() { close(up.probeGate) })
	t.Cleanup(endProbe)
	t.Cleanup(endStream)

	cfgPath := filepath.Join(dir, "config.yaml")
	base := hrConfigYAML(upSrv.URL, "a.key", hrPricedModel, 0)
	hrWriteFile(t, cfgPath, base) // breaker disabled
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	clk := &brClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	a, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	e := &hrEnv{t: t, dir: dir, cfgPath: cfgPath, a: a, srv: httptest.NewServer(a.Handler),
		client: &http.Client{Timeout: 20 * time.Second}}
	t.Cleanup(e.srv.Close)

	// A long response admitted under the disabled configuration.
	streamDone := make(chan hrResult, 1)
	go func() {
		r, err := e.doRaw("POST", "/v1/responses", key, `{"model":"`+hrPricedModel+`","input":"hi"}`)
		if err != nil {
			r.status = -1
		}
		streamDone <- r
	}()
	<-up.streamIn

	hrWriteFile(t, cfgPath, base+"policy: {breaker: {threshold: 1, base: 10s, max: 40s}}\n")
	if st, err := a.Reload(cfgPath); err != nil || !st.OK {
		t.Fatalf("reload: %+v %v", st, err)
	}
	up.setMode("hangup")
	if r := e.responses(key, hrPricedModel); r.status != http.StatusBadGateway {
		t.Fatalf("unreachable attempt: %d %s", r.status, r.body)
	}
	if b := brStatus(e, key); b["state"] != "open" || b["open_until"] != "2026-10-01T12:00:10Z" {
		t.Fatalf("breaker after the failure = %v", b)
	}

	// Half-open: one probe reaches the upstream and is held there.
	clk.add(10 * time.Second)
	up.setMode("hold")
	probeDone := make(chan int, 1)
	go func() {
		r, err := e.doRaw("POST", "/v1/responses", key, `{"model":"`+hrPricedModel+`","input":"hi"}`)
		if err != nil {
			r.status = -1
		}
		probeDone <- r.status
	}()
	<-up.probeIn

	// The old response ends successfully: it is not the probe and was
	// admitted under the old configuration, so the breaker is unchanged.
	endStream()
	if r := <-streamDone; r.status != http.StatusOK {
		t.Fatalf("old stream: %d %s", r.status, r.body)
	}
	if b := brStatus(e, key); b["state"] != "half_open" {
		t.Fatalf("breaker after the old lease ended = %v, want half_open", b)
	}
	if r := e.responses(key, hrPricedModel); r.status != http.StatusServiceUnavailable {
		t.Fatalf("second request during the probe: %d %s, want 503", r.status, r.body)
	}

	// The probe fails: re-open for 2x10s. Exactly one probe was sent.
	endProbe()
	if s := <-probeDone; s != http.StatusBadGateway {
		t.Fatalf("probe: %d", s)
	}
	if b := brStatus(e, key); b["state"] != "open" || b["open_until"] != "2026-10-01T12:00:30Z" {
		t.Fatalf("breaker after the failed probe = %v", b)
	}
	if n := up.hitCount(); n != 3 {
		t.Fatalf("upstream hits = %d, want 3 (old stream, failure, one probe)", n)
	}
}
