package control

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// TestMultiUserNoSecretsInResponsesOrLogs drives success and failure paths
// with every credential kind and asserts no token, CSRF value, backend error
// text or PII reaches a response body (other than the one-time create
// response and the CSRF field of /me) or the log.
func TestMultiUserNoSecretsInResponsesOrLogs(t *testing.T) {
	logs := &syncLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	f := newMUFixture(t, nil)
	root := f.addUser("root", "admin")
	alice := f.addUser("alice", "user")
	seedTwoOwners(f, alice, root)

	var bodies []string
	keep := func(code int, body string) { bodies = append(bodies, fmt.Sprintf("%d %s", code, body)) }
	for _, tok := range []string{alice.Key, root.Key, serviceToken, ingestServiceToken, "lrk_bad", alice.Session} {
		for _, p := range []string{"/control/v1/status", "/control/v1/usage?group=key", "/control/v1/analytics?range=24h",
			"/control/v1/analytics/dimensions", "/control/v1/diagnostics", "/control/v1/budgets?scope=user&key=" + alice.ID} {
			rec := f.bearerGet(p, tok)
			keep(rec.Code, rec.Body.String())
		}
		rec := f.bearerPost("/control/v1/admit", tok, `{"class":"interactive","model":"gpt"}`)
		keep(rec.Code, rec.Body.String())
	}
	for _, u := range []muUser{alice, root} {
		for _, p := range []string{"/ui/v1/me/keys", "/ui/v1/me/usage", "/ui/v1/me/budget", "/ui/v1/admin/users",
			"/ui/v1/admin/audit", "/ui/v1/admin/users/" + alice.ID + "/keys", "/ui/v1/admin/diagnostics"} {
			rec := f.session(http.MethodGet, p, u, "")
			keep(rec.Code, rec.Body.String())
		}
		bad := u
		bad.CSRF = "forged"
		rec := f.session(http.MethodPost, "/ui/v1/me/keys", bad, `{"name":"x"}`)
		keep(rec.Code, rec.Body.String())
	}
	f.setAuthErr(fmt.Errorf("%w: %w", core.ErrAuthUnavailable, errDBDown))
	rec := f.bearerGet("/control/v1/usage", alice.Key)
	keep(rec.Code, rec.Body.String())
	_ = f.ids.Close()
	rec = f.session(http.MethodGet, "/ui/v1/me", alice, "")
	keep(rec.Code, rec.Body.String())

	secrets := []string{alice.Key, root.Key, alice.Session, root.Session, alice.CSRF, root.CSRF,
		serviceToken, ingestServiceToken, "lrk_", "lrs_", "sqlite", "/var/lib/secret", muIssuer, muClientID}
	all := strings.Join(bodies, "\n")
	for _, s := range secrets {
		if strings.Contains(all, s) {
			t.Errorf("response leaked %q", s)
		}
	}
	for _, s := range append(secrets, "alice@example.test", "Name alice", "root@example.test") {
		if strings.Contains(logs.String(), s) {
			t.Errorf("log leaked %q:\n%s", s, logs.String())
		}
	}
}

func TestSessionMalformedIDsAre404(t *testing.T) {
	f := newMUFixture(t, nil)
	root := f.addUser("root", "admin")
	for _, id := range []string{strings.Repeat("a", 65), "u_%27%20OR%201%3D1", "u_x%2Fkeys", "u_%00"} {
		for _, req := range []struct{ method, path string }{
			{http.MethodDelete, "/ui/v1/me/keys/" + id},
			{http.MethodPost, "/ui/v1/admin/users/" + id + "/disable"},
			{http.MethodGet, "/ui/v1/admin/users/" + id + "/keys"},
			{http.MethodDelete, "/ui/v1/admin/users/" + id + "/keys/k_x"},
			{http.MethodDelete, "/ui/v1/admin/users/u_x/keys/" + id},
		} {
			rec := f.session(req.method, req.path, root, "")
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d, want 404", req.method, req.path, rec.Code)
			}
		}
	}
}

func TestMultiUserStaticUserClientIsOwnerScoped(t *testing.T) {
	f := newMUFixture(t, nil)
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")
	f.addStaticUser(aliceStaticToken, alice)
	seedTwoOwners(f, alice, bob)

	rec := f.bearerGet("/control/v1/usage?group=model", aliceStaticToken)
	wantStatus(t, rec, http.StatusOK)
	if got := usageKeys(t, rec); len(got) != 1 || got["gpt-alice"] != 2 {
		t.Fatalf("static user usage = %v", got)
	}
	// A static user client never authenticates the session API.
	req := f.sessionReq(http.MethodGet, "/ui/v1/me", muUser{}, "")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: aliceStaticToken})
	wantStatus(t, f.serve(req), http.StatusUnauthorized)
}
