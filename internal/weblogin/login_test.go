package weblogin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func (h *harness) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

// startLogin performs GET /auth/login and returns the response, the
// authorization URL and the login binding cookie.
func (h *harness) startLogin() (*httptest.ResponseRecorder, string, *http.Cookie) {
	h.t.Helper()
	w := h.do(httptest.NewRequest(http.MethodGet, testPublicURL+LoginPath, nil))
	var c *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == LoginCookieName {
			c = ck
		}
	}
	return w, w.Header().Get("Location"), c
}

func TestLoginRedirectsWithPKCEStateNonceAndBindingCookie(t *testing.T) {
	h := newHarness(t)
	w, loc, _ := h.startLogin()
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body %q", w.Code, w.Body.String())
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("location: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != h.idp.url("/tenant/authorize") {
		t.Fatalf("authorize endpoint = %q", got)
	}
	q := u.Query()
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range q {
			if !yield(k) {
				return
			}
		}
	})
	want := []string{"client_id", "code_challenge", "code_challenge_method", "max_age", "nonce", "prompt", "redirect_uri", "response_type", "scope", "state"}
	if !slices.Equal(keys, want) {
		t.Fatalf("query keys = %v, want %v", keys, want)
	}
	exact := map[string]string{
		"client_id":             testClientID,
		"code_challenge_method": "S256",
		"max_age":               "0",
		"prompt":                "login",
		"redirect_uri":          testRedirectURI,
		"response_type":         "code",
		"scope":                 "openid profile",
	}
	for k, v := range exact {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if !b64Token.MatchString(q.Get(k)) {
			t.Errorf("%s = %q, want 43 base64url chars", k, q.Get(k))
		}
	}
	if q.Get("state") == q.Get("nonce") {
		t.Error("state and nonce are equal")
	}
	set := w.Header().Values("Set-Cookie")
	if len(set) != 1 {
		t.Fatalf("Set-Cookie = %q", set)
	}
	name, rest, _ := strings.Cut(set[0], "=")
	val, attrs, _ := strings.Cut(rest, ";")
	if name != LoginCookieName || len(val) != flowCookieLen || strings.Trim(val, b64Alphabet) != "" {
		t.Fatalf("binding cookie = %q", set[0])
	}
	if attrs != " Path=/; Max-Age=600; HttpOnly; Secure; SameSite=Lax" {
		t.Fatalf("cookie attributes = %q", attrs)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers = %v", w.Header())
	}
}

func TestLoginRedirectsNonCanonicalHost(t *testing.T) {
	h := newHarness(t)
	for _, host := range []string{"127.0.0.1:8080", "evil.example.test", "router.example.test:8443"} {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+LoginPath+"?next=https://evil.example.test", nil)
		w := h.do(r)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != testPublicURL+LoginPath {
			t.Fatalf("%s: %d %q", host, w.Code, w.Header().Get("Location"))
		}
		if len(w.Header().Values("Set-Cookie")) != 0 {
			t.Fatalf("%s: cookie set on non-canonical host", host)
		}
	}
	// Default port and case are canonicalized.
	r := httptest.NewRequest(http.MethodGet, "https://ROUTER.example.test:443"+LoginPath, nil)
	if w := h.do(r); w.Code != http.StatusFound || len(w.Header().Values("Set-Cookie")) != 1 {
		t.Fatalf("canonical host with :443: %d, cookies %q", w.Code, w.Header().Values("Set-Cookie"))
	}
}

func TestLoginProviderUnavailable(t *testing.T) {
	h := newHarness(t)
	h.idp.discoveryRaw = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }
	w, _, c := h.startLogin()
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "login failed: provider_unavailable\n" || c != nil {
		t.Fatalf("got %d %q cookie=%v", w.Code, w.Body.String(), c)
	}
	// Retry gap: the next login inside 30s does not hit the provider.
	h.startLogin()
	if n := h.idp.hitCount("/tenant/.well-known/openid-configuration"); n != 1 {
		t.Fatalf("discovery hits = %d, want 1", n)
	}
}

// startLoginFrom is startLogin from the TCP peer remote ("ip:port").
func (h *harness) startLoginFrom(remote string, hdr ...string) (*httptest.ResponseRecorder, string, *http.Cookie) {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodGet, testPublicURL+LoginPath, nil)
	r.RemoteAddr = remote
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := h.do(r)
	var c *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == LoginCookieName {
			c = ck
		}
	}
	return w, w.Header().Get("Location"), c
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
