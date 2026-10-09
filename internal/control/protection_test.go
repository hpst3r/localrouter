package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// fakePinger is an injectable storage-health probe.
type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

// fakeInflight is an injectable concurrency snapshot.
type fakeInflight struct{ st core.InflightStats }

func (f fakeInflight) InflightStats() core.InflightStats { return f.st }

// /readyz is unauthenticated, reports only readiness, and exposes no account
// or credential data.
func TestReadyzIsMinimal(t *testing.T) {
	f := newFixture(true)
	rec := f.do(t, http.MethodGet, "/readyz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.TrimSpace(body) != `{"ready":true}` {
		t.Fatalf("body %q", body)
	}
}

// A failed local storage probe makes the router not-ready, and shutdown
// (which fails the probe) does the same.
func TestReadyzNotReadyOnStorageFailure(t *testing.T) {
	f := newFixture(true)
	f.srv.deps.Storage = fakePinger{err: errors.New("database is closed")}
	rec := f.do(t, http.MethodGet, "/readyz", "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"ready":false}` {
		t.Fatalf("body %q", rec.Body)
	}
}

// /readyz is a GET liveness/readiness probe only.
func TestReadyzMethodRestriction(t *testing.T) {
	f := newFixture(true)
	if rec := f.do(t, http.MethodPost, "/readyz", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /readyz status %d", rec.Code)
	}
	if rec := f.do(t, http.MethodPost, "/healthz", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status %d", rec.Code)
	}
}

// Diagnostics must be behind the same auth as the rest of the control API.
func TestDiagnosticsAuth(t *testing.T) {
	f := newFixture(true)
	if rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status %d", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", secretKey); rec.Code != http.StatusOK {
		t.Fatalf("valid key: status %d", rec.Code)
	}
}

// Diagnostics reports capacity, storage health and per-account reasons; it
// must not invent health it cannot observe, and must leak nothing sensitive.
func TestDiagnosticsShape(t *testing.T) {
	f := newFixture(false)
	f.srv.deps.Storage = fakePinger{}
	f.srv.deps.Inflight = fakeInflight{st: core.InflightStats{
		GlobalLimit: 8, GlobalActive: 2, GlobalPeak: 3,
		Clients: []core.ClientInflight{{Name: "tester", Limit: 4, Active: 2}},
	}}
	rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	doc := decode(t, rec)
	if doc["schema_version"] != float64(SchemaVersion) || doc["ready"] != true {
		t.Fatalf("doc %v", doc)
	}
	inflight, _ := doc["inflight"].(map[string]any)
	if inflight["global_limit"] != float64(8) || inflight["global_active"] != float64(2) {
		t.Fatalf("inflight %v", inflight)
	}
	storage, _ := doc["storage"].(map[string]any)
	if storage["configured"] != true || storage["ok"] != true {
		t.Fatalf("storage %v", storage)
	}
	accts, _ := doc["accounts"].([]any)
	if len(accts) != 4 {
		t.Fatalf("accounts %v", accts)
	}
	first, _ := accts[0].(map[string]any)
	if first["id"] != "primary" {
		t.Fatalf("first account %v", first)
	}
	if _, leaked := first["windows"]; leaked {
		t.Fatal("diagnostics must not expose per-account quota windows")
	}
	body := rec.Body.String()
	for _, bad := range []string{secretKey, "/home/", "sk-", "key_file"} {
		if strings.Contains(body, bad) {
			t.Fatalf("diagnostics leaked %q: %s", bad, body)
		}
	}
}

// With no storage probe configured, diagnostics reports it as unconfigured
// rather than claiming it is healthy.
func TestDiagnosticsStorageUnconfigured(t *testing.T) {
	f := newFixture(false)
	f.srv.deps.Storage = nil
	rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	doc := decode(t, rec)
	storage, _ := doc["storage"].(map[string]any)
	if storage["configured"] != false || storage["ok"] != false {
		t.Fatalf("unconfigured storage %v", storage)
	}
}

var _ = json.Marshal
