package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
)

func TestAdminListUsersPaginated(t *testing.T) {
	f := newMUFixture(t, nil)
	root := f.addUser("root", "admin")
	alice := f.addUser("alice", "user")
	f.addUser("bob", "user")

	var all []userDoc
	cursor := ""
	for page := 0; page < 5; page++ {
		target := "/ui/v1/admin/users?limit=2"
		if cursor != "" {
			target += "&after=" + cursor
		}
		rec := f.session(http.MethodGet, target, root, "")
		wantStatus(t, rec, http.StatusOK)
		var doc struct {
			Users      []userDoc `json:"users"`
			NextCursor string    `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		all = append(all, doc.Users...)
		if doc.NextCursor == "" {
			break
		}
		cursor = doc.NextCursor
	}
	if len(all) != 3 {
		t.Fatalf("listed %d users: %+v", len(all), all)
	}
	// Non-admin sessions and user keys cannot list users.
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/admin/users", alice, ""), http.StatusForbidden)
	for _, bad := range []string{"limit=0", "limit=201", "limit=x", "after=" + strings.Repeat("a", 65)} {
		wantStatus(t, f.session(http.MethodGet, "/ui/v1/admin/users?"+bad, root, ""), http.StatusBadRequest)
	}
}

func TestAdminUserLifecycle(t *testing.T) {
	f := newMUFixture(t, nil)
	root := f.addUser("root", "admin")
	alice := f.addUser("alice", "user")

	// Non-admin cannot act.
	wantStatus(t, f.session(http.MethodPost, "/ui/v1/admin/users/"+root.ID+"/disable", alice, ""), http.StatusForbidden)
	// Admin cannot disable or delete themselves.
	for _, act := range []string{"disable", "delete"} {
		rec := f.session(http.MethodPost, "/ui/v1/admin/users/"+root.ID+"/"+act, root, "")
		wantStatus(t, rec, http.StatusConflict)
	}
	// Unknown user.
	wantStatus(t, f.session(http.MethodPost, "/ui/v1/admin/users/u_nobody/disable", root, ""), http.StatusNotFound)
	// CSRF is required.
	req := f.sessionReq(http.MethodPost, "/ui/v1/admin/users/"+alice.ID+"/disable", root, "")
	req.Header.Del(csrfHeader)
	wantStatus(t, f.serve(req), http.StatusForbidden)

	wantStatus(t, f.session(http.MethodPost, "/ui/v1/admin/users/"+alice.ID+"/disable", root, ""), http.StatusNoContent)
	wantStatus(t, f.bearerGet("/control/v1/usage", alice.Key), http.StatusUnauthorized)
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/me", alice, ""), http.StatusUnauthorized)
	u, _ := f.ids.User(context.Background(), alice.ID)
	if u.Status != identity.StatusDisabled {
		t.Fatalf("status = %s", u.Status)
	}
	wantStatus(t, f.session(http.MethodPost, "/ui/v1/admin/users/"+alice.ID+"/enable", root, ""), http.StatusNoContent)
	// Enabling never resurrects credentials.
	wantStatus(t, f.bearerGet("/control/v1/usage", alice.Key), http.StatusUnauthorized)

	wantStatus(t, f.session(http.MethodPost, "/ui/v1/admin/users/"+alice.ID+"/delete", root, ""), http.StatusNoContent)
	wantStatus(t, f.session(http.MethodPost, "/ui/v1/admin/users/"+alice.ID+"/delete", root, ""), http.StatusConflict)
	wantStatus(t, f.session(http.MethodPost, "/ui/v1/admin/users/"+alice.ID+"/disable", root, `{"x":1}`), http.StatusBadRequest)
}

func TestAdminUserKeys(t *testing.T) {
	f := newMUFixture(t, nil)
	root := f.addUser("root", "admin")
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")

	rec := f.session(http.MethodGet, "/ui/v1/admin/users/"+alice.ID+"/keys", root, "")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), alice.KeyID) || strings.Contains(rec.Body.String(), "lrk_") {
		t.Fatalf("admin key list = %s", rec.Body.String())
	}
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/admin/users/u_nobody/keys", root, ""), http.StatusNotFound)
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/admin/users/"+bob.ID+"/keys", alice, ""), http.StatusForbidden)

	// Key must belong to the named user.
	wantStatus(t, f.session(http.MethodDelete, "/ui/v1/admin/users/"+bob.ID+"/keys/"+alice.KeyID, root, ""), http.StatusNotFound)
	wantStatus(t, f.session(http.MethodDelete, "/ui/v1/admin/users/"+alice.ID+"/keys/"+alice.KeyID, root, ""), http.StatusNoContent)
	wantStatus(t, f.bearerGet("/control/v1/usage", alice.Key), http.StatusUnauthorized)
	wantStatus(t, f.bearerGet("/control/v1/usage", bob.Key), http.StatusOK)
}

func TestAdminAudit(t *testing.T) {
	f := newMUFixture(t, nil)
	root := f.addUser("root", "admin")
	alice := f.addUser("alice", "user")
	wantStatus(t, f.session(http.MethodPost, "/ui/v1/admin/users/"+alice.ID+"/disable", root, ""), http.StatusNoContent)

	rec := f.session(http.MethodGet, "/ui/v1/admin/audit?limit=200", root, "")
	wantStatus(t, rec, http.StatusOK)
	var doc struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || len(doc.Events) == 0 {
		t.Fatalf("audit = %s", rec.Body.String())
	}
	found := false
	for _, e := range doc.Events {
		if e["action"] == identity.AuditUserDisabled && e["actor_kind"] == "admin" && e["actor_user_id"] == root.ID &&
			e["target_user_id"] == alice.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("disable not audited: %s", rec.Body.String())
	}
	for _, pii := range []string{"alice@example.test", "Name alice", muIssuer, "lrs_", "lrk_"} {
		if strings.Contains(rec.Body.String(), pii) {
			t.Fatalf("audit leaked %q", pii)
		}
	}
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/admin/audit", alice, ""), http.StatusUnauthorized) // disabled
	bob := f.addUser("bob", "user")
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/admin/audit", bob, ""), http.StatusForbidden)
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/admin/audit?after=-1", root, ""), http.StatusBadRequest)
}

func TestAdminGlobalReads(t *testing.T) {
	f := newMUFixture(t, nil)
	root := f.addUser("root", "admin")
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")
	seedTwoOwners(f, alice, bob)

	rec := f.session(http.MethodGet, "/ui/v1/admin/usage?group=user", root, "")
	wantStatus(t, rec, http.StatusOK)
	if got := usageKeys(t, rec); got[alice.ID] != 2 || got[bob.ID] != 1 || got[""] != 1 {
		t.Fatalf("admin global usage = %v", got)
	}
	rec = f.session(http.MethodGet, "/ui/v1/admin/analytics?range=24h&group=user", root, "")
	wantStatus(t, rec, http.StatusOK)
	if res := decodeAnalytics(t, rec.Body.Bytes()); res.Totals.Requests != 4 {
		t.Fatalf("admin analytics totals = %+v", res.Totals)
	}
	for _, p := range []string{"/ui/v1/admin/status", "/ui/v1/admin/diagnostics", "/ui/v1/admin/analytics/dimensions?range=24h",
		"/ui/v1/admin/budgets?scope=account&key=primary"} {
		wantStatus(t, f.session(http.MethodGet, p, root, ""), http.StatusOK)
		wantStatus(t, f.session(http.MethodGet, p, alice, ""), http.StatusForbidden)
	}
}

func TestAdminRoleIsReadFresh(t *testing.T) {
	f := newMUFixture(t, nil)
	root := f.addUser("root", "admin")
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/admin/users", root, ""), http.StatusOK)
	// The next allowed login maps the subject to plain user: the existing
	// session loses admin powers on its next request.
	if _, err := f.ids.ResolveLogin(context.Background(), identity.Login{Issuer: muIssuer, Subject: "root",
		Role: core.RoleUser, AuthTime: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/admin/users", root, ""), http.StatusForbidden)
	wantStatus(t, f.session(http.MethodPost, "/ui/v1/admin/users/"+root.ID+"/enable", root, ""), http.StatusForbidden)
	rec := f.session(http.MethodGet, "/ui/v1/me", root, "")
	if !strings.Contains(rec.Body.String(), `"role":"user"`) {
		t.Fatalf("me role = %s", rec.Body.String())
	}
}

func TestAdminPowersAreSessionOnly(t *testing.T) {
	f := newMUFixture(t, nil)
	root := f.addUser("root", "admin")
	// An admin's API key is a plain user key: no global bearer views.
	for _, p := range []string{"/control/v1/status", "/control/v1/diagnostics", "/control/v1/budgets?scope=account&key=primary"} {
		wantStatus(t, f.bearerGet(p, root.Key), http.StatusForbidden)
	}
	wantStatus(t, f.bearerGet("/control/v1/usage?group=user", root.Key), http.StatusBadRequest)
	// And admin session routes refuse the bearer.
	req := f.sessionReq(http.MethodGet, "/ui/v1/admin/users", muUser{}, "")
	req.Header.Set("Authorization", "Bearer "+root.Key)
	wantStatus(t, f.serve(req), http.StatusBadRequest)
}
