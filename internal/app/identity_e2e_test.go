package app

// End-to-end browser login through the real App.Handler against the signed
// loopback TLS IdP fixture: /auth/login -> provider authorize -> /auth/callback
// (real code exchange with client_secret_basic read from the private secret
// file, PKCE, nonce, RS256 signature, fresh auth_time) -> identity store
// session cookie -> user API key -> inference. No external network.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

type loginEnv struct {
	t     *testing.T
	clock *idClock
	idp   *fakeIdP
	f     *idFixture
	a     *App
}

func newLoginEnv(t *testing.T) *loginEnv {
	t.Helper()
	e := &loginEnv{t: t, clock: newIDClock()}
	e.idp = newFakeIdP(t, e.clock)
	e.f = newIDFixture(t)
	e.f.issuer = e.idp.issuer
	e.f.access = "    claim: groups\n    user_values: [lr-users]\n    admin_values: [lr-admins]\n    admin_subjects: [root-sub]\n"
	e.a = e.f.build(e.idp.overrides(e.clock))
	return e
}

func canonical(r *http.Request) { r.Host = "localhost:8787" }

func cookieFrom(w *httptest.ResponseRecorder, name string) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

// login runs the whole browser flow for subject with the given claims. It
// returns the session cookie (on success) and the callback response.
func (e *loginEnv) login(subject string, claims map[string]any) (string, *httptest.ResponseRecorder) {
	e.t.Helper()
	w := serve(e.a, "GET", "/auth/login", "", canonical)
	if w.Code != http.StatusFound {
		e.t.Fatalf("/auth/login: %d %s", w.Code, w.Body)
	}
	binding := cookieFrom(w, weblogin.LoginCookieName)
	cb := e.idp.authorize(e.t, w.Header().Get("Location"), subject, claims)
	w = serve(e.a, "GET", "/auth/callback?"+cb.Encode(), "", canonical, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: weblogin.LoginCookieName, Value: binding})
	})
	if fails := e.idp.failureList(); len(fails) > 0 {
		e.t.Fatalf("provider rejected the exchange: %v", fails)
	}
	return cookieFrom(w, weblogin.SessionCookieName), w
}

func (e *loginEnv) session(cookie string) identity.Session {
	e.t.Helper()
	s, err := e.a.Identity.AuthenticateSession(context.Background(), cookie)
	if err != nil {
		e.t.Fatalf("session cookie does not authenticate: %v", err)
	}
	return s
}

func TestSignedBrowserLoginToInference(t *testing.T) {
	e := newLoginEnv(t)
	cookie, w := e.login("nora", map[string]any{"groups": []string{"lr-users"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != weblogin.LandingPath || cookie == "" {
		t.Fatalf("callback: %d Location=%q cookie set=%v body=%s", w.Code, w.Header().Get("Location"), cookie != "", w.Body)
	}
	s := e.session(cookie)
	if s.Principal.Kind != core.PrincipalSession || s.Principal.Role != core.RoleUser {
		t.Fatalf("session principal = %+v", s.Principal)
	}
	k, err := e.a.Identity.CreateKey(context.Background(), s.Principal.UserID, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if w := serve(e.a, "GET", "/v1/models", "", withBearer(k.Token)); w.Code != http.StatusOK {
		t.Fatalf("inference with the new user key: %d", w.Code)
	}

	// Local logout: weblogin checks Origin and Sec-Fetch-Site, the adapter
	// checks CSRF against the session, the store revokes it.
	logout := func(csrf string) *httptest.ResponseRecorder {
		return serve(e.a, "POST", "/auth/logout", "", canonical, func(r *http.Request) {
			r.Header.Set("Origin", fixturePublicURL)
			r.Header.Set(weblogin.CSRFHeader, csrf)
			r.AddCookie(&http.Cookie{Name: weblogin.SessionCookieName, Value: cookie})
		})
	}
	if w := logout("wrong"); w.Code != http.StatusForbidden {
		t.Fatalf("logout with a wrong CSRF token: %d", w.Code)
	}
	e.session(cookie)
	if w := logout(identity.CSRFToken(cookie)); w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	if _, err := e.a.Identity.AuthenticateSession(context.Background(), cookie); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("session after logout: %v", err)
	}
	if w := serve(e.a, "GET", "/v1/models", "", withBearer(k.Token)); w.Code != http.StatusOK {
		t.Fatalf("logout must not revoke API keys: %d", w.Code)
	}
}

// TestSignedLoginAdminSubjectNeedsClaim: an admin subject is admin only on a
// login that carries an allowed claim value; without one it is refused.
func TestSignedLoginAdminSubjectNeedsClaim(t *testing.T) {
	e := newLoginEnv(t)
	if cookie, w := e.login("root-sub", map[string]any{}); w.Code != http.StatusForbidden || cookie != "" || !strings.Contains(w.Body.String(), weblogin.ReasonNotPermitted) {
		t.Fatalf("admin subject without a claim value: %d %q cookie=%v", w.Code, w.Body, cookie != "")
	}
	users, err := e.a.Identity.ListUsers(context.Background(), "", identity.MaxListLimit)
	if err != nil || len(users) != 0 {
		t.Fatalf("refused login provisioned a user: %d %v", len(users), err)
	}
	cookie, w := e.login("root-sub", map[string]any{"groups": []string{"lr-users"}})
	if cookie == "" {
		t.Fatalf("admin subject with a user value: %d %s", w.Code, w.Body)
	}
	if s := e.session(cookie); s.Principal.Role != core.RoleAdmin {
		t.Fatalf("admin subject with an allowed claim: role %s, want admin", s.Principal.Role)
	}
	cookie, _ = e.login("ops", map[string]any{"groups": "lr-admins"})
	if s := e.session(cookie); s.Principal.Role != core.RoleAdmin {
		t.Fatalf("admin claim value: role %s", s.Principal.Role)
	}
}

// TestSignedLoginSubjectOnlyAdminNeedsClaim: with admin_subjects as the only
// admin mapping, the listed subject is admin only while its token carries an
// allowed user value. Once the provider stops sending one, the login is
// refused and the denial revokes the admin's existing session and keys.
func TestSignedLoginSubjectOnlyAdminNeedsClaim(t *testing.T) {
	e := &loginEnv{t: t, clock: newIDClock()}
	e.idp = newFakeIdP(t, e.clock)
	e.f = newIDFixture(t)
	e.f.issuer = e.idp.issuer
	e.f.access = "    claim: groups\n    user_values: [lr-users]\n    admin_subjects: [root-sub]\n"
	e.a = e.f.build(e.idp.overrides(e.clock))

	cookie, w := e.login("root-sub", map[string]any{"groups": []string{"lr-users"}})
	if cookie == "" {
		t.Fatalf("admin subject with a user value: %d %s", w.Code, w.Body)
	}
	s := e.session(cookie)
	if s.Principal.Role != core.RoleAdmin {
		t.Fatalf("admin subject: role %s, want admin", s.Principal.Role)
	}
	k, err := e.a.Identity.CreateKey(context.Background(), s.Principal.UserID, "ops", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := e.login("plain", map[string]any{"groups": "lr-users"}); e.session(c).Principal.Role != core.RoleUser {
		t.Fatal("an unlisted subject with a user value is not a plain user")
	}
	for _, claims := range []map[string]any{{}, {"groups": []string{}}, {"groups": []string{"other"}}} {
		if c, w := e.login("root-sub", claims); w.Code != http.StatusForbidden || c != "" || !strings.Contains(w.Body.String(), weblogin.ReasonNotPermitted) {
			t.Fatalf("admin subject with claims %v: %d %q", claims, w.Code, w.Body)
		}
	}
	if w := serve(e.a, "GET", "/ui/v1/admin/users", "", sessionReq(cookie)); w.Code != http.StatusUnauthorized {
		t.Fatalf("admin session after the claim was removed: %d, want 401", w.Code)
	}
	if w := serve(e.a, "GET", "/v1/models", "", withBearer(k.Token)); w.Code != http.StatusUnauthorized {
		t.Fatalf("admin's key after the claim was removed: %d, want 401", w.Code)
	}
}

// TestSignedLoginPolicyDenialRevokes: a verified login that the policy now
// refuses (the user left the group at the provider) revokes that user's keys
// and sessions; the next request is denied.
func TestSignedLoginPolicyDenialRevokes(t *testing.T) {
	e := newLoginEnv(t)
	cookie, _ := e.login("omar", map[string]any{"groups": []string{"lr-users"}})
	s := e.session(cookie)
	k, err := e.a.Identity.CreateKey(context.Background(), s.Principal.UserID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, w := e.login("omar", map[string]any{"groups": []string{"other"}}); w.Code != http.StatusForbidden {
		t.Fatalf("login after leaving the group: %d", w.Code)
	}
	if w := serve(e.a, "GET", "/v1/models", "", withBearer(k.Token)); w.Code != http.StatusUnauthorized {
		t.Fatalf("key after policy denial: %d, want 401", w.Code)
	}
	if _, err := e.a.Identity.AuthenticateSession(context.Background(), cookie); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("session after policy denial: %v", err)
	}
}

// TestSignedLoginDisabledUserRefused: a locally disabled user cannot log back
// in, and nothing is resurrected by enabling.
func TestSignedLoginDisabledUserRefused(t *testing.T) {
	e := newLoginEnv(t)
	cookie, _ := e.login("pia", map[string]any{"groups": []string{"lr-users"}})
	uid := e.session(cookie).Principal.UserID
	if err := e.a.Identity.DisableUser(context.Background(), identity.Actor{Kind: identity.ActorCLI}, uid); err != nil {
		t.Fatal(err)
	}
	if _, w := e.login("pia", map[string]any{"groups": []string{"lr-users"}}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "user_disabled") {
		t.Fatalf("disabled user login: %d %q", w.Code, w.Body)
	}
	if err := e.a.Identity.EnableUser(context.Background(), identity.Actor{Kind: identity.ActorCLI}, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Identity.AuthenticateSession(context.Background(), cookie); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("enable resurrected the old session: %v", err)
	}
	if c, _ := e.login("pia", map[string]any{"groups": []string{"lr-users"}}); c == "" {
		t.Fatal("re-enabled user cannot log in again")
	}
}

// sessionReq builds a /ui/v1 request with the session cookie; unsafe methods
// also carry the exact public Origin and the session's CSRF token, as the
// browser UI sends them.
func sessionReq(cookie string) reqOpt {
	return func(r *http.Request) {
		canonical(r)
		r.AddCookie(&http.Cookie{Name: weblogin.SessionCookieName, Value: cookie})
		if r.Method != http.MethodGet {
			r.Header.Set("Origin", fixturePublicURL)
			r.Header.Set("X-LocalRouter-CSRF", identity.CSRFToken(cookie))
		}
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return v
}

type usageRows struct {
	Rows []core.UsageRow `json:"rows"`
}

func totalRequests(u usageRows) int64 {
	var n int64
	for _, r := range u.Rows {
		n += r.Requests
	}
	return n
}

// TestSignedLoginSessionAPIFullStack drives the browser session API and
// inference together: self-service key creation, owner-scoped usage of real
// proxied requests, admin lifecycle effective on the next request, and the
// session role read fresh on every request.
func TestSignedLoginSessionAPIFullStack(t *testing.T) {
	up := newFakeUpstream(t, false)
	e := &loginEnv{t: t, clock: newIDClock()}
	e.idp = newFakeIdP(t, e.clock)
	e.f = newIDFixture(t)
	e.f.issuer = e.idp.issuer
	e.f.upstream = up.srv.URL
	e.a = e.f.build(e.idp.overrides(e.clock))

	if w := serve(e.a, "GET", "/", "", canonical); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/ui/" {
		t.Fatalf("legacy widget in multi-user mode: %d %q", w.Code, w.Header().Get("Location"))
	}

	userCookie, _ := e.login("quinn", map[string]any{"groups": []string{"lr-users"}})
	w := serve(e.a, "GET", "/ui/v1/me", "", sessionReq(userCookie))
	me := decode[struct {
		User struct {
			ID, Role, Status string
		} `json:"user"`
		CSRF string `json:"csrf_token"`
	}](t, w)
	if w.Code != http.StatusOK || me.User.Role != "user" || me.User.Status != "active" || me.CSRF != identity.CSRFToken(userCookie) {
		t.Fatalf("/ui/v1/me: %d %+v", w.Code, me)
	}
	w = serve(e.a, "POST", "/ui/v1/me/keys", `{"name":"laptop"}`, sessionReq(userCookie))
	created := decode[struct {
		Token string `json:"token"`
	}](t, w)
	if w.Code != http.StatusCreated || !identity.IsUserKeyToken(created.Token) {
		t.Fatalf("create key: %d", w.Code)
	}
	if w := serve(e.a, "POST", "/ui/v1/me/keys", `{"name":"x"}`, func(r *http.Request) {
		canonical(r)
		r.AddCookie(&http.Cookie{Name: weblogin.SessionCookieName, Value: userCookie})
		r.Header.Set("X-LocalRouter-CSRF", identity.CSRFToken(userCookie))
	}); w.Code != http.StatusForbidden {
		t.Fatalf("unsafe session request without Origin: %d", w.Code)
	}

	// Two proxied requests by the user, one by the service client.
	for i := 0; i < 2; i++ {
		if w := serve(e.a, "POST", "/v1/chat/completions", chatBody, withBearer(created.Token)); w.Code != http.StatusOK {
			t.Fatalf("user inference: %d %s", w.Code, w.Body)
		}
	}
	if w := serve(e.a, "POST", "/v1/chat/completions", chatBody, withBearer(svcStaticKey)); w.Code != http.StatusOK {
		t.Fatalf("service inference: %d", w.Code)
	}
	if got := totalRequests(decode[usageRows](t, serve(e.a, "GET", "/ui/v1/me/usage", "", sessionReq(userCookie)))); got != 2 {
		t.Fatalf("user's own usage = %d requests, want 2", got)
	}
	if got := totalRequests(decode[usageRows](t, serve(e.a, "GET", "/control/v1/usage", "", withBearer(created.Token)))); got != 2 {
		t.Fatalf("user key /control/v1/usage = %d requests, want own 2", got)
	}
	if w := serve(e.a, "GET", "/control/v1/status", "", withBearer(created.Token)); w.Code != http.StatusForbidden {
		t.Fatalf("user key on global status: %d, want 403", w.Code)
	}
	if got := totalRequests(decode[usageRows](t, serve(e.a, "GET", "/control/v1/usage", "", withBearer(svcStaticKey)))); got != 3 {
		t.Fatalf("service global usage = %d requests, want 3", got)
	}
	otherCookie, _ := e.login("rae", map[string]any{"groups": []string{"lr-users"}})
	if got := totalRequests(decode[usageRows](t, serve(e.a, "GET", "/ui/v1/me/usage", "", sessionReq(otherCookie)))); got != 0 {
		t.Fatalf("another user sees %d requests, want 0", got)
	}
	if w := serve(e.a, "GET", "/ui/v1/admin/users", "", sessionReq(userCookie)); w.Code != http.StatusForbidden {
		t.Fatalf("user session on admin users: %d", w.Code)
	}

	// Admin disables the user: the very next request with the key and the
	// session is denied.
	adminCookie, _ := e.login("sam", map[string]any{"groups": []string{"lr-admins"}})
	if w := serve(e.a, "GET", "/ui/v1/admin/users", "", sessionReq(adminCookie)); w.Code != http.StatusOK {
		t.Fatalf("admin users: %d", w.Code)
	}
	if w := serve(e.a, "POST", "/ui/v1/admin/users/"+me.User.ID+"/disable", "", sessionReq(adminCookie)); w.Code != http.StatusNoContent {
		t.Fatalf("admin disable: %d %s", w.Code, w.Body)
	}
	if w := serve(e.a, "POST", "/v1/chat/completions", chatBody, withBearer(created.Token)); w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user's key: %d, want 401", w.Code)
	}
	if w := serve(e.a, "GET", "/ui/v1/me", "", sessionReq(userCookie)); w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user's session: %d, want 401", w.Code)
	}

	// The admin loses the admin claim at the provider and logs in again: the
	// earlier admin session is now a user session on its next request.
	if c, _ := e.login("sam", map[string]any{"groups": []string{"lr-users"}}); c == "" {
		t.Fatal("re-login as user failed")
	}
	if w := serve(e.a, "GET", "/ui/v1/admin/users", "", sessionReq(adminCookie)); w.Code != http.StatusForbidden {
		t.Fatalf("old admin session after role downgrade: %d, want 403", w.Code)
	}
}
