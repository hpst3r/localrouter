package control

import (
	"net/http"
	"strings"
	"testing"
)

// encoding/json matches member names case-insensitively and lets the last
// duplicate win; the session API accepts exact-case, unique members only, so
// a body means the same thing to every parser that inspects it.
func TestSessionJSONExactCaseAndUniqueMembers(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")
	for name, body := range map[string]string{
		"upper-case name":       `{"NAME":"SENTINEL-1"}`,
		"title-case ttl":        `{"name":"SENTINEL-2","Ttl_Seconds":3600}`,
		"duplicate name":        `{"name":"SENTINEL-3","name":"second"}`,
		"duplicate ttl":         `{"name":"SENTINEL-4","ttl_seconds":3600,"ttl_seconds":86400}`,
		"escaped duplicate":     `{"name":"SENTINEL-5","name":"second"}`,
		"nested duplicate":      `{"name":{"a":"SENTINEL-6","a":"b"}}`,
		"case variant after ok": `{"name":"SENTINEL-7","ttl_seconds":3600,"TTL_SECONDS":60}`,
	} {
		rec := f.session(http.MethodPost, "/ui/v1/me/keys", alice, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400; body %s", name, rec.Code, rec.Body.String())
		}
		if b := rec.Body.String(); strings.Contains(b, "SENTINEL") || strings.Contains(strings.ToLower(b), "ttl_seconds\"") {
			t.Errorf("%s: refusal echoes the request: %s", name, b)
		}
	}
	rec := f.session(http.MethodGet, "/ui/v1/me/keys", alice, "")
	if n := strings.Count(rec.Body.String(), `"id"`); n != 1 {
		t.Fatalf("rejected bodies created keys (%d keys): %s", n, rec.Body.String())
	}

	// Exact members, including JSON escapes that decode to them, still work.
	for _, body := range []string{
		`{"name":"ok","ttl_seconds":3600}`,
		` { "ttl_seconds" : 7200 , "name" : "spaced" } `,
		`{"name":"escaped"}`,
	} {
		wantStatus(t, f.session(http.MethodPost, "/ui/v1/me/keys", alice, body), http.StatusCreated)
	}
}
