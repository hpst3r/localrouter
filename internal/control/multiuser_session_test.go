package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/identity"
)

func TestSessionMe(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")

	rec := f.session(http.MethodGet, "/ui/v1/me", alice, "")
	wantStatus(t, rec, http.StatusOK)
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	m := decodeMap(t, rec)
	u, _ := m["user"].(map[string]any)
	want := map[string]any{"id": alice.ID, "role": "user", "status": "active",
		"display_name": "Name alice", "email": "alice@example.test", "last_login_at": t0.Format(time.RFC3339)}
	if len(u) != len(want) {
		t.Fatalf("user = %v", u)
	}
	for k, v := range want {
		if u[k] != v {
			t.Fatalf("user[%s] = %v, want %v", k, u[k], v)
		}
	}
	if m["csrf_token"] != identity.CSRFToken(alice.Session) || len(m) != 2 {
		t.Fatalf("me doc = %v", m)
	}
}

func TestSessionOwnKeysCRUD(t *testing.T) {
	f := newMUFixture(t, nil)
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")

	rec := f.session(http.MethodPost, "/ui/v1/me/keys", alice, `{"name":"ci runner","ttl_seconds":7200}`)
	wantStatus(t, rec, http.StatusCreated)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("token response cacheable")
	}
	m := decodeMap(t, rec)
	tok, _ := m["token"].(string)
	if !identity.IsUserKeyToken(tok) || len(m) != 2 {
		t.Fatalf("create doc = %v", m)
	}
	key, _ := m["key"].(map[string]any)
	if key["name"] != "ci runner" || key["revoked_at"] != nil || key["expires_at"] != t0.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("key = %v", key)
	}
	for _, field := range []string{"id", "name", "created_at", "expires_at", "revoked_at"} {
		if _, ok := key[field]; !ok {
			t.Fatalf("key missing %s: %v", field, key)
		}
	}
	if len(key) != 5 {
		t.Fatalf("key has extra fields: %v", key)
	}
	newID := key["id"].(string)
	// The new token authenticates the bearer API immediately.
	wantStatus(t, f.bearerGet("/control/v1/usage", tok), http.StatusOK)

	rec = f.session(http.MethodGet, "/ui/v1/me/keys", alice, "")
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "lrk_") {
		t.Fatal("key list leaked a token")
	}
	var list struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Keys) != 2 {
		t.Fatalf("list = %s (%v)", rec.Body.String(), err)
	}

	// Bob cannot revoke or see alice's key: 404, no oracle.
	wantStatus(t, f.session(http.MethodDelete, "/ui/v1/me/keys/"+newID, bob, ""), http.StatusNotFound)
	rec = f.session(http.MethodGet, "/ui/v1/me/keys", bob, "")
	if strings.Contains(rec.Body.String(), newID) || strings.Contains(rec.Body.String(), alice.KeyID) {
		t.Fatal("bob sees alice's key ids")
	}
	wantStatus(t, f.session(http.MethodDelete, "/ui/v1/me/keys/k_doesnotexist", alice, ""), http.StatusNotFound)

	wantStatus(t, f.session(http.MethodDelete, "/ui/v1/me/keys/"+newID, alice, ""), http.StatusNoContent)
	wantStatus(t, f.bearerGet("/control/v1/usage", tok), http.StatusUnauthorized)
}
