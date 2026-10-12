package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

// bearerFixture is a real identity store with one logged-in user plus a
// static-key table: "svc-key" -> service client, "owned-key" -> user client
// owned by the user.
type bearerFixture struct {
	clock   *idClock
	store   *identity.Store
	userID  string
	auth    *bearerAuth
	lookups int
}

func newBearerFixture(t *testing.T, role string) *bearerFixture {
	t.Helper()
	ctx := context.Background()
	f := &bearerFixture{clock: newIDClock()}
	f.store = openTestIdentity(t, f.clock)
	if _, err := (loginHooks{store: f.store}).CompleteLogin(ctx, verified(f.clock, "gina", role), true); err != nil {
		t.Fatal(err)
	}
	f.userID = userBySubjectLogin(t, f.store, f.clock, "gina")
	static := map[string]string{"svc-key": "agent", "owned-key": "gina-cli", "legacy-key": "old"}
	f.auth = &bearerAuth{
		store: f.store,
		lookup: func(b string) (string, bool) {
			f.lookups++
			n, ok := static[b]
			return n, ok
		},
		statics: map[string]staticPrincipal{
			"agent":    {client: core.Client{Name: "agent", Class: core.ClassBackground, Host: "box", Ingest: true}, role: core.RoleService},
			"gina-cli": {client: core.Client{Name: "gina-cli", Class: core.ClassInteractive, Host: "laptop"}, role: core.RoleUser, owner: f.userID},
			"old":      {client: core.Client{Name: "old", Class: core.ClassInteractive}},
		},
	}
	// As registerOwnedKeys does for every configured owned key.
	if _, err := f.store.RegisterStaticKey(ctx, f.userID, "owned-key"); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestBearerUserKeyPrincipal(t *testing.T) {
	ctx := context.Background()
	f := newBearerFixture(t, weblogin.RoleAdmin)
	key, err := f.store.CreateKey(ctx, f.userID, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.auth.Authenticate(ctx, key.Token)
	if err != nil {
		t.Fatalf("user key: %v", err)
	}
	want := core.Principal{
		Kind: core.PrincipalUserKey, Role: core.RoleUser, UserID: f.userID, KeyID: key.ID,
		Client: core.Client{Name: key.ID, Class: core.ClassInteractive},
	}
	if p != want {
		t.Fatalf("principal = %+v, want %+v (admin users get RoleUser on keys)", p, want)
	}
	if f.lookups != 0 {
		t.Fatalf("user key token consulted the static table %d times", f.lookups)
	}

	if err := f.store.DisableUser(ctx, identity.Actor{Kind: identity.ActorCLI}, f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Authenticate(ctx, key.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("key of disabled user: err = %v, want ErrUnauthenticated", err)
	}
}

func TestBearerMalformedUserKeyNeverFallsBackToStatic(t *testing.T) {
	ctx := context.Background()
	f := newBearerFixture(t, weblogin.RoleUser)
	for _, tok := range []string{"lrk_", "lrk_short", "lrk_" + strings.Repeat("a", 26) + "_tooshort", "lrs_" + strings.Repeat("A", 43)} {
		f.auth.lookup = func(string) (string, bool) { f.lookups++; return "agent", true }
		if _, err := f.auth.Authenticate(ctx, tok); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("token %.8q...: err = %v, want ErrUnauthenticated", tok, err)
		}
	}
	if f.lookups != 0 {
		t.Fatalf("reserved-prefix tokens reached the static table %d times", f.lookups)
	}
}

func TestBearerStaticRoles(t *testing.T) {
	ctx := context.Background()
	f := newBearerFixture(t, weblogin.RoleUser)

	p, err := f.auth.Authenticate(ctx, "svc-key")
	if err != nil {
		t.Fatalf("service key: %v", err)
	}
	want := core.Principal{Kind: core.PrincipalStaticClient, Role: core.RoleService, Client: core.Client{Name: "agent", Class: core.ClassBackground, Host: "box", Ingest: true}}
	if p != want {
		t.Fatalf("service principal = %+v, want %+v", p, want)
	}

	p, err = f.auth.Authenticate(ctx, "owned-key")
	if err != nil {
		t.Fatalf("owned static key: %v", err)
	}
	want = core.Principal{Kind: core.PrincipalStaticClient, Role: core.RoleUser, UserID: f.userID, Client: core.Client{Name: "gina-cli", Class: core.ClassInteractive, Host: "laptop"}}
	if p != want {
		t.Fatalf("owned principal = %+v, want %+v", p, want)
	}

	if _, err := f.auth.Authenticate(ctx, "legacy-key"); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("static key without an explicit role: err = %v, want ErrUnauthenticated", err)
	}
	if _, err := f.auth.Authenticate(ctx, "unknown"); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("unknown key: err = %v, want ErrUnauthenticated", err)
	}
	if _, err := f.auth.Authenticate(ctx, ""); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("empty bearer: err = %v, want ErrUnauthenticated", err)
	}
}

// TestBearerOwnedStaticKeyFollowsOwner: the owner of a role "user" static key
// is read fresh on every request, so disable, policy denial, a lapsed login
// window and an unknown owner each deny the next request.
func TestBearerOwnedStaticKeyFollowsOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("ingest is never granted to an owned key", func(t *testing.T) {
		f := newBearerFixture(t, weblogin.RoleUser)
		sp := f.auth.statics["gina-cli"]
		sp.client.Ingest = true
		f.auth.statics["gina-cli"] = sp
		p, err := f.auth.Authenticate(ctx, "owned-key")
		if err != nil || p.Client.Ingest {
			t.Fatalf("principal = %+v, err = %v; want Ingest false", p, err)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		f := newBearerFixture(t, weblogin.RoleUser)
		if err := f.store.DisableUser(ctx, identity.Actor{Kind: identity.ActorCLI}, f.userID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.auth.Authenticate(ctx, "owned-key"); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
	t.Run("policy denied", func(t *testing.T) {
		f := newBearerFixture(t, weblogin.RoleUser)
		if err := f.store.DenyLogin(ctx, idTestIssuer, "gina"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.auth.Authenticate(ctx, "owned-key"); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
	t.Run("login window lapsed", func(t *testing.T) {
		f := newBearerFixture(t, weblogin.RoleUser)
		f.clock.Advance(identity.MaxLoginAge + time.Minute)
		if _, err := f.auth.Authenticate(ctx, "owned-key"); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
	t.Run("unknown owner", func(t *testing.T) {
		f := newBearerFixture(t, weblogin.RoleUser)
		sp := f.auth.statics["gina-cli"]
		sp.owner = "u_doesnotexist"
		f.auth.statics["gina-cli"] = sp
		if _, err := f.auth.Authenticate(ctx, "owned-key"); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
	t.Run("owner missing", func(t *testing.T) {
		f := newBearerFixture(t, weblogin.RoleUser)
		sp := f.auth.statics["gina-cli"]
		sp.owner = ""
		f.auth.statics["gina-cli"] = sp
		if _, err := f.auth.Authenticate(ctx, "owned-key"); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
}

// TestBearerStoreUnavailableFailsClosed: a closed or failing store is 503
// material (ErrAuthUnavailable) for every credential that needs it, never a
// fallback to the static table.
func TestBearerStoreUnavailableFailsClosed(t *testing.T) {
	ctx := context.Background()
	f := newBearerFixture(t, weblogin.RoleUser)
	key, err := f.store.CreateKey(ctx, f.userID, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.auth.lookup = func(string) (string, bool) { f.lookups++; return "agent", true }
	if _, err := f.auth.Authenticate(ctx, key.Token); !errors.Is(err, core.ErrAuthUnavailable) {
		t.Fatalf("user key on closed store: err = %v, want ErrAuthUnavailable", err)
	}
	if f.lookups != 0 {
		t.Fatal("user key fell back to the static table")
	}
	f.auth.lookup = func(string) (string, bool) { return "gina-cli", true }
	if _, err := f.auth.Authenticate(ctx, "owned-key"); !errors.Is(err, core.ErrAuthUnavailable) {
		t.Fatalf("owned static key on closed store: err = %v, want ErrAuthUnavailable", err)
	}
	nilStore := &bearerAuth{lookup: f.auth.lookup, statics: f.auth.statics}
	if _, err := nilStore.Authenticate(ctx, "owned-key"); !errors.Is(err, core.ErrAuthUnavailable) {
		t.Fatalf("owned static key without a store: err = %v, want ErrAuthUnavailable", err)
	}
	if _, err := nilStore.Authenticate(ctx, key.Token); !errors.Is(err, core.ErrAuthUnavailable) {
		t.Fatalf("user key without a store: err = %v, want ErrAuthUnavailable", err)
	}
}
