package weblogin

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	jose "github.com/go-jose/go-jose/v4"
)

// fakeIdP is a test-only OpenID provider on a loopback TLS server. It signs
// real ID tokens with RSA or ECDSA keys published in its JWKS, and its token
// endpoint enforces what a real provider would: single-use codes, the exact
// redirect URI, the configured client authentication and the PKCE S256
// verifier. Any enforcement failure is recorded in failures.

const (
	testClientID     = "client-1"
	testClientSecret = "s3cret-client-value-0123456789abcdef"
	testSubject      = "user-sub-1"
	testPublicURL    = "https://router.example.test"
	testRedirectURI  = testPublicURL + "/auth/callback"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type signingKey struct {
	kid  string
	alg  string
	priv crypto.Signer
}

var (
	rsaOnce sync.Once
	rsaKeys [2]*rsa.PrivateKey
)

func testRSAKey(i int) *rsa.PrivateKey {
	rsaOnce.Do(func() {
		for j := range rsaKeys {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			rsaKeys[j] = k
		}
	})
	return rsaKeys[i]
}

func newECKey() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}

type grant struct {
	challenge, method, redirectURI, nonce string
	authTime                              time.Time
	used                                  bool
}

type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	issuer string
	clock  *fakeClock

	mu        sync.Mutex
	authStyle string // "basic" or "post"
	published []signingKey
	signer    signingKey
	grants    map[string]*grant
	hits      map[string]int
	failures  []string
	// Hooks for individual tests.
	discoveryEdit func(map[string]any)
	discoveryRaw  http.HandlerFunc
	tokenRaw      http.HandlerFunc
	tokenGate     func() // runs before a token request is processed
	claimsEdit    func(map[string]any)
	sign          func(claims []byte) string
}

func newFakeIdP(t *testing.T, clock *fakeClock) *fakeIdP {
	t.Helper()
	k := signingKey{kid: "rsa-1", alg: "RS256", priv: testRSAKey(0)}
	p := &fakeIdP{
		t:         t,
		clock:     clock,
		authStyle: "basic",
		published: []signingKey{k},
		signer:    k,
		grants:    map[string]*grant{},
		hits:      map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/tenant/.well-known/openid-configuration", p.serveDiscovery)
	mux.HandleFunc("/tenant/jwks", p.serveJWKS)
	mux.HandleFunc("/tenant/token", p.serveToken)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p.count(r.URL.Path)
		http.NotFound(w, r)
	})
	srv, _ := newTestTLSServer(t, mux)
	p.srv = srv
	p.issuer = hostURL(idpHost, srv, "/tenant")
	return p
}

func (p *fakeIdP) count(path string) {
	p.mu.Lock()
	p.hits[path]++
	p.mu.Unlock()
}

func (p *fakeIdP) hitCount(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[path]
}

func (p *fakeIdP) failf(format string, args ...any) {
	p.mu.Lock()
	p.failures = append(p.failures, fmt.Sprintf(format, args...))
	p.mu.Unlock()
}

func (p *fakeIdP) failureList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.failures...)
}

func (p *fakeIdP) url(path string) string { return hostURL(idpHost, p.srv, path) }

func (p *fakeIdP) discoveryDoc() map[string]any {
	return map[string]any{
		"issuer":                                p.issuer,
		"authorization_endpoint":                p.url("/tenant/authorize"),
		"token_endpoint":                        p.url("/tenant/token"),
		"jwks_uri":                              p.url("/tenant/jwks"),
		"userinfo_endpoint":                     p.url("/tenant/userinfo"),
		"response_types_supported":              []string{"code"},
		"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		"subject_types_supported":               []string{"public"},
	}
}

func (p *fakeIdP) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	p.count(r.URL.Path)
	p.mu.Lock()
	raw, edit := p.discoveryRaw, p.discoveryEdit
	p.mu.Unlock()
	if raw != nil {
		raw(w, r)
		return
	}
	doc := p.discoveryDoc()
	if edit != nil {
		edit(doc)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

func (p *fakeIdP) serveJWKS(w http.ResponseWriter, r *http.Request) {
	p.count(r.URL.Path)
	p.mu.Lock()
	var set jose.JSONWebKeySet
	for _, k := range p.published {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: k.priv.Public(), KeyID: k.kid, Algorithm: k.alg, Use: "sig"})
	}
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(set)
}

// rotate publishes and signs with a new key; keepOld keeps the old key
// published as well.
func (p *fakeIdP) rotate(k signingKey, keepOld bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if keepOld {
		p.published = append(p.published, k)
	} else {
		p.published = []signingKey{k}
	}
	p.signer = k
}

// authorize plays the browser + user at the authorization endpoint: it
// checks the request, records the grant and returns the callback query the
// IdP would redirect to.
func (p *fakeIdP) authorize(t *testing.T, location string) url.Values {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != p.url("/tenant/authorize") {
		t.Fatalf("authorize endpoint = %q", got)
	}
	q := u.Query()
	code := randomToken(t)
	p.mu.Lock()
	p.grants[code] = &grant{
		challenge:   q.Get("code_challenge"),
		method:      q.Get("code_challenge_method"),
		redirectURI: q.Get("redirect_uri"),
		nonce:       q.Get("nonce"),
		authTime:    p.clock.Now(),
	}
	p.mu.Unlock()
	return url.Values{"code": {code}, "state": {q.Get("state")}}
}

func (p *fakeIdP) serveToken(w http.ResponseWriter, r *http.Request) {
	p.count(r.URL.Path)
	p.mu.Lock()
	raw, gate := p.tokenRaw, p.tokenGate
	p.mu.Unlock()
	if gate != nil {
		gate()
	}
	if raw != nil {
		raw(w, r)
		return
	}
	fail := func(format string, args ...any) {
		p.failf(format, args...)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"raw-idp-body-marker"}`))
	}
	if r.Method != http.MethodPost {
		fail("token method %s", r.Method)
		return
	}
	if err := r.ParseForm(); err != nil {
		fail("token form: %v", err)
		return
	}
	f := r.PostForm
	p.mu.Lock()
	style := p.authStyle
	p.mu.Unlock()
	switch style {
	case "basic":
		id, secret, ok := r.BasicAuth()
		if !ok || id != testClientID || secret != testClientSecret || f.Has("client_secret") {
			fail("client auth basic mismatch")
			return
		}
	case "post":
		if _, _, ok := r.BasicAuth(); ok || f.Get("client_id") != testClientID || f.Get("client_secret") != testClientSecret {
			fail("client auth post mismatch")
			return
		}
	}
	if f.Get("grant_type") != "authorization_code" {
		fail("grant_type %q", f.Get("grant_type"))
		return
	}
	p.mu.Lock()
	g := p.grants[f.Get("code")]
	reused := g != nil && g.used
	if g != nil {
		g.used = true
	}
	p.mu.Unlock()
	switch {
	case g == nil:
		fail("unknown code")
		return
	case reused:
		fail("code reused")
		return
	case f.Get("redirect_uri") != g.redirectURI || g.redirectURI != testRedirectURI:
		fail("redirect_uri mismatch")
		return
	case g.method != "S256":
		fail("pkce method %q", g.method)
		return
	}
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	if f.Get("code_verifier") == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		fail("pkce verifier mismatch")
		return
	}
	now := p.clock.Now()
	claims := map[string]any{
		"iss":       p.issuer,
		"sub":       testSubject,
		"aud":       testClientID,
		"exp":       now.Add(5 * time.Minute).Unix(),
		"iat":       now.Unix(),
		"auth_time": g.authTime.Unix(),
		"nonce":     g.nonce,
		"roles":     []string{"LocalRouter.User"},
		"email":     "user@example.test",
		"name":      "Test User",
	}
	p.mu.Lock()
	edit, sign, signer := p.claimsEdit, p.sign, p.signer
	p.mu.Unlock()
	if edit != nil {
		edit(claims)
	}
	payload, _ := json.Marshal(claims)
	var idToken string
	if sign != nil {
		idToken = sign(payload)
	} else {
		idToken = oidctest.SignIDToken(signer.priv, signer.kid, signer.alg, string(payload))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "access-token-marker",
		"token_type":   "Bearer",
		"expires_in":   300,
		"id_token":     idToken,
	})
}

func randomToken(t *testing.T) string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// recordingHooks is the fake identity adapter.
type recordingHooks struct {
	mu        sync.Mutex
	clock     Clock
	logins    []VerifiedLogin
	jits      []bool
	denials   []string // reason
	logouts   []string // secret|csrf
	loginErr  error
	logoutErr error
	sessions  []string
}

func (h *recordingHooks) CompleteLogin(ctx context.Context, l VerifiedLogin, jit bool) (Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loginErr != nil {
		return Session{}, h.loginErr
	}
	h.logins = append(h.logins, l)
	h.jits = append(h.jits, jit)
	b := make([]byte, 32)
	rand.Read(b)
	secret := base64.RawURLEncoding.EncodeToString(b)
	h.sessions = append(h.sessions, secret)
	return Session{Secret: secret, ExpiresAt: h.clock.Now().Add(24 * time.Hour)}, nil
}

func (h *recordingHooks) LoginDenied(ctx context.Context, issuer, subject, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.denials = append(h.denials, issuer+"|"+subject+"|"+reason)
	return nil
}

func (h *recordingHooks) Logout(ctx context.Context, secret, csrf string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logouts = append(h.logouts, secret+"|"+csrf)
	return h.logoutErr
}

func (h *recordingHooks) snapshot() ([]VerifiedLogin, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]VerifiedLogin(nil), h.logins...), append([]string(nil), h.denials...)
}

// harness wires a Service to a fakeIdP with a fake clock and hooks.
type harness struct {
	t     *testing.T
	idp   *fakeIdP
	clock *fakeClock
	hooks *recordingHooks
	svc   *Service
	h     http.Handler
	logs  *lockedBuffer
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	idp := newFakeIdP(t, clock)
	hooks := &recordingHooks{clock: clock}
	_, pool := testTLS(t)
	h := &harness{t: t, idp: idp, clock: clock, hooks: hooks, logs: &lockedBuffer{}}
	cfg := Config{
		PublicBaseURL: testPublicURL,
		Issuer:        idp.issuer,
		ClientID:      testClientID,
		ClientSecret:  func() (string, error) { return testClientSecret, nil },
		SigningAlgs:   []string{"RS256", "ES256"},
		Policy:        testPolicy(),
		Clock:         clock,
		Hooks:         hooks,
		Logger:        newTestLogger(h.logs),
		Outbound:      Outbound{RootCAs: pool, TestDialContext: testDial(idp.srv)},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.svc = svc
	h.h = svc.Handler()
	return h
}

// lockedBuffer collects log output from concurrent goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newTestLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
