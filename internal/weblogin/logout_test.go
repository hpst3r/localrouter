package weblogin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const clearedSession = SessionCookieName + "=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"

func (h *harness) logout(mod func(r *http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, testPublicURL+LogoutPath, nil)
	r.Header.Set("Origin", testPublicURL)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set(CSRFHeader, "csrf-1")
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "session-secret-1"})
	if mod != nil {
		mod(r)
	}
	return h.do(r)
}

func TestLogoutRevokesSessionAndClearsCookie(t *testing.T) {
	h := newHarness(t)
	w := h.logout(nil)
	if w.Code != http.StatusNoContent || setCookies(w)[SessionCookieName] != clearedSession {
		t.Fatalf("got %d %v", w.Code, w.Header())
	}
	if len(h.hooks.logouts) != 1 || h.hooks.logouts[0] != "session-secret-1|csrf-1" {
		t.Fatalf("logouts = %v", h.hooks.logouts)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("logout response cacheable")
	}
}

func TestLogoutWithoutSessionIsIdempotent(t *testing.T) {
	h := newHarness(t)
	w := h.logout(func(r *http.Request) { r.Header.Del("Cookie") })
	if w.Code != http.StatusNoContent || setCookies(w)[SessionCookieName] != clearedSession || len(h.hooks.logouts) != 0 {
		t.Fatalf("got %d, logouts %v", w.Code, h.hooks.logouts)
	}
}

func TestLogoutRejectsCrossSiteAndBadCSRF(t *testing.T) {
	cases := map[string]struct {
		mod    func(r *http.Request)
		status int
		body   string
	}{
		"no origin":        {func(r *http.Request) { r.Header.Del("Origin") }, 403, "logout failed: cross_site\n"},
		"foreign origin":   {func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.test") }, 403, "logout failed: cross_site\n"},
		"null origin":      {func(r *http.Request) { r.Header.Set("Origin", "null") }, 403, "logout failed: cross_site\n"},
		"origin with path": {func(r *http.Request) { r.Header.Set("Origin", testPublicURL+"/") }, 403, "logout failed: cross_site\n"},
		"cross-site fetch": {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403, "logout failed: cross_site\n"},
		"same-site fetch":  {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, 403, "logout failed: cross_site\n"},
		"other host":       {func(r *http.Request) { r.Host = "evil.example.test" }, 403, "logout failed: cross_site\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			w := h.logout(c.mod)
			if w.Code != c.status || w.Body.String() != c.body || len(h.hooks.logouts) != 0 {
				t.Fatalf("got %d %q logouts=%v", w.Code, w.Body.String(), h.hooks.logouts)
			}
			if _, ok := setCookies(w)[SessionCookieName]; ok {
				t.Fatal("cookie cleared on rejected logout")
			}
		})
	}
	h := newHarness(t)
	h.hooks.logoutErr = errors.Join(errors.New("adapter"), ErrCSRF)
	w := h.logout(nil)
	if w.Code != http.StatusForbidden || w.Body.String() != "logout failed: csrf\n" {
		t.Fatalf("bad csrf: %d %q", w.Code, w.Body.String())
	}
	if _, ok := setCookies(w)[SessionCookieName]; ok {
		t.Fatal("cookie cleared on csrf failure")
	}
	h.hooks.logoutErr = errors.New("store down")
	if w := h.logout(nil); w.Code != http.StatusServiceUnavailable || w.Body.String() != "logout failed: logout_failed\n" {
		t.Fatalf("store error: %d %q", w.Code, w.Body.String())
	}
	if w := h.do(httptest.NewRequest(http.MethodGet, testPublicURL+LogoutPath, nil)); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET logout: %d", w.Code)
	}
}
