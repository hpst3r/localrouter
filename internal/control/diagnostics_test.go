package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// countPinger counts probes so tests can prove readiness/diagnostics perform a
// single bounded storage probe per request.
type countPinger struct {
	n   int
	err error
}

func (p *countPinger) Ping(context.Context) error { p.n++; return p.err }

// Readiness requires a wired storage probe: without Storage the instance must
// report not-ready rather than claiming health it cannot observe.
func TestReadyzNotReadyWithoutStorage(t *testing.T) {
	f := newFixture(true)
	f.srv.deps.Storage = nil
	rec := f.do(t, http.MethodGet, "/readyz", "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil storage: status %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"ready":false}` {
		t.Fatalf("nil storage body %q", rec.Body)
	}
}

// A shutdown predicate (Ready returning false) flips readiness without any
// storage failure, so /readyz reflects process shutdown.
func TestReadyzNotReadyOnShutdown(t *testing.T) {
	f := newFixture(true)
	f.srv.deps.Ready = func() bool { return false }
	rec := f.do(t, http.MethodGet, "/readyz", "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("shutdown: status %d", rec.Code)
	}
	// Nil Ready defaults to ready.
	f.srv.deps.Ready = nil
	if rec := f.do(t, http.MethodGet, "/readyz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("nil Ready: status %d", rec.Code)
	}
}

// Readiness and diagnostics must each perform exactly one bounded storage
// probe per request, so a hung or private database is never probed twice.
func TestReadinessSingleStorageProbe(t *testing.T) {
	f := newFixture(false) // diagnostics needs auth otherwise; open here
	p := &countPinger{}
	f.srv.deps.Storage = p
	f.do(t, http.MethodGet, "/readyz", "", "")
	if p.n != 1 {
		t.Fatalf("/readyz probes = %d, want 1", p.n)
	}
	p.n = 0
	f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	if p.n != 1 {
		t.Fatalf("diagnostics probes = %d, want 1", p.n)
	}
}

// Storage errors may embed filesystem secret paths or credentials; the
// diagnostics document must expose only a generic, pre-defined reason.
func TestDiagnosticsStorageErrorSanitized(t *testing.T) {
	f := newFixture(false)
	f.srv.deps.Storage = fakePinger{err: errors.New("open /home/wporter/.secrets/db.key: permission denied (sk-live-DEADBEEF)")}
	rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	doc := decode(t, rec)
	storage, _ := doc["storage"].(map[string]any)
	if storage["configured"] != true || storage["ok"] != false {
		t.Fatalf("storage %v", storage)
	}
	if storage["error"] != storageErrorUnavailable {
		t.Fatalf("storage error = %v, want generic %q", storage["error"], storageErrorUnavailable)
	}
	body := rec.Body.String()
	for _, bad := range []string{"/home/", "sk-", ".key", "permission denied"} {
		if strings.Contains(body, bad) {
			t.Fatalf("storage error leaked %q: %s", bad, body)
		}
	}
	// A deadline is reported as a distinct, still-generic timeout reason.
	f.srv.deps.Storage = fakePinger{err: context.DeadlineExceeded}
	doc = decode(t, f.do(t, http.MethodGet, "/control/v1/diagnostics", "", ""))
	storage, _ = doc["storage"].(map[string]any)
	if storage["error"] != "storage probe timed out" {
		t.Fatalf("deadline error = %v", storage["error"])
	}
}

// A raw quota snapshot error (which can carry URLs, keys or paths) must never
// be echoed as the account reason.
func TestDiagnosticsSnapshotErrorSanitized(t *testing.T) {
	f := newFixture(false)
	q := &fakeQuota{snaps: map[string]core.Snapshot{
		"none": {AccountID: "none", Err: "usage api: 401 https://api.example.com/v1?key=sk-live-LEAK"},
	}}
	f.srv.deps.Quota = q
	rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	doc := decode(t, rec)
	accts, _ := doc["accounts"].([]any)
	var none map[string]any
	for _, a := range accts {
		m := a.(map[string]any)
		if m["id"] == "none" {
			none = m
		}
	}
	if none == nil {
		t.Fatal("account \"none\" missing")
	}
	if none["reason"] != snapshotErrorUnavailable {
		t.Fatalf("reason = %v, want generic %q", none["reason"], snapshotErrorUnavailable)
	}
	body := rec.Body.String()
	for _, bad := range []string{"api.example.com", "sk-live", "401"} {
		if strings.Contains(body, bad) {
			t.Fatalf("snapshot error leaked %q: %s", bad, body)
		}
	}
}

// An account is unhealthy when it is missing a snapshot, stale beyond the
// freshness window, cooling down, or policy-inadmissible for interactive work.
func TestDiagnosticsUnhealthyAccounts(t *testing.T) {
	pol := &fakePolicy{states: map[string]core.AccountState{
		"missing": {InteractiveAdmissible: true},
		"stale":   {InteractiveAdmissible: true, Stale: true},
		"exhausted": {
			InteractiveAdmissible: false, BackgroundAdmissible: false,
			Reason: "weekly reserve exhausted",
		},
		"cooling": {InteractiveAdmissible: true, CooldownUntil: t0.Add(time.Minute)},
		"fresh":   {InteractiveAdmissible: true},
	}}
	q := &fakeQuota{snaps: map[string]core.Snapshot{
		"stale":   {AccountID: "stale", FetchedAt: t0.Add(-30 * time.Minute)},
		"cooling": {AccountID: "cooling", FetchedAt: t0.Add(-time.Minute)},
		"fresh":   {AccountID: "fresh", FetchedAt: t0.Add(-time.Minute)},
	}}
	srv := New(Deps{
		Accounts: []core.Account{
			{ID: "missing", Provider: core.ProviderOllama},
			{ID: "stale", Provider: core.ProviderOllama},
			{ID: "exhausted", Provider: core.ProviderOllama},
			{ID: "cooling", Provider: core.ProviderOllama},
			{ID: "fresh", Provider: core.ProviderOllama},
		},
		Quota: q, Policy: pol, Storage: fakePinger{}, Clock: fakeClock{t0},
	}, Options{StaleAfter: 10 * time.Minute})

	rec := serve(t, srv, http.MethodGet, "/control/v1/diagnostics")
	doc := decode(t, rec)
	byID := map[string]map[string]any{}
	for _, a := range doc["accounts"].([]any) {
		m := a.(map[string]any)
		byID[m["id"].(string)] = m
	}
	for _, id := range []string{"missing", "stale", "exhausted", "cooling"} {
		if byID[id]["healthy"] != false {
			t.Errorf("%s healthy = %v, want false (%v)", id, byID[id]["healthy"], byID[id])
		}
	}
	if byID["missing"]["stale"] != true {
		t.Errorf("missing must be stale without a snapshot: %v", byID["missing"])
	}
	if byID["stale"]["stale"] != true || byID["stale"]["snapshot_age_s"] != float64(1800) {
		t.Errorf("stale account = %v", byID["stale"])
	}
	if byID["exhausted"]["reason"] != "weekly reserve exhausted" {
		t.Errorf("exhausted reason = %v", byID["exhausted"]["reason"])
	}
	if byID["cooling"]["cooldown"] != true {
		t.Errorf("cooling account = %v", byID["cooling"])
	}
	if byID["fresh"]["healthy"] != true || byID["fresh"]["stale"] != false {
		t.Errorf("fresh account = %v", byID["fresh"])
	}
	// A never-fetched snapshot reports no age (nullable).
	if _, ok := byID["missing"]["snapshot_age_s"]; !ok {
		t.Errorf("snapshot_age_s key absent: %v", byID["missing"])
	} else if byID["missing"]["snapshot_age_s"] != nil {
		t.Errorf("missing snapshot_age_s = %v, want null", byID["missing"]["snapshot_age_s"])
	}
}

// serve runs one request against a server built outside newFixture.
func serve(t *testing.T, srv *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}
