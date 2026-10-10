package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
)

// The profile and the admin user list carry the time of the user's last
// allowed login (null while there is none), which the UI shows.
func TestSessionUserDocLastLoginAt(t *testing.T) {
	f := newMUFixture(t, nil)
	root := f.addUser("root", core.RoleAdmin)
	alice := f.addUser("alice", core.RoleUser)

	f.clock.Advance(2 * time.Hour)
	later := f.clock.Now()
	if _, err := f.ids.ResolveLogin(context.Background(), identity.Login{Issuer: muIssuer, Subject: "alice",
		Role: core.RoleUser, AuthTime: later}); err != nil {
		t.Fatal(err)
	}
	rec := f.session(http.MethodGet, "/ui/v1/me", alice, "")
	wantStatus(t, rec, http.StatusOK)
	if u, _ := decodeMap(t, rec)["user"].(map[string]any); u["last_login_at"] != later.Format(time.RFC3339) {
		t.Fatalf("me last_login_at = %v, want %s", u["last_login_at"], later.Format(time.RFC3339))
	}

	wantStatus(t, f.session(http.MethodPost, "/ui/v1/admin/users/"+alice.ID+"/disable", root, ""), http.StatusNoContent)
	rec = f.session(http.MethodGet, "/ui/v1/admin/users", root, "")
	wantStatus(t, rec, http.StatusOK)
	var doc struct {
		Users []map[string]json.RawMessage `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, u := range doc.Users {
		var id string
		_ = json.Unmarshal(u["id"], &id)
		v, ok := u["last_login_at"]
		if !ok {
			t.Fatalf("admin user %s has no last_login_at: %s", id, rec.Body.String())
		}
		got[id] = string(v)
	}
	if got[root.ID] != `"`+t0.Format(time.RFC3339)+`"` || got[alice.ID] != "null" {
		t.Fatalf("admin list last_login_at = %v (root %s, disabled alice %s)", got, root.ID, alice.ID)
	}
}
