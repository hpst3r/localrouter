package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

type userCreds struct {
	user    User
	key     NewAPIKey
	session NewSession
}

func (f *fixture) userWithCreds(subject string, role core.Role) userCreds {
	f.t.Helper()
	ctx := context.Background()
	u := f.user(subject, role)
	k, err := f.s.CreateKey(ctx, u.ID, "k", 0)
	if err != nil {
		f.t.Fatal(err)
	}
	ns, err := f.s.CreateSession(ctx, u.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return userCreds{user: u, key: k, session: ns}
}

func (f *fixture) assertCredsDenied(s *Store, c userCreds) {
	f.t.Helper()
	ctx := context.Background()
	if _, err := s.AuthenticateKey(ctx, c.key.Token); !errors.Is(err, core.ErrUnauthenticated) {
		f.t.Errorf("key of %s = %v, want ErrUnauthenticated", c.user.ID, err)
	}
	if _, err := s.AuthenticateSession(ctx, c.session.Token); !errors.Is(err, core.ErrUnauthenticated) {
		f.t.Errorf("session of %s = %v, want ErrUnauthenticated", c.user.ID, err)
	}
}

func (f *fixture) assertCredsWork(s *Store, c userCreds) {
	f.t.Helper()
	ctx := context.Background()
	if _, err := s.AuthenticateKey(ctx, c.key.Token); err != nil {
		f.t.Errorf("key of %s = %v, want ok", c.user.ID, err)
	}
	if _, err := s.AuthenticateSession(ctx, c.session.Token); err != nil {
		f.t.Errorf("session of %s = %v, want ok", c.user.ID, err)
	}
}

func TestDisableUserRevokesEverythingImmediatelyAcrossStores(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	victim := f.userWithCreds("sub-victim", core.RoleAdmin)
	bystander := f.userWithCreds("sub-bystander", core.RoleUser)
	cli := f.second()

	if err := cli.DisableUser(ctx, Actor{Kind: ActorCLI}, victim.user.ID); err != nil {
		t.Fatalf("DisableUser = %v", err)
	}
	f.assertCredsDenied(f.s, victim)
	f.assertCredsWork(f.s, bystander)

	u, err := f.s.User(ctx, victim.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != StatusDisabled || !u.LastLoginAt.IsZero() {
		t.Fatalf("disabled user = %+v", u)
	}
	keys, _ := f.s.ListKeys(ctx, victim.user.ID)
	if len(keys) != 1 || keys[0].RevokedAt.IsZero() || keys[0].RevokeReason != RevokeUserDisabled {
		t.Fatalf("keys after disable = %+v", keys)
	}
	var n int
	f.s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, victim.user.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d sessions survive disable", n)
	}

	if _, err := f.s.ResolveLogin(ctx, f.login("sub-victim", core.RoleAdmin)); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("login of disabled user = %v, want ErrUserDisabled", err)
	}
	if _, err := f.s.CreateKey(ctx, victim.user.ID, "x", 0); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("CreateKey for disabled user = %v", err)
	}
	if _, err := f.s.CreateSession(ctx, victim.user.ID); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("CreateSession for disabled user = %v", err)
	}
	if err := f.s.DisableUser(ctx, Actor{Kind: ActorCLI}, victim.user.ID); err != nil {
		t.Fatalf("disable twice = %v, want nil", err)
	}
}

func TestEnableUserDoesNotResurrectCredentials(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.user("sub-admin", core.RoleAdmin)
	c := f.userWithCreds("sub-cycle", core.RoleUser)
	actor := Actor{Kind: ActorAdmin, UserID: admin.ID}
	if err := f.s.DisableUser(ctx, actor, c.user.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.EnableUser(ctx, actor, c.user.ID); err != nil {
		t.Fatalf("EnableUser = %v", err)
	}
	u, _ := f.s.User(ctx, c.user.ID)
	if u.Status != StatusActive {
		t.Fatalf("status after enable = %q", u.Status)
	}
	f.assertCredsDenied(f.s, c)
	// Enabling does not count as a login: new credentials need a fresh one.
	if _, err := f.s.CreateKey(ctx, c.user.ID, "new", 0); !errors.Is(err, ErrStaleLogin) {
		t.Fatalf("CreateKey right after enable = %v, want ErrStaleLogin", err)
	}
	f.user("sub-cycle", core.RoleUser)
	k, err := f.s.CreateKey(ctx, c.user.ID, "new", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AuthenticateKey(ctx, k.Token); err != nil {
		t.Fatalf("new key after re-login = %v", err)
	}
	f.assertCredsDenied(f.s, c)
	if err := f.s.EnableUser(ctx, actor, c.user.ID); err != nil {
		t.Fatalf("enable active user = %v, want nil", err)
	}
}

func TestLifecycleActorsAndUnknownUsers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.userWithCreds("sub-x", core.RoleUser)
	for _, a := range []Actor{
		{Kind: ActorUser, UserID: u.user.ID},  // users cannot change their own status
		{Kind: ActorAdmin, UserID: u.user.ID}, // not an admin
		{},
	} {
		if err := f.s.DisableUser(ctx, a, u.user.ID); !errors.Is(err, ErrInvalid) {
			t.Errorf("DisableUser by %+v = %v, want ErrInvalid", a, err)
		}
	}
	f.assertCredsWork(f.s, u)
	for name, op := range map[string]func(context.Context, Actor, string) error{
		"disable": f.s.DisableUser, "enable": f.s.EnableUser,
	} {
		if err := op(ctx, Actor{Kind: ActorCLI}, "u_unknown"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s unknown = %v, want ErrNotFound", name, err)
		}
	}
}
