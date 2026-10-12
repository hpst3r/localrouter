package control

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// seedTwoOwners writes rows for alice (two keys), bob and one legacy unowned
// row, all with distinct client/model labels so a leak is visible.
func seedTwoOwners(f *muFixture, alice, bob muUser) {
	f.record("a1", "primary", "gpt-alice", alice.KeyID, alice.ID, alice.KeyID, 100)
	f.record("a2", "primary", "gpt-alice", "static-alice", alice.ID, "", 50)
	f.record("b1", "primary", "gpt-bob", bob.KeyID, bob.ID, bob.KeyID, 7000)
	f.record("l1", "primary", "gpt-legacy", "legacy-client", "", "", 900)
}

func TestMultiUserUsageUserKeySeesOnlyOwnRows(t *testing.T) {
	f := newMUFixture(t, nil)
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")
	seedTwoOwners(f, alice, bob)

	rec := f.bearerGet("/control/v1/usage?group=client", alice.Key)
	wantStatus(t, rec, http.StatusOK)
	got := usageKeys(t, rec)
	if len(got) != 2 || got[alice.KeyID] != 1 || got["static-alice"] != 1 {
		t.Fatalf("alice usage rows = %v, want only her two clients", got)
	}
}

func TestMultiUserUsageServiceSeesAllAndUserGroupIsGlobalOnly(t *testing.T) {
	f := newMUFixture(t, nil)
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")
	seedTwoOwners(f, alice, bob)

	rec := f.bearerGet("/control/v1/usage?group=user", serviceToken)
	wantStatus(t, rec, http.StatusOK)
	got := usageKeys(t, rec)
	if got[alice.ID] != 2 || got[bob.ID] != 1 || got[""] != 1 || len(got) != 3 {
		t.Fatalf("service group=user rows = %v", got)
	}

	rec = f.bearerGet("/control/v1/usage?group=user", alice.Key)
	wantStatus(t, rec, http.StatusBadRequest)

	rec = f.bearerGet("/control/v1/usage?group=key", alice.Key)
	wantStatus(t, rec, http.StatusOK)
	if got := usageKeys(t, rec); len(got) != 2 || got[alice.KeyID] != 1 || got[""] != 1 {
		t.Fatalf("alice group=key rows = %v", got)
	}
}

func TestMultiUserBearerAuthFailures(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")

	// No bearer, unknown bearer, malformed lrk_ token: 401 with no reflection.
	for _, tok := range []string{"", "nope", "lrk_notavalidtoken"} {
		rec := f.bearerGet("/control/v1/usage", tok)
		wantStatus(t, rec, http.StatusUnauthorized)
		if tok != "" && strings.Contains(rec.Body.String(), tok) {
			t.Fatalf("token reflected: %s", rec.Body.String())
		}
	}
	// A valid session cookie is never a bearer credential.
	req := httptest.NewRequest(http.MethodGet, "/control/v1/usage", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: alice.Session})
	wantStatus(t, f.serve(req), http.StatusUnauthorized)

	// Backend failure: 503, backend text never echoed.
	f.setAuthErr(fmt.Errorf("%w: %w", core.ErrAuthUnavailable, errDBDown))
	rec := f.bearerGet("/control/v1/usage", alice.Key)
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if strings.Contains(rec.Body.String(), "sqlite") || strings.Contains(rec.Body.String(), "/var/lib") {
		t.Fatalf("backend error leaked: %s", rec.Body.String())
	}
	// Any unclassified error also fails closed.
	f.setAuthErr(errors.New("weird"))
	wantStatus(t, f.bearerGet("/control/v1/status", serviceToken), http.StatusServiceUnavailable)
}

func TestMultiUserMissingSeamFailsClosed(t *testing.T) {
	f := newMUFixture(t, func(d *Deps) { d.AuthenticatePrincipal = nil })
	for _, path := range []string{"/control/v1/status", "/control/v1/usage", "/control/v1/analytics",
		"/control/v1/analytics/dimensions", "/control/v1/diagnostics", "/control/v1/budgets"} {
		rec := f.bearerGet(path, serviceToken)
		wantStatus(t, rec, http.StatusServiceUnavailable)
	}
	for _, path := range []string{"/control/v1/admit", "/control/v1/ingest"} {
		wantStatus(t, f.bearerPost(path, ingestServiceToken, `{}`), http.StatusServiceUnavailable)
	}
}

func TestMultiUserMultiUserFlagInDepsAlsoEnables(t *testing.T) {
	f := newMUFixture(t, nil)
	srv := New(Deps{MultiUser: true, Authenticate: func(string) (core.Client, bool) {
		return core.Client{Name: "x", Ingest: true}, true
	}}, Options{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/control/v1/status", nil)
	req.Header.Set("Authorization", "Bearer anything")
	srv.Handler().ServeHTTP(rec, req)
	wantStatus(t, rec, http.StatusServiceUnavailable)
	_ = f
}

func TestMultiUserUserBearerForbiddenGlobalViews(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")
	f.addStaticUser(aliceStaticToken, alice)
	for _, tok := range []string{alice.Key, aliceStaticToken} {
		for _, path := range []string{"/control/v1/status", "/control/v1/diagnostics",
			"/control/v1/budgets?scope=account&key=primary"} {
			rec := f.bearerGet(path, tok)
			wantStatus(t, rec, http.StatusForbidden)
			if strings.Contains(rec.Body.String(), "primary") {
				t.Fatalf("%s leaked account data: %s", path, rec.Body.String())
			}
		}
		wantStatus(t, f.bearerPost("/control/v1/ingest", tok, `{"schema_version":1,"host":"h"}`), http.StatusForbidden)
	}
	// Service may read the global views.
	for _, path := range []string{"/control/v1/status", "/control/v1/diagnostics"} {
		wantStatus(t, f.bearerGet(path, serviceToken), http.StatusOK)
	}
}

func TestMultiUserLegacyWidgetRedirects(t *testing.T) {
	f := newMUFixture(t, nil)
	rec := f.bearerGet("/", "")
	wantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); loc != "/ui/" {
		t.Fatalf("Location = %q", loc)
	}
	if strings.Contains(rec.Body.String(), "localStorage") {
		t.Fatal("legacy widget served in multi-user mode")
	}
	wantStatus(t, f.bearerGet("/healthz", ""), http.StatusOK)
	wantStatus(t, f.bearerGet("/readyz", ""), http.StatusOK)
}
