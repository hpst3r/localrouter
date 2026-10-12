package app

import (
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

// TestMultiUserReloadInvariants: the identity block is restart-only (any
// change, including turning it off), while clients — roles and owners
// included — limits and budget defaults are part of the live generation and
// keep using the one process-wide identity store.
func TestMultiUserReloadInvariants(t *testing.T) {
	clock := newIDClock()
	f := newIDFixture(t)
	f.budgets = "budgets:\n  reserve_usd: \"0.01\"\n  users: {daily_usd: \"5\"}\n"
	a := f.build(Overrides{IdentityClock: clock})
	uid, key, _ := seedUser(t, a, clock, "lee", weblogin.RoleUser)
	store := a.Identity

	reloadExpect := func(name string, wantField string) {
		t.Helper()
		st, err := a.ReloadConfig(f.load())
		if wantField == "" {
			if err != nil {
				t.Fatalf("%s: reload: %v (%+v)", name, err, st)
			}
			return
		}
		if !errors.Is(err, ErrRestartRequired) || !slices.Contains(st.RestartOnly, wantField) {
			t.Fatalf("%s: reload err = %v restart_only = %v, want restart for %q", name, err, st.RestartOnly, wantField)
		}
	}

	f.issuer = "https://other-idp.fixture.test/o/localrouter/"
	reloadExpect("issuer change", "identity")
	f.issuer = idTestIssuer
	f.access = "    claim: groups\n    user_values: [lr-users, more]\n    admin_values: [lr-admins]\n"
	reloadExpect("access policy change", "identity")
	f.access = ""

	// Live: a new owned static client, per-user concurrency and a changed
	// user budget default.
	bcWrite(t, filepath.Join(f.dir, "owned.key"), "owned-static-key-0000000000")
	f.clients = "  - {name: lee-cli, class: interactive, key_file: " + filepath.Join(f.dir, "owned.key") + ", role: user, owner: " + uid + "}\n"
	f.limits = "limits: {max_concurrent_per_user: 3}\n"
	f.budgets = "budgets:\n  reserve_usd: \"0.01\"\n  users: {daily_usd: \"7\"}\n"
	reloadExpect("clients/limits/budgets", "")
	if a.Identity != store {
		t.Fatal("reload replaced the identity store")
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer("owned-static-key-0000000000")); w.Code != http.StatusOK {
		t.Fatalf("reloaded owned static key: %d", w.Code)
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer(key)); w.Code != http.StatusOK {
		t.Fatalf("user key after reload: %d", w.Code)
	}
	if got := a.current.Load().budgets.(*budgetReport).UserLimits(uid); len(got) != 1 || got[0].Micros != 7_000_000 {
		t.Fatalf("reloaded user default = %+v", got)
	}

	// Turning multi-user mode off is a restart too, never a live downgrade
	// to legacy authentication.
	f.identity = false
	f.clients = ""
	f.limits = ""
	f.budgets = "budgets:\n  reserve_usd: \"0.01\"\n  clients:\n    svc: {daily_usd: \"1\"}\n"
	cfgPath := f.write()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := a.ReloadConfig(cfg)
	if !errors.Is(err, ErrRestartRequired) || !slices.Contains(st.RestartOnly, "identity") {
		t.Fatalf("disabling identity: err = %v restart_only = %v", err, st.RestartOnly)
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer(key)); w.Code != http.StatusOK {
		t.Fatalf("failed reload disturbed the live generation: %d", w.Code)
	}
}

// TestMultiUserCloseUnderLoad: Close races in-flight authenticated requests.
// Every response is a clean 200, 401 or 503 (never a panic or a success after
// the store is gone), Close returns, the lock is released and a later reload
// is refused.
func TestMultiUserCloseUnderLoad(t *testing.T) {
	clock := newIDClock()
	f := newIDFixture(t)
	a, err := Build(f.load(), bcLogger(), Overrides{IdentityClock: clock})
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := seedUser(t, a, clock, "max", weblogin.RoleUser)

	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make(chan int, 400)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 50; j++ {
				codes <- serve(a, "GET", "/v1/models", "", withBearer(key)).Code
			}
		}()
	}
	close(start)
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK && c != http.StatusServiceUnavailable && c != http.StatusUnauthorized {
			t.Fatalf("response during Close: %d", c)
		}
	}
	if w := serve(a, "GET", "/v1/models", "", withBearer(key)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("user key after Close: %d, want 503", w.Code)
	}
	if !lockFree(t, f.dataDir()) {
		t.Fatal("identity.lock held after Close")
	}
	if _, err := a.ReloadConfig(f.load()); !errors.Is(err, ErrNotServing) {
		t.Fatalf("reload after Close: %v, want ErrNotServing", err)
	}
}
