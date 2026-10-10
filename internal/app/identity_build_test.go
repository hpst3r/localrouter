package app

// Build/Close lifecycle of the multi-user identity runtime: identity.lock
// ownership, identity.db open/pin, ledger RequireScope, and cleanup on every
// failure path. Real config files, real SQLite, no network (OIDC discovery is
// lazy, so Build never contacts the issuer).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
)

const idUpstreamKey = "id-upstream-key-000000000000"

// idFixture is a multi-user config on disk. Rewrite with write() after
// changing fields.
type idFixture struct {
	t        *testing.T
	dir      string
	issuer   string
	clients  string // extra YAML under clients:
	budgets  string // whole budgets block, "" for none
	limits   string // whole limits block, "" for none
	identity bool
	access   string // access block body override
	upstream string // account base URL (without /v1)
}

func newIDFixture(t *testing.T) *idFixture {
	t.Helper()
	t.Setenv("ID_UPSTREAM_KEY", idUpstreamKey)
	f := &idFixture{t: t, dir: t.TempDir(), issuer: idTestIssuer, identity: true, upstream: "http://127.0.0.1:1"}
	bcWrite(t, filepath.Join(f.dir, "pricing.yaml"), "models:\n  id-priced:\n    input: 1\n    output: 0\n")
	bcWrite(t, filepath.Join(f.dir, "oidc.secret"), "fixture-client-secret")
	bcWrite(t, filepath.Join(f.dir, "svc.key"), "svc-static-key-000000000000")
	return f
}

func (f *idFixture) dataDir() string { return filepath.Join(f.dir, "data") }

func (f *idFixture) body() string {
	b := "listen: 127.0.0.1:0\n" +
		"data_dir: " + f.dataDir() + "\n" +
		"pricing_file: " + filepath.Join(f.dir, "pricing.yaml") + "\n" +
		"control: {require_auth: true}\n" +
		"accounts:\n  - id: acct\n    provider: openai_compat\n    base_url: " + f.upstream + "/v1\n    api_key_env: ID_UPSTREAM_KEY\n" +
		"routes:\n  - name: r\n    models: [id-priced]\n    interactive: [acct]\n    background: [acct]\n"
	if f.identity {
		access := f.access
		if access == "" {
			access = "    claim: groups\n    user_values: [lr-users]\n    admin_values: [lr-admins]\n"
		}
		b += "identity:\n  public_base_url: https://localhost:8787\n" +
			"  oidc:\n    issuer: " + f.issuer + "\n    client_id: " + idTestClientID + "\n    client_secret_file: " + filepath.Join(f.dir, "oidc.secret") + "\n" +
			"  access:\n" + access
		b += "clients:\n  - {name: svc, class: background, key_file: " + filepath.Join(f.dir, "svc.key") + ", role: service, ingest: true}\n"
	} else {
		b += "clients:\n  - {name: svc, class: background, key_file: " + filepath.Join(f.dir, "svc.key") + ", ingest: true}\n"
	}
	b += f.clients
	if f.budgets != "" {
		b += f.budgets
	}
	if f.limits != "" {
		b += f.limits
	}
	return b
}

func (f *idFixture) write() string {
	f.t.Helper()
	p := filepath.Join(f.dir, "config.yaml")
	bcWrite(f.t, p, f.body())
	return p
}

func (f *idFixture) load() *config.Config {
	f.t.Helper()
	cfg, err := config.Load(f.write())
	if err != nil {
		f.t.Fatalf("load config: %v", err)
	}
	return cfg
}

func (f *idFixture) build(ov Overrides) *App {
	f.t.Helper()
	a, err := Build(f.load(), bcLogger(), ov)
	if err != nil {
		f.t.Fatalf("build: %v", err)
	}
	f.t.Cleanup(func() { _ = a.Close() })
	return a
}

// lockFree reports whether identity.lock in dir can be acquired right now.
func lockFree(t *testing.T, dataDir string) bool {
	t.Helper()
	l, err := acquireIdentityLock(filepath.Join(dataDir, "identity.lock"))
	if errors.Is(err, errIdentityLockHeld) {
		return false
	}
	if err != nil {
		t.Fatalf("probe lock: %v", err)
	}
	_ = l.Close()
	return true
}

func TestBuildIdentityOpensStoreAndHoldsLock(t *testing.T) {
	ctx := context.Background()
	f := newIDFixture(t)
	a, err := Build(f.load(), bcLogger(), Overrides{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a.Identity == nil || a.WebLogin == nil {
		t.Fatalf("Identity = %v, WebLogin = %v; want both set", a.Identity, a.WebLogin)
	}
	if err := a.Identity.Ping(ctx); err != nil {
		t.Fatalf("identity ping: %v", err)
	}
	fi, err := os.Stat(filepath.Join(f.dataDir(), "identity.db"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity.db: %v mode %v", err, fi)
	}
	if lockFree(t, f.dataDir()) {
		t.Fatal("identity.lock is not held while the app is open")
	}
	if _, err := a.Ledger.Summary(ctx, time.Time{}, "client"); !errors.Is(err, core.ErrInvalidScope) {
		t.Fatalf("unscoped ledger read in multi-user mode: err = %v, want ErrInvalidScope", err)
	}
	if b, err := Build(f.load(), bcLogger(), Overrides{}); err == nil {
		_ = b.Close()
		t.Fatal("second Build on the same data dir succeeded")
	} else if !errors.Is(err, errIdentityLockHeld) {
		t.Fatalf("second Build err = %v, want errIdentityLockHeld", err)
	}
	if err := a.Identity.Ping(ctx); err != nil {
		t.Fatalf("the refused second Build disturbed the first: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := a.Identity.Ping(ctx); !errors.Is(err, identity.ErrClosed) {
		t.Fatalf("identity after Close: err = %v, want ErrClosed", err)
	}
	if !lockFree(t, f.dataDir()) {
		t.Fatal("identity.lock still held after Close")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestBuildLegacyHasNoIdentity(t *testing.T) {
	ctx := context.Background()
	f := newIDFixture(t)
	f.identity = false
	f.write()
	cfg, err := config.Load(filepath.Join(f.dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build(cfg, bcLogger(), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Identity != nil || a.WebLogin != nil {
		t.Fatal("legacy config built identity components")
	}
	for _, name := range []string{"identity.db", "identity.lock"} {
		if _, err := os.Stat(filepath.Join(f.dataDir(), name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy mode created %s (err %v)", name, err)
		}
	}
	if _, err := a.Ledger.Summary(ctx, time.Time{}, "client"); err != nil {
		t.Fatalf("legacy unscoped ledger read: %v", err)
	}
}

// TestBuildIdentityBindingMismatchFailsClosed: identity.db pins the issuer on
// first start; a different issuer refuses startup and releases everything.
func TestBuildIdentityBindingMismatchFailsClosed(t *testing.T) {
	f := newIDFixture(t)
	a := f.build(Overrides{})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	f.issuer = "https://other-idp.fixture.test/o/localrouter/"
	_, err := Build(f.load(), bcLogger(), Overrides{})
	if !errors.Is(err, identity.ErrBindingMismatch) {
		t.Fatalf("build with another issuer: err = %v, want ErrBindingMismatch", err)
	}
	if strings.Contains(err.Error(), "other-idp") {
		t.Fatalf("startup error echoes the issuer: %v", err)
	}
	if !lockFree(t, f.dataDir()) {
		t.Fatal("failed Build left identity.lock held")
	}
	f.issuer = idTestIssuer
	f.build(Overrides{})
}

// TestIdentityLockIsServerOnly: identity.lock excludes a second server, not
// the `localrouter users` CLI, which opens identity.db next to the running
// app without the lock. Its local disable is effective on the app's very next
// request, and the app keeps its ownership throughout.
func TestIdentityLockIsServerOnly(t *testing.T) {
	ctx := context.Background()
	clock := newIDClock()
	f := newIDFixture(t)
	cfg := f.load()
	a := f.build(Overrides{IdentityClock: clock})
	uid, key, sess := seedUser(t, a, clock, "cli-target", "user")

	opts := cfg.Identity.StoreOptions()
	opts.Clock = clock
	cli, err := identity.Open(ctx, filepath.Join(f.dataDir(), "identity.db"), opts)
	if err != nil {
		t.Fatalf("CLI-style open next to the running app: %v", err)
	}
	defer cli.Close()
	if lockFree(t, f.dataDir()) {
		t.Fatal("the app lost identity.lock when the CLI opened the database")
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer(key)); w.Code != 200 {
		t.Fatalf("key before the CLI disable: %d", w.Code)
	}
	if err := cli.DisableUser(ctx, identity.Actor{Kind: identity.ActorCLI}, uid); err != nil {
		t.Fatalf("CLI disable: %v", err)
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer(key)); w.Code != 401 {
		t.Fatalf("key after the CLI disable: %d, want 401", w.Code)
	}
	if w := serve(a, "GET", "/ui/v1/me", "", sessionReq(sess)); w.Code != 401 {
		t.Fatalf("session after the CLI disable: %d, want 401", w.Code)
	}
}

// TestBuildIdentityBudgetOwnershipFailureReleasesLock: when the budget store
// is owned elsewhere, Build fails after identity.db is open and releases the
// identity store and lock.
func TestBuildIdentityBudgetOwnershipFailureReleasesLock(t *testing.T) {
	f := newIDFixture(t)
	f.budgets = "budgets:\n  reserve_usd: \"0.01\"\n  users: {daily_usd: \"1\"}\n"
	cfg := f.load()
	if err := os.MkdirAll(f.dataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := budget.AcquireOwnership(filepath.Join(f.dataDir(), "budgets.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if a, err := Build(cfg, bcLogger(), Overrides{}); !errors.Is(err, budget.ErrOwnershipHeld) {
		if a != nil {
			_ = a.Close()
		}
		t.Fatalf("Build with budgets owned elsewhere: %v, want ErrOwnershipHeld", err)
	}
	if !lockFree(t, f.dataDir()) {
		t.Fatal("failed Build left identity.lock held")
	}
	_ = held.Close()
	f.build(Overrides{})
}

// TestBuildIdentityLateFailureReleasesLock: a failure after identity.db is
// open (here the first generation's client keys) closes the store and drops
// the lock.
func TestBuildIdentityLateFailureReleasesLock(t *testing.T) {
	f := newIDFixture(t)
	cfg := f.load()
	if err := os.Remove(filepath.Join(f.dir, "svc.key")); err != nil {
		t.Fatal(err)
	}
	if a, err := Build(cfg, bcLogger(), Overrides{}); err == nil {
		_ = a.Close()
		t.Fatal("Build with a missing client key succeeded")
	}
	if !lockFree(t, f.dataDir()) {
		t.Fatal("failed Build left identity.lock held")
	}
}
