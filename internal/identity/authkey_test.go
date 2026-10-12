package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestAuthenticateKeyValid(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-auth", core.RoleUser)
	k, err := f.s.CreateKey(ctx, u.ID, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.s.AuthenticateKey(ctx, k.Token)
	if err != nil {
		t.Fatalf("AuthenticateKey = %v", err)
	}
	want := core.Principal{
		Kind:   core.PrincipalUserKey,
		Role:   core.RoleUser,
		Client: core.Client{Class: core.ClassInteractive},
		UserID: u.ID,
		KeyID:  k.ID,
	}
	if p != want {
		t.Fatalf("principal = %+v, want %+v", p, want)
	}
}

func TestAuthenticateKeyAdminKeyIsUserRole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-admin-key", core.RoleAdmin)
	k, err := f.s.CreateKey(ctx, u.ID, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.s.AuthenticateKey(ctx, k.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.Role != core.RoleUser {
		t.Fatalf("admin's API key role = %q, want user (admin powers are session-only)", p.Role)
	}
}

func TestAuthenticateKeyRejectsForgedAndMalformed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-forge", core.RoleUser)
	k, err := f.s.CreateKey(ctx, u.ID, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.s.CreateKey(ctx, u.ID, "other", 0)
	if err != nil {
		t.Fatal(err)
	}
	secretStart := len("lrk_") + 26 + 1
	// Right key id, another key's (valid) secret.
	forged := k.Token[:secretStart] + other.Token[secretStart:]
	// Valid grammar, unknown key id.
	unknownID := "lrk_" + strings.Repeat("a", 26) + "_" + k.Token[secretStart:]
	for name, tok := range map[string]string{
		"forged secret": forged,
		"unknown id":    unknownID,
		"malformed":     "lrk_nope",
		"static format": "lr-" + strings.Repeat("A", 43),
		"empty":         "",
	} {
		_, err := f.s.AuthenticateKey(ctx, tok)
		if !errors.Is(err, core.ErrUnauthenticated) {
			t.Errorf("%s: AuthenticateKey = %v, want ErrUnauthenticated", name, err)
			continue
		}
		if tok != "" && strings.Contains(err.Error(), tok) {
			t.Errorf("%s: error leaks the token", name)
		}
	}
}

func TestAuthenticateKeyExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-exp", core.RoleUser)
	k, err := f.s.CreateKey(ctx, u.ID, "k", MinKeyTTL)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(MinKeyTTL - time.Millisecond)
	if _, err := f.s.AuthenticateKey(ctx, k.Token); err != nil {
		t.Fatalf("before expiry = %v", err)
	}
	f.clock.Advance(time.Millisecond)
	if _, err := f.s.AuthenticateKey(ctx, k.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("at expiry = %v, want ErrUnauthenticated", err)
	}
}

func TestAuthenticateKeyRequiresLoginWithin30Days(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-30d", core.RoleUser)
	k, err := f.s.CreateKey(ctx, u.ID, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(MaxLoginAge)
	if _, err := f.s.AuthenticateKey(ctx, k.Token); err != nil {
		t.Fatalf("exactly 30d after login = %v", err)
	}
	f.clock.Advance(time.Millisecond)
	if _, err := f.s.AuthenticateKey(ctx, k.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("30d+1ms after login = %v, want ErrUnauthenticated", err)
	}
	// A fresh allowed login revives the (unexpired, unrevoked) key.
	f.user("sub-30d", core.RoleUser)
	if _, err := f.s.AuthenticateKey(ctx, k.Token); err != nil {
		t.Fatalf("after fresh login = %v", err)
	}
}

func TestAuthenticateKeyClosedStoreIsUnavailable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-closed", core.RoleUser)
	k, err := f.s.CreateKey(ctx, u.ID, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.s.Close()
	_, err = f.s.AuthenticateKey(ctx, k.Token)
	if !errors.Is(err, core.ErrAuthUnavailable) || errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("closed store = %v, want ErrAuthUnavailable only", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	s2 := f.second()
	if _, err := s2.AuthenticateKey(cctx, k.Token); !errors.Is(err, core.ErrAuthUnavailable) {
		t.Fatalf("canceled ctx = %v, want ErrAuthUnavailable", err)
	}
}
