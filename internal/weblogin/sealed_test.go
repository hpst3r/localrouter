package weblogin

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// randToken returns 32 random bytes as 43 base64url characters.
func randToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return b64(b)
}

// The audit repro, behind a reverse proxy: every browser and the attacker
// share one TCP peer. Pending flows live in the browser's sealed cookie, so
// an anonymous flood holds no server state and cannot refuse anyone.
func TestLoginFloodBehindSharedProxyStillLogsIn(t *testing.T) {
	h := newHarness(t)
	const proxy = "10.0.0.1:"
	for i := 0; i < 1000; i++ {
		w, _, c := h.startLoginFrom(proxy + strconv.Itoa(1024+i))
		if w.Code != http.StatusFound || c == nil {
			t.Fatalf("flood request %d: %d %q", i, w.Code, w.Body.String())
		}
	}
	if n := h.svc.redeemed.len(); n != 0 {
		t.Fatalf("server state after an anonymous flood: %d entries", n)
	}
	h.clock.Advance(9 * time.Minute)
	// Two real browsers behind the same proxy.
	wa, locA, ca := h.startLoginFrom(proxy + "5000")
	wb, locB, cb := h.startLoginFrom(proxy + "5001")
	if wa.Code != http.StatusFound || wb.Code != http.StatusFound || ca.Value == cb.Value {
		t.Fatalf("real logins: %d %d", wa.Code, wb.Code)
	}
	qa, qb := h.idp.authorize(t, locA), h.idp.authorize(t, locB)
	// B's callback in A's browser is login CSRF and does not burn B's flow.
	w := h.callback(qb, ca)
	if w.Code != http.StatusBadRequest || w.Body.String() != "login failed: login_csrf\n" {
		t.Fatalf("cross-browser callback: %d %q", w.Code, w.Body.String())
	}
	if n := h.idp.hitCount("/tenant/token"); n != 0 {
		t.Fatalf("cross-browser callback reached the token endpoint: %d", n)
	}
	for name, f := range map[string]struct {
		q url.Values
		c *http.Cookie
	}{"A": {qa, ca}, "B": {qb, cb}} {
		if w := h.callback(f.q, f.c); w.Code != http.StatusSeeOther {
			t.Fatalf("browser %s: %d %q", name, w.Code, w.Body.String())
		}
	}
	if logins, _ := h.hooks.snapshot(); len(logins) != 2 {
		t.Fatalf("CompleteLogin calls = %d, want 2", len(logins))
	}
	if f := h.idp.failureList(); len(f) != 0 {
		t.Fatalf("fake IdP rejected an exchange: %v", f)
	}
}

// A redeemed state is remembered until its flow expires: replaying the
// callback, or bringing a second code minted for the same authorization
// request (an IdP that does not enforce single-use codes, or a reloaded
// authorize URL), never reaches the token endpoint or a hook again.
func TestCallbackReplayNeverRunsHooksTwice(t *testing.T) {
	for name, c := range map[string]struct {
		edit   func(map[string]any)
		status int
	}{
		"admitted": {nil, http.StatusSeeOther},
		"denied":   {func(c map[string]any) { c["roles"] = []string{"Reader"} }, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.idp.claimsEdit = c.edit
			_, loc, ck := h.startLogin()
			q := h.idp.authorize(t, loc)
			if w := h.callback(q, ck); w.Code != c.status {
				t.Fatalf("first callback: %d %q", w.Code, w.Body.String())
			}
			for _, replay := range []url.Values{q, h.idp.authorize(t, loc)} {
				w := h.callback(replay, ck)
				if w.Code != http.StatusBadRequest || w.Body.String() != "login failed: login_expired\n" {
					t.Fatalf("replay: %d %q", w.Code, w.Body.String())
				}
			}
			if n := h.idp.hitCount("/tenant/token"); n != 1 {
				t.Fatalf("token endpoint hits = %d, want 1", n)
			}
			if logins, denials := h.hooks.snapshot(); len(logins)+len(denials) != 1 {
				t.Fatalf("hooks ran %d+%d times, want once", len(logins), len(denials))
			}
		})
	}
}

// Concurrent callbacks for one state: one exchanges, the others are refused
// before the token endpoint, and the hooks run once.
func TestCallbackConcurrentSameStateRunsHooksOnce(t *testing.T) {
	h := newHarness(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.idp.tokenGate = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	_, loc, ck := h.startLogin()
	q := h.idp.authorize(t, loc)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- h.callback(q, ck) }()
	<-entered
	var wg sync.WaitGroup
	codes := make(chan string, 7)
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := h.callback(q, ck)
			codes <- strconv.Itoa(w.Code) + " " + w.Body.String()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("concurrent callbacks waited on the in-flight exchange")
	}
	close(release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != "400 login failed: login_expired\n" {
			t.Errorf("concurrent callback: %q", c)
		}
	}
	if w := <-first; w.Code != http.StatusSeeOther {
		t.Fatalf("first callback: %d %q", w.Code, w.Body.String())
	}
	if n := h.idp.hitCount("/tenant/token"); n != 1 {
		t.Fatalf("token endpoint hits = %d, want 1", n)
	}
	if logins, _ := h.hooks.snapshot(); len(logins) != 1 {
		t.Fatalf("CompleteLogin calls = %d, want 1", len(logins))
	}
}

// Only states the IdP vouched for are remembered. Anonymous callbacks with
// bogus codes are forgotten when their exchange fails, so they cannot fill
// the cache, and the flow itself can still complete.
func TestCallbackFailedExchangeIsForgotten(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxRedeemedLogins = 1 })
	for i := 0; i < 50; i++ {
		_, loc, ck := h.startLoginFrom("10.0.0.1:1")
		bogus := url.Values{"code": {"bogus-" + strconv.Itoa(i)}, "state": h.idp.authorize(t, loc)["state"]}
		h.expectFailure(h.callback(bogus, ck), http.StatusBadGateway, "exchange_failed")
	}
	if n := h.svc.redeemed.len(); n != 0 {
		t.Fatalf("failed exchanges left %d entries", n)
	}
	_, loc, ck := h.startLoginFrom("10.0.0.1:1")
	q := h.idp.authorize(t, loc)
	h.expectFailure(h.callback(url.Values{"code": {"bogus"}, "state": q["state"]}, ck), http.StatusBadGateway, "exchange_failed")
	if w := h.callback(q, ck); w.Code != http.StatusSeeOther {
		t.Fatalf("flow after a failed exchange: %d %q", w.Code, w.Body.String())
	}
}

// The cache is bounded and never forgets a live state: when it is full of
// recently redeemed states, callbacks fail closed before the token endpoint
// until the oldest flows expire.
func TestCallbackRedeemedCacheIsBoundedAndFailsClosed(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxRedeemedLogins = 2 })
	if w := h.login(); w.Code != http.StatusSeeOther {
		t.Fatalf("login 1: %d", w.Code)
	}
	h.clock.Advance(time.Minute)
	if w := h.loginWith(func(c map[string]any) { c["roles"] = []string{"Reader"} }); w.Code != http.StatusForbidden {
		t.Fatalf("denied login 2: %d", w.Code)
	}
	h.idp.claimsEdit = nil
	w := h.login()
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "login failed: busy\n" {
		t.Fatalf("full cache: %d %q", w.Code, w.Body.String())
	}
	if n := h.idp.hitCount("/tenant/token"); n != 2 {
		t.Fatalf("token endpoint hits = %d, want 2", n)
	}
	// The first state expires and frees one entry; the second is still live.
	h.clock.Advance(9*time.Minute + time.Second)
	if w := h.login(); w.Code != http.StatusSeeOther {
		t.Fatalf("after the oldest flow expired: %d %q", w.Code, w.Body.String())
	}
	if w := h.login(); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("still full: %d", w.Code)
	}
	if n := h.svc.redeemed.len(); n != 2 {
		t.Fatalf("redeemed states = %d, want the cap 2", n)
	}
}

// expectCSRF asserts a login_csrf refusal that never reached the token
// endpoint.
func (h *harness) expectCSRF(w *httptest.ResponseRecorder, what string) {
	h.t.Helper()
	if w.Code != http.StatusBadRequest || w.Body.String() != "login failed: login_csrf\n" {
		h.t.Fatalf("%s: %d %q, want 400 login_csrf", what, w.Code, w.Body.String())
	}
	if n := h.idp.hitCount("/tenant/token"); n != 0 {
		h.t.Fatalf("%s: token endpoint hits = %d", what, n)
	}
}

func withCookieValue(c *http.Cookie, v string) *http.Cookie {
	return &http.Cookie{Name: c.Name, Value: v}
}

// Every modification of a sealed cookie fails authentication, closed, before
// any outbound call; the untouched cookie still completes afterwards.
func TestSealedCookieTamperFailsClosed(t *testing.T) {
	h := newHarness(t)
	_, loc, ck := h.startLogin()
	q := h.idp.authorize(t, loc)
	raw, err := base64.RawURLEncoding.DecodeString(ck.Value)
	if err != nil || len(raw) != flowSealedLen || len(ck.Value) != flowCookieLen {
		t.Fatalf("cookie: %d chars, %d bytes, %v", len(ck.Value), len(raw), err)
	}
	// Every bit of version, GCM nonce, ciphertext and tag.
	for i := range raw {
		for bit := 0; bit < 8; bit++ {
			b := bytes.Clone(raw)
			b[i] ^= 1 << bit
			h.expectCSRF(h.callback(q, withCookieValue(ck, base64.RawURLEncoding.EncodeToString(b))), "flipped bit")
		}
	}
	v := ck.Value
	for name, bad := range map[string]string{
		"truncated":         v[:len(v)-1],
		"extended":          v + "A",
		"padded":            v + "==",
		"std alphabet":      strings.NewReplacer("-", "+", "_", "/").Replace(v),
		"oversized":         strings.Repeat(v, 400),
		"empty":             "",
		"plain state value": q.Get("state"),
		"swapped halves":    v[94:] + v[:94],
	} {
		if bad == v {
			continue // no '-' or '_' to swap in this cookie
		}
		h.expectCSRF(h.callback(q, withCookieValue(ck, bad)), name)
	}
	// Decoders skip CR/LF; the exact length check still refuses them.
	for _, bad := range []string{v[:100] + "\n" + v[101:], v[:100] + "\r\n" + v[102:], v[:187] + "\n"} {
		if _, ok := h.svc.sealer.open(bad); ok {
			t.Fatal("cookie with a line break opened")
		}
	}
	// Two login cookies are refused even if one is genuine.
	r := httptest.NewRequest(http.MethodGet, testPublicURL+CallbackPath+"?"+q.Encode(), nil)
	r.Header.Add("Cookie", LoginCookieName+"="+v+"; "+LoginCookieName+"="+v)
	h.expectCSRF(h.do(r), "duplicate cookie")
	if w := h.callback(q, ck); w.Code != http.StatusSeeOther {
		t.Fatalf("genuine cookie after tamper attempts: %d %q", w.Code, w.Body.String())
	}
}

// A cookie sealed under another key (another process, or this one before a
// restart) or for another public origin does not open.
func TestSealedCookieBoundToProcessKeyAndOrigin(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	other := newFlowSealer(testPublicURL)
	f, v := other.newFlow(now, h.svc.cfg.LoginTimeout)
	h.expectCSRF(h.callback(url.Values{"code": {"c"}, "state": {f.state}}, &http.Cookie{Name: LoginCookieName, Value: v}), "other key")
	// Same key, other origin: the AAD binds the cookie to PublicBaseURL.
	key := bytes.Repeat([]byte{7}, 32)
	here := newFlowSealerKey(key, testPublicURL)
	f, v = newFlowSealerKey(key, "https://other.example.test").newFlow(now, time.Minute)
	if _, ok := here.open(v); ok {
		t.Fatal("cookie sealed for another origin opened")
	}
	// Control: the same key and origin do open, so the refusal above is the
	// origin and not the construction.
	f, v = newFlowSealerKey(key, testPublicURL).newFlow(now, time.Minute)
	if got, ok := here.open(v); !ok || got.state != f.state || !got.created.Equal(now) || !got.expires.Equal(now.Add(time.Minute)) {
		t.Fatalf("control cookie: %v", ok)
	}
}

// The flow is valid for exactly LoginTimeout from its start, and never
// before it (a clock stepped back).
func TestSealedCookieExpiryBoundaries(t *testing.T) {
	for name, c := range map[string]struct {
		advance time.Duration
		ok      bool
	}{
		"at expiry":       {10 * time.Minute, true},
		"just after":      {10*time.Minute + time.Nanosecond, false},
		"clock went back": {-time.Nanosecond, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			_, loc, ck := h.startLogin()
			q := h.idp.authorize(t, loc)
			h.clock.Advance(c.advance)
			w := h.callback(q, ck)
			if c.ok {
				if w.Code != http.StatusSeeOther {
					t.Fatalf("got %d %q", w.Code, w.Body.String())
				}
				return
			}
			h.expectFailure(w, http.StatusBadRequest, "login_expired")
			if n := h.idp.hitCount("/tenant/token"); n != 0 {
				t.Fatalf("token endpoint hits = %d", n)
			}
		})
	}
}

// The cookie is a fixed, small size and carries none of the flow's secrets
// in the clear; every login draws fresh ones.
func TestSealedCookieSizeAndSecrecy(t *testing.T) {
	h := newHarness(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		w, loc, ck := h.startLogin()
		if set := w.Header().Get("Set-Cookie"); len(set) > 300 || len(ck.Value) != flowCookieLen {
			t.Fatalf("Set-Cookie is %d bytes, value %d", len(set), len(ck.Value))
		}
		u, _ := url.Parse(loc)
		f, ok := h.svc.sealer.open(ck.Value)
		if !ok || f.state != u.Query().Get("state") || f.nonce != u.Query().Get("nonce") {
			t.Fatal("sealed flow does not match the authorization request")
		}
		raw, _ := base64.RawURLEncoding.DecodeString(ck.Value)
		for _, secret := range []string{f.state, f.nonce, f.verifier} {
			dec, _ := base64.RawURLEncoding.DecodeString(secret)
			if strings.Contains(ck.Value, secret) || bytes.Contains(raw, dec) || seen[secret] {
				t.Fatal("flow secret visible in the cookie or reused")
			}
			seen[secret] = true
		}
		if strings.Contains(loc, f.verifier) {
			t.Fatal("PKCE verifier in the authorization request")
		}
	}
}

// Many browsers behind one address complete concurrently, each exactly once,
// while each also replays its callback.
func TestConcurrentLoginsBehindSharedAddress(t *testing.T) {
	h := newHarness(t)
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, testPublicURL+LoginPath, nil)
			r.RemoteAddr = "10.0.0.1:443"
			w := h.do(r)
			var ck *http.Cookie
			for _, c := range w.Result().Cookies() {
				ck = c
			}
			q := h.idp.authorize(t, w.Header().Get("Location"))
			var inner sync.WaitGroup
			for j := 0; j < 2; j++ {
				inner.Add(1)
				go func() {
					defer inner.Done()
					if w := h.callback(q, ck); w.Code == http.StatusSeeOther {
						mu.Lock()
						ok++
						mu.Unlock()
					}
				}()
			}
			inner.Wait()
		}()
	}
	wg.Wait()
	logins, _ := h.hooks.snapshot()
	if ok != n || len(logins) != n {
		t.Fatalf("completed %d, CompleteLogin %d, want %d each", ok, len(logins), n)
	}
	if got := h.svc.redeemed.len(); got != n {
		t.Fatalf("redeemed states = %d, want %d", got, n)
	}
}

// An ID token that was not issued for this flow's nonce (an injected code
// from another authorization) is not remembered: it cannot spend the
// victim's state, and the victim's own code still completes.
func TestCallbackWrongNonceDoesNotSpendState(t *testing.T) {
	h := newHarness(t)
	_, loc, ck := h.startLogin()
	h.idp.claimsEdit = func(c map[string]any) { c["nonce"] = randToken() }
	h.expectFailure(h.callback(h.idp.authorize(t, loc), ck), http.StatusBadGateway, "token_invalid")
	if n := h.svc.redeemed.len(); n != 0 {
		t.Fatalf("unverified token left %d entries", n)
	}
	h.idp.claimsEdit = nil
	if w := h.callback(h.idp.authorize(t, loc), ck); w.Code != http.StatusSeeOther {
		t.Fatalf("victim's own code: %d %q", w.Code, w.Body.String())
	}
}

// A sealed flow must span exactly LoginTimeout; anything else is refused
// even though it authenticates.
func TestSealedCookieLifetimeMustMatchLoginTimeout(t *testing.T) {
	h := newHarness(t)
	f, v := h.svc.sealer.newFlow(h.clock.Now(), h.svc.cfg.LoginTimeout+time.Minute)
	h.expectFailure(h.callback(url.Values{"code": {"c"}, "state": {f.state}}, &http.Cookie{Name: LoginCookieName, Value: v}),
		http.StatusBadRequest, "login_expired")
}

// A value of the wrong length is refused before it is decoded: an oversized
// cookie costs no allocation.
func TestSealedCookieLengthCheckedBeforeDecoding(t *testing.T) {
	s := newFlowSealer(testPublicURL)
	big := strings.Repeat("A", 64<<10)
	if n := testing.AllocsPerRun(10, func() { s.open(big) }); n != 0 {
		t.Fatalf("opening a 64 KiB value allocated %v times", n)
	}
}
