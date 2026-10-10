package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// Account ids are operator configuration: admit hides them from ordinary
// users, so their own usage and analytics must not reveal them either. A
// user scope cannot group or filter by account, the usage default group is
// model for it, and the dimensions doc has no account entry. Global readers
// (service bearer, admin session) keep the account dimension.
func TestUserScopeCannotSeeAccountIDs(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", core.RoleUser)
	admin := f.addUser("root", core.RoleAdmin)
	f.addStaticUser(aliceStaticToken, alice)
	f.record("r1", "primary-acct", "gpt", alice.KeyID, alice.ID, alice.KeyID, 5)

	reads := map[string]func(string) *httptest.ResponseRecorder{
		"key":     func(q string) *httptest.ResponseRecorder { return f.bearerGet("/control/v1/"+q, alice.Key) },
		"static":  func(q string) *httptest.ResponseRecorder { return f.bearerGet("/control/v1/"+q, aliceStaticToken) },
		"session": func(q string) *httptest.ResponseRecorder { return f.session(http.MethodGet, "/ui/v1/me/"+q, alice, "") },
	}
	for name, get := range reads {
		for _, q := range []string{"usage?group=account", "analytics?range=24h&group=account",
			"analytics?range=24h&group=model&filter.account=primary-acct",
			"analytics/dimensions?range=24h&filter.account=primary-acct"} {
			r := get(q)
			if r.Code != http.StatusBadRequest {
				t.Errorf("%s %s = %d, want 400: %s", name, q, r.Code, r.Body.String())
			}
			if strings.Contains(r.Body.String(), "primary-acct") {
				t.Errorf("%s %s echoes account id: %s", name, q, r.Body.String())
			}
		}
		r := get("usage")
		wantStatus(t, r, http.StatusOK)
		var doc usageDoc
		if err := json.Unmarshal(r.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Group != "model" {
			t.Errorf("%s default usage group = %q, want model", name, doc.Group)
		}
		for _, q := range []string{"usage", "analytics?range=24h", "analytics/dimensions?range=24h"} {
			r := get(q)
			wantStatus(t, r, http.StatusOK)
			if strings.Contains(r.Body.String(), "primary-acct") || strings.Contains(r.Body.String(), `"account"`) {
				t.Errorf("%s %s reveals account: %s", name, q, r.Body.String())
			}
		}
	}

	globals := map[string]func(string) *httptest.ResponseRecorder{
		"service": func(q string) *httptest.ResponseRecorder { return f.bearerGet("/control/v1/"+q, serviceToken) },
		"admin": func(q string) *httptest.ResponseRecorder {
			return f.session(http.MethodGet, "/ui/v1/admin/"+q, admin, "")
		},
	}
	for name, get := range globals {
		r := get("usage")
		wantStatus(t, r, http.StatusOK)
		var doc usageDoc
		if err := json.Unmarshal(r.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Group != "account" || !strings.Contains(r.Body.String(), "primary-acct") {
			t.Errorf("%s default usage = %s, want account grouping", name, r.Body.String())
		}
		for _, q := range []string{"analytics?range=24h&group=account&filter.account=primary-acct", "analytics/dimensions?range=24h"} {
			r := get(q)
			wantStatus(t, r, http.StatusOK)
			if !strings.Contains(r.Body.String(), "primary-acct") {
				t.Errorf("%s %s lacks account: %s", name, q, r.Body.String())
			}
		}
	}
}
