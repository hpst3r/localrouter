package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// A narrowing is ordered by the policy generation, not the wall clock: a
// login recorded while the clock ran fast does not survive a later
// narrowing, and a clock that was fast at the narrowing and later stepped
// back does not lock out allowed logins.
func TestPolicyGenerationIgnoresClockSteps(t *testing.T) {
	ctx := context.Background()

	t.Run("fast login does not survive narrowing", func(t *testing.T) {
		f := newFixture(t)
		f.apply(f.s, basePolicy)
		f.clock.Advance(time.Hour) // server clock fast by 1h
		u := f.user("contractor", core.RoleUser)
		k, err := f.s.CreateKey(ctx, u.ID, "ci", 0)
		if err != nil {
			t.Fatal(err)
		}
		f.register(f.s, u.ID, staticTokA)
		f.clock.Advance(-55 * time.Minute) // NTP steps the clock back
		narrow := basePolicy
		narrow.UserValues = []string{"lr-users"} // contractors removed
		if !f.apply(f.s, narrow) {
			t.Fatal("not narrowed")
		}
		if _, err := f.s.AuthenticateKey(ctx, k.Token); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("key after narrowing = %v, want ErrUnauthenticated", err)
		}
		f.staticDenied(u.ID, staticTokA, "after narrowing")
		if _, err := f.s.CreateKey(ctx, u.ID, "ci2", 0); !errors.Is(err, ErrStaleLogin) {
			t.Fatalf("CreateKey after narrowing = %v, want ErrStaleLogin", err)
		}
		if _, err := f.s.CreateSession(ctx, u.ID); !errors.Is(err, ErrStaleLogin) {
			t.Fatalf("CreateSession after narrowing = %v, want ErrStaleLogin", err)
		}
	})

	t.Run("clock stepped back after narrowing", func(t *testing.T) {
		f := newFixture(t)
		f.apply(f.s, basePolicy)
		admin := f.user("root-sub", core.RoleAdmin)
		f.clock.Advance(time.Hour) // fast clock at restart
		narrow := basePolicy
		narrow.AdminSubjects = nil
		if !f.apply(f.s, narrow) {
			t.Fatal("not narrowed")
		}
		f.clock.Advance(-time.Hour + time.Minute) // clock corrected
		if _, err := f.s.ResolveLogin(ctx, f.login("root-sub", core.RoleAdmin)); err != nil {
			t.Fatal(err)
		}
		sess, err := f.s.CreateSession(ctx, admin.ID)
		if err != nil {
			t.Fatalf("CreateSession after allowed login = %v", err)
		}
		if _, err := f.s.AuthenticateSession(ctx, sess.Token); err != nil {
			t.Fatalf("session after allowed login = %v", err)
		}
		if _, err := f.s.CreateKey(ctx, admin.ID, "k", 0); err != nil {
			t.Fatalf("CreateKey after allowed login = %v", err)
		}
	})
}

// Last login is the verified auth time exactly; a narrowing does not
// rewrite it and a later login does not raise it.
func TestResolveLoginStoresAuthTimeAcrossNarrowing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.apply(f.s, basePolicy)
	u := f.user("nora", core.RoleUser)
	f.clock.Advance(time.Minute)
	narrow := basePolicy
	narrow.UserValues = []string{"lr-users"}
	if !f.apply(f.s, narrow) {
		t.Fatal("not narrowed")
	}
	l := f.login("nora", core.RoleUser)
	l.AuthTime = f.clock.Now().Add(-30 * time.Second) // predates the narrowing
	if _, err := f.s.ResolveLogin(ctx, l); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.User(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastLoginAt.Equal(l.AuthTime.UTC().Truncate(time.Millisecond)) {
		t.Fatalf("last login = %v, want auth time %v", got.LastLoginAt, l.AuthTime)
	}
	if _, err := f.s.CreateSession(ctx, u.ID); err != nil {
		t.Fatalf("CreateSession after re-login = %v", err)
	}
}

// A narrowing applied through one *Store (the CLI process) is seen by
// another on the same file (the server) on its next read, and a login
// through the server is seen by the CLI.
func TestPolicyGenerationSharedAcrossStores(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.apply(f.s, basePolicy)
	u := f.user("nora", core.RoleUser)
	k, err := f.s.CreateKey(ctx, u.ID, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	cli := f.second()
	narrow := basePolicy
	narrow.UserValues = []string{"lr-users"}
	if !f.apply(cli, narrow) {
		t.Fatal("not narrowed")
	}
	if _, err := f.s.AuthenticateKey(ctx, k.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("server: key after CLI narrowing = %v", err)
	}
	if _, err := f.s.ResolveLogin(ctx, f.login("nora", core.RoleUser)); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.AuthenticateKey(ctx, k.Token); err != nil {
		t.Fatalf("cli: key after server re-login = %v", err)
	}
	// A second narrowing suspends again.
	narrow.UserValues = nil
	narrow.AdminValues = []string{"lr-users", "lr-admins"}
	narrow.Claim = "roles"
	if !f.apply(f.s, narrow) {
		t.Fatal("second narrowing not detected")
	}
	if _, err := cli.AuthenticateKey(ctx, k.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("cli: key after second narrowing = %v", err)
	}
}

// Every open of a store at the current version has users.login_generation.
func TestFreshDatabaseHasLoginGeneration(t *testing.T) {
	f := newFixture(t)
	var n int
	if err := f.s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'login_generation' AND "notnull" = 1 AND dflt_value = '0'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("users.login_generation INTEGER NOT NULL DEFAULT 0 missing")
	}
}

// An identity.db at schema v2 (no login generation) migrates to v3 in
// place: users, keys, static keys and sessions keep working, and a stale
// policy_epoch left by a pre-release build is ignored.
func TestIdentityMigratesV2Database(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.apply(f.s, basePolicy)
	u := f.user("sub-a", core.RoleUser)
	k, err := f.s.CreateKey(ctx, u.ID, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.register(f.s, u.ID, staticTokA)
	// Roll the file back to exactly the v2 shape.
	for _, q := range []string{
		`ALTER TABLE users DROP COLUMN login_generation`,
		`DELETE FROM meta WHERE key = 'policy_generation'`,
		`INSERT INTO meta (key, value) VALUES ('policy_epoch', '9999999999999')`,
		`UPDATE schema_version SET version = 2`,
	} {
		if _, err := f.s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	f.s.Close()

	s, err := Open(ctx, f.path, f.opts)
	if err != nil {
		t.Fatalf("Open v2 database = %v", err)
	}
	defer s.Close()
	f.s = s
	var v int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != len(migrations) || v != 3 {
		t.Fatalf("schema version = %d, %v; want 3", v, err)
	}
	var gen int64
	if err := s.db.QueryRow(`SELECT login_generation FROM users WHERE id = ?`, u.ID).Scan(&gen); err != nil || gen != 0 {
		t.Fatalf("migrated login_generation = %d, %v; want 0", gen, err)
	}
	if f.apply(s, basePolicy) {
		t.Fatal("unchanged policy narrowed after migration")
	}
	if _, err := s.AuthenticateKey(ctx, k.Token); err != nil {
		t.Fatalf("v2 API key after migration = %v", err)
	}
	f.staticOK(u.ID, staticTokA)
	if _, err := s.CreateSession(ctx, u.ID); err != nil {
		t.Fatalf("CreateSession after migration = %v", err)
	}
	narrow := basePolicy
	narrow.UserValues = []string{"lr-users"}
	if !f.apply(s, narrow) {
		t.Fatal("not narrowed")
	}
	if _, err := s.AuthenticateKey(ctx, k.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("migrated key after narrowing = %v", err)
	}
}
