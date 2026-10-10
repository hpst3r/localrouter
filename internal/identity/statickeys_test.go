package identity

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	staticTokA = "owned-static-token-a-0000000000"
	staticTokB = "owned-static-token-b-0000000000"
)

func (f *fixture) register(s *Store, owner, token string) StaticKeyState {
	f.t.Helper()
	st, err := s.RegisterStaticKey(context.Background(), owner, token)
	if err != nil {
		f.t.Fatalf("RegisterStaticKey = %v", err)
	}
	return st
}

func (f *fixture) staticOK(owner, token string) {
	f.t.Helper()
	p, err := f.s.AuthenticateStaticKey(context.Background(), owner, token)
	want := core.Principal{Kind: core.PrincipalStaticClient, Role: core.RoleUser, UserID: owner}
	if err != nil || p != want {
		f.t.Fatalf("AuthenticateStaticKey = %+v, %v; want %+v", p, err, want)
	}
}

func (f *fixture) staticDenied(owner, token, when string) {
	f.t.Helper()
	if p, err := f.s.AuthenticateStaticKey(context.Background(), owner, token); !errors.Is(err, core.ErrUnauthenticated) {
		f.t.Fatalf("AuthenticateStaticKey %s = %+v, %v; want ErrUnauthenticated", when, p, err)
	}
}

func TestStaticKeyRegisteredAuthenticatesOnlyForItsOwner(t *testing.T) {
	f := newFixture(t)
	u := f.user("sub-a", core.RoleUser)
	other := f.user("sub-b", core.RoleUser)
	if st := f.register(f.s, u.ID, staticTokA); st != StaticKeyActive {
		t.Fatalf("state = %q, want active", st)
	}
	f.staticOK(u.ID, staticTokA)
	f.staticDenied(other.ID, staticTokA, "for another owner")
	f.staticDenied(u.ID, staticTokB, "of an unregistered token")
}

// Review finding C at the store: disable revokes the binding permanently.
// Enable, a fresh login and re-registering the same token (every reload and
// restart does) leave it revoked.
func TestStaticKeyDisableIsPermanent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-a", core.RoleUser)
	f.register(f.s, u.ID, staticTokA)
	if err := f.second().DisableUser(ctx, Actor{Kind: ActorCLI}, u.ID); err != nil {
		t.Fatal(err)
	}
	f.staticDenied(u.ID, staticTokA, "after a CLI disable")
	if err := f.s.EnableUser(ctx, Actor{Kind: ActorCLI}, u.ID); err != nil {
		t.Fatal(err)
	}
	f.user("sub-a", core.RoleUser)
	f.staticDenied(u.ID, staticTokA, "after enable and a fresh login")
	if st := f.register(f.s, u.ID, staticTokA); st != StaticKeyRevoked {
		t.Fatalf("re-registration state = %q, want revoked", st)
	}
	f.staticDenied(u.ID, staticTokA, "after re-registration")
}

// A policy denial revokes the bindings like a disable: a later allowed login
// does not bring the key back.
func TestStaticKeyPolicyDenialIsPermanent(t *testing.T) {
	f := newFixture(t)
	u := f.user("sub-a", core.RoleUser)
	f.register(f.s, u.ID, staticTokA)
	if err := f.s.DenyLogin(context.Background(), testIssuer, "sub-a"); err != nil {
		t.Fatal(err)
	}
	f.user("sub-a", core.RoleUser)
	f.staticDenied(u.ID, staticTokA, "after a policy denial and a fresh login")
	var reason string
	if err := f.s.db.QueryRow(`SELECT revoke_reason FROM static_keys WHERE digest = ?`, tokenDigest(staticTokA)).Scan(&reason); err != nil || reason != RevokePolicyDenied {
		t.Fatalf("revoke reason = %q, %v; want %q", reason, err, RevokePolicyDenied)
	}
}

// Delete revokes and scrubs: the owner's digests leave the registry and the
// database bytes, a re-registration for the deleted owner stores nothing,
// and other owners' bindings are untouched.
func TestStaticKeyDeleteScrubsDigests(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-a", core.RoleUser)
	bystander := f.user("sub-b", core.RoleUser)
	f.register(f.s, u.ID, staticTokA)
	f.register(f.s, bystander.ID, staticTokB)
	if err := f.second().DeleteUser(ctx, Actor{Kind: ActorCLI}, u.ID); err != nil {
		t.Fatal(err)
	}
	f.staticDenied(u.ID, staticTokA, "of a deleted owner")
	if st := f.register(f.s, u.ID, staticTokA); st != StaticKeyRevoked {
		t.Fatalf("registration for a deleted owner = %q, want revoked", st)
	}
	f.staticDenied(u.ID, staticTokA, "re-registered for a deleted owner")
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM static_keys WHERE user_id = ?`, u.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deleted owner's registry rows = %d, %v; want 0", n, err)
	}
	f.staticOK(bystander.ID, staticTokB)
	if err := f.s.Ping(ctx); err != nil { // completes the pending scrub
		t.Fatal(err)
	}
	raw := fileBytes(t, f.path)
	if !bytes.Contains(raw, tokenDigest(staticTokB)) {
		t.Fatal("positive control: live binding's digest not found in the database bytes")
	}
	if bytes.Contains(raw, tokenDigest(staticTokA)) {
		t.Fatal("deleted owner's static key digest still in identity.db/-wal")
	}
}

// A binding never moves to another owner, revoked or not.
func TestStaticKeyBindingNeverMoves(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-a", core.RoleUser)
	other := f.user("sub-b", core.RoleUser)
	f.register(f.s, u.ID, staticTokA)
	if _, err := f.s.RegisterStaticKey(ctx, other.ID, staticTokA); !errors.Is(err, ErrStaticKeyConflict) {
		t.Fatalf("register for another owner = %v, want ErrStaticKeyConflict", err)
	}
	f.staticOK(u.ID, staticTokA)
	if err := f.s.DisableUser(ctx, Actor{Kind: ActorCLI}, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RegisterStaticKey(ctx, other.ID, staticTokA); !errors.Is(err, ErrStaticKeyConflict) {
		t.Fatalf("register a revoked key for another owner = %v, want ErrStaticKeyConflict", err)
	}
	f.staticDenied(other.ID, staticTokA, "moved to another owner")
}

// A key first seen while the owner's credentials stand revoked is stored
// revoked: it may be one the owner held before. A key rotated in after the
// owner's next allowed login works; the old one stays dead.
func TestStaticKeyFirstSeenWhileRevoked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-a", core.RoleUser)
	if err := f.s.DisableUser(ctx, Actor{Kind: ActorCLI}, u.ID); err != nil {
		t.Fatal(err)
	}
	if st := f.register(f.s, u.ID, staticTokA); st != StaticKeyRevoked {
		t.Fatalf("registered while disabled = %q, want revoked", st)
	}
	if err := f.s.EnableUser(ctx, Actor{Kind: ActorCLI}, u.ID); err != nil {
		t.Fatal(err)
	}
	if st := f.register(f.s, u.ID, staticTokB); st != StaticKeyRevoked {
		t.Fatalf("registered after enable, before a login = %q, want revoked", st)
	}
	f.user("sub-a", core.RoleUser)
	f.staticDenied(u.ID, staticTokA, "first seen while disabled")
	f.staticDenied(u.ID, staticTokB, "first seen before the post-enable login")
	const rotated = "owned-static-token-c-0000000000"
	if st := f.register(f.s, u.ID, rotated); st != StaticKeyActive {
		t.Fatalf("rotated after the login = %q, want active", st)
	}
	f.staticOK(u.ID, rotated)
}

// A key configured for an owner that is not provisioned yet is bound
// pending, denied, and works once a user with exactly that id logs in.
func TestStaticKeyPendingOwnerActivatesOnFirstLogin(t *testing.T) {
	probe := newFixture(t) // same seeded Rand: learns the id the next JIT mints
	id := probe.user("sub-a", core.RoleUser).ID

	f := newFixture(t)
	if st := f.register(f.s, id, staticTokA); st != StaticKeyPending {
		t.Fatalf("unknown owner = %q, want pending", st)
	}
	f.staticDenied(id, staticTokA, "of an unprovisioned owner")
	if u := f.user("sub-a", core.RoleUser); u.ID != id {
		t.Fatalf("JIT id = %s, want %s (seeded)", u.ID, id)
	}
	if st := f.register(f.s, id, staticTokA); st != StaticKeyActive {
		t.Fatalf("after provisioning = %q, want active", st)
	}
	f.staticOK(id, staticTokA)
}

// The login window applies; a later allowed login restores the key (it was
// suspended, not revoked).
func TestStaticKeyFollowsLoginWindow(t *testing.T) {
	f := newFixture(t)
	u := f.user("sub-a", core.RoleUser)
	f.register(f.s, u.ID, staticTokA)
	f.clock.Advance(MaxLoginAge + time.Minute)
	f.staticDenied(u.ID, staticTokA, "with a lapsed login")
	if st := f.register(f.s, u.ID, staticTokB); st != StaticKeyActive {
		t.Fatalf("registered with a lapsed (not revoked) login = %q, want active", st)
	}
	f.user("sub-a", core.RoleUser)
	f.staticOK(u.ID, staticTokA)
	f.staticOK(u.ID, staticTokB)
}

func TestStaticKeyInvalidAndUnavailable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-a", core.RoleUser)
	for _, c := range []struct{ owner, token string }{
		{"", staticTokA}, {"u_short", staticTokA}, {"k_" + u.ID[2:], staticTokA}, {u.ID + "x", staticTokA},
		{u.ID, ""}, {u.ID, strings.Repeat("x", maxStaticKeyBytes+1)},
	} {
		if _, err := f.s.RegisterStaticKey(ctx, c.owner, c.token); !errors.Is(err, ErrInvalid) {
			t.Errorf("RegisterStaticKey(%q, len %d) = %v, want ErrInvalid", c.owner, len(c.token), err)
		}
		f.staticDenied(c.owner, c.token, "with invalid input")
	}
	f.register(f.s, u.ID, staticTokA)
	f.s.Close()
	if _, err := f.s.AuthenticateStaticKey(ctx, u.ID, staticTokA); !errors.Is(err, core.ErrAuthUnavailable) {
		t.Fatalf("AuthenticateStaticKey on a closed store = %v, want ErrAuthUnavailable", err)
	}
	if _, err := f.s.RegisterStaticKey(ctx, u.ID, staticTokA); !errors.Is(err, ErrClosed) {
		t.Fatalf("RegisterStaticKey on a closed store = %v, want ErrClosed", err)
	}
}

// Only digests are stored: the plaintext never reaches the database bytes.
func TestStaticKeyPlaintextNeverStored(t *testing.T) {
	f := newFixture(t)
	u := f.user("sub-a", core.RoleUser)
	f.register(f.s, u.ID, staticTokA)
	f.staticOK(u.ID, staticTokA)
	if bytes.Contains(fileBytes(t, f.path), []byte(staticTokA)) {
		t.Fatal("static key plaintext in identity.db/-wal")
	}
}

// An identity.db created by the v1 binary (no registry) migrates to v2 in
// place: users, keys and the pinned binding survive and the registry works.
func TestStaticKeyMigratesV1Database(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	u := f.user("sub-a", core.RoleUser)
	k, err := f.s.CreateKey(ctx, u.ID, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Roll the file back to exactly the v1 shape.
	for _, q := range []string{`DROP TABLE static_keys`, `UPDATE schema_version SET version = 1`} {
		if _, err := f.s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f.s.Close()

	s, err := Open(ctx, f.path, f.opts)
	if err != nil {
		t.Fatalf("Open v1 database = %v", err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != len(migrations) || v != 2 {
		t.Fatalf("schema version = %d, %v; want 2", v, err)
	}
	if _, err := s.AuthenticateKey(ctx, k.Token); err != nil {
		t.Fatalf("v1 API key after migration = %v", err)
	}
	f.s = s
	if st := f.register(s, u.ID, staticTokA); st != StaticKeyActive {
		t.Fatalf("register after migration = %q", st)
	}
	f.staticOK(u.ID, staticTokA)
}
