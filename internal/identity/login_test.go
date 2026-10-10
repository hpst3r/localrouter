package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestResolveLoginRequiresProvisionForUnknownSubject(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	l := f.login("unknown-sub", core.RoleUser)
	l.Provision = false
	if _, err := f.s.ResolveLogin(ctx, l); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("ResolveLogin without provision = %v, want ErrNotProvisioned", err)
	}
	users, err := f.s.ListUsers(ctx, "", MaxListLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatalf("users after denied JIT = %d, want 0", len(users))
	}
}

func TestResolveLoginProvisionsOpaqueUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	l := f.login("sub-alice", core.RoleAdmin)
	u, err := f.s.ResolveLogin(ctx, l)
	if err != nil {
		t.Fatalf("ResolveLogin = %v", err)
	}
	if !strings.HasPrefix(u.ID, "u_") || len(u.ID) != 2+26 {
		t.Fatalf("user id %q is not an opaque u_<base32> id", u.ID)
	}
	for _, pii := range []string{"alice", "example", "sub"} {
		if strings.Contains(u.ID, pii) {
			t.Fatalf("user id %q derived from identity data", u.ID)
		}
	}
	if u.Status != StatusActive || u.Role != core.RoleAdmin {
		t.Fatalf("user = %+v, want active admin", u)
	}
	if !u.LastLoginAt.Equal(l.AuthTime) || u.Email != l.Email || u.DisplayName != l.DisplayName {
		t.Fatalf("user = %+v, want login time/profile from %+v", u, l)
	}
	got, err := f.s.User(ctx, u.ID)
	if err != nil || got != u {
		t.Fatalf("User(id) = %+v, %v; want %+v", got, err, u)
	}
}

func TestResolveLoginIdentityIsIssuerSubjectNotEmail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.user("sub-1", core.RoleUser)

	again := f.login("sub-1", core.RoleUser)
	again.Email = "changed@example.test"
	again.DisplayName = "Changed"
	f.clock.Advance(time.Hour)
	again.AuthTime = f.clock.Now()
	u, err := f.s.ResolveLogin(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != first.ID || u.Email != "changed@example.test" || !u.LastLoginAt.Equal(again.AuthTime) {
		t.Fatalf("re-login = %+v, want same user %s with updated profile/login", u, first.ID)
	}

	other := f.login("sub-2", core.RoleUser)
	other.Email = "changed@example.test" // same email, different subject
	u2, err := f.s.ResolveLogin(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if u2.ID == first.ID {
		t.Fatal("same email linked two different subjects to one user")
	}
}

func TestResolveLoginAppliesPolicyRoleEachLogin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.user("sub-role", core.RoleAdmin)
	u, err := f.s.ResolveLogin(ctx, f.login("sub-role", core.RoleUser))
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != admin.ID || u.Role != core.RoleUser {
		t.Fatalf("demoting login = %+v, want same user with role user", u)
	}
}

func TestResolveLoginRejectsInvalidInput(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const secretSubject = "very-private-subject"
	cases := map[string]struct {
		mutate func(*Login)
		want   error
	}{
		"other issuer":     {func(l *Login) { l.Issuer = "https://evil.example.test/" }, ErrInvalid},
		"empty subject":    {func(l *Login) { l.Subject = "" }, ErrInvalid},
		"long subject":     {func(l *Login) { l.Subject = strings.Repeat("s", MaxSubjectBytes+1) }, ErrInvalid},
		"invalid utf8 sub": {func(l *Login) { l.Subject = secretSubject + "\xff" }, ErrInvalid},
		"control sub":      {func(l *Login) { l.Subject = secretSubject + "\n" }, ErrInvalid},
		"legacy role":      {func(l *Login) { l.Role = core.RoleLegacy }, ErrInvalid},
		"service role":     {func(l *Login) { l.Role = core.RoleService }, ErrInvalid},
		"empty role":       {func(l *Login) { l.Role = "" }, ErrInvalid},
		"zero auth time":   {func(l *Login) { l.AuthTime = time.Time{} }, ErrInvalid},
		"future auth time": {func(l *Login) { l.AuthTime = f.clock.Now().Add(2 * time.Minute) }, ErrInvalid},
		"stale auth time":  {func(l *Login) { l.AuthTime = f.clock.Now().Add(-DefaultMaxAuthAge - time.Second) }, ErrStaleLogin},
	}
	for name, c := range cases {
		l := f.login(secretSubject, core.RoleUser)
		c.mutate(&l)
		_, err := f.s.ResolveLogin(ctx, l)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: ResolveLogin = %v, want %v", name, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), secretSubject) || strings.Contains(err.Error(), "example.test") {
			t.Errorf("%s: error %q leaks login data", name, err)
		}
	}
	users, err := f.s.ListUsers(ctx, "", MaxListLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatalf("rejected logins created %d users", len(users))
	}
}

func TestResolveLoginBoundsProfileText(t *testing.T) {
	f := newFixture(t)
	l := f.login("sub-profile", core.RoleUser)
	l.DisplayName = "Bad\xffName\x00" + strings.Repeat("x", 500)
	l.Email = strings.Repeat("e", 500) + "@example.test"
	u, err := f.s.ResolveLogin(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{u.DisplayName, u.Email} {
		if !utf8.ValidString(v) || len(v) > core.MaxLabelBytes || strings.ContainsRune(v, 0) {
			t.Fatalf("stored profile value %q not sanitized/bounded", v)
		}
	}
}
