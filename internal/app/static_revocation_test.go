package app

// Owned (role user) static keys under local revocation, through the real
// App.Handler: Build/ReloadConfig register every configured owned key in
// identity.db, and disable, policy denial and delete revoke them for good, so
// enable plus a fresh login, a reload, a restart or a restored key file never
// bring one back. Service keys are untouched.

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

const (
	ownedKeyA = "owned-static-key-a-000000000000"
	ownedKeyB = "owned-static-key-b-000000000000"
)

var cliActor = identity.Actor{Kind: identity.ActorCLI}

// ownClient points the fixture's extra clients at one role-user static client
// "gina-cli" owned by uid, holding the given key file names under f.dir.
func ownClient(f *idFixture, uid string, keyFiles ...string) {
	files := ""
	for i, k := range keyFiles {
		if i > 0 {
			files += ", "
		}
		files += filepath.Join(f.dir, k)
	}
	f.clients = "  - {name: gina-cli, class: interactive, key_files: [" + files + "], role: user, owner: " + uid + "}\n"
}

func reloadOK(t *testing.T, a *App, f *idFixture) {
	t.Helper()
	if st, err := a.ReloadConfig(f.load()); err != nil || !st.OK {
		t.Fatalf("reload: %+v %v", st, err)
	}
}

func models(a *App, tok string) int {
	return serve(a, "GET", "/v1/models", "", withBearer(tok)).Code
}

func relogin(t *testing.T, a *App, clock *idClock, sub string) {
	t.Helper()
	if _, err := (loginHooks{store: a.Identity}).CompleteLogin(context.Background(), verified(clock, sub, weblogin.RoleUser), true); err != nil {
		t.Fatalf("login %s: %v", sub, err)
	}
}

// TestOwnedStaticKeyNotResurrectedByEnable is review finding C: after
// disable → enable → fresh login the owned static key used to act for the
// user again; the identity API key stayed revoked.
func TestOwnedStaticKeyNotResurrectedByEnable(t *testing.T) {
	ctx := context.Background()
	clock := newIDClock()
	f := newIDFixture(t)
	a := f.build(Overrides{IdentityClock: clock})
	uid, key, _ := seedUser(t, a, clock, "gina", weblogin.RoleUser)
	bcWrite(t, filepath.Join(f.dir, "a.key"), ownedKeyA)
	ownClient(f, uid, "a.key")
	reloadOK(t, a, f)

	if c := models(a, ownedKeyA); c != http.StatusOK {
		t.Fatalf("owned key baseline: %d", c)
	}
	if err := a.Identity.DisableUser(ctx, cliActor, uid); err != nil {
		t.Fatal(err)
	}
	if c := models(a, ownedKeyA); c != http.StatusUnauthorized {
		t.Fatalf("owned key of disabled owner: %d, want 401", c)
	}
	if err := a.Identity.EnableUser(ctx, cliActor, uid); err != nil {
		t.Fatal(err)
	}
	relogin(t, a, clock, "gina")
	if c := models(a, key); c != http.StatusUnauthorized {
		t.Fatalf("API key resurrected: %d", c)
	}
	if c := models(a, ownedKeyA); c != http.StatusUnauthorized {
		t.Fatalf("owned static key resurrected after disable->enable->login: %d, want 401", c)
	}
	if c := models(a, svcStaticKey); c != http.StatusOK {
		t.Fatalf("service key: %d", c)
	}
}

// Every configured owned key is registered when its generation is built, so
// one never presented before the disable is revoked with the rest.
func TestOwnedStaticKeyNeverUsedIsRevokedToo(t *testing.T) {
	ctx := context.Background()
	clock := newIDClock()
	f := newIDFixture(t)
	a := f.build(Overrides{IdentityClock: clock})
	uid, _, _ := seedUser(t, a, clock, "gina", weblogin.RoleUser)
	bcWrite(t, filepath.Join(f.dir, "a.key"), ownedKeyA)
	ownClient(f, uid, "a.key")
	reloadOK(t, a, f)
	if err := a.Identity.DisableUser(ctx, cliActor, uid); err != nil {
		t.Fatal(err)
	}
	if err := a.Identity.EnableUser(ctx, cliActor, uid); err != nil {
		t.Fatal(err)
	}
	relogin(t, a, clock, "gina")
	if c := models(a, ownedKeyA); c != http.StatusUnauthorized {
		t.Fatalf("never-used owned key after disable->enable->login: %d, want 401", c)
	}
}

// Revocation is durable across a restart, a rotation brings in a working new
// key while the old one stays dead (also when its file is restored), and the
// user's API key and session are unaffected by the static key changes.
func TestOwnedStaticKeyRestartRotationAndRestore(t *testing.T) {
	ctx := context.Background()
	clock := newIDClock()
	f := newIDFixture(t)
	a := f.build(Overrides{IdentityClock: clock})
	uid, _, _ := seedUser(t, a, clock, "gina", weblogin.RoleUser)
	bcWrite(t, filepath.Join(f.dir, "a.key"), ownedKeyA)
	ownClient(f, uid, "a.key")
	reloadOK(t, a, f)
	if err := a.Identity.DenyLogin(ctx, idTestIssuer, "gina"); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	a = f.build(Overrides{IdentityClock: clock}) // restart, same config and data
	relogin(t, a, clock, "gina")
	if c := models(a, ownedKeyA); c != http.StatusUnauthorized {
		t.Fatalf("owned key after policy denial, restart and login: %d, want 401", c)
	}

	_, key, session := seedUser(t, a, clock, "gina", weblogin.RoleUser)
	bcWrite(t, filepath.Join(f.dir, "b.key"), ownedKeyB)
	ownClient(f, uid, "b.key")
	reloadOK(t, a, f)
	if c := models(a, ownedKeyB); c != http.StatusOK {
		t.Fatalf("rotated owned key: %d, want 200", c)
	}
	if c := models(a, ownedKeyA); c != http.StatusUnauthorized {
		t.Fatalf("old owned key after rotation: %d, want 401", c)
	}
	bcWrite(t, filepath.Join(f.dir, "restored.key"), ownedKeyA)
	ownClient(f, uid, "b.key", "restored.key")
	reloadOK(t, a, f)
	if c := models(a, ownedKeyA); c != http.StatusUnauthorized {
		t.Fatalf("restored old owned key: %d, want 401", c)
	}
	if c := models(a, ownedKeyB); c != http.StatusOK {
		t.Fatalf("rotated key next to a restored one: %d, want 200", c)
	}
	if c := models(a, key); c != http.StatusOK {
		t.Fatalf("user API key: %d, want 200", c)
	}
	if _, err := a.Identity.AuthenticateSession(ctx, session); err != nil {
		t.Fatalf("user session: %v", err)
	}
}

// A disable from the users CLI (another *Store on the file, no server lock)
// revokes on the server's next request.
func TestOwnedStaticKeyCLIDisableCrossProcess(t *testing.T) {
	ctx := context.Background()
	clock := newIDClock()
	f := newIDFixture(t)
	a := f.build(Overrides{IdentityClock: clock})
	uid, _, _ := seedUser(t, a, clock, "gina", weblogin.RoleUser)
	bcWrite(t, filepath.Join(f.dir, "a.key"), ownedKeyA)
	ownClient(f, uid, "a.key")
	reloadOK(t, a, f)
	cfg := f.load()
	opts := cfg.Identity.StoreOptions()
	opts.Clock = clock
	cli, err := identity.Open(ctx, filepath.Join(f.dataDir(), "identity.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := cli.DisableUser(ctx, cliActor, uid); err != nil {
		t.Fatal(err)
	}
	if c := models(a, ownedKeyA); c != http.StatusUnauthorized {
		t.Fatalf("owned key after CLI disable: %d, want 401", c)
	}
	if err := cli.EnableUser(ctx, cliActor, uid); err != nil {
		t.Fatal(err)
	}
	relogin(t, a, clock, "gina")
	reloadOK(t, a, f)
	if c := models(a, ownedKeyA); c != http.StatusUnauthorized {
		t.Fatalf("owned key after CLI disable/enable, login and reload: %d, want 401", c)
	}
}

// An owner that is not provisioned does not stop startup; its key is denied.
// A key never moves to another owner: such a reload is refused and the
// running generation keeps serving.
func TestOwnedStaticKeyPendingOwnerAndReassignment(t *testing.T) {
	clock := newIDClock()
	f := newIDFixture(t)
	bcWrite(t, filepath.Join(f.dir, "a.key"), ownedKeyA)
	const unknown = "u_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	ownClient(f, unknown, "a.key")
	a := f.build(Overrides{IdentityClock: clock})
	if c := models(a, ownedKeyA); c != http.StatusUnauthorized {
		t.Fatalf("owned key of an unprovisioned owner: %d, want 401", c)
	}
	if c := models(a, svcStaticKey); c != http.StatusOK {
		t.Fatalf("service key: %d", c)
	}
	uid, _, _ := seedUser(t, a, clock, "gina", weblogin.RoleUser)
	ownClient(f, uid, "a.key")
	before := a.ReloadStatus().Generation
	if st, err := a.ReloadConfig(f.load()); err == nil || st.OK || st.Reason != "client keys invalid" || strings.Contains(err.Error(), ownedKeyA) {
		t.Fatalf("reload moving a key to another owner: %+v %v; want refused as client keys invalid", st, err)
	}
	if g := a.ReloadStatus().Generation; g != before {
		t.Fatalf("generation %d published, want %d kept", g, before)
	}
	if c := models(a, ownedKeyA); c != http.StatusUnauthorized {
		t.Fatalf("reassigned owned key: %d, want 401", c)
	}
}

// A store that cannot register the owned keys fails the generation closed;
// it is not reported as a key file problem.
func TestOwnedStaticKeyRegistrationFailureRefusesGeneration(t *testing.T) {
	clock := newIDClock()
	f := newIDFixture(t)
	a := f.build(Overrides{IdentityClock: clock})
	uid, _, _ := seedUser(t, a, clock, "gina", weblogin.RoleUser)
	bcWrite(t, filepath.Join(f.dir, "a.key"), ownedKeyA)
	ownClient(f, uid, "a.key")
	if err := a.Identity.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err := a.ReloadConfig(f.load()); err == nil || st.OK || st.Reason != "identity unavailable" {
		t.Fatalf("reload with the identity store closed: %+v %v; want refused as identity unavailable", st, err)
	}
}
