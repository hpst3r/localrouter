package app_test

// Pooled backends end to end through the real App (docs/SPEC.md "Pooled
// backends (routes[].balance)"): P13 reload semantics and P17 accounting.

import (
	"database/sql"
	"encoding/json"
	"fmt"
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

// poolUpstream serves /<account>/v1/...; while held, every request blocks
// until release. arrived carries the account of each request that reached it.
type poolUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	gate    chan struct{}
	arrived chan string
}

func newPoolUpstream(t *testing.T) *poolUpstream {
	u := &poolUpstream{arrived: make(chan string, 64)}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		acct := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)[0]
		u.mu.Lock()
		gate := u.gate
		u.mu.Unlock()
		u.arrived <- acct
		if gate != nil {
			<-gate
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "r-" + acct, "object": "response",
			"usage": map[string]any{"input_tokens": 100, "output_tokens": 10}})
	}))
	t.Cleanup(u.srv.Close)
	t.Cleanup(u.release)
	return u
}

func (u *poolUpstream) hold() { u.mu.Lock(); u.gate = make(chan struct{}); u.mu.Unlock() }

func (u *poolUpstream) release() {
	u.mu.Lock()
	if u.gate != nil {
		close(u.gate)
		u.gate = nil
	}
	u.mu.Unlock()
}

func (u *poolUpstream) wait(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-u.arrived:
		if got != want {
			t.Fatalf("request reached %s, want %s", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no request reached %s", want)
	}
}

// poolYAML renders a pooled route over members (a subset of p1, p2) with the
// given p2 weight; extraAccount adds an account (restart-only).
func poolYAML(upURL string, members []string, p2Weight int, extraAccount bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `listen: 127.0.0.1:0
host_name: poolhost
data_dir: data
pricing_file: pricing.yaml
control: {require_auth: true}
clients:
  - {name: ide, class: interactive, key_file: ide.key}
accounts:
  - {id: p1, provider: openai_compat, base_url: %[1]s/p1/v1, api_key_env: HR_UPSTREAM_KEY, cost_basis: api_equivalent}
  - {id: p2, provider: openai_compat, base_url: %[1]s/p2/v1, api_key_env: HR_UPSTREAM_KEY, cost_basis: api_equivalent}
`, upURL)
	if extraAccount {
		fmt.Fprintf(&b, "  - {id: p3, provider: openai_compat, base_url: %s/p3/v1, api_key_env: HR_UPSTREAM_KEY}\n", upURL)
	}
	list := strings.Join(members, ", ")
	b.WriteString("routes:\n  - name: pool\n    models: [qwen]\n    interactive: [" + list + "]\n    upstreams:\n")
	for _, m := range members {
		up := map[string]string{"p1": "up-a", "p2": "up-b"}[m]
		fmt.Fprintf(&b, "      %s: {upstream_model: %s, protocols: [responses], input_modalities: [text]}\n", m, up)
	}
	b.WriteString("    balance:\n      strategy: least_inflight\n      identity: Qwen Q4\n      sticky_ttl: 15m\n      members:\n")
	for _, m := range members {
		w := 1
		if m == "p2" {
			w = p2Weight
		}
		fmt.Fprintf(&b, "        %s: {weight: %d, max_inflight: 1}\n", m, w)
	}
	return b.String()
}

type poolEnv struct {
	*hrEnv
	logs *lockedBuffer
}

func newPoolEnv(t *testing.T, up *poolUpstream) *poolEnv {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	hrWriteFile(t, filepath.Join(dir, "ide.key"), poolKey)
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), "models:\n  up-a:\n    input: 1\n    output: 2\n")
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, poolYAML(up.srv.URL, []string{"p1", "p2"}, 1, false))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	clk := &brClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	a, err := app.Build(cfg, slog.New(slog.NewJSONHandler(logs, nil)), app.Overrides{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	e := &hrEnv{t: t, dir: dir, cfgPath: cfgPath, a: a, srv: httptest.NewServer(a.Handler),
		client: &http.Client{Timeout: 20 * time.Second}}
	t.Cleanup(e.srv.Close)
	return &poolEnv{hrEnv: e, logs: logs}
}

const poolKey = "pool-client-key-0123456789"

// ask sends one /v1/responses request, optionally with a session label.
func (e *poolEnv) ask(session string) hrResult {
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/responses", strings.NewReader(`{"model":"qwen","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+poolKey)
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("X-LocalRouter-Session", session)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return hrResult{status: resp.StatusCode, header: resp.Header, body: b}
}

func (e *poolEnv) askAsync(session string) <-chan hrResult {
	ch := make(chan hrResult, 1)
	go func() {
		req, _ := http.NewRequest("POST", e.srv.URL+"/v1/responses", strings.NewReader(`{"model":"qwen","input":"hi"}`))
		req.Header.Set("Authorization", "Bearer "+poolKey)
		if session != "" {
			req.Header.Set("X-LocalRouter-Session", session)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			ch <- hrResult{}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ch <- hrResult{status: resp.StatusCode, body: b}
	}()
	return ch
}

func (e *poolEnv) reload(t *testing.T, yaml string) (ok bool, restartOnly []string) {
	t.Helper()
	hrWriteFile(t, e.cfgPath, yaml)
	st, _ := e.a.Reload(e.cfgPath)
	return st.OK, st.RestartOnly
}

func (e *poolEnv) inflight(t *testing.T, id string) float64 {
	t.Helper()
	var doc struct {
		Accounts []struct {
			ID       string  `json:"id"`
			Inflight float64 `json:"inflight"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(e.do("GET", "/control/v1/status", poolKey, "").body, &doc); err != nil {
		t.Fatal(err)
	}
	for _, a := range doc.Accounts {
		if a.ID == id {
			return a.Inflight
		}
	}
	t.Fatalf("no account %s", id)
	return 0
}

func TestP13PoolReloadEndToEnd(t *testing.T) {
	up := newPoolUpstream(t)
	e := newPoolEnv(t, up)

	// R1 (session S) binds S to p1 and stays in flight across a reload.
	up.hold()
	r1 := e.askAsync("S")
	up.wait(t, "p1")
	if ok, _ := e.reload(t, poolYAML(up.srv.URL, []string{"p1", "p2"}, 3, false)); !ok {
		t.Fatal("weight reload rejected")
	}
	// The cap still counts R1's lease from the old generation: R2 goes to p2,
	// and a third request is saturated.
	r2 := e.askAsync("")
	up.wait(t, "p2")
	if r := e.ask(""); r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") != "1" || !strings.Contains(string(r.body), "pool_saturated") {
		t.Fatalf("saturated after reload: %d %s", r.status, r.body)
	}
	up.release()
	for _, ch := range []<-chan hrResult{r1, r2} {
		if r := <-ch; r.status != http.StatusOK {
			t.Fatalf("held request: %d %s", r.status, r.body)
		}
	}
	// The binding survived: with p2 at weight 3 the strategy would pick p2.
	if r := e.ask("S"); r.status != http.StatusOK || !strings.Contains(string(r.body), "r-p1") {
		t.Fatalf("sticky after reload: %d %s", r.status, r.body)
	}
	<-up.arrived
	if r := e.ask(""); !strings.Contains(string(r.body), "r-p2") {
		t.Fatalf("strategy after reload: %s", r.body)
	}
	<-up.arrived

	// Remove p2 while it holds a lease: p2 is never picked again and its lease
	// releases without underflow.
	up.hold()
	r3 := e.askAsync("")
	up.wait(t, "p2")
	if ok, _ := e.reload(t, poolYAML(up.srv.URL, []string{"p1"}, 1, false)); !ok {
		t.Fatal("member removal rejected")
	}
	up.release()
	if r := <-r3; r.status != http.StatusOK {
		t.Fatalf("r3: %d", r.status)
	}
	for range 3 {
		if r := e.ask("T"); r.status != http.StatusOK || !strings.Contains(string(r.body), "r-p1") {
			t.Fatalf("after removal: %d %s", r.status, r.body)
		}
		<-up.arrived
	}
	if n := e.inflight(t, "p2"); n != 0 {
		t.Fatalf("p2 inflight = %v", n)
	}
	// Adding an account stays restart-only.
	if ok, ro := e.reload(t, poolYAML(up.srv.URL, []string{"p1"}, 1, true)); ok || !strings.Contains(strings.Join(ro, ","), "accounts") {
		t.Fatalf("account add: ok=%v restart_only=%v", ok, ro)
	}
}

func TestP17PoolAccountingEndToEnd(t *testing.T) {
	up := newPoolUpstream(t)
	e := newPoolEnv(t, up)
	// Mixed traffic over both members: hold one on p1 so the next goes to p2.
	for range 3 {
		up.hold()
		r1 := e.askAsync("")
		up.wait(t, "p1")
		r2 := e.askAsync("")
		up.wait(t, "p2")
		up.release()
		<-r1
		<-r2
	}
	if r := e.ask(""); r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	<-up.arrived

	analytics := func(q string) map[string]json.RawMessage {
		t.Helper()
		r := e.do("GET", "/control/v1/analytics?from=2026-10-01T11:00:00Z&to=2026-10-01T13:00:00Z&"+q, poolKey, "")
		if r.status != http.StatusOK {
			t.Fatalf("analytics %s: %d %s", q, r.status, r.body)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(r.body, &m)
		return m
	}
	type row struct {
		Key          string   `json:"key"`
		Requests     int64    `json:"requests"`
		InputTokens  int64    `json:"input_tokens"`
		OutputTokens int64    `json:"output_tokens"`
		CostUSD      *float64 `json:"cost_usd"`
	}
	var byRoute, byAcct []row
	_ = json.Unmarshal(analytics("group=route")["breakdown"], &byRoute)
	_ = json.Unmarshal(analytics("group=account&filter.route=pool")["breakdown"], &byAcct)
	if len(byRoute) != 1 || byRoute[0].Key != "pool" || byRoute[0].Requests != 7 {
		t.Fatalf("group=route: %+v", byRoute)
	}
	var sum row
	members := map[string]row{}
	for _, r := range byAcct {
		sum.Requests += r.Requests
		sum.InputTokens += r.InputTokens
		sum.OutputTokens += r.OutputTokens
		members[r.Key] = r
	}
	if sum.Requests != byRoute[0].Requests || sum.InputTokens != byRoute[0].InputTokens || sum.OutputTokens != byRoute[0].OutputTokens {
		t.Fatalf("member sum %+v != route %+v", sum, byRoute[0])
	}
	if members["p1"].Requests != 4 || members["p2"].Requests != 3 || members["p1"].CostUSD == nil {
		t.Fatalf("members %+v", members)
	}
	// Unpriced member rows (p2 -> up-b) keep cost_usd NULL, never 0.
	db, err := sql.Open("sqlite", filepath.Join(e.dir, "data", "localrouter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var priced, nulls int
	if err := db.QueryRow(`SELECT COUNT(cost_usd), COUNT(*) - COUNT(cost_usd) FROM requests WHERE account_id = 'p2'`).Scan(&priced, &nulls); err != nil {
		t.Fatal(err)
	}
	if priced != 0 || nulls != 3 {
		t.Fatalf("p2 rows priced=%d null=%d", priced, nulls)
	}
	if !strings.Contains(e.logs.String(), `"pool_pick":"least_inflight"`) {
		t.Fatal("pooled attempt events lack pool_pick")
	}
}
