package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type fakeClock struct{ t time.Time }

func (c fakeClock) Now() time.Time { return c.t }

type fakeQuota struct{ snaps map[string]core.Snapshot }

func (q *fakeQuota) Latest(id string) (core.Snapshot, bool) {
	s, ok := q.snaps[id]
	return s, ok
}
func (q *fakeQuota) ObserveHeaders(string, http.Header) {}
func (q *fakeQuota) RequestRefresh(string, bool)        {}

type fakePolicy struct {
	mu       sync.Mutex
	states   map[string]core.AccountState
	dryRuns  []dryRunCall
	acquires int
	decision core.Decision
}

type dryRunCall struct {
	class      core.Class
	candidates []string
}

func (p *fakePolicy) Acquire(core.Class, []string, map[string]bool) (core.Lease, core.Decision) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.acquires++
	return nil, core.Decision{Reason: "acquire must not be called"}
}

func (p *fakePolicy) DryRun(c core.Class, cands []string) core.Decision {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dryRuns = append(p.dryRuns, dryRunCall{c, cands})
	return p.decision
}

func (p *fakePolicy) Status(id string) core.AccountState { return p.states[id] }

type fakeLedger struct {
	since time.Time
	group string
	rows  []core.UsageRow
	err   error
}

func (l *fakeLedger) Record(context.Context, core.RequestRecord) error { return nil }
func (l *fakeLedger) Summary(_ context.Context, since time.Time, group string) ([]core.UsageRow, error) {
	l.since, l.group = since, group
	return l.rows, l.err
}
func (l *fakeLedger) Close() error { return nil }

const secretKey = "sk-client-SECRET-0123456789"

type fixture struct {
	srv    *Server
	h      http.Handler
	policy *fakePolicy
	ledger *fakeLedger
}

func newFixture(requireAuth bool) *fixture {
	cost := 1.25
	pol := &fakePolicy{
		states: map[string]core.AccountState{
			"primary": {Inflight: 2, BackgroundAdmissible: false, InteractiveAdmissible: true},
			"stale":   {CooldownUntil: t0.Add(5 * time.Minute)},
			"expired": {CooldownUntil: t0.Add(-time.Minute), BackgroundAdmissible: true, InteractiveAdmissible: true},
		},
		decision: core.Decision{Allow: true, AccountID: "secondary", Reason: "admitted"},
	}
	q := &fakeQuota{snaps: map[string]core.Snapshot{
		"primary": {
			AccountID: "primary", FetchedAt: t0.Add(-30 * time.Second),
			Windows: []core.Window{
				{Kind: core.Window5h, UsedFrac: 0.99, ResetAt: t0.Add(-time.Second), WindowSeconds: 18000},
				{Kind: core.WindowWeekly, UsedFrac: 1.3, ResetAt: t0.Add(48 * time.Hour), WindowSeconds: 604800},
			},
		},
		"stale": {
			AccountID: "stale", FetchedAt: t0.Add(-20 * time.Minute),
			Windows: []core.Window{{Kind: core.Window5h, UsedFrac: 0.484, WindowSeconds: 18000}},
			Err:     "usage api: HTTP 503",
		},
		"expired": {AccountID: "expired", FetchedAt: t0},
	}}
	led := &fakeLedger{rows: []core.UsageRow{{Key: "primary", Requests: 3, InputTokens: 1000, CostUSD: &cost}}}
	srv := New(Deps{
		Accounts: []core.Account{
			{ID: "primary", Provider: core.ProviderCodex, Reserve: map[string]float64{"5h": 0.1, "weekly": 0.2}},
			{ID: "stale", Provider: core.ProviderOllama},
			{ID: "none", Provider: core.ProviderOpenAICompat},
			{ID: "expired", Provider: core.ProviderOllama},
		},
		Quota:  q,
		Policy: pol,
		Ledger: led,
		Routes: []core.Route{{
			Name: "gpt", Models: []string{"gpt-5", "gpt-5-mini"},
			Interactive: []string{"primary", "secondary"}, Background: []string{"secondary"},
		}},
		Authenticate: func(b string) (core.Client, bool) {
			if b == secretKey {
				return core.Client{Name: "tester", Class: core.ClassInteractive}, true
			}
			return core.Client{}, false
		},
		Clock: fakeClock{t0},
	}, Options{RequireAuth: requireAuth, StaleAfter: 10 * time.Minute})
	return &fixture{srv: srv, h: srv.Handler(), policy: pol, ledger: led}
}

func (f *fixture) do(t *testing.T, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), secretKey) {
		t.Fatalf("%s %s: response leaks client key", method, path)
	}
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body)
	}
	return m
}

func TestHealthz(t *testing.T) {
	f := newFixture(true)
	rec := f.do(t, "GET", "/healthz", "", "")
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body)
	}
}

func TestStatusShape(t *testing.T) {
	f := newFixture(false)
	rec := f.do(t, "GET", "/control/v1/status", "", "")
	if rec.Code != 200 {
		t.Fatalf("status code %d", rec.Code)
	}
	doc := decode(t, rec)
	if doc["schema_version"] != float64(1) {
		t.Fatalf("schema_version = %v", doc["schema_version"])
	}
	if doc["now"] != "2026-10-01T12:00:00Z" {
		t.Fatalf("now = %v", doc["now"])
	}
	accts := doc["accounts"].([]any)
	if len(accts) != 4 {
		t.Fatalf("accounts = %d", len(accts))
	}
	wantKeys := []string{"id", "provider", "healthy", "cooldown_until", "inflight", "reserve", "windows",
		"background_admissible", "interactive_admissible", "snapshot_age_s", "stale", "error"}
	byID := map[string]map[string]any{}
	for i, a := range accts {
		m := a.(map[string]any)
		for _, k := range wantKeys {
			if _, ok := m[k]; !ok {
				t.Errorf("account %d missing key %q", i, k)
			}
		}
		byID[m["id"].(string)] = m
	}
	// Order preserved.
	if accts[0].(map[string]any)["id"] != "primary" || accts[2].(map[string]any)["id"] != "none" {
		t.Fatalf("account order not preserved")
	}

	p := byID["primary"]
	if p["healthy"] != true || p["stale"] != false || p["error"] != nil || p["cooldown_until"] != nil {
		t.Errorf("primary flags: %v", p)
	}
	if p["inflight"] != float64(2) || p["background_admissible"] != false || p["interactive_admissible"] != true {
		t.Errorf("primary policy state: %v", p)
	}
	if p["snapshot_age_s"] != float64(30) {
		t.Errorf("primary snapshot_age_s = %v", p["snapshot_age_s"])
	}
	if r := p["reserve"].(map[string]any); r["5h"] != 0.1 || r["weekly"] != 0.2 {
		t.Errorf("reserve = %v", r)
	}
	wins := p["windows"].([]any)
	w5 := wins[0].(map[string]any)
	if w5["kind"] != "5h" || w5["rolled"] != true || w5["used_frac"] != float64(0) || w5["remaining_frac"] != float64(1) {
		t.Errorf("rolled window = %v", w5)
	}
	if w5["window_seconds"] != float64(18000) || w5["reset_at"] != "2026-10-01T11:59:59Z" {
		t.Errorf("rolled window meta = %v", w5)
	}
	ww := wins[1].(map[string]any)
	if ww["rolled"] != false || ww["used_frac"] != float64(1) || ww["remaining_frac"] != float64(0) {
		t.Errorf("clamped weekly window = %v", ww)
	}

	s := byID["stale"]
	if s["stale"] != true || s["error"] != "usage api: HTTP 503" || s["healthy"] != false {
		t.Errorf("stale account = %v", s)
	}
	if s["cooldown_until"] != "2026-10-01T12:05:00Z" {
		t.Errorf("cooldown_until = %v", s["cooldown_until"])
	}
	sw := s["windows"].([]any)[0].(map[string]any)
	if sw["reset_at"] != nil || sw["used_frac"] != 0.484 || sw["rolled"] != false {
		t.Errorf("unknown-reset window = %v", sw)
	}

	n := byID["none"]
	if n["snapshot_age_s"] != nil || n["stale"] != true || n["error"] != nil {
		t.Errorf("no-snapshot account = %v", n)
	}
	if w := n["windows"].([]any); len(w) != 0 {
		t.Errorf("no-snapshot windows = %v", w)
	}
	if r := n["reserve"].(map[string]any); len(r) != 0 {
		t.Errorf("no-reserve = %v", r)
	}

	e := byID["expired"]
	if e["cooldown_until"] != nil || e["healthy"] != true {
		t.Errorf("expired cooldown should be cleared: %v", e)
	}
}

func TestParseSince(t *testing.T) {
	good := map[string]time.Duration{
		"24h": 24 * time.Hour, "168h": 168 * time.Hour, "7d": 7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour, "400d": 400 * 24 * time.Hour, "90m": 90 * time.Minute,
	}
	for in, want := range good {
		got, err := parseSince(in)
		if err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "0h", "-1h", "401d", "0d", "1.5d", "d", "9601h", "7 d"} {
		if _, err := parseSince(in); err == nil {
			t.Errorf("parseSince(%q) should fail", in)
		}
	}
}

func TestUsage(t *testing.T) {
	f := newFixture(false)
	rec := f.do(t, "GET", "/control/v1/usage", "", "")
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	doc := decode(t, rec)
	if f.ledger.group != "account" || !f.ledger.since.Equal(t0.Add(-24*time.Hour)) {
		t.Errorf("defaults: group=%q since=%v", f.ledger.group, f.ledger.since)
	}
	rows := doc["rows"].([]any)
	row := rows[0].(map[string]any)
	for _, k := range []string{"key", "requests", "input_tokens", "cached_input_tokens", "output_tokens",
		"reasoning_tokens", "cost_usd", "unknown_usage_requests"} {
		if _, ok := row[k]; !ok {
			t.Errorf("row missing %q", k)
		}
	}
	if row["cost_usd"] != 1.25 {
		t.Errorf("cost_usd = %v", row["cost_usd"])
	}

	f.do(t, "GET", "/control/v1/usage?since=7d&group=model", "", "")
	if f.ledger.group != "model" || !f.ledger.since.Equal(t0.Add(-7*24*time.Hour)) {
		t.Errorf("7d/model: group=%q since=%v", f.ledger.group, f.ledger.since)
	}

	for _, q := range []string{"?group=provider", "?since=bogus", "?since=500d", "?group=ACCOUNT"} {
		rec := f.do(t, "GET", "/control/v1/usage"+q, "", "")
		if rec.Code != 400 {
			t.Errorf("%s: code %d", q, rec.Code)
		}
		if m := decode(t, rec); m["error"] == nil {
			t.Errorf("%s: missing error body", q)
		}
	}

	f.ledger.rows, f.ledger.err = nil, errors.New("disk I/O at /secret/path")
	rec = f.do(t, "GET", "/control/v1/usage", "", "")
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "/secret/path") {
		t.Errorf("ledger error: %d %s", rec.Code, rec.Body)
	}

	f.ledger.err = nil
	rec = f.do(t, "GET", "/control/v1/usage", "", "")
	if !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Errorf("empty rows should be [], got %s", rec.Body)
	}
}

func TestAdmitUsesDryRun(t *testing.T) {
	f := newFixture(false)
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"background","model":"gpt-5-mini"}`, "")
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	m := decode(t, rec)
	if m["decision"] != "allow" || m["account_id"] != "secondary" || m["reason"] != "admitted" {
		t.Errorf("admit = %v", m)
	}
	if f.policy.acquires != 0 {
		t.Fatalf("Acquire called %d times", f.policy.acquires)
	}
	if len(f.policy.dryRuns) != 1 || f.policy.dryRuns[0].class != core.ClassBackground ||
		strings.Join(f.policy.dryRuns[0].candidates, ",") != "secondary" {
		t.Fatalf("dry runs = %+v", f.policy.dryRuns)
	}

	f.policy.decision = core.Decision{Allow: false, Reason: "reserve"}
	m = decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, ""))
	if m["decision"] != "deny" || m["reason"] != "reserve" {
		t.Errorf("deny = %v", m)
	}
	if got := strings.Join(f.policy.dryRuns[1].candidates, ","); got != "primary,secondary" {
		t.Errorf("interactive candidates = %s", got)
	}

	cases := map[string]int{
		`{"class":"interactive","model":"unknown"}`: 404,
		`{"class":"bulk","model":"gpt-5"}`:          400,
		`{"model":"gpt-5"}`:                         400,
		`not json`:                                  400,
	}
	for body, want := range cases {
		if rec := f.do(t, "POST", "/control/v1/admit", body, ""); rec.Code != want {
			t.Errorf("%s: code %d want %d", body, rec.Code, want)
		}
	}
	if rec := f.do(t, "GET", "/control/v1/admit", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET admit: %d", rec.Code)
	}
	if f.policy.acquires != 0 {
		t.Fatalf("Acquire called")
	}
}

func TestAdmitAccount(t *testing.T) {
	f := newFixture(false)
	f.policy.decision = core.Decision{Allow: false, Reason: "background reserve on weekly"}
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"background","account":"stale"}`, "")
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	m := decode(t, rec)
	if m["decision"] != "deny" || m["account_id"] != "stale" || m["reason"] != "background reserve on weekly" {
		t.Errorf("admit account = %v", m)
	}
	if len(f.policy.dryRuns) != 1 || f.policy.dryRuns[0].class != core.ClassBackground ||
		strings.Join(f.policy.dryRuns[0].candidates, ",") != "stale" {
		t.Fatalf("dry runs = %+v", f.policy.dryRuns)
	}

	f.policy.decision = core.Decision{Allow: true, AccountID: "stale", Reason: "admitted"}
	if m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","account":"stale"}`, "")); m["decision"] != "allow" {
		t.Errorf("allow = %v", m)
	}

	cases := map[string]int{
		`{"class":"background","account":"ghost"}`:                 404,
		`{"class":"background","account":"stale","model":"gpt-5"}`: 400,
		`{"class":"background"}`:                                   400,
		`{"class":"bulk","account":"stale"}`:                       400,
	}
	for body, want := range cases {
		if rec := f.do(t, "POST", "/control/v1/admit", body, ""); rec.Code != want {
			t.Errorf("%s: code %d want %d", body, rec.Code, want)
		}
	}
	if len(f.policy.dryRuns) != 2 || f.policy.acquires != 0 {
		t.Fatalf("invalid requests reached policy: dryRuns=%d acquires=%d", len(f.policy.dryRuns), f.policy.acquires)
	}
}

func TestStatusExtraWindowKinds(t *testing.T) {
	q := &fakeQuota{snaps: map[string]core.Snapshot{"claude-max": {
		AccountID: "claude-max", FetchedAt: t0,
		Windows: []core.Window{
			{Kind: core.Window5h, UsedFrac: 0.2, WindowSeconds: 18000},
			{Kind: core.WindowWeekly, UsedFrac: 0.4, WindowSeconds: 604800},
			{Kind: "weekly_fable", UsedFrac: 0.7, ResetAt: t0.Add(time.Hour), WindowSeconds: 604800},
		},
	}}}
	srv := New(Deps{
		Accounts: []core.Account{{ID: "claude-max", Provider: core.ProviderClaude}},
		Quota:    q, Policy: &fakePolicy{}, Clock: fakeClock{t0},
	}, Options{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/control/v1/status", nil))
	doc := decode(t, rec)
	a := doc["accounts"].([]any)[0].(map[string]any)
	if a["provider"] != "claude" {
		t.Errorf("provider = %v", a["provider"])
	}
	wins := a["windows"].([]any)
	if len(wins) != 3 {
		t.Fatalf("windows = %v", wins)
	}
	w := wins[2].(map[string]any)
	if w["kind"] != "weekly_fable" || w["used_frac"] != 0.7 || w["reset_at"] != "2026-10-01T13:00:00Z" {
		t.Errorf("extra window = %v", w)
	}
}

func TestRequireAuth(t *testing.T) {
	f := newFixture(true)
	paths := []struct{ method, path, body string }{
		{"GET", "/control/v1/status", ""},
		{"GET", "/control/v1/usage", ""},
		{"POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`},
	}
	for _, p := range paths {
		for _, key := range []string{"", "wrong"} {
			rec := f.do(t, p.method, p.path, p.body, key)
			if rec.Code != 401 {
				t.Errorf("%s %s key=%q: code %d", p.method, p.path, key, rec.Code)
			}
		}
		if rec := f.do(t, p.method, p.path, p.body, secretKey); rec.Code != 200 {
			t.Errorf("%s %s with key: code %d", p.method, p.path, rec.Code)
		}
	}
	if len(f.policy.dryRuns) != 1 {
		t.Errorf("unauthenticated admit reached policy: %d", len(f.policy.dryRuns))
	}
	// Open endpoints.
	for _, p := range []string{"/healthz", "/"} {
		if rec := f.do(t, "GET", p, "", ""); rec.Code != 200 {
			t.Errorf("%s: code %d", p, rec.Code)
		}
	}
	// Fail closed without an authenticator.
	f.srv.deps.Authenticate = nil
	if rec := f.do(t, "GET", "/control/v1/status", "", secretKey); rec.Code != 401 {
		t.Errorf("nil Authenticate: code %d", rec.Code)
	}
	// Open mode ignores missing keys.
	if rec := newFixture(false).do(t, "GET", "/control/v1/status", "", ""); rec.Code != 200 {
		t.Errorf("open mode: code %d", rec.Code)
	}
}

func TestWidget(t *testing.T) {
	f := newFixture(false)
	rec := f.do(t, "GET", "/", "", "")
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("csp = %q", csp)
	}
	body := rec.Body.String()
	if regexp.MustCompile(`(?i)https?://`).MatchString(body) || strings.Contains(body, `//cdn`) {
		t.Errorf("widget references external URLs")
	}
	if strings.Contains(body, "innerHTML") {
		t.Errorf("widget must not use innerHTML")
	}
	for _, want := range []string{"/control/v1/status", "/control/v1/usage?since=24h&group=account", "15000", "localStorage", "prefers-color-scheme",
		"quota only", "cache write", "cache_creation_input_tokens"} {
		if !strings.Contains(body, want) {
			t.Errorf("widget missing %q", want)
		}
	}
	if rec := f.do(t, "GET", "/nope", "", ""); rec.Code != 404 {
		t.Errorf("unknown path: %d", rec.Code)
	}
}
