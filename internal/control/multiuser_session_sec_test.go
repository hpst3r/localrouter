package control

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

const createKeyBody = `{"name":"k2"}`

func TestSessionRequiresValidCookie(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")

	none := muUser{CSRF: alice.CSRF}
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/me", none, ""), http.StatusUnauthorized)
	bad := alice
	bad.Session = "lrs_" + strings.Repeat("A", 43)
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/me", bad, ""), http.StatusUnauthorized)

	// Two session cookies are ambiguous: refused even if one is valid, in
	// either order.
	req := f.sessionReq(http.MethodGet, "/ui/v1/me", alice, "")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: bad.Session})
	wantStatus(t, f.serve(req), http.StatusUnauthorized)
	req = f.sessionReq(http.MethodGet, "/ui/v1/me", bad, "")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: alice.Session})
	wantStatus(t, f.serve(req), http.StatusUnauthorized)
}

func TestSessionRoutesRefuseAuthorizationHeader(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")
	// A valid cookie plus any Authorization header: refused (no mixing).
	req := f.sessionReq(http.MethodGet, "/ui/v1/me", alice, "")
	req.Header.Set("Authorization", "Bearer "+alice.Key)
	wantStatus(t, f.serve(req), http.StatusBadRequest)
	// A bearer alone never authenticates a session route.
	req = f.sessionReq(http.MethodGet, "/ui/v1/me/keys", muUser{}, "")
	req.Header.Set("Authorization", "Bearer "+alice.Key)
	wantStatus(t, f.serve(req), http.StatusBadRequest)
}

func TestSessionUnsafeRequiresOriginFetchSiteAndCSRF(t *testing.T) {
	f := newMUFixture(t, nil)
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")

	cases := map[string]func(*http.Request){
		"no origin":            func(r *http.Request) { r.Header.Del("Origin") },
		"null origin":          func(r *http.Request) { r.Header.Set("Origin", "null") },
		"other origin":         func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"http origin":          func(r *http.Request) { r.Header.Set("Origin", "http://router.example.test") },
		"origin with path":     func(r *http.Request) { r.Header.Set("Origin", muOrigin+"/") },
		"two origins":          func(r *http.Request) { r.Header.Add("Origin", muOrigin) },
		"cross-site":           func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"same-site":            func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		"fetch-site none":      func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") },
		"no csrf":              func(r *http.Request) { r.Header.Del(csrfHeader) },
		"wrong csrf":           func(r *http.Request) { r.Header.Set(csrfHeader, "x") },
		"other session's csrf": func(r *http.Request) { r.Header.Set(csrfHeader, bob.CSRF) },
		"two csrf":             func(r *http.Request) { r.Header.Add(csrfHeader, alice.CSRF) },
		"forwarded host spoof": func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Set("X-Forwarded-Host", "evil.example")
			r.Host = "evil.example"
		},
	}
	for name, mutate := range cases {
		req := f.sessionReq(http.MethodPost, "/ui/v1/me/keys", alice, createKeyBody)
		mutate(req)
		rec := f.serve(req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403 (%s)", name, rec.Code, rec.Body.String())
		}
	}
	keys, err := f.ids.ListKeys(context.Background(), alice.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("a refused request created a key: %d %v", len(keys), err)
	}
	// Absent Sec-Fetch-Site (older browsers, non-browser clients) is fine.
	req := f.sessionReq(http.MethodPost, "/ui/v1/me/keys", alice, createKeyBody)
	req.Header.Del("Sec-Fetch-Site")
	wantStatus(t, f.serve(req), http.StatusCreated)
}

func TestSessionStrictJSONBodies(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")
	for name, body := range map[string]string{
		"unknown field":  `{"name":"x","user_id":"u_other"}`,
		"trailing":       `{"name":"x"}{"name":"y"}`,
		"array":          `[{"name":"x"}]`,
		"not json":       `name=x`,
		"negative ttl":   `{"name":"x","ttl_seconds":-1}`,
		"zero ttl":       `{"name":"x","ttl_seconds":0}`,
		"huge ttl":       `{"name":"x","ttl_seconds":9223372036854775807}`,
		"above ceiling":  `{"name":"x","ttl_seconds":7776001}`,
		"below min ttl":  `{"name":"x","ttl_seconds":60}`,
		"blank name":     `{"name":"   "}`,
		"control name":   `{"name":"a\u0000b"}`,
		"too long name":  `{"name":"` + strings.Repeat("n", 65) + `"}`,
		"fractional ttl": `{"name":"x","ttl_seconds":1.5}`,
	} {
		rec := f.session(http.MethodPost, "/ui/v1/me/keys", alice, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "u_other") || strings.Contains(rec.Body.String(), "nnnnnnnn") {
			t.Errorf("%s: body echoed: %s", name, rec.Body.String())
		}
	}
	req := f.sessionReq(http.MethodPost, "/ui/v1/me/keys", alice, createKeyBody)
	req.Header.Set("Content-Type", "text/plain")
	wantStatus(t, f.serve(req), http.StatusUnsupportedMediaType)

	big := `{"name":"` + strings.Repeat("x", maxSessionBody) + `"}`
	wantStatus(t, f.session(http.MethodPost, "/ui/v1/me/keys", alice, big), http.StatusRequestEntityTooLarge)
}

func TestSessionIdentityUnavailableFailsClosed(t *testing.T) {
	f := newMUFixture(t, func(d *Deps) { d.Identity = nil })
	alice := muUser{Session: "lrs_" + strings.Repeat("A", 43), CSRF: "x"}
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/me", alice, ""), http.StatusServiceUnavailable)
	wantStatus(t, f.session(http.MethodPost, "/ui/v1/me/keys", alice, createKeyBody), http.StatusServiceUnavailable)

	g := newMUFixture(t, nil)
	bob := g.addUser("bob", "user")
	if err := g.ids.Close(); err != nil {
		t.Fatal(err)
	}
	rec := g.session(http.MethodGet, "/ui/v1/me", bob, "")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if strings.Contains(rec.Body.String(), "closed") || strings.Contains(rec.Body.String(), "identity:") {
		t.Fatalf("store error leaked: %s", rec.Body.String())
	}
	// The bearer API fails closed the same way through the authenticator.
	wantStatus(t, g.bearerGet("/control/v1/usage", bob.Key), http.StatusServiceUnavailable)
}

func TestSessionMissingPublicOriginFailsUnsafeClosed(t *testing.T) {
	for _, origin := range []string{"", "http://router.example.test", "https://router.example.test/path", "not a url"} {
		f := newMUFixture(t, func(d *Deps) { d.PublicBaseURL = origin })
		alice := f.addUser("alice", "user")
		req := f.sessionReq(http.MethodPost, "/ui/v1/me/keys", alice, createKeyBody)
		req.Header.Set("Origin", origin)
		rec := f.serve(req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("origin %q: status %d, want 503", origin, rec.Code)
		}
		// Safe reads still work.
		wantStatus(t, f.session(http.MethodGet, "/ui/v1/me", alice, ""), http.StatusOK)
	}
}

func TestSessionDisabledUserNextRequestDenied(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/me", alice, ""), http.StatusOK)
	if err := f.ids.DisableUser(context.Background(), identity.Actor{Kind: identity.ActorCLI}, alice.ID); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/me", alice, ""), http.StatusUnauthorized)
	wantStatus(t, f.bearerGet("/control/v1/usage", alice.Key), http.StatusUnauthorized)
}

func TestSessionNoCORS(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")
	req := f.sessionReq(http.MethodGet, "/ui/v1/me", alice, "")
	req.Header.Set("Origin", "https://evil.example")
	rec := f.serve(req)
	for k := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Fatalf("CORS header %s set", k)
		}
	}
	pre := f.sessionReq(http.MethodOptions, "/ui/v1/me/keys", alice, "")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	rec = f.serve(pre)
	if rec.Code == http.StatusOK || rec.Code == http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preflight answered: %d %v", rec.Code, rec.Header())
	}
}

func TestSessionCookieNamesMatchWeblogin(t *testing.T) {
	if sessionCookieName != weblogin.SessionCookieName || csrfHeader != weblogin.CSRFHeader {
		t.Fatalf("control (%s, %s) disagrees with weblogin (%s, %s)",
			sessionCookieName, csrfHeader, weblogin.SessionCookieName, weblogin.CSRFHeader)
	}
}
