package weblogin

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Any base64url run this long is a state, nonce, verifier, code, binding,
// session secret or JWT segment.
var longToken = regexp.MustCompile(`[A-Za-z0-9_-]{20,}`)

func TestLogsNeverContainSecretsOrIdentity(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Outbound.Timeout = 300 * time.Millisecond })
	// Success, then a spread of failures touching every code path.
	h.login()
	h.loginWith(func(c map[string]any) { c["iss"] = "https://wrong.example.test/" })
	h.loginWith(func(c map[string]any) { c["aud"] = "other-client-with-a-long-identifier" })
	h.loginWith(func(c map[string]any) { c["nonce"] = "wrong" })
	h.loginWith(func(c map[string]any) { c["roles"] = "Nope" })
	h.loginWith(func(c map[string]any) { delete(c, "auth_time") })
	h.loginWith(nil)
	_, loc, c := h.startLogin()
	q := h.idp.authorize(t, loc)
	q.Set("error", "access_denied")
	q.Set("error_description", "user user@example.test said no")
	h.callback(q, c)
	h.idp.tokenRaw = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"raw-idp-body-marker code ` + r.FormValue("code") + `"}`))
	}
	h.login()
	// Sealed-flow failures: tampered, foreign and replayed cookies.
	h.idp.tokenRaw = nil
	_, loc, c = h.startLogin()
	q = h.idp.authorize(t, loc)
	h.callback(q, &http.Cookie{Name: LoginCookieName, Value: c.Value[:len(c.Value)-2] + "AA"})
	_, foreign := newFlowSealer(testPublicURL).newFlow(h.clock.Now(), 10*time.Minute)
	h.callback(q, &http.Cookie{Name: LoginCookieName, Value: foreign})
	h.callback(q, c)
	h.callback(q, c)
	h.logout(nil)

	logs := h.logs.String()
	if logs == "" {
		t.Fatal("no logs captured")
	}
	for _, code := range []string{"code=login_csrf", "code=login_expired", "code=exchange_failed", "code=token_invalid"} {
		if !strings.Contains(logs, code) {
			t.Errorf("path not exercised: %s", code)
		}
	}
	for _, s := range []string{
		testClientSecret, testSubject, "user@example.test", "Test User", "access-token-marker",
		"eyJ", idpHost, "raw-idp-body-marker", "router.example.test", "said no", "session-secret-1", "csrf-1", "?", "http",
	} {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain %q", s)
		}
	}
	if m := longToken.FindString(logs); m != "" {
		t.Errorf("logs contain token-like value %q", m)
	}
	if t.Failed() || testing.Verbose() {
		t.Log(logs)
	}
}
