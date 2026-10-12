package app

// Tests for the weblogin.Hooks adapter over a real identity.Store (temp
// SQLite file, deterministic clock). The OIDC layer is not involved: these
// drive the adapter with already-verified logins, exactly what weblogin hands
// it after signature, nonce, auth_time and policy checks.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

const (
	idTestIssuer   = "https://idp.fixture.test/o/localrouter/"
	idTestClientID = "localrouter-fixture"
)

// idClock is a deterministic, settable clock shared by the store and the
// adapter under test.
type idClock struct {
	mu sync.Mutex
	t  time.Time
}

func newIDClock() *idClock { return &idClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)} }

func (c *idClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *idClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openTestIdentity(t *testing.T, clock core.Clock) *identity.Store {
	t.Helper()
	st, err := identity.Open(context.Background(), filepath.Join(t.TempDir(), "identity.db"),
		identity.Options{Issuer: idTestIssuer, ClientID: idTestClientID, Clock: clock})
	if err != nil {
		t.Fatalf("open identity: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func verified(clock core.Clock, sub, role string) weblogin.VerifiedLogin {
	return weblogin.VerifiedLogin{Issuer: idTestIssuer, Subject: sub, Role: role, AuthTime: clock.Now(), Email: sub + "@example.test", DisplayName: "Fixture " + sub}
}

func TestLoginHooksCompleteLoginCreatesSession(t *testing.T) {
	ctx := context.Background()
	clock := newIDClock()
	st := openTestIdentity(t, clock)
	h := loginHooks{store: st}

	sess, err := h.CompleteLogin(ctx, verified(clock, "alice", weblogin.RoleAdmin), true)
	if err != nil {
		t.Fatalf("complete login: %v", err)
	}
	if sess.Secret == "" || !sess.ExpiresAt.After(clock.Now()) {
		t.Fatalf("session = %+v, want a secret and a future expiry", sess)
	}
	got, err := st.AuthenticateSession(ctx, sess.Secret)
	if err != nil {
		t.Fatalf("returned secret does not authenticate: %v", err)
	}
	if got.Principal.Kind != core.PrincipalSession || got.Principal.Role != core.RoleAdmin || got.Principal.UserID == "" {
		t.Fatalf("principal = %+v, want admin session", got.Principal)
	}
}

// TestLoginHooksKeepsPolicyRole: the role is weblogin's policy decision
// (claim values, then admin-subject promotion of an admitted login); the
// adapter records it unchanged and refuses anything but user or admin.
func TestLoginHooksKeepsPolicyRole(t *testing.T) {
	ctx := context.Background()
	clock := newIDClock()
	st := openTestIdentity(t, clock)
	h := loginHooks{store: st}

	for _, tc := range []struct {
		sub, role string
		want      core.Role
	}{
		{"plain-sub", weblogin.RoleUser, core.RoleUser},
		{"claim-admin", weblogin.RoleAdmin, core.RoleAdmin},
	} {
		sess, err := h.CompleteLogin(ctx, verified(clock, tc.sub, tc.role), true)
		if err != nil {
			t.Fatalf("%s: %v", tc.sub, err)
		}
		got, err := st.AuthenticateSession(ctx, sess.Secret)
		if err != nil {
			t.Fatal(err)
		}
		if got.Principal.Role != tc.want {
			t.Fatalf("%s with policy role %s: session role %s, want %s", tc.sub, tc.role, got.Principal.Role, tc.want)
		}
	}
	for _, role := range []string{"service", "legacy", ""} {
		if _, err := h.CompleteLogin(ctx, verified(clock, "odd-sub", role), true); err == nil {
			t.Fatalf("policy role %q was accepted", role)
		}
	}
}

func TestLoginHooksErrorMapping(t *testing.T) {
	ctx := context.Background()
	clock := newIDClock()
	st := openTestIdentity(t, clock)
	h := loginHooks{store: st}

	if _, err := h.CompleteLogin(ctx, verified(clock, "nobody", weblogin.RoleUser), false); !errors.Is(err, weblogin.ErrNotProvisioned) {
		t.Fatalf("unknown subject without JIT: err = %v, want ErrNotProvisioned", err)
	}

	if _, err := h.CompleteLogin(ctx, verified(clock, "bob", weblogin.RoleUser), true); err != nil {
		t.Fatal(err)
	}
	bob := userBySubjectLogin(t, st, clock, "bob")
	if err := st.DisableUser(ctx, identity.Actor{Kind: identity.ActorCLI}, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CompleteLogin(ctx, verified(clock, "bob", weblogin.RoleUser), true); !errors.Is(err, weblogin.ErrUserDisabled) {
		t.Fatalf("disabled user: err = %v, want ErrUserDisabled", err)
	}

	if _, err := h.CompleteLogin(ctx, verified(clock, "carol", weblogin.RoleUser), true); err != nil {
		t.Fatal(err)
	}
	carol := userBySubjectLogin(t, st, clock, "carol")
	if err := st.DeleteUser(ctx, identity.Actor{Kind: identity.ActorCLI}, carol); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CompleteLogin(ctx, verified(clock, "carol", weblogin.RoleUser), true); !errors.Is(err, weblogin.ErrUserDisabled) {
		t.Fatalf("deleted (tombstoned) subject: err = %v, want ErrUserDisabled", err)
	}

	stale := verified(clock, "dave", weblogin.RoleUser)
	stale.AuthTime = clock.Now().Add(-2 * time.Hour)
	_, err := h.CompleteLogin(ctx, stale, true)
	if err == nil || errors.Is(err, weblogin.ErrNotProvisioned) || errors.Is(err, weblogin.ErrUserDisabled) {
		t.Fatalf("stale auth_time: err = %v, want a generic failure", err)
	}
	if errors.Is(err, identity.ErrStaleLogin) {
		t.Fatalf("store error leaked through the adapter: %v", err)
	}
	users, err := st.ListUsers(ctx, "", identity.MaxListLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("stale login provisioned a user: %d users, want 2 (bob, carol)", len(users))
	}
}

// userBySubjectLogin finds the id of the user a just-completed login created
// (the only active user with the given display name).
func userBySubjectLogin(t *testing.T, st *identity.Store, clock core.Clock, sub string) string {
	t.Helper()
	users, err := st.ListUsers(context.Background(), "", identity.MaxListLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.DisplayName == "Fixture "+sub {
			return u.ID
		}
	}
	t.Fatalf("no user for %s", sub)
	return ""
}

func TestLoginHooksLoginDeniedRevokesCredentials(t *testing.T) {
	ctx := context.Background()
	clock := newIDClock()
	st := openTestIdentity(t, clock)
	h := loginHooks{store: st}

	sess, err := h.CompleteLogin(ctx, verified(clock, "erin", weblogin.RoleUser), true)
	if err != nil {
		t.Fatal(err)
	}
	erin := userBySubjectLogin(t, st, clock, "erin")
	key, err := st.CreateKey(ctx, erin, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.LoginDenied(ctx, idTestIssuer, "erin", weblogin.ReasonNotPermitted); err != nil {
		t.Fatalf("login denied: %v", err)
	}
	if _, err := st.AuthenticateKey(ctx, key.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("key after policy denial: err = %v, want ErrUnauthenticated", err)
	}
	if _, err := st.AuthenticateSession(ctx, sess.Secret); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("session after policy denial: err = %v, want ErrUnauthenticated", err)
	}
	if err := h.LoginDenied(ctx, idTestIssuer, "never-seen", weblogin.ReasonGroupOverage); err != nil {
		t.Fatalf("denial for unknown subject must be a no-op: %v", err)
	}
	if users, _ := st.ListUsers(ctx, "", identity.MaxListLimit); len(users) != 1 {
		t.Fatalf("denial created a user: %d users", len(users))
	}
}

func TestLoginHooksLogoutRequiresCSRF(t *testing.T) {
	ctx := context.Background()
	clock := newIDClock()
	st := openTestIdentity(t, clock)
	h := loginHooks{store: st}

	sess, err := h.CompleteLogin(ctx, verified(clock, "frank", weblogin.RoleUser), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Logout(ctx, sess.Secret, "not-the-csrf-token"); !errors.Is(err, weblogin.ErrCSRF) {
		t.Fatalf("wrong csrf: err = %v, want ErrCSRF", err)
	}
	if _, err := st.AuthenticateSession(ctx, sess.Secret); err != nil {
		t.Fatalf("session revoked despite csrf mismatch: %v", err)
	}
	if err := h.Logout(ctx, sess.Secret, identity.CSRFToken(sess.Secret)); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := st.AuthenticateSession(ctx, sess.Secret); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("session after logout: err = %v, want ErrUnauthenticated", err)
	}
	if err := h.Logout(ctx, sess.Secret, identity.CSRFToken(sess.Secret)); err != nil {
		t.Fatalf("second logout of a gone session must be nil: %v", err)
	}
}
