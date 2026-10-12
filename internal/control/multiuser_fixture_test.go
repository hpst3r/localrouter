package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/ledger"
)

// Multi-user fixtures use the real identity store and the real SQLite ledger
// so owner scoping is proven against the actual SQL, not a fake.

const (
	muIssuer   = "https://idp.example.test/application/o/localrouter/"
	muClientID = "localrouter-client"
	muOrigin   = "https://router.example.test"

	serviceToken       = "svc-token-SECRET-0123456789"
	ingestServiceToken = "ingest-token-SECRET-0123456789"
	aliceStaticToken   = "alice-static-SECRET-0123456789"
)

type muClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *muClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *muClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type lockedRand struct {
	mu sync.Mutex
	r  *rand.Rand
}

func (l *lockedRand) Read(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.Read(p)
}

// muUser is a provisioned identity-store user with credentials.
type muUser struct {
	ID      string
	Session string // session cookie value
	CSRF    string
	Key     string // API key token
	KeyID   string
}

type muFixture struct {
	t      *testing.T
	clock  *muClock
	ids    *identity.Store
	led    *ledger.Ledger
	deps   Deps
	srv    *Server
	h      http.Handler
	static map[string]core.Principal

	authMu  sync.Mutex
	authErr error // when set, AuthenticatePrincipal fails with it
}

// newMUFixture builds a multi-user server over a real identity store and
// ledger. mutate may adjust Deps before the server is built.
func newMUFixture(t *testing.T, mutate func(*Deps)) *muFixture {
	t.Helper()
	dir := t.TempDir()
	clock := &muClock{t: t0}
	ids, err := identity.Open(context.Background(), filepath.Join(dir, "identity.db"), identity.Options{
		Issuer: muIssuer, ClientID: muClientID, Clock: clock,
		Rand: &lockedRand{r: rand.New(rand.NewSource(7))},
	})
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = ids.Close() })
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"), nil, nil)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	led.RequireScope()

	f := &muFixture{t: t, clock: clock, ids: ids, led: led, static: map[string]core.Principal{
		serviceToken: {Kind: core.PrincipalStaticClient, Role: core.RoleService,
			Client: core.Client{Name: "svc", Class: core.ClassBackground}},
		ingestServiceToken: {Kind: core.PrincipalStaticClient, Role: core.RoleService,
			Client: core.Client{Name: "agent", Class: core.ClassBackground, Ingest: true}},
	}}
	f.deps = Deps{
		Accounts: []core.Account{
			{ID: "primary", Provider: core.ProviderCodex},
			{ID: "claude-a", Provider: core.ProviderClaude, QuotaSource: QuotaSourceAgent},
		},
		Policy:  &fakePolicy{decision: core.Decision{Allow: true, AccountID: "primary", Reason: "admitted"}},
		Ledger:  led,
		Routes:  []core.Route{{Models: []string{"gpt"}, Interactive: []string{"primary"}, Background: []string{"primary"}}},
		Clients: []ClientInfo{{Name: "svc", Class: "background"}, {Name: "agent", Class: "background", Ingest: true}},
		// The legacy seam must never be consulted in multi-user mode.
		Authenticate: func(string) (core.Client, bool) {
			t.Error("legacy Authenticate called in multi-user mode")
			return core.Client{Name: "svc", Ingest: true}, true
		},
		AuthenticatePrincipal: f.authenticate,
		Identity:              ids,
		PublicBaseURL:         muOrigin,
		Storage:               led,
		Clock:                 clock,
	}
	if mutate != nil {
		mutate(&f.deps)
	}
	f.srv = New(f.deps, Options{MultiUser: true})
	f.h = f.srv.Handler()
	return f
}

// authenticate mimics the app's BearerAuthenticator: exact user-key grammar
// goes to the identity store only (Client.Name = KeyID), anything else to the
// static table.
func (f *muFixture) authenticate(ctx context.Context, tok string) (core.Principal, error) {
	f.authMu.Lock()
	err := f.authErr
	f.authMu.Unlock()
	if err != nil {
		return core.Principal{}, err
	}
	if identity.IsUserKeyToken(tok) {
		p, err := f.ids.AuthenticateKey(ctx, tok)
		if err != nil {
			return core.Principal{}, err
		}
		p.Client.Name = p.KeyID
		return p, nil
	}
	if p, ok := f.static[tok]; ok {
		return p, nil
	}
	return core.Principal{}, core.ErrUnauthenticated
}

func (f *muFixture) setAuthErr(err error) {
	f.authMu.Lock()
	f.authErr = err
	f.authMu.Unlock()
}

// addUser provisions a user through a fresh verified login and gives them a
// session and an API key.
func (f *muFixture) addUser(subject string, role core.Role) muUser {
	f.t.Helper()
	ctx := context.Background()
	u, err := f.ids.ResolveLogin(ctx, identity.Login{Issuer: muIssuer, Subject: subject, Role: role,
		AuthTime: f.clock.Now(), Email: subject + "@example.test", DisplayName: "Name " + subject, Provision: true})
	if err != nil {
		f.t.Fatalf("ResolveLogin(%s): %v", subject, err)
	}
	ns, err := f.ids.CreateSession(ctx, u.ID)
	if err != nil {
		f.t.Fatalf("CreateSession: %v", err)
	}
	k, err := f.ids.CreateKey(ctx, u.ID, "laptop", 0)
	if err != nil {
		f.t.Fatalf("CreateKey: %v", err)
	}
	return muUser{ID: u.ID, Session: ns.Token, CSRF: ns.CSRF, Key: k.Token, KeyID: k.ID}
}

// addStaticUser registers a user-owned static client for u.
func (f *muFixture) addStaticUser(tok string, u muUser) {
	f.static[tok] = core.Principal{Kind: core.PrincipalStaticClient, Role: core.RoleUser,
		Client: core.Client{Name: "static-" + u.ID, Class: core.ClassInteractive}, UserID: u.ID}
}

// record writes one ledger row at t0-1h for the given owner.
func (f *muFixture) record(id, account, model, client, userID, keyID string, tokens int64) {
	f.t.Helper()
	err := f.led.Record(context.Background(), core.RequestRecord{
		ID: id, StartedAt: t0.Add(-time.Hour), FinishedAt: t0.Add(-time.Hour + time.Second),
		AccountID: account, Provider: core.ProviderCodex, Route: "gpt", Model: model, Class: core.ClassInteractive,
		Client: client, Host: "h1", Status: 200, UsageKnown: true,
		Usage:  core.Usage{InputTokens: tokens, OutputTokens: 1},
		UserID: userID, KeyID: keyID,
	})
	if err != nil {
		f.t.Fatalf("Record: %v", err)
	}
}

// bearerGet issues a GET with a bearer token.
func (f *muFixture) bearerGet(target, tok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// bearerPost issues a POST with a bearer token and JSON body.
func (f *muFixture) bearerPost(target, tok, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// sessionReq builds a /ui/v1 request carrying u's session cookie and, for
// unsafe methods, a valid Origin and CSRF header.
func (f *muFixture) sessionReq(method, target string, u muUser, body string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "https://router.example.test"+target, r)
	if u.Session != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: u.Session})
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", muOrigin)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set(csrfHeader, u.CSRF)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	return req
}

func (f *muFixture) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *muFixture) session(method, target string, u muUser, body string) *httptest.ResponseRecorder {
	return f.serve(f.sessionReq(method, target, u, body))
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, code, rec.Body.String())
	}
}

// usageKeys returns the row keys of a usage document.
func usageKeys(t *testing.T, rec *httptest.ResponseRecorder) map[string]int64 {
	t.Helper()
	var doc usageDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	out := map[string]int64{}
	for _, r := range doc.Rows {
		out[r.Key] = r.Requests
	}
	return out
}

var errDBDown = errors.New("sqlite: database is locked /var/lib/secret/identity.db")
