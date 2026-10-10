package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestRevokeKeyIsOwnerScopedAndImmediate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	alice := f.user("sub-alice", core.RoleUser)
	bob := f.user("sub-bob", core.RoleUser)
	ak, err := f.s.CreateKey(ctx, alice.ID, "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	cli := f.second() // another *Store on the same file

	// Bob cannot revoke (or learn about) Alice's key.
	if err := cli.RevokeKey(ctx, Actor{Kind: ActorUser, UserID: bob.ID}, bob.ID, ak.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke other user's key = %v, want ErrNotFound", err)
	}
	if _, err := f.s.AuthenticateKey(ctx, ak.Token); err != nil {
		t.Fatalf("key revoked by non-owner: %v", err)
	}

	if err := cli.RevokeKey(ctx, Actor{Kind: ActorUser, UserID: alice.ID}, alice.ID, ak.ID); err != nil {
		t.Fatalf("owner revoke = %v", err)
	}
	if _, err := f.s.AuthenticateKey(ctx, ak.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("revoked key via other store instance = %v, want ErrUnauthenticated", err)
	}
	keys, err := f.s.ListKeys(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[0].RevokedAt.Equal(f.clock.Now()) || keys[0].RevokeReason != RevokeUser {
		t.Fatalf("revoked key metadata = %+v", keys)
	}
	// Idempotent; the first reason is kept.
	if err := f.s.RevokeKey(ctx, Actor{Kind: ActorCLI}, alice.ID, ak.ID); err != nil {
		t.Fatalf("second revoke = %v", err)
	}
	keys, _ = f.s.ListKeys(ctx, alice.ID)
	if keys[0].RevokeReason != RevokeUser {
		t.Fatalf("reason overwritten: %q", keys[0].RevokeReason)
	}
}

func TestRevokeKeyReasonFollowsActorAndFreesSlot(t *testing.T) {
	f := newFixture(t)
	f.opts.MaxKeysPerUser = 1
	s := f.second()
	ctx := context.Background()
	u := f.user("sub-reason", core.RoleUser)
	admin := f.user("sub-admin", core.RoleAdmin)
	k, err := s.CreateKey(ctx, u.ID, "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, Actor{Kind: ActorAdmin, UserID: admin.ID}, u.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.ListKeys(ctx, u.ID)
	if keys[0].RevokeReason != RevokeAdmin {
		t.Fatalf("reason = %q, want admin", keys[0].RevokeReason)
	}
	if _, err := s.CreateKey(ctx, u.ID, "b", 0); err != nil {
		t.Fatalf("revoked key still counted against the limit: %v", err)
	}
}

func TestRevokeKeyRejectsInvalidActor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-actor", core.RoleUser)
	k, _ := f.s.CreateKey(ctx, u.ID, "a", 0)
	for _, a := range []Actor{
		{},
		{Kind: "root"},
		{Kind: ActorUser},                    // missing user id
		{Kind: ActorCLI, UserID: u.ID},       // cli carries no user id
		{Kind: ActorUser, UserID: "u_other"}, // user acting on another user's key
		{Kind: ActorAdmin, UserID: u.ID},     // not an admin
	} {
		if err := f.s.RevokeKey(ctx, a, u.ID, k.ID); !errors.Is(err, ErrInvalid) {
			t.Errorf("actor %+v: RevokeKey = %v, want ErrInvalid", a, err)
		}
	}
	if _, err := f.s.AuthenticateKey(ctx, k.Token); err != nil {
		t.Fatalf("key revoked by invalid actor: %v", err)
	}
}
