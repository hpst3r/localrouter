package weblogin

// Independent publication-review probes (login-audit). Scratch only; run via
// go test -overlay. Each test states the desired behaviour it checks.

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// F2: 1000 anonymous flows from ONE identical RemoteAddr (shared NGINX, same
// ephemeral port reported) store nothing server-side, and a real browser
// behind the same address still redeems.
func TestAuditFloodIdenticalRemoteAddrNoState(t *testing.T) {
	h := newHarness(t)
	const peer = "10.0.0.1:443"
	for i := 0; i < 1000; i++ {
		w, _, c := h.startLoginFrom(peer, "X-Forwarded-For", "203.0.113.9")
		if w.Code != http.StatusFound || c == nil {
			t.Fatalf("flood %d: %d", i, w.Code)
		}
	}
	if n := h.svc.redeemed.len(); n != 0 {
		t.Fatalf("redeem table after anonymous flood: %d", n)
	}
	if len(h.svc.exchanges) != 0 {
		t.Fatalf("exchange slots held after flood: %d", len(h.svc.exchanges))
	}
	_, loc, ck := h.startLoginFrom(peer)
	if w := h.callback(h.idp.authorize(t, loc), ck); w.Code != http.StatusSeeOther {
		t.Fatalf("legit login behind same peer: %d %q", w.Code, w.Body.String())
	}
	if n := h.idp.hitCount("/tenant/token"); n != 1 {
		t.Fatalf("token hits = %d, want 1", n)
	}
}

// Every pre-exchange refusal: never reaches the token endpoint, never spends
// the genuine flow, logs nothing but fixed codes.
func TestAuditPreExchangeRefusalsNeverReachIdP(t *testing.T) {
	h := newHarness(t)
	_, locA, ckA := h.startLogin()
	_, locB, ckB := h.startLogin()
	qA, qB := h.idp.authorize(t, locA), h.idp.authorize(t, locB)
	_, foreign := newFlowSealer(testPublicURL).newFlow(h.clock.Now(), h.svc.cfg.LoginTimeout)
	// Same key as the service but for another origin cannot be built from
	// outside; emulate a cookie minted by a sibling Service on another host.
	other := newHarness(t, func(c *Config) { c.PublicBaseURL = "https://other.example.test" })
	_, otherVal := other.svc.sealer.newFlow(h.clock.Now(), h.svc.cfg.LoginTimeout)
	ckOther := &http.Cookie{Name: LoginCookieName, Value: otherVal}

	req := func(host, rawQuery string, cookieHdr ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "https://"+host+CallbackPath+"?"+rawQuery, nil)
		for _, c := range cookieHdr {
			r.Header.Add("Cookie", c)
		}
		return r
	}
	pub := strings.TrimPrefix(testPublicURL, "https://")
	cA := LoginCookieName + "=" + ckA.Value
	cases := map[string]struct {
		r    *http.Request
		code string
	}{
		"wrong host":              {req("evil.example.test", qA.Encode(), cA), "bad_request"},
		"host with port 8443":     {req(pub+":8443", qA.Encode(), cA), "bad_request"},
		"mixed: A cookie B state": {req(pub, qB.Encode(), cA), "login_csrf"},
		"mixed: B cookie A state": {req(pub, qA.Encode(), LoginCookieName+"="+ckB.Value), "login_csrf"},
		"foreign key":             {req(pub, qA.Encode(), LoginCookieName+"="+foreign), "login_csrf"},
		"sibling service cookie":  {req(pub, qA.Encode(), LoginCookieName+"="+ckOther.Value), "login_csrf"},
		"no cookie":               {req(pub, qA.Encode()), "login_csrf"},
		"cookie wrong name":       {req(pub, qA.Encode(), "lr_login="+ckA.Value), "login_csrf"},
		"genuine + garbage":       {req(pub, qA.Encode(), cA, LoginCookieName+"=x"), "login_csrf"},
		"garbage + genuine":       {req(pub, qA.Encode(), LoginCookieName+"=x", cA), "login_csrf"},
		"A + B cookies":           {req(pub, qA.Encode(), cA+"; "+LoginCookieName+"="+ckB.Value), "login_csrf"},
		"dup state":               {req(pub, qA.Encode()+"&state="+qA.Get("state"), cA), "bad_request"},
		"dup code":                {req(pub, qA.Encode()+"&code=x", cA), "bad_request"},
		"error with code":         {req(pub, qA.Encode()+"&error=server_error", cA), "idp_error"},
		"empty error":             {req(pub, qA.Encode()+"&error=", cA), "idp_error"},
		"state 42 chars":          {req(pub, "code=c&state="+qA.Get("state")[:42], cA), "bad_request"},
		"code with space":         {req(pub, "code=a%20b&state="+qA.Get("state"), cA), "bad_request"},
	}
	for name, c := range cases {
		w := h.do(c.r)
		if !strings.HasSuffix(w.Body.String(), c.code+"\n") {
			t.Errorf("%s: %d %q, want %s", name, w.Code, w.Body.String(), c.code)
		}
		if n := h.idp.hitCount("/tenant/token"); n != 0 {
			t.Fatalf("%s reached the token endpoint (%d)", name, n)
		}
		// Every callback response clears the login cookie (Max-Age<0).
		if sc := w.Header().Values("Set-Cookie"); len(sc) == 0 || !strings.Contains(sc[0], LoginCookieName+"=;") {
			t.Errorf("%s: login cookie not cleared: %q", name, sc)
		}
	}
	if n := h.svc.redeemed.len(); n != 0 {
		t.Fatalf("refusals left %d entries", n)
	}
	for name, f := range map[string]struct {
		q url.Values
		c *http.Cookie
	}{"A": {qA, ckA}, "B": {qB, ckB}} {
		if w := h.callback(f.q, f.c); w.Code != http.StatusSeeOther {
			t.Fatalf("genuine %s after attacks: %d %q", name, w.Code, w.Body.String())
		}
	}
	logs := h.logs.String()
	for _, s := range []string{ckA.Value, ckB.Value, foreign, ckOther.Value, qA.Get("state"), qA.Get("code"), qB.Get("code"), "evil.example.test"} {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain a secret/attacker value %q", s[:min(12, len(s))])
		}
	}
	if m := longToken.FindString(logs); m != "" {
		t.Errorf("token-like value in logs %q", m)
	}
}

// A browser that disconnects mid-exchange releases its slot and its
// in-flight entry, and the flow can still complete with a fresh code.
func TestAuditCanceledExchangeReleasesSlotAndEntry(t *testing.T) {
	h := newHarness(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.idp.tokenGate = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	_, loc, ck := h.startLogin()
	q := h.idp.authorize(t, loc)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodGet, testPublicURL+CallbackPath+"?"+q.Encode(), nil).WithContext(ctx)
	r.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.do(r) }()
	<-entered
	if h.svc.redeemed.len() != 1 || len(h.svc.exchanges) != 1 {
		t.Fatalf("in flight: entries %d slots %d", h.svc.redeemed.len(), len(h.svc.exchanges))
	}
	cancel()
	w := <-done
	close(release)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("canceled exchange: %d %q", w.Code, w.Body.String())
	}
	if h.svc.redeemed.len() != 0 || len(h.svc.exchanges) != 0 {
		t.Fatalf("after cancel: entries %d slots %d", h.svc.redeemed.len(), len(h.svc.exchanges))
	}
	h.idp.tokenGate = nil
	if w := h.callback(h.idp.authorize(t, loc), ck); w.Code != http.StatusSeeOther {
		t.Fatalf("retry with fresh code: %d %q", w.Code, w.Body.String())
	}
}

// Each seal draws a fresh GCM nonce and fresh flow secrets; each Service
// draws its own key.
func TestAuditSealNonceAndKeyFreshness(t *testing.T) {
	s := newFlowSealer(testPublicURL)
	now := time.Unix(1_800_000_000, 0)
	nonces := map[string]bool{}
	for i := 0; i < 20000; i++ {
		_, v := s.newFlow(now, 10*time.Minute)
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || len(raw) != flowSealedLen || raw[0] != flowVersion {
			t.Fatalf("layout: %d %v", len(raw), err)
		}
		n := string(raw[1:13])
		if nonces[n] {
			t.Fatalf("GCM nonce repeated after %d seals", i)
		}
		nonces[n] = true
	}
	a, b := newFlowSealer(testPublicURL), newFlowSealer(testPublicURL)
	_, v := a.newFlow(now, time.Minute)
	if _, ok := b.open(v); ok {
		t.Fatal("two sealers share a key")
	}
	if _, ok := a.open(v); !ok {
		t.Fatal("control: own cookie must open")
	}
}

// Desired: prompt=login and max_age=0 on every authorization request, PKCE
// S256, no verifier in the URL, login cookie __Host-, Secure, HttpOnly,
// Path=/, SameSite=Lax, Max-Age = LoginTimeout.
func TestAuditAuthorizeRequestAndCookieAttributes(t *testing.T) {
	h := newHarness(t)
	w, loc, ck := h.startLogin()
	u, _ := url.Parse(loc)
	q := u.Query()
	for k, want := range map[string]string{"prompt": "login", "max_age": "0", "code_challenge_method": "S256", "response_type": "code"} {
		if q.Get(k) != want || len(q[k]) != 1 {
			t.Errorf("%s = %q", k, q[k])
		}
	}
	if q.Has("code_verifier") || q.Get("redirect_uri") != testPublicURL+CallbackPath {
		t.Errorf("authorize query %v", q)
	}
	sc := w.Header().Get("Set-Cookie")
	for _, attr := range []string{"Path=/", "Max-Age=600", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(sc, attr) {
			t.Errorf("login cookie lacks %s: %q", attr, sc)
		}
	}
	if strings.Contains(strings.ToLower(sc), "domain=") || ck.Name != "__Host-lr_login" {
		t.Errorf("cookie scope: %q", sc)
	}
}

// RESIDUAL demonstration (expected to PASS, documenting accepted behaviour):
// one IdP account that the policy DENIES can fill the default 256-entry
// redemption cache within LoginTimeout and refuse every other login with 503
// until those flows expire.
func TestAuditResidualDeniedAccountFillsRedeemCache(t *testing.T) {
	h := newHarness(t)
	h.idp.claimsEdit = func(c map[string]any) { c["roles"] = []string{"NotAllowed"}; c["sub"] = "attacker" }
	for i := 0; i < 256; i++ {
		if w := h.login(); w.Code != http.StatusForbidden {
			t.Fatalf("denied login %d: %d %q", i, w.Code, w.Body.String())
		}
	}
	h.idp.claimsEdit = nil
	if w := h.login(); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("legit login while cache full: %d", w.Code)
	}
	h.clock.Advance(10*time.Minute + time.Second)
	if w := h.login(); w.Code != http.StatusSeeOther {
		t.Fatalf("legit login after expiry: %d", w.Code)
	}
}

// Desired: a full cache never evicts an IN-FLIGHT entry, however old, so a
// concurrent replay can never start a second exchange for that state.
// (Gap found by mutant m7: no existing test pins this.)
func TestAuditFullCacheNeverEvictsInFlight(t *testing.T) {
	tb := newRedeemTable(1)
	t0 := time.Unix(1_800_000_000, 0)
	if tb.begin("X", t0.Add(time.Minute), t0) != redeemBegun {
		t.Fatal("begin X")
	}
	if got := tb.begin("Y", t0.Add(time.Hour), t0.Add(30*time.Minute)); got != redeemFull {
		t.Fatalf("full table with an expired in-flight entry admitted Y: %v", got)
	}
	if tb.begin("X", t0.Add(time.Minute), t0) != redeemReplay {
		t.Fatal("in-flight X evicted")
	}

	h := newHarness(t, func(c *Config) { c.MaxRedeemedLogins = 1 })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.idp.tokenGate = func() {
		first := false
		once.Do(func() { first = true; close(entered) })
		if first {
			<-release
		}
	}
	_, locX, ckX := h.startLogin()
	qX := h.idp.authorize(t, locX)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.callback(qX, ckX) }()
	<-entered
	h.clock.Advance(11 * time.Minute) // X's flow has expired while in flight
	_, locY, ckY := h.startLogin()
	if w := h.callback(h.idp.authorize(t, locY), ckY); w.Code != http.StatusServiceUnavailable {
		t.Errorf("Y while X in flight in a full table: %d %q", w.Code, w.Body.String())
	}
	close(release)
	<-done
	if n := h.idp.hitCount("/tenant/token"); n != 1 {
		t.Errorf("token hits = %d, want 1 (only X)", n)
	}
}
