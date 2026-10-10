package app

// SIGHUP reload must enforce the same identity file invariants as startup
// (config.CheckFiles) in multi-user mode, and no generation may accept the
// OIDC client secret as a static client bearer. Legacy reload is unchanged.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertReloadRefusedOldServing(t *testing.T, a *App, path string) {
	t.Helper()
	before := a.ReloadStatus().Generation
	st, err := a.Reload(path)
	if err == nil {
		t.Fatalf("reload accepted a key file aliasing the OIDC client secret: %+v", st)
	}
	if st.OK || st.Generation != before {
		t.Fatalf("status = %+v; want failed reload at generation %d", st, before)
	}
	for _, s := range []string{st.Reason, err.Error()} {
		if strings.Contains(s, fixtureClientSecret) {
			t.Fatalf("reload failure leaks the client secret: %q", s)
		}
	}
	if w := serve(a, "GET", "/control/v1/status", "", withBearer(fixtureClientSecret)); w.Code != http.StatusUnauthorized {
		t.Fatalf("OIDC client secret as bearer: %d, want 401", w.Code)
	}
	if w := serve(a, "GET", "/control/v1/status", "", withBearer("svc-static-key-000000000000")); w.Code != http.StatusOK {
		t.Fatalf("old generation static key: %d, want 200", w.Code)
	}
}

func TestReloadRefusesKeyFileAliasingClientSecret(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(oldname, newname string) error
	}{
		{"symlink", os.Symlink},
		{"hardlink", os.Link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIDFixture(t)
			a := f.build(Overrides{IdentityClock: newIDClock()})
			alias := filepath.Join(f.dir, "mon.key")
			if err := tc.link(filepath.Join(f.dir, "oidc.secret"), alias); err != nil {
				t.Fatal(err)
			}
			f.clients = "  - {name: mon, class: background, key_file: " + alias + ", role: service}\n"
			path := f.write()
			assertReloadRefusedOldServing(t, a, path)
			if st := a.ReloadStatus(); st.Reason != "configuration invalid" {
				t.Fatalf("reason = %q, want configuration invalid", st.Reason)
			}
		})
	}
}

func TestMakeGenerationRefusesKeyEqualToClientSecret(t *testing.T) {
	f := newIDFixture(t)
	a := f.build(Overrides{IdentityClock: newIDClock()})
	copyKey := filepath.Join(f.dir, "mon.key")
	bcWrite(t, copyKey, fixtureClientSecret+"\n")
	f.clients = "  - {name: mon, class: background, key_file: " + copyKey + ", role: service}\n"
	// Separate files with equal bytes pass CheckFiles; makeGeneration must
	// still refuse the generation.
	assertReloadRefusedOldServing(t, a, f.write())
	if st := a.ReloadStatus(); st.Reason != "client keys invalid" {
		t.Fatalf("reason = %q, want client keys invalid", st.Reason)
	}
}

// Legacy reload does not run CheckFiles: a group/world-writable config file,
// which startup checks reject, still reloads exactly as before.
func TestLegacyReloadSkipsCheckFiles(t *testing.T) {
	f := newIDFixture(t)
	f.identity = false
	a := f.build(Overrides{})
	f.clients = "  - {name: extra, class: interactive, key_file: " + filepath.Join(f.dir, "extra.key") + "}\n"
	bcWrite(t, filepath.Join(f.dir, "extra.key"), "extra-static-key-00000000000")
	path := f.write()
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := f.load().CheckFiles(path); err == nil {
		t.Fatal("expected CheckFiles to reject a world-writable config file")
	}
	st, err := a.Reload(path)
	if err != nil || !st.OK || st.Generation != 2 {
		t.Fatalf("legacy reload: %+v, %v; want OK generation 2", st, err)
	}
}

// The same world-writable config file is refused on reload in multi-user mode.
func TestMultiUserReloadRunsCheckFiles(t *testing.T) {
	f := newIDFixture(t)
	a := f.build(Overrides{IdentityClock: newIDClock()})
	path := f.write()
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	st, err := a.Reload(path)
	if err == nil || st.OK || st.Reason != "configuration invalid" || st.Generation != 1 {
		t.Fatalf("multi-user reload of world-writable config: %+v, %v", st, err)
	}
}
