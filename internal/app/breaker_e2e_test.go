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
