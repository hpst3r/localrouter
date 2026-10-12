package identity

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestCreateSessionRequiresFreshLogin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.s.CreateSession(ctx, "u_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user = %v, want ErrNotFound", err)
	}
	u := f.user("sub-fresh", core.RoleUser)
	f.clock.Advance(DefaultMaxAuthAge + time.Millisecond)
	if _, err := f.s.CreateSession(ctx, u.ID); !errors.Is(err, ErrStaleLogin) {
		t.Fatalf("session long after login = %v, want ErrStaleLogin", err)
	}
}

func TestCreateSessionTokensAndCSRF(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-sess", core.RoleAdmin)
	ns, err := f.s.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("CreateSession = %v", err)
	}
	now := f.clock.Now()
	if !strings.HasPrefix(ns.Token, "lrs_") || len(ns.Token) != 4+43 {
		t.Fatalf("session token %q not lrs_<43 base64url>", ns.Token)
	}
	if ns.UserID != u.ID || !ns.ExpiresAt.Equal(now.Add(MaxSessionAbsolute)) || !ns.IdleExpiresAt.Equal(now.Add(MaxSessionIdle)) {
		t.Fatalf("session = %+v", ns)
	}
	if ns.CSRF == "" || ns.CSRF == ns.Token || ns.CSRF != CSRFToken(ns.Token) {
		t.Fatalf("CSRF %q not derived from the session token", ns.CSRF)
	}
	other, err := f.s.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidCSRF(ns.Token, ns.CSRF) {
		t.Fatal("ValidCSRF rejects the session's own token")
	}
	for name, c := range map[string][2]string{
		"other session's csrf": {ns.Token, other.CSRF},
		"empty csrf":           {ns.Token, ""},
		"csrf is the token":    {ns.Token, ns.Token},
		"empty session":        {"", CSRFToken("")},
	} {
		if ValidCSRF(c[0], c[1]) {
			t.Errorf("%s: ValidCSRF = true", name)
		}
	}
	var hash []byte
	if err := f.s.db.QueryRow(`SELECT token_hash FROM sessions WHERE user_id = ? ORDER BY created_at LIMIT 1`, u.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if d := sha256.Sum256([]byte(ns.Token)); string(hash) != string(d[:]) && string(hash) != string(tokenDigest(other.Token)) {
		t.Fatal("stored session digest is not SHA-256 of the token")
	}
	assertNoValueInTables(t, f.s, ns.Token, ns.Token[4:], ns.CSRF, other.Token[4:], other.CSRF)
}

func TestAuthenticateSessionReadsCurrentRole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-live-role", core.RoleAdmin)
	ns, err := f.s.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := f.s.AuthenticateSession(ctx, ns.Token)
	if err != nil {
		t.Fatalf("AuthenticateSession = %v", err)
	}
	want := core.Principal{Kind: core.PrincipalSession, Role: core.RoleAdmin, UserID: u.ID}
	if sess.Principal != want || !sess.ExpiresAt.Equal(ns.ExpiresAt) {
		t.Fatalf("session = %+v, want principal %+v", sess, want)
	}
	// A demoting login is effective on the next request of an existing session.
	f.user("sub-live-role", core.RoleUser)
	sess, err = f.second().AuthenticateSession(ctx, ns.Token)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Principal.Role != core.RoleUser {
		t.Fatalf("role after demotion = %q, want user", sess.Principal.Role)
	}
}

func TestAuthenticateSessionRejectsMalformedAndUnknown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tok := range []string{"", "lrs_short", "lrs_" + strings.Repeat("A", 43), "lrk_" + strings.Repeat("A", 43)} {
		if _, err := f.s.AuthenticateSession(ctx, tok); !errors.Is(err, core.ErrUnauthenticated) {
			t.Errorf("AuthenticateSession(%q) = %v, want ErrUnauthenticated", tok, err)
		}
	}
	f.s.Close()
	if _, err := f.s.AuthenticateSession(ctx, "lrs_"+strings.Repeat("A", 43)); !errors.Is(err, core.ErrAuthUnavailable) {
		t.Fatalf("closed store = %v, want ErrAuthUnavailable", err)
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-idle", core.RoleUser)
	ns, _ := f.s.CreateSession(ctx, u.ID)
	f.clock.Advance(MaxSessionIdle - time.Millisecond)
	if _, err := f.s.AuthenticateSession(ctx, ns.Token); err != nil {
		t.Fatalf("just before idle expiry = %v", err)
	}
	// That use slid the idle deadline; stay idle for a full window.
	f.clock.Advance(MaxSessionIdle)
	if _, err := f.s.AuthenticateSession(ctx, ns.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("after idle window = %v, want ErrUnauthenticated", err)
	}
}

func TestSessionAbsoluteExpiryDespiteActivity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-abs", core.RoleUser)
	ns, _ := f.s.CreateSession(ctx, u.ID)
	for elapsed := time.Duration(0); elapsed+7*time.Hour < MaxSessionAbsolute; elapsed += 7 * time.Hour {
		f.clock.Advance(7 * time.Hour)
		sess, err := f.s.AuthenticateSession(ctx, ns.Token)
		if err != nil {
			t.Fatalf("active session at %v = %v", elapsed+7*time.Hour, err)
		}
		if sess.IdleExpiresAt.After(ns.ExpiresAt) {
			t.Fatalf("idle deadline %v beyond absolute %v", sess.IdleExpiresAt, ns.ExpiresAt)
		}
	}
	f.clock.Advance(ns.ExpiresAt.Sub(f.clock.Now()))
	if _, err := f.s.AuthenticateSession(ctx, ns.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("at absolute expiry = %v, want ErrUnauthenticated", err)
	}
}

func TestSessionTouchIsThrottled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-touch", core.RoleUser)
	ns, _ := f.s.CreateSession(ctx, u.ID)
	seen := func() int64 {
		var v int64
		if err := f.s.db.QueryRow(`SELECT last_seen_at FROM sessions`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	start := seen()
	f.clock.Advance(SessionTouchInterval - time.Millisecond)
	if _, err := f.s.AuthenticateSession(ctx, ns.Token); err != nil {
		t.Fatal(err)
	}
	if seen() != start {
		t.Fatal("session touched within the throttle interval")
	}
	f.clock.Advance(time.Millisecond)
	sess, err := f.s.AuthenticateSession(ctx, ns.Token)
	if err != nil {
		t.Fatal(err)
	}
	if seen() != toMS(f.clock.Now()) || !sess.IdleExpiresAt.Equal(f.clock.Now().Add(MaxSessionIdle)) {
		t.Fatalf("session not touched after the interval: idle=%v", sess.IdleExpiresAt)
	}
}

func TestRevokeSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	alice := f.user("sub-a", core.RoleUser)
	bob := f.user("sub-b", core.RoleUser)
	a1, _ := f.s.CreateSession(ctx, alice.ID)
	a2, _ := f.s.CreateSession(ctx, alice.ID)
	b1, _ := f.s.CreateSession(ctx, bob.ID)
	cli := f.second()

	if err := cli.RevokeSession(ctx, a1.Token); err != nil {
		t.Fatalf("RevokeSession = %v", err)
	}
	if err := cli.RevokeSession(ctx, a1.Token); err != nil {
		t.Fatalf("RevokeSession twice = %v", err)
	}
	if err := cli.RevokeSession(ctx, "garbage"); err != nil {
		t.Fatalf("RevokeSession(malformed) = %v, want nil (logout is idempotent)", err)
	}
	if _, err := f.s.AuthenticateSession(ctx, a1.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("revoked session = %v", err)
	}
	if _, err := f.s.AuthenticateSession(ctx, a2.Token); err != nil {
		t.Fatalf("sibling session revoked: %v", err)
	}

	if err := cli.RevokeUserSessions(ctx, Actor{Kind: ActorUser, UserID: alice.ID}, alice.ID); err != nil {
		t.Fatalf("RevokeUserSessions = %v", err)
	}
	if _, err := f.s.AuthenticateSession(ctx, a2.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("session after revoke-all = %v", err)
	}
	if _, err := f.s.AuthenticateSession(ctx, b1.Token); err != nil {
		t.Fatalf("other user's session revoked: %v", err)
	}
	if err := cli.RevokeUserSessions(ctx, Actor{Kind: ActorUser, UserID: bob.ID}, alice.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revoke-all of another user = %v, want ErrInvalid", err)
	}
}

func TestPurgeExpiredSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-purge", core.RoleUser)
	old, _ := f.s.CreateSession(ctx, u.ID)
	f.clock.Advance(MaxSessionIdle)
	f.user("sub-purge", core.RoleUser)
	fresh, _ := f.s.CreateSession(ctx, u.ID)
	n, err := f.s.PurgeExpiredSessions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("PurgeExpiredSessions = %d, %v; want 1", n, err)
	}
	if _, err := f.s.AuthenticateSession(ctx, old.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatal("expired session survived")
	}
	if _, err := f.s.AuthenticateSession(ctx, fresh.Token); err != nil {
		t.Fatalf("live session purged: %v", err)
	}
}
