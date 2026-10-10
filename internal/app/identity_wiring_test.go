package app

// Generation wiring in multi-user mode, driven through the real App.Handler:
// the principal bearer authenticator on inference, the login/UI mounts, the
// identity-aware readiness probe, collector ownership, per-user budget
// defaults and per-user concurrency. Upstreams are loopback httptest servers;
// no OIDC provider is contacted (discovery is lazy and every request here
// avoids it).

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/control"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

const svcStaticKey = "svc-static-key-000000000000"

// seedUser logs a user in through the app's own hooks adapter (as weblogin
// would after a verified login) and returns the user id, a fresh API key
// token and the session token.
func seedUser(t *testing.T, a *App, clock core.Clock, sub, role string) (userID, key, session string) {
	t.Helper()
	ctx := context.Background()
	sess, err := (loginHooks{store: a.Identity}).CompleteLogin(ctx, verified(clock, sub, role), true)
	if err != nil {
		t.Fatalf("seed login: %v", err)
	}
	s, err := a.Identity.AuthenticateSession(ctx, sess.Secret)
	if err != nil {
		t.Fatal(err)
	}
	k, err := a.Identity.CreateKey(ctx, s.Principal.UserID, "fixture", 0)
	if err != nil {
		t.Fatal(err)
	}
	return s.Principal.UserID, k.Token, sess.Secret
}

type reqOpt func(*http.Request)

func withBearer(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func withHost(h string) reqOpt { return func(r *http.Request) { r.Host = h } }

func serve(a *App, method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	a.Handler.ServeHTTP(w, r)
	return w
}

func TestMultiUserInferenceAuthenticatesPrincipals(t *testing.T) {
	clock := newIDClock()
	f := newIDFixture(t)
	a := f.build(Overrides{IdentityClock: clock})
	uid, key, session := seedUser(t, a, clock, "hana", weblogin.RoleUser)

	if w := serve(a, "GET", "/v1/models", "", withBearer(key)); w.Code != http.StatusOK {
		t.Fatalf("user key: %d %s", w.Code, w.Body)
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer(svcStaticKey)); w.Code != http.StatusOK {
		t.Fatalf("service static key: %d %s", w.Code, w.Body)
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer("lrk_"+strings.Repeat("a", 26)+"_x")); w.Code != http.StatusUnauthorized {
		t.Fatalf("malformed user key: %d, want 401", w.Code)
	}
	cookieOnly := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: weblogin.SessionCookieName, Value: session}) }
	if w := serve(a, "GET", "/v1/models", "", cookieOnly); w.Code != http.StatusUnauthorized {
		t.Fatalf("session cookie on /v1: %d, want 401", w.Code)
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer(session)); w.Code != http.StatusUnauthorized {
		t.Fatalf("session token as bearer: %d, want 401", w.Code)
	}

	if err := a.Identity.DisableUser(context.Background(), identity.Actor{Kind: identity.ActorCLI}, uid); err != nil {
		t.Fatal(err)
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer(key)); w.Code != http.StatusUnauthorized {
		t.Fatalf("key of a just-disabled user: %d, want 401", w.Code)
	}
	if err := a.Identity.Close(); err != nil {
		t.Fatal(err)
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer(key)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("user key with the identity store closed: %d, want 503", w.Code)
	}
}

func TestMultiUserMountsLoginAndUserUI(t *testing.T) {
	f := newIDFixture(t)
	a := f.build(Overrides{})

	// A non-canonical host is redirected to the public origin by weblogin
	// itself, without any provider traffic.
	w := serve(a, "GET", "/auth/login", "", withHost("127.0.0.1:8787"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "https://localhost:8787/auth/login" {
		t.Fatalf("/auth/login: %d Location=%q", w.Code, w.Header().Get("Location"))
	}
	w = serve(a, "GET", "/ui/", "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/ui/: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := serve(a, "GET", "/v1/models", "", withHost("evil.example")); w.Code != http.StatusForbidden {
		t.Fatalf("HostGuard bypassed: %d", w.Code)
	}
	if w := serve(a, "GET", "/ui/", "", withHost("evil.example")); w.Code != http.StatusForbidden {
		t.Fatalf("HostGuard bypassed on /ui/: %d", w.Code)
	}
}

func TestLegacyHasNoLoginOrUserUI(t *testing.T) {
	f := newIDFixture(t)
	f.identity = false
	a := f.build(Overrides{})
	if w := serve(a, "GET", "/auth/login", "", withHost("127.0.0.1:8787")); w.Code == http.StatusSeeOther {
		t.Fatalf("legacy mode serves /auth/login: %d", w.Code)
	}
	if w := serve(a, "GET", "/ui/", ""); w.Code == http.StatusOK {
		t.Fatalf("legacy mode serves /ui/: %d", w.Code)
	}
}

func TestMultiUserReadinessFoldsIdentityStore(t *testing.T) {
	f := newIDFixture(t)
	a := f.build(Overrides{})
	if w := serve(a, "GET", "/readyz", ""); w.Code != http.StatusOK {
		t.Fatalf("/readyz: %d %s", w.Code, w.Body)
	}
	if err := a.Identity.Close(); err != nil {
		t.Fatal(err)
	}
	if w := serve(a, "GET", "/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with identity store closed: %d, want 503", w.Code)
	}
	if w := serve(a, "GET", "/healthz", ""); w.Code != http.StatusOK {
		t.Fatalf("/healthz must stay a liveness probe: %d", w.Code)
	}
}

// TestCollectorRecordsAreNeverOwned: server-local collectors (claude/hermes
// logs) write unowned rows whatever a record carries, so a log file can never
// attribute usage to a user.
func TestCollectorRecordsAreNeverOwned(t *testing.T) {
	ctx := context.Background()
	f := newIDFixture(t)
	a := f.build(Overrides{})
	c := collectorLedger{Ledger: a.Ledger, app: a}
	now := time.Now().UTC()
	rec := core.RequestRecord{ID: "collector-1", StartedAt: now, Client: "u_aaaaaaaaaaaaaaaaaaaaaaaaaa", AccountID: "acct", Model: "id-priced", UserID: "u_aaaaaaaaaaaaaaaaaaaaaaaaaa", KeyID: "k_aaaaaaaaaaaaaaaaaaaaaaaaaa", Usage: core.Usage{InputTokens: 1}}
	if err := c.Record(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec.ID = "collector-2"
	if err := c.RecordBatch(ctx, []core.RequestRecord{rec}); err != nil {
		t.Fatal(err)
	}
	rows, err := a.Ledger.SummaryScoped(ctx, now.Add(-time.Hour), "user", core.DataScope{AllUsers: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Key != "" || rows[0].Requests != 2 {
		t.Fatalf("collector rows by owner = %+v, want one unowned group of 2", rows)
	}
}

// fakeUpstream is a loopback OpenAI-compatible upstream. When hold is
// non-nil every chat request blocks until it is closed.
type fakeUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	hits    int
	entered chan struct{}
	hold    chan struct{}
}

func newFakeUpstream(t *testing.T, hold bool) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{entered: make(chan struct{}, 16)}
	if hold {
		u.hold = make(chan struct{})
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		u.mu.Lock()
		u.hits++
		u.mu.Unlock()
		u.entered <- struct{}{}
		if u.hold != nil {
			<-u.hold
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"id-priced","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(func() {
		if u.hold != nil {
			select {
			case <-u.hold:
			default:
				close(u.hold)
			}
		}
		u.srv.Close()
	})
	return u
}

func (u *fakeUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

const chatBody = `{"model":"id-priced","messages":[{"role":"user","content":"hi"}]}`

// TestMultiUserBudgetDefaultChargesUser: budgets.users is enforced per user
// across keys by the generation's gate, while an unowned service key is
// governed by its client/account ceilings only. The budget report exposes
// the same per-user ceilings.
func TestMultiUserBudgetDefaultChargesUser(t *testing.T) {
	clock := newIDClock()
	up := newFakeUpstream(t, false)
	f := newIDFixture(t)
	f.upstream = up.srv.URL
	f.budgets = "budgets:\n  reserve_usd: \"0.01\"\n  clients:\n    svc: {daily_usd: \"10\"}\n  users: {daily_usd: \"0\"}\n"
	a := f.build(Overrides{IdentityClock: clock})
	uid, key, _ := seedUser(t, a, clock, "ines", weblogin.RoleUser)

	if w := serve(a, "POST", "/v1/chat/completions", chatBody, withBearer(key)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("user over a zero user budget: %d %s", w.Code, w.Body)
	}
	if up.count() != 0 {
		t.Fatal("a budget-denied attempt reached the upstream")
	}
	if w := serve(a, "POST", "/v1/chat/completions", chatBody, withBearer(svcStaticKey)); w.Code != http.StatusOK {
		t.Fatalf("unowned service key: %d %s", w.Code, w.Body)
	}

	src, ok := a.current.Load().budgets.(control.UserBudgetSource)
	if !ok {
		t.Fatal("generation budget source does not report user ceilings")
	}
	got := src.UserLimits(uid)
	if len(got) != 1 || got[0] != (budget.Limit{Scope: budget.ScopeUser, Key: uid, Period: budget.PeriodDay, Micros: 0}) {
		t.Fatalf("UserLimits = %+v", got)
	}
}

// TestMultiUserConcurrencyPerUserAcrossKeys: max_concurrent_per_user counts
// one user across all of their keys; another user is unaffected.
func TestMultiUserConcurrencyPerUserAcrossKeys(t *testing.T) {
	clock := newIDClock()
	up := newFakeUpstream(t, true)
	f := newIDFixture(t)
	f.upstream = up.srv.URL
	f.limits = "limits: {max_concurrent_per_user: 1}\n"
	a := f.build(Overrides{IdentityClock: clock})
	uid, key1, _ := seedUser(t, a, clock, "jo", weblogin.RoleUser)
	k2, err := a.Identity.CreateKey(context.Background(), uid, "second", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, other, _ := seedUser(t, a, clock, "kim", weblogin.RoleUser)

	done := make(chan int, 1)
	go func() { done <- serve(a, "POST", "/v1/chat/completions", chatBody, withBearer(key1)).Code }()
	select {
	case <-up.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first request never reached the upstream")
	}
	if w := serve(a, "POST", "/v1/chat/completions", chatBody, withBearer(k2.Token)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("same user, second key, while first is active: %d, want 429", w.Code)
	}
	go func() { _ = serve(a, "POST", "/v1/chat/completions", chatBody, withBearer(other)) }()
	select {
	case <-up.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("another user's request was not admitted")
	}
	close(up.hold)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("first request: %d", code)
	}
	if a.Limiter.UserActive(uid) != 0 {
		t.Fatalf("user slot leaked: %d active", a.Limiter.UserActive(uid))
	}
}
