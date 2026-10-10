package weblogin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// callback sends the IdP's redirect back to /auth/callback with cookie.
func (h *harness) callback(q url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, testPublicURL+CallbackPath+"?"+q.Encode(), nil)
	if cookie != nil {
		r.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	return h.do(r)
}

// login runs the whole browser flow and returns the callback response.
func (h *harness) login() *httptest.ResponseRecorder {
	h.t.Helper()
	w, loc, c := h.startLogin()
	if w.Code != http.StatusFound || c == nil {
		h.t.Fatalf("login start: status %d", w.Code)
	}
	return h.callback(h.idp.authorize(h.t, loc), c)
}

func setCookies(w *httptest.ResponseRecorder) map[string]string {
	m := map[string]string{}
	for _, v := range w.Header().Values("Set-Cookie") {
		name, _, _ := strings.Cut(v, "=")
		m[name] = v
	}
	return m
}

func TestCallbackHappyPathCreatesSession(t *testing.T) {
	h := newHarness(t)
	authAt := h.clock.Now()
	w := h.login()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != LandingPath {
		t.Fatalf("status %d location %q body %q", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if f := h.idp.failureList(); len(f) != 0 {
		t.Fatalf("fake IdP rejected exchange: %v", f)
	}
	logins, denials := h.hooks.snapshot()
	if len(logins) != 1 || len(denials) != 0 {
		t.Fatalf("logins=%v denials=%v", logins, denials)
	}
	want := VerifiedLogin{
		Issuer:      h.idp.issuer,
		Subject:     testSubject,
		Role:        RoleUser,
		AuthTime:    authAt,
		Email:       "user@example.test",
		DisplayName: "Test User",
	}
	if got := logins[0]; got != want || !got.AuthTime.Equal(authAt) {
		t.Fatalf("login = %+v, want %+v", got, want)
	}
	if !h.hooks.jits[0] {
		t.Fatal("jit flag not passed from policy")
	}
	ck := setCookies(w)
	wantSession := SessionCookieName + "=" + h.hooks.sessions[0] + "; Path=/; Max-Age=86400; HttpOnly; Secure; SameSite=Lax"
	if ck[SessionCookieName] != wantSession {
		t.Fatalf("session cookie = %q, want %q", ck[SessionCookieName], wantSession)
	}
	if ck[LoginCookieName] != LoginCookieName+"=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax" {
		t.Fatalf("login cookie not cleared: %q", ck[LoginCookieName])
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers = %v", w.Header())
	}
}

// expectFailure asserts a fixed failure page, no session and no hook call.
func (h *harness) expectFailure(w *httptest.ResponseRecorder, status int, code string) {
	h.t.Helper()
	if w.Code != status || w.Body.String() != "login failed: "+code+"\n" {
		h.t.Fatalf("got %d %q, want %d %s", w.Code, w.Body.String(), status, code)
	}
	if _, ok := setCookies(w)[SessionCookieName]; ok {
		h.t.Fatal("session cookie set on failure")
	}
	if logins, _ := h.hooks.snapshot(); len(logins) != 0 {
		h.t.Fatalf("CompleteLogin called on failure: %v", logins)
	}
	if !strings.Contains(h.logs.String(), "code="+code) {
		h.t.Fatalf("failure code not logged: %s", h.logs.String())
	}
}

func TestCallbackStateIsSingleUseAndRequired(t *testing.T) {
	h := newHarness(t)
	w, loc, c := h.startLogin()
	_ = w
	q := h.idp.authorize(t, loc)

	// A state that is not the one sealed in this browser's cookie.
	unknown := url.Values{"code": q["code"], "state": {randToken()}}
	h.expectFailure(h.callback(unknown, c), http.StatusBadRequest, "login_csrf")
	if n := h.idp.hitCount("/tenant/token"); n != 0 {
		t.Fatalf("token endpoint called %d times for unknown state", n)
	}

	h2 := newHarness(t)
	w, loc, c = h2.startLogin()
	q = h2.idp.authorize(t, loc)
	if first := h2.callback(q, c); first.Code != http.StatusSeeOther {
		t.Fatalf("first callback = %d", first.Code)
	}
	h2.hooks.mu.Lock()
	h2.hooks.logins = nil
	h2.hooks.mu.Unlock()
	h2.expectFailure(h2.callback(q, c), http.StatusBadRequest, "login_expired")
	if n := h2.idp.hitCount("/tenant/token"); n != 1 {
		t.Fatalf("replayed state reached token endpoint: hits=%d", n)
	}
}

func TestCallbackRejectsExpiredFlow(t *testing.T) {
	h := newHarness(t)
	_, loc, c := h.startLogin()
	q := h.idp.authorize(t, loc)
	h.clock.Advance(10*time.Minute + time.Second)
	h.expectFailure(h.callback(q, c), http.StatusBadRequest, "login_expired")
	if n := h.idp.hitCount("/tenant/token"); n != 0 {
		t.Fatalf("expired flow reached token endpoint: hits=%d", n)
	}
}

func TestCallbackRequiresBrowserBindingCookie(t *testing.T) {
	for name, cookie := range map[string]func(c *http.Cookie) *http.Cookie{
		"missing":       func(*http.Cookie) *http.Cookie { return nil },
		"other browser": func(c *http.Cookie) *http.Cookie { return &http.Cookie{Name: c.Name, Value: randToken()} },
		"empty":         func(c *http.Cookie) *http.Cookie { return &http.Cookie{Name: c.Name, Value: ""} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			_, loc, c := h.startLogin()
			q := h.idp.authorize(t, loc)
			h.expectFailure(h.callback(q, cookie(c)), http.StatusBadRequest, "login_csrf")
			if n := h.idp.hitCount("/tenant/token"); n != 0 {
				t.Fatalf("token endpoint called: %d", n)
			}
			// Nothing is stored for a pending flow, so a forged callback
			// cannot cancel it: the right browser still completes.
			if w := h.callback(q, c); w.Code != http.StatusSeeOther {
				t.Fatalf("right browser after a forged callback: %d %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestCallbackIdPErrorIsMappedNotReflected(t *testing.T) {
	cases := []struct {
		idpError, code string
		status         int
	}{
		{"access_denied", "access_denied", http.StatusForbidden},
		{"login_required", "login_required", http.StatusBadRequest},
		{"interaction_required", "interaction_required", http.StatusBadRequest},
		{"consent_required", "consent_required", http.StatusBadRequest},
		{"<script>alert(1)</script>", "idp_error", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			h := newHarness(t)
			_, loc, ck := h.startLogin()
			q := h.idp.authorize(t, loc)
			bad := url.Values{
				"error":             {c.idpError},
				"error_description": {"hostile-description-marker <b>x</b>"},
				"error_uri":         {"https://evil.example.test/"},
				"state":             q["state"],
			}
			w := h.callback(bad, ck)
			h.expectFailure(w, c.status, c.code)
			if strings.Contains(w.Body.String()+h.logs.String(), "hostile") || strings.Contains(h.logs.String(), "evil") {
				t.Fatal("IdP error text reflected or logged")
			}
			// The error response ends the flow in this browser.
			if got := setCookies(w)[LoginCookieName]; got != LoginCookieName+"=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax" {
				t.Fatalf("login cookie not cleared: %q", got)
			}
			if n := h.idp.hitCount("/tenant/token"); n != 0 {
				t.Fatalf("token endpoint called: %d", n)
			}
		})
	}
}

func TestCallbackRejectsMalformedParameters(t *testing.T) {
	h := newHarness(t)
	_, loc, ck := h.startLogin()
	q := h.idp.authorize(t, loc)
	for name, v := range map[string]url.Values{
		"short state":     {"code": q["code"], "state": {"abc"}},
		"no code":         {"state": {randToken()}},
		"huge code":       {"code": {strings.Repeat("a", 4097)}, "state": {randToken()}},
		"control in code": {"code": {"a\x01b"}, "state": {randToken()}},
		"duplicate state": {"code": q["code"], "state": {q.Get("state"), q.Get("state")}},
	} {
		w := h.callback(v, ck)
		if w.Code != http.StatusBadRequest || w.Body.String() != "login failed: bad_request\n" {
			t.Errorf("%s: got %d %q", name, w.Code, w.Body.String())
		}
	}
	if n := h.idp.hitCount("/tenant/token"); n != 0 {
		t.Fatalf("token endpoint called: %d", n)
	}
}

func TestCallbackPolicyDenialCallsLoginDeniedOnly(t *testing.T) {
	cases := []struct {
		name, reason string
		edit         func(map[string]any)
	}{
		{"no allowed value", ReasonNotPermitted, func(c map[string]any) { c["roles"] = []string{"Reader"} }},
		{"claim absent", ReasonNotPermitted, func(c map[string]any) { delete(c, "roles") }},
		{"admin email only", ReasonNotPermitted, func(c map[string]any) {
			delete(c, "roles")
			c["email"] = "admin-subject-1"
		}},
		{"malformed", ReasonClaimMalformed, func(c map[string]any) { c["roles"] = 3 }},
		{"overage", ReasonGroupOverage, func(c map[string]any) {
			c["_claim_names"] = map[string]any{"roles": "src1"}
			c["_claim_sources"] = map[string]any{"src1": map[string]any{"endpoint": "https://graph.example.test/x"}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.expectFailure(h.loginWith(c.edit), http.StatusForbidden, c.reason)
			_, denials := h.hooks.snapshot()
			if want := h.idp.issuer + "|" + testSubject + "|" + c.reason; len(denials) != 1 || denials[0] != want {
				t.Fatalf("denials = %v, want [%s]", denials, want)
			}
			if n := h.idp.hitCount("/graph"); n != 0 {
				t.Fatal("distributed claim source fetched")
			}
		})
	}
}

func TestCallbackAdminRolePassedToHook(t *testing.T) {
	h := newHarness(t)
	if w := h.loginWith(func(c map[string]any) { c["roles"] = []string{"LocalRouter.User", "LocalRouter.Admin"} }); w.Code != http.StatusSeeOther {
		t.Fatalf("admin login: %d", w.Code)
	}
	h2 := newHarness(t)
	if w := h2.loginWith(func(c map[string]any) {
		c["sub"] = "admin-subject-1"
		c["roles"] = "LocalRouter.User"
	}); w.Code != http.StatusSeeOther {
		t.Fatalf("admin subject login: %d %q", w.Code, w.Body.String())
	}
	for _, hh := range []*harness{h, h2} {
		if logins, _ := hh.hooks.snapshot(); len(logins) != 1 || logins[0].Role != RoleAdmin {
			t.Fatalf("logins = %+v, want one admin", logins)
		}
	}
}

// An admin subject removed from every allowed group is denied: no session,
// and LoginDenied runs once so the adapter can revoke its credentials.
func TestCallbackAdminSubjectWithoutAllowedClaimDenied(t *testing.T) {
	for name, edit := range map[string]func(c map[string]any){
		"claim absent":     func(c map[string]any) { delete(c, "roles") },
		"no allowed value": func(c map[string]any) { c["roles"] = []string{"Reader"} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			w := h.loginWith(func(c map[string]any) {
				c["sub"] = "admin-subject-1"
				edit(c)
			})
			h.expectFailure(w, http.StatusForbidden, ReasonNotPermitted)
			_, denials := h.hooks.snapshot()
			if want := h.idp.issuer + "|admin-subject-1|" + ReasonNotPermitted; len(denials) != 1 || denials[0] != want {
				t.Fatalf("denials = %v, want [%s]", denials, want)
			}
		})
	}
}

type fixedHooks struct {
	recordingHooks
	sess Session
}

func (f *fixedHooks) CompleteLogin(ctx context.Context, l VerifiedLogin, jit bool) (Session, error) {
	return f.sess, nil
}

func TestCallbackHookOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not provisioned", fmt.Errorf("identity: %w", ErrNotProvisioned), http.StatusForbidden, "not_provisioned"},
		{"disabled", fmt.Errorf("identity: %w", ErrUserDisabled), http.StatusForbidden, "user_disabled"},
		{"store down", errors.New("database is locked: secret-detail-marker"), http.StatusServiceUnavailable, "login_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.hooks.loginErr = c.err
			h.expectFailure(h.login(), c.status, c.code)
			if strings.Contains(h.logs.String(), "secret-detail-marker") {
				t.Fatal("hook error text logged")
			}
		})
	}
	for name, sess := range map[string]func(now time.Time) Session{
		"empty secret": func(now time.Time) Session { return Session{ExpiresAt: now.Add(time.Hour)} },
		"short secret": func(now time.Time) Session { return Session{Secret: "abc", ExpiresAt: now.Add(time.Hour)} },
		"cookie-unsafe": func(now time.Time) Session {
			return Session{Secret: randToken() + ";Domain=x", ExpiresAt: now.Add(time.Hour)}
		},
		"already expired": func(now time.Time) Session { return Session{Secret: randToken(), ExpiresAt: now} },
	} {
		t.Run(name, func(t *testing.T) {
			var fh *fixedHooks
			h := newHarness(t, func(cfg *Config) {
				fh = &fixedHooks{sess: sess(cfg.Clock.Now())}
				cfg.Hooks = fh
			})
			w := h.login()
			if w.Code != http.StatusServiceUnavailable || w.Body.String() != "login failed: login_failed\n" {
				t.Fatalf("got %d %q", w.Code, w.Body.String())
			}
			if _, ok := setCookies(w)[SessionCookieName]; ok {
				t.Fatal("unsafe session cookie set")
			}
		})
	}
}

func TestCallbackExchangeConcurrencyIsBounded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxConcurrentExchanges = 1 })
	h.svc.exchangeWait = 50 * time.Millisecond
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.idp.mu.Lock()
	h.idp.tokenRaw = func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		http.Error(w, "late", http.StatusBadGateway)
	}
	h.idp.mu.Unlock()

	_, loc1, c1 := h.startLogin()
	q1 := h.idp.authorize(t, loc1)
	_, loc2, c2 := h.startLogin()
	q2 := h.idp.authorize(t, loc2)

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- h.callback(q1, c1) }()
	<-entered
	w := h.callback(q2, c2)
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "login failed: busy\n" {
		t.Fatalf("second exchange: %d %q", w.Code, w.Body.String())
	}
	if n := h.idp.hitCount("/tenant/token"); n != 1 {
		t.Fatalf("token hits = %d, want 1", n)
	}
	close(release)
	<-done
}

func TestCallbackSanitizesInformationalClaims(t *testing.T) {
	cases := []struct {
		name                   string
		edit                   func(map[string]any)
		wantEmail, wantDisplay string
	}{
		{"control chars stripped", func(c map[string]any) { c["name"] = "Eve\x1b[31m\nAdmin" }, "user@example.test", "Eve[31mAdmin"},
		{"long name truncated on rune boundary", func(c map[string]any) { c["name"] = strings.Repeat("é", 100) }, "user@example.test", strings.Repeat("é", 64)},
		{"preferred_username fallback", func(c map[string]any) {
			delete(c, "name")
			c["preferred_username"] = "eve"
		}, "user@example.test", "eve"},
		{"non-string ignored", func(c map[string]any) {
			c["name"] = 5
			c["email"] = []string{"a@b"}
		}, "", ""},
		{"oversized email dropped", func(c map[string]any) { c["email"] = strings.Repeat("a", 250) + "@example.test" }, "", "Test User"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			if w := h.loginWith(c.edit); w.Code != http.StatusSeeOther {
				t.Fatalf("login: %d %q", w.Code, w.Body.String())
			}
			logins, _ := h.hooks.snapshot()
			if logins[0].Email != c.wantEmail || logins[0].DisplayName != c.wantDisplay {
				t.Fatalf("email %q display %q", logins[0].Email, logins[0].DisplayName)
			}
		})
	}
}

func TestCallbackOnlyOnCanonicalHost(t *testing.T) {
	h := newHarness(t)
	_, loc, c := h.startLogin()
	q := h.idp.authorize(t, loc)
	r := httptest.NewRequest(http.MethodGet, "https://evil.example.test"+CallbackPath+"?"+q.Encode(), nil)
	r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	h.expectFailure(h.do(r), http.StatusBadRequest, "bad_request")
	if n := h.idp.hitCount("/tenant/token"); n != 0 {
		t.Fatalf("token endpoint hit %d", n)
	}
	// The real flow is untouched by the stray request.
	if w := h.callback(q, c); w.Code != http.StatusSeeOther {
		t.Fatalf("canonical callback: %d", w.Code)
	}
}

func TestCallbackRefusesOversizedIDToken(t *testing.T) {
	h := newHarness(t)
	h.expectFailure(h.loginWith(func(c map[string]any) { c["pad"] = strings.Repeat("p", 70<<10) }), http.StatusBadGateway, "token_invalid")
}
