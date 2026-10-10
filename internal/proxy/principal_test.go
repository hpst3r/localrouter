package proxy

// Multi-user inference authentication (Deps.MultiUser + AuthenticatePrincipal).
// The principal is the only source of a request's owner: RequestRecord.UserID
// and KeyID come from it, never from headers or the body, and a user key's
// client identity is its opaque key id.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	// Token shapes only; nothing authenticates them but the fake below.
	userTokenA1 = "lrk_SENTINEL-user-a-key-1"
	userTokenA2 = "lrk_SENTINEL-user-a-key-2"
	userTokenB1 = "lrk_SENTINEL-user-b-key-1"
	serviceTok  = "sk-service-SENTINEL"
	ownedTok    = "sk-owned-static-SENTINEL"

	puserA = "u_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	puserB = "u_bbbbbbbbbbbbbbbbbbbbbbbbbb"
	pkeyA1 = "k_a1a1a1a1a1a1a1a1a1a1a1a1a1"
	pkeyA2 = "k_a2a2a2a2a2a2a2a2a2a2a2a2a2"
	pkeyB1 = "k_b1b1b1b1b1b1b1b1b1b1b1b1b1"
)

func userKeyPrincipal(user, key string) core.Principal {
	return core.Principal{Kind: core.PrincipalUserKey, Role: core.RoleUser, UserID: user, KeyID: key,
		Client: core.Client{Class: core.ClassInteractive}}
}

// principalAuth is a fake BearerAuthenticator over a fixed token table that
// records every call (token and whether a context was supplied).
type principalAuth struct {
	mu    sync.Mutex
	table map[string]core.Principal
	errs  map[string]error
	calls []string
	noCtx int
}

func newPrincipalAuth() *principalAuth {
	return &principalAuth{
		table: map[string]core.Principal{
			userTokenA1: userKeyPrincipal(puserA, pkeyA1),
			userTokenA2: userKeyPrincipal(puserA, pkeyA2),
			userTokenB1: userKeyPrincipal(puserB, pkeyB1),
			serviceTok: {Kind: core.PrincipalStaticClient, Role: core.RoleService,
				Client: core.Client{Name: "agent", Class: core.ClassBackground, Host: "vm9"}},
			ownedTok: {Kind: core.PrincipalStaticClient, Role: core.RoleUser, UserID: puserA,
				Client: core.Client{Name: "laptop", Class: core.ClassInteractive}},
		},
		errs: map[string]error{},
	}
}

func (a *principalAuth) authenticate(ctx context.Context, bearer string) (core.Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, bearer)
	if ctx == nil {
		a.noCtx++
	}
	if err, ok := a.errs[bearer]; ok {
		return core.Principal{}, err
	}
	if p, ok := a.table[bearer]; ok {
		return p, nil
	}
	return core.Principal{}, core.ErrUnauthenticated
}

func (a *principalAuth) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func multiUserHarness(t *testing.T) (*harness, *principalAuth) {
	h := newHarness(t)
	auth := newPrincipalAuth()
	h.multiUser, h.authPrincipal = true, auth.authenticate
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	return h, auth
}

// A user key's request is attributed to its owner and key, and its client
// identity is the key id.
func TestMultiUserRecordCarriesPrincipalOwner(t *testing.T) {
	h, _ := multiUserHarness(t)
	h.start()
	resp := h.post("/v1/responses", userTokenA1, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows := h.waitRows(1)
	if r := rows[0]; r.UserID != puserA || r.KeyID != pkeyA1 || r.Client != pkeyA1 || r.Host != "" {
		t.Fatalf("record owner = user %q key %q client %q host %q", r.UserID, r.KeyID, r.Client, r.Host)
	}
}

// Authentication failures: a backend outage is 503 (fail closed), anything
// else 401. Neither reflects the token, reaches policy/upstream, or falls back
// to the legacy static authenticator (clientKey is valid there).
func TestMultiUserAuthFailureStatuses(t *testing.T) {
	h, auth := multiUserHarness(t)
	auth.errs[userTokenB1] = fmt.Errorf("identity: %w: db locked", core.ErrAuthUnavailable)
	auth.errs[userTokenA2] = errors.New("some unexpected failure")
	h.start()
	cases := []struct {
		token string
		want  int
	}{
		{userTokenB1, http.StatusServiceUnavailable},
		{userTokenA2, http.StatusUnauthorized},
		{"lrk_SENTINEL-unknown", http.StatusUnauthorized},
		{clientKey, http.StatusUnauthorized},
		{"", http.StatusUnauthorized},
	}
	for _, c := range cases {
		resp := h.post("/v1/responses", c.token, respBody, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("token %q: status %d, want %d", c.token, resp.StatusCode, c.want)
		}
		if c.token != "" && strings.Contains(string(body), c.token) {
			t.Errorf("token %q reflected in body", c.token)
		}
		if strings.Contains(string(body), "db locked") {
			t.Errorf("backend error text leaked: %s", body)
		}
	}
	if len(h.policy.classes()) != 0 {
		t.Fatal("failed authentication reached policy")
	}
	if auth.noCtx != 0 {
		t.Fatal("authenticator called without a context")
	}
	if strings.Contains(h.logs.String(), "SENTINEL") {
		t.Fatal("token logged")
	}
}

// Multi-user mode without the principal authenticator is a wiring error: it
// fails closed with 503 and never consults the legacy authenticator.
func TestMultiUserMissingAuthenticatorFailsClosed(t *testing.T) {
	h, _ := multiUserHarness(t)
	h.authPrincipal = nil
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", resp.StatusCode)
	}
	if len(h.policy.classes()) != 0 {
		t.Fatal("reached policy")
	}
}

// /v1 never authenticates from cookies: a browser session cookie alone is 401
// without consulting the authenticator, and cookies are never sent upstream.
func TestMultiUserIgnoresCookies(t *testing.T) {
	h := newHarness(t)
	auth := newPrincipalAuth()
	h.multiUser, h.authPrincipal = true, auth.authenticate
	var upstreamCookie atomic.Value
	upstreamCookie.Store("")
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		upstreamCookie.Store(r.Header.Get("Cookie"))
		okJSON(w, r)
	})
	singleRoute(h, "a")
	h.start()
	cookie := "__Host-localrouter_session=" + userTokenA1
	resp := h.post("/v1/responses", "", respBody, map[string]string{"Cookie": cookie})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie-only status %d, want 401", resp.StatusCode)
	}
	if auth.callCount() != 0 {
		t.Fatal("authenticator consulted without a bearer header")
	}
	resp = h.post("/v1/responses", userTokenA1, respBody, map[string]string{"Cookie": cookie})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer+cookie status %d", resp.StatusCode)
	}
	if c := upstreamCookie.Load().(string); c != "" {
		t.Fatalf("cookie forwarded upstream: %q", c)
	}
}

// Only inference-capable, well-formed principals are accepted. Sessions, admin
// and legacy roles, and user keys whose server-side identity fields disagree
// (client name other than the key id, a host, ingest, a non-interactive class)
// or whose opaque ids are malformed are rejected as 401 before any work.
func TestMultiUserRejectsInvalidPrincipals(t *testing.T) {
	good := userKeyPrincipal(puserA, pkeyA1)
	mod := func(f func(*core.Principal)) core.Principal { p := good; f(&p); return p }
	static := func(role core.Role, owner, name string, class core.Class) core.Principal {
		return core.Principal{Kind: core.PrincipalStaticClient, Role: role, UserID: owner,
			Client: core.Client{Name: name, Class: class}}
	}
	cases := map[string]core.Principal{
		"session":            {Kind: core.PrincipalSession, Role: core.RoleUser, UserID: puserA},
		"unknown kind":       mod(func(p *core.Principal) { p.Kind = "bogus" }),
		"key admin role":     mod(func(p *core.Principal) { p.Role = core.RoleAdmin }),
		"key service role":   mod(func(p *core.Principal) { p.Role = core.RoleService }),
		"key no user":        mod(func(p *core.Principal) { p.UserID = "" }),
		"key no key id":      mod(func(p *core.Principal) { p.KeyID = "" }),
		"key bad user":       mod(func(p *core.Principal) { p.UserID = "u_a\x00" }),
		"key bad key id":     mod(func(p *core.Principal) { p.KeyID = "k_a b" }),
		"key spoofed client": mod(func(p *core.Principal) { p.Client.Name = "agent" }),
		"key host":           mod(func(p *core.Principal) { p.Client.Host = "vm9" }),
		"key ingest":         mod(func(p *core.Principal) { p.Client.Ingest = true }),
		"key background":     mod(func(p *core.Principal) { p.Client.Class = core.ClassBackground }),
		"key no class":       mod(func(p *core.Principal) { p.Client.Class = "" }),
		"static legacy":      static(core.RoleLegacy, "", "agent", core.ClassInteractive),
		"static admin":       static(core.RoleAdmin, puserA, "agent", core.ClassInteractive),
		"static no role":     static("", "", "agent", core.ClassInteractive),
		"static user no own": static(core.RoleUser, "", "laptop", core.ClassInteractive),
		"static bad owner":   static(core.RoleUser, "u_ä", "laptop", core.ClassInteractive),
		"static no name":     static(core.RoleService, "", "", core.ClassInteractive),
		"static bad class":   static(core.RoleService, "", "agent", "premium"),
		"static key id": mod(func(p *core.Principal) {
			*p = static(core.RoleService, "", "agent", core.ClassInteractive)
			p.KeyID = pkeyA1
		}),
	}
	h, auth := multiUserHarness(t)
	h.start()
	for name, pr := range cases {
		auth.mu.Lock()
		auth.table["tok-"+name] = pr
		auth.mu.Unlock()
		resp := h.post("/v1/responses", "tok-"+name, respBody, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
	if len(h.policy.classes()) != 0 {
		t.Fatal("invalid principal reached policy")
	}
	// The well-formed principals of every accepted shape still work.
	for _, tok := range []string{userTokenA1, serviceTok, ownedTok} {
		resp := h.post("/v1/responses", tok, respBody, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", tok, resp.StatusCode)
		}
	}
}

// A policy denial in multi-user mode is a generic 429: the policy's reason
// names accounts and quota state, which ordinary users must not see. It is
// still logged for operators. Legacy mode keeps the reason (see TestDenied*).
func TestMultiUserPolicyDenialIsGeneric(t *testing.T) {
	h, _ := multiUserHarness(t)
	h.policy.deny["a"] = true
	h.start()
	resp := h.post("/v1/responses", userTokenA1, respBody, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if strings.Contains(string(body), testReason) {
		t.Fatalf("policy reason disclosed: %s", body)
	}
	if !strings.Contains(h.logs.String(), testReason) {
		t.Fatal("policy reason not logged for operators")
	}
}

// /v1/models uses the same multi-user authentication as inference.
func TestMultiUserModelsAuthentication(t *testing.T) {
	h, auth := multiUserHarness(t)
	auth.errs[userTokenB1] = core.ErrAuthUnavailable
	h.start()
	get := func(tok string) int {
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for tok, want := range map[string]int{
		userTokenA1: http.StatusOK,
		serviceTok:  http.StatusOK,
		clientKey:   http.StatusUnauthorized, // legacy-only key
		userTokenB1: http.StatusServiceUnavailable,
		"":          http.StatusUnauthorized,
	} {
		if got := get(tok); got != want {
			t.Errorf("token %q: %d, want %d", tok, got, want)
		}
	}
}

// With MultiUser false the principal authenticator is never consulted, even
// when wired: legacy keys authenticate exactly as before and rows stay unowned.
func TestLegacyModeIgnoresPrincipalAuthenticator(t *testing.T) {
	h, auth := multiUserHarness(t)
	h.multiUser = false
	h.start()
	resp := h.post("/v1/responses", userTokenA1, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("user key in legacy mode: %d, want 401", resp.StatusCode)
	}
	resp = h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy key: %d", resp.StatusCode)
	}
	if auth.callCount() != 0 {
		t.Fatal("principal authenticator consulted in legacy mode")
	}
	if r := h.waitRows(1)[0]; r.UserID != "" || r.KeyID != "" || r.Client != "alice" {
		t.Fatalf("legacy row owner = %q/%q client %q", r.UserID, r.KeyID, r.Client)
	}
}

// Ownership and class come from the principal only: headers and body fields
// cannot set an owner or key, and a background service cannot upgrade itself
// to interactive.
func TestMultiUserOwnerAndClassNotForgeable(t *testing.T) {
	h, _ := multiUserHarness(t)
	h.start()
	spoof := map[string]string{
		"X-LocalRouter-User": puserB, "X-LocalRouter-Key": pkeyB1, "X-LocalRouter-Client": "agent",
		"X-LocalRouter-Class": "interactive",
	}
	body := `{"model":"gpt-x","input":"hi","user":"` + puserB + `","user_id":"` + puserB + `","key_id":"` + pkeyB1 + `"}`
	for _, tok := range []string{serviceTok, userTokenA1} {
		resp := h.post("/v1/responses", tok, body, spoof)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", tok, resp.StatusCode)
		}
	}
	rows := h.waitRows(2)
	if r := rows[0]; r.UserID != "" || r.KeyID != "" || r.Client != "agent" || r.Class != core.ClassBackground || r.Host != "vm9" {
		t.Fatalf("service row = user %q key %q client %q class %q host %q", r.UserID, r.KeyID, r.Client, r.Class, r.Host)
	}
	if r := rows[1]; r.UserID != puserA || r.KeyID != pkeyA1 || r.Client != pkeyA1 || r.Class != core.ClassInteractive {
		t.Fatalf("user row = user %q key %q client %q class %q", r.UserID, r.KeyID, r.Client, r.Class)
	}
	if cl := h.policy.classes(); cl[0] != core.ClassBackground {
		t.Fatalf("service admitted as %q", cl[0])
	}
}
