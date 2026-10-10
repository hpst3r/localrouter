package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestDenyLoginRevokesExistingUserCredentials(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.userWithCreds("sub-denied", core.RoleAdmin)
	other := f.userWithCreds("sub-other", core.RoleUser)

	if err := f.second().DenyLogin(ctx, testIssuer, "sub-denied"); err != nil {
		t.Fatalf("DenyLogin = %v", err)
	}
	f.assertCredsDenied(f.s, c)
	f.assertCredsWork(f.s, other)
	u, _ := f.s.User(ctx, c.user.ID)
	if u.Status != StatusActive || !u.LastLoginAt.IsZero() {
		t.Fatalf("user after policy denial = %+v, want active with cleared login", u)
	}
	keys, _ := f.s.ListKeys(ctx, c.user.ID)
	if keys[0].RevokeReason != RevokePolicyDenied {
		t.Fatalf("revoke reason = %q", keys[0].RevokeReason)
	}
	// A later allowed login works, but old credentials stay dead.
	f.user("sub-denied", core.RoleUser)
	if _, err := f.s.CreateKey(ctx, c.user.ID, "new", 0); err != nil {
		t.Fatalf("CreateKey after allowed re-login = %v", err)
	}
	f.assertCredsDenied(f.s, c)
}

func TestDenyLoginUnknownAndInvalid(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.s.DenyLogin(ctx, testIssuer, "sub-never-seen"); err != nil {
		t.Fatalf("DenyLogin unknown = %v, want nil", err)
	}
	users, _ := f.s.ListUsers(ctx, "", MaxListLimit)
	if len(users) != 0 {
		t.Fatal("DenyLogin created a user")
	}
	if err := f.s.DenyLogin(ctx, "https://evil.example.test/", "sub"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("DenyLogin other issuer = %v, want ErrInvalid", err)
	}
	if err := f.s.DenyLogin(ctx, testIssuer, "bad\xff"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("DenyLogin invalid subject = %v, want ErrInvalid", err)
	}
}
