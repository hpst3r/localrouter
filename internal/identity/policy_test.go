package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

var basePolicy = AccessPolicy{
	Claim:         "groups",
	UserValues:    []string{"lr-users", "contractors"},
	AdminValues:   []string{"lr-admins"},
	AdminSubjects: []string{"root-sub"},
}

func (f *fixture) apply(s *Store, p AccessPolicy) bool {
	f.t.Helper()
	narrowed, err := s.ApplyAccessPolicy(context.Background(), p)
	if err != nil {
		f.t.Fatalf("ApplyAccessPolicy = %v", err)
	}
	return narrowed
}

func TestAccessPolicyNarrows(t *testing.T) {
	with := func(mod func(*AccessPolicy)) AccessPolicy {
		p := basePolicy
		mod(&p)
		return p.canonical()
	}
	cases := []struct {
		name string
		p    AccessPolicy
		want bool
	}{
		{"identical", with(func(*AccessPolicy) {}), false},
		{"reordered and duplicated", with(func(p *AccessPolicy) {
			p.UserValues = []string{"contractors", "lr-users", "contractors"}
		}), false},
		{"user value added", with(func(p *AccessPolicy) { p.UserValues = append(p.UserValues, "interns") }), false},
		{"admin value added", with(func(p *AccessPolicy) { p.AdminValues = append(p.AdminValues, "lr-root") }), false},
		{"admin subject added", with(func(p *AccessPolicy) { p.AdminSubjects = append(p.AdminSubjects, "x") }), false},
		{"user value promoted to admin", with(func(p *AccessPolicy) {
			p.UserValues = []string{"lr-users"}
			p.AdminValues = []string{"lr-admins", "contractors"}
		}), false},
		{"claim changed", with(func(p *AccessPolicy) { p.Claim = "roles" }), true},
		{"user value removed", with(func(p *AccessPolicy) { p.UserValues = []string{"lr-users"} }), true},
		{"admin value removed", with(func(p *AccessPolicy) { p.AdminValues = []string{"lr-ops"} }), true},
		{"admin value demoted to user", with(func(p *AccessPolicy) {
			p.UserValues = append(p.UserValues, "lr-admins")
			p.AdminValues = nil
		}), true},
		{"admin subject removed", with(func(p *AccessPolicy) { p.AdminSubjects = nil }), true},
	}
	old := basePolicy.canonical()
	for _, c := range cases {
		if got := c.p.narrows(old); got != c.want {
			t.Errorf("%s: narrows = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestApplyAccessPolicyFirstRecordWithoutUsers(t *testing.T) {
	f := newFixture(t)
	if f.apply(f.s, basePolicy) {
		t.Fatal("first policy on an empty store narrowed")
	}
	u := f.user("nora", core.RoleAdmin)
	k, err := f.s.CreateKey(context.Background(), u.ID, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	if f.apply(f.s, basePolicy) {
		t.Fatal("identical policy narrowed")
	}
	widened := basePolicy
	widened.UserValues = append([]string{"interns"}, widened.UserValues...)
	if f.apply(f.s, widened) {
		t.Fatal("widened policy narrowed")
	}
	if _, err := f.s.AuthenticateKey(context.Background(), k.Token); err != nil {
		t.Fatalf("key after widening = %v", err)
	}
	if got, _ := f.s.User(context.Background(), u.ID); got.Role != core.RoleAdmin {
		t.Fatalf("admin demoted by a widening: %q", got.Role)
	}
}

// Users but no recorded policy (an upgraded store) is treated as narrowed.
// A narrowing deletes every session, demotes every admin and suspends every
// credential, in this and any other *Store on the file, until the owner's
// next allowed login, which revives (but does not reissue) their keys.
func TestApplyAccessPolicyNarrowingSuspendsCredentials(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.user("root-sub", core.RoleAdmin)
	user := f.user("nora", core.RoleUser)
	key, err := f.s.CreateKey(ctx, user.ID, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := f.s.CreateSession(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st := f.register(f.s, user.ID, staticTokA); st != StaticKeyActive {
		t.Fatalf("static key state = %q", st)
	}
	cli := f.second()

	f.clock.Advance(1500 * time.Millisecond)
	if !f.apply(f.s, basePolicy) {
		t.Fatal("users without a recorded policy did not narrow")
	}
	for name, s := range map[string]*Store{"server": f.s, "cli": cli} {
		if _, err := s.AuthenticateKey(ctx, key.Token); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("%s: key after narrowing = %v", name, err)
		}
		if _, err := s.AuthenticateSession(ctx, sess.Token); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("%s: session after narrowing = %v", name, err)
		}
	}
	f.staticDenied(user.ID, staticTokA, "after narrowing")
	if _, err := f.s.CreateKey(ctx, user.ID, "k2", 0); !errors.Is(err, ErrStaleLogin) {
		t.Fatalf("CreateKey after narrowing = %v, want ErrStaleLogin", err)
	}
	if _, err := f.s.CreateSession(ctx, user.ID); !errors.Is(err, ErrStaleLogin) {
		t.Fatalf("CreateSession after narrowing = %v, want ErrStaleLogin", err)
	}
	if got, _ := f.s.User(ctx, admin.ID); got.Role != core.RoleUser || got.LastLoginAt.IsZero() {
		t.Fatalf("admin after narrowing = %+v; want demoted, last login kept", got)
	}
	if st := f.register(f.s, user.ID, staticTokA); st != StaticKeyActive {
		t.Fatalf("static key re-registered = %q, want active (not revoked)", st)
	}
	evs, err := f.s.AuditEvents(ctx, 0, MaxListLimit)
	if err != nil {
		t.Fatal(err)
	}
	if last := evs[len(evs)-1]; last.Action != AuditAccessPolicyNarrowed || last.ActorKind != ActorSystem {
		t.Fatalf("last audit event = %+v", last)
	}

	// A fresh allowed login whose auth_time predates the narrowing (second
	// precision) still counts as a login under the new policy.
	l := f.login("nora", core.RoleUser)
	l.AuthTime = l.AuthTime.Truncate(time.Second)
	if _, err := f.s.ResolveLogin(ctx, l); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.AuthenticateKey(ctx, key.Token); err != nil {
		t.Fatalf("key after re-login = %v", err)
	}
	f.staticOK(user.ID, staticTokA)
	if _, err := f.s.CreateSession(ctx, user.ID); err != nil {
		t.Fatalf("CreateSession after re-login = %v", err)
	}
	// The admin, not yet logged in again, stays suspended.
	if _, err := f.s.CreateSession(ctx, admin.ID); !errors.Is(err, ErrStaleLogin) {
		t.Fatalf("admin CreateSession before re-login = %v", err)
	}
}
