package app

import (
	"context"
	"log/slog"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/weblogin"
)

// TestWebLoginConfigIsTheConfigMapping: the login service gets exactly the
// configured block as mapped by config.WebLoginConfig — the access policy
// including admin_subjects, with no invented admin value — plus only the
// process-level seams (roots, test dialer, clock, logger) and the app's hooks.
func TestWebLoginConfigIsTheConfigMapping(t *testing.T) {
	f := newIDFixture(t)
	for name, access := range map[string]string{
		"values and subjects": "    claim: groups\n    user_values: [lr-users]\n    admin_values: [lr-admins]\n    admin_subjects: [root-sub]\n",
		"subjects only":       "    claim: groups\n    user_values: [lr-users]\n    admin_subjects: [root-sub]\n",
	} {
		f.access = access
		ic := f.load().Identity
		st := openTestIdentity(t, newIDClock())
		clock := newIDClock()
		logger := slog.New(slog.DiscardHandler)
		dial := func(context.Context, string, string) (net.Conn, error) { return nil, os.ErrClosed }
		_, pool := fixtureTLS()
		ov := Overrides{IdentityRootCAs: pool, IdentityTestDialContext: dial}

		got := webLoginConfig(ic, st, clock, logger, ov)
		want := ic.WebLoginConfig(nil, loginHooks{store: st})
		if !reflect.DeepEqual(got.Policy, want.Policy) {
			t.Fatalf("%s: policy = %+v, want config mapping %+v", name, got.Policy, want.Policy)
		}
		if got.Hooks != want.Hooks {
			t.Fatalf("%s: hooks = %#v, want %#v", name, got.Hooks, want.Hooks)
		}
		if got.Clock != clock || got.Logger != logger || got.Outbound.RootCAs != ov.IdentityRootCAs || got.Outbound.TestDialContext == nil {
			t.Fatalf("%s: process seams not applied", name)
		}
		got.ClientSecret, want.ClientSecret = nil, nil
		got.Clock, got.Logger, got.Hooks, want.Hooks = nil, nil, nil, nil
		got.Outbound.RootCAs, got.Outbound.TestDialContext = nil, nil
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: config = %+v\nwant %+v", name, got, want)
		}
		if _, err := weblogin.New(webLoginConfig(ic, st, clock, logger, ov)); err != nil {
			t.Fatalf("%s: weblogin rejects the mapped config: %v", name, err)
		}
	}
}

// TestWebLoginClientSecretIsConfigReader: the secret is re-read with the
// config package's reader at every exchange, so a rotated secret applies
// without a restart, and one CheckFiles would reject (loose mode, inner
// whitespace) is refused at runtime too.
func TestWebLoginClientSecretIsConfigReader(t *testing.T) {
	f := newIDFixture(t)
	ic := f.load().Identity
	wc := webLoginConfig(ic, openTestIdentity(t, newIDClock()), newIDClock(), nil, Overrides{})
	p := ic.OIDC.ClientSecretFile
	if got, err := wc.ClientSecret(); err != nil || got != fixtureClientSecret {
		t.Fatalf("secret = %q, %v", got, err)
	}
	bcWrite(t, p, "rotated-secret\n")
	if got, err := wc.ClientSecret(); err != nil || got != "rotated-secret" {
		t.Fatalf("rotated secret = %q, %v", got, err)
	}
	bcWrite(t, p, "two words")
	if got, err := wc.ClientSecret(); err == nil || got != "" {
		t.Fatalf("secret with inner whitespace accepted: %q", got)
	}
	bcWrite(t, p, "loose-secret")
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if got, err := wc.ClientSecret(); err == nil || got != "" || strings.Contains(err.Error(), "loose-secret") {
		t.Fatalf("group-readable secret: %q %v", got, err)
	}
}
