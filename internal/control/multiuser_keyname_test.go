package control

import (
	"net/http"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// A self-service API key name containing Unicode format characters (bidi
// overrides and isolates, zero-width characters, BOM, soft hyphen) is
// refused like any other invalid name, so a key cannot be named to read as
// something else in a key list or an admin revoke dialog. The refusal does
// not echo the name and creates no key.
func TestSessionCreateKeyRejectsFormatCharacters(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", core.RoleUser)

	before := f.session(http.MethodGet, "/ui/v1/me/keys", alice, "")
	wantStatus(t, before, http.StatusOK)
	invalid := f.session(http.MethodPost, "/ui/v1/me/keys", alice, `{"name":""}`)
	wantStatus(t, invalid, http.StatusBadRequest)

	for _, name := range []string{`ci\u202egnp.exe`, `\u2066ci\u2069`, `c\u200bi`, `ci\u200e`, `\ufeffci`, `c\u00adi`, `ci\u2060`, `ci\u061c`} {
		rec := f.session(http.MethodPost, "/ui/v1/me/keys", alice, `{"name":"`+name+`"}`)
		if rec.Code != http.StatusBadRequest || rec.Body.String() != invalid.Body.String() {
			t.Errorf("name %s: %d %s, want the invalid-name 400 %s", name, rec.Code, rec.Body.String(), invalid.Body.String())
		}
	}
	rec := f.session(http.MethodGet, "/ui/v1/me/keys", alice, "")
	wantStatus(t, rec, http.StatusOK)
	if rec.Body.String() != before.Body.String() {
		t.Fatalf("a key was created: %s", rec.Body.String())
	}
	wantStatus(t, f.session(http.MethodPost, "/ui/v1/me/keys", alice, `{"name":"ci runner \u00e9"}`), http.StatusCreated)
}
