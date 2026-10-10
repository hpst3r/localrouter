package app

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

// TestGenerationModeMustMatchIdentityRuntime: a generation is never built in
// multi-user mode without the process's identity store and login service
// (which would leave the principal seam nil or unbacked), nor in legacy mode
// while an identity store is open (a silent downgrade to legacy auth).
func TestGenerationModeMustMatchIdentityRuntime(t *testing.T) {
	f := newIDFixture(t)
	a := f.build(Overrides{})
	multi := f.load()

	store, svc := a.Identity, a.WebLogin
	a.Identity, a.WebLogin = nil, nil
	if _, reason, err := a.makeGeneration(multi, 99); err == nil || reason != "identity invalid" {
		t.Fatalf("multi-user generation without an identity store: reason %q err %v", reason, err)
	}
	a.Identity = store
	if _, _, err := a.makeGeneration(multi, 99); err == nil {
		t.Fatal("multi-user generation without a login service was built")
	}
	a.WebLogin = svc

	f.identity = false
	legacy := f.load()
	if _, reason, err := a.makeGeneration(legacy, 99); err == nil || reason != "identity invalid" {
		t.Fatalf("legacy generation with an open identity store: reason %q err %v", reason, err)
	}
}

// TestStaticClientCannotTakeUserKeyName: a user API key is the client named
// by its key id ("k_..."), so in multi-user mode a static client in that
// namespace would share a user key's per-client budget, concurrency slots
// and ledger client label. Config validation refuses the name; the
// generation build refuses it again (defense in depth, for a config that
// skipped validation), so the live generation keeps serving the user key.
func TestStaticClientCannotTakeUserKeyName(t *testing.T) {
	clock := newIDClock()
	up := newFakeUpstream(t, false)
	f := newIDFixture(t)
	f.upstream = up.srv.URL
	f.budgets = "budgets:\n  reserve_usd: \"0.01\"\n  users: {daily_usd: \"5\"}\n"
	a := f.build(Overrides{IdentityClock: clock})
	uid, key, _ := seedUser(t, a, clock, "kira", weblogin.RoleUser)
	keys, err := a.Identity.ListKeys(context.Background(), uid)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list keys: %v %v", keys, err)
	}
	kid := keys[0].ID
	collide := config.ClientConfig{Name: kid, Class: "interactive", KeyFile: filepath.Join(f.dir, "collide.key"), Role: "service"}
	bcWrite(t, collide.KeyFile, "collide-static-key-0000000000")
	unvalidated := func() *config.Config {
		c := f.load()
		c.Clients = append(c.Clients, collide)
		return c
	}

	f.clients = "  - {name: " + kid + ", class: interactive, key_file: " + collide.KeyFile + ", role: service}\n"
	if _, err := config.Load(f.write()); err == nil || !strings.Contains(err.Error(), "must not start with k_") {
		t.Fatalf("config.Load of a static client named like a user key id: %v", err)
	}
	f.clients = ""

	if _, err := a.ReloadConfig(unvalidated()); err == nil {
		w := serve(a, "POST", "/v1/chat/completions", chatBody, withBearer(key))
		t.Fatalf("reload accepted a static client named like a user key id; that user's key now gets %d", w.Code)
	}
	if w := serve(a, "POST", "/v1/chat/completions", chatBody, withBearer(key)); w.Code != http.StatusOK {
		t.Fatalf("user key after the refused reload: %d %s", w.Code, w.Body)
	}

	cfg := unvalidated()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := Build(cfg, bcLogger(), Overrides{}); err == nil {
		_ = b.Close()
		t.Fatal("Build accepted a static client named like a user key id")
	} else if !strings.Contains(err.Error(), "user API key namespace") || strings.Contains(err.Error(), kid) {
		t.Fatalf("Build refusal: %v", err)
	}
}
