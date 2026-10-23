package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/policy"
)

// Pooled-backend control surface (docs/SPEC.md "Pooled backends").

func poolControl(t *testing.T, br policy.BreakerOptions) (*fixture, *policy.Policy, core.Route, *time.Time) {
	t.Helper()
	accts := []core.Account{
		{ID: "a", Provider: core.ProviderOpenAICompat, BaseURL: "https://gpu1.lab.example/v1"},
		{ID: "b", Provider: core.ProviderOpenAICompat, BaseURL: "https://gpu2.lab.example/v1"},
		{ID: "c", Provider: core.ProviderOpenAICompat, BaseURL: "https://c.example/v1"},
	}
	now := t0
	clk := stepClock{t: &now}
	q := &fakeQuota{snaps: map[string]core.Snapshot{}}
	pol := policy.New(accts, q, policy.Options{Clock: clk, Breaker: br})
	pool := core.Route{Name: "qwen-local", Models: []string{"qwen"}, Interactive: []string{"a", "b"}, Background: []string{"b", "a"},
		BalanceStrategy: "least_inflight", BalanceIdentity: "Qwen3.5-9B Q4_K_M", BalanceStickyTTL: 15 * time.Minute,
		BalanceWeight: map[string]int{"a": 2, "b": 1}, BalanceMaxInflight: map[string]int{"a": 1, "b": 0}}
	srv := New(Deps{Accounts: accts, Quota: q, Policy: pol, Ledger: &fakeLedger{}, Storage: fakePinger{}, Clock: clk,
		Routes: []core.Route{pool, {Name: "plain", Models: []string{"plain"}, Interactive: []string{"c"}}}},
		Options{StaleAfter: 10 * time.Minute})
	f := &fixture{srv: srv}
	f.h = srv.Handler()
	return f, pol, pool, &now
}

func admitWithSession(t *testing.T, f *fixture, body, session string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/control/v1/admit", strings.NewReader(body))
	if session != "" {
		req.Header.Set("X-LocalRouter-Session", session)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admit: %d %s", rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestP12AdmitPooledDryRun: the admit dry run on a pooled route reports the
// member AcquireBalanced would pick and creates no lease, no affinity entry
// and no breaker probe.
func TestP12AdmitPooledDryRun(t *testing.T) {
	f, pol, pool, now := poolControl(t, policy.BreakerOptions{Threshold: 1, Base: 5 * time.Second})
	held, _, _, _, _ := pol.AcquireBalanced(core.ClassInteractive, []string{"a"}, nil, pool, "")
	m := admitWithSession(t, f, `{"class":"interactive","model":"qwen"}`, "S")
	if m["decision"] != "allow" || m["account_id"] != "b" || m["reason"] != "admitted b (pool least_inflight; skipped a: saturated (1/1 in flight))" {
		t.Fatalf("admit = %v", m)
	}
	held.Release(core.Outcome{Status: 200})
	if pol.Status("b").Inflight != 0 || pol.AffinityEntries("") != 0 {
		t.Fatal("dry run leased or bound")
	}
	// Background lists b first, but on an idle pool a's weight 2 wins.
	if m := admitWithSession(t, f, `{"class":"background","model":"qwen"}`, ""); m["account_id"] != "a" {
		t.Fatalf("background admit = %v", m)
	}
	// A live binding is reported as sticky.
	l, _, _, _, _ := pol.AcquireBalanced(core.ClassInteractive, []string{"b"}, nil, pool, "S")
	l.Release(core.Outcome{Status: 200})
	if m := admitWithSession(t, f, `{"class":"interactive","model":"qwen"}`, "S"); m["account_id"] != "b" || !strings.Contains(m["reason"].(string), "pool sticky") {
		t.Fatalf("sticky admit = %v", m)
	}
	// Half-open member: the dry run names it but never claims the probe.
	l, _, _, _, _ = pol.AcquireBalanced(core.ClassInteractive, []string{"a"}, nil, pool, "")
	l.Release(core.Outcome{Unreachable: "dial"})
	*now = now.Add(5 * time.Second)
	if m := admitWithSession(t, f, `{"class":"interactive","model":"qwen"}`, ""); m["account_id"] != "a" {
		t.Fatalf("half-open admit = %v", m)
	}
	if st, _, _, _ := pol.Breaker("a"); st != policy.BreakerHalfOpen {
		t.Fatalf("breaker = %s", st)
	}
	if l, _, _, _, _ := pol.AcquireBalanced(core.ClassInteractive, []string{"a"}, nil, pool, ""); l == nil {
		t.Fatal("dry run claimed the probe")
	} else {
		l.Release(core.Outcome{Status: 200})
	}
	// An unpooled route keeps the plain DryRun reason.
	if m := admitWithSession(t, f, `{"class":"interactive","model":"plain"}`, ""); m["reason"] != "admitted c" {
		t.Fatalf("plain admit = %v", m)
	}
}

// TestP19DiagnosticsPools: the pools block lists each pooled route with its
// members, counters and breaker state, and never a URL or key.
func TestP19DiagnosticsPools(t *testing.T) {
	f, pol, pool, _ := poolControl(t, policy.BreakerOptions{Threshold: 1})
	l, _, _, _, _ := pol.AcquireBalanced(core.ClassInteractive, []string{"a", "b"}, nil, pool, "S")
	defer l.Release(core.Outcome{Status: 200})
	rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	body := rec.Body.String()
	var doc struct {
		Pools []json.RawMessage `json:"pools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || len(doc.Pools) != 1 {
		t.Fatalf("pools: %v %s", err, body)
	}
	want := `{"route":"qwen-local","strategy":"least_inflight","identity":"Qwen3.5-9B Q4_K_M","sticky_ttl_s":900,"sticky_entries":1,` +
		`"members":[{"account":"a","weight":2,"max_inflight":1,"inflight":1,"breaker":"closed"},{"account":"b","weight":1,"max_inflight":0,"inflight":0,"breaker":"closed"}]}`
	if string(doc.Pools[0]) != want {
		t.Fatalf("pool =\n%s\nwant\n%s", doc.Pools[0], want)
	}
	for _, bad := range []string{"http", "lab.example", "key"} {
		if strings.Contains(string(doc.Pools[0]), bad) {
			t.Fatalf("pools leaks %q", bad)
		}
	}
}

func TestP19DiagnosticsWithoutPoolsOmitsBlock(t *testing.T) {
	f := newFixture(false)
	rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	if strings.Contains(rec.Body.String(), `"pools"`) {
		t.Fatalf("pools block without a pooled route: %s", rec.Body)
	}
}
