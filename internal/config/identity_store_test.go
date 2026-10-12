package config

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

// The configured lifetimes reach a real identity store: the store opens with
// the pinned binding, a default-TTL key expires at keys.max_ttl, the
// per-user key cap is keys.max_per_user, and a store-issued user ID is a
// valid static-client owner.
func TestIdentityStoreOptionsOpenRealStore(t *testing.T) {
	c, err := Load(write(t, withBlock(`  keys: {max_ttl: 720h, require_login_within: 168h, max_per_user: 2}
  session: {idle_ttl: 1h, absolute_ttl: 4h}
`)))
	if err != nil {
		t.Fatal(err)
	}
	opts := c.Identity.StoreOptions()
	want := identity.Options{
		Issuer: "https://idp.example.test/realms/lr", ClientID: "localrouter",
		KeyMaxTTL: 720 * time.Hour, LoginMaxAge: 168 * time.Hour,
		SessionIdleTTL: time.Hour, SessionAbsoluteTTL: 4 * time.Hour, MaxKeysPerUser: 2,
	}
	if !reflect.DeepEqual(opts, want) {
		t.Fatalf("StoreOptions = %+v\nwant %+v", opts, want)
	}

	ctx := context.Background()
	st, err := identity.Open(ctx, filepath.Join(t.TempDir(), "identity.db"), opts)
	if err != nil {
		t.Fatalf("identity.Open with config options: %v", err)
	}
	defer st.Close()
	u, err := st.ResolveLogin(ctx, identity.Login{
		Issuer: opts.Issuer, Subject: "sub-1", Role: core.RoleUser,
		AuthTime: time.Now(), Provision: true,
	})
	if err != nil {
		t.Fatalf("ResolveLogin: %v", err)
	}
	k, err := st.CreateKey(ctx, u.ID, "laptop", 0)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if !strings.HasPrefix(k.Token, userKeyPrefix) || !identity.IsUserKeyToken(k.Token) {
		t.Fatal("store-issued key does not carry the reserved user key prefix")
	}
	if got := k.ExpiresAt.Sub(k.CreatedAt); got != 720*time.Hour {
		t.Fatalf("default key ttl = %s, want keys.max_ttl 720h", got)
	}
	if _, err := st.CreateKey(ctx, u.ID, "second", 0); err != nil {
		t.Fatalf("second key: %v", err)
	}
	if _, err := st.CreateKey(ctx, u.ID, "third", 0); !errors.Is(err, identity.ErrKeyLimit) {
		t.Fatalf("third key err = %v, want ErrKeyLimit (max_per_user 2)", err)
	}

	if _, err := Load(write(t, identityBase+`
clients:
  - {name: s, class: interactive, key_file: k, role: user, owner: `+u.ID+`}
`)); err != nil {
		t.Fatalf("store-issued owner %q rejected: %v", u.ID, err)
	}
}

// The default config opens a store at the ceilings.
func TestIdentityStoreOptionsDefaultsOpen(t *testing.T) {
	c, err := Load(write(t, identityBase))
	if err != nil {
		t.Fatal(err)
	}
	st, err := identity.Open(context.Background(), filepath.Join(t.TempDir(), "identity.db"), c.Identity.StoreOptions())
	if err != nil {
		t.Fatalf("identity.Open with default options: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

type nopHooks struct{ inertHooks }

func TestIdentityWebLoginConfigMapping(t *testing.T) {
	c, err := Load(write(t, withAccess(`    claim: roles
    user_values: [u1, u2]
    admin_values: [a1]
    admin_subjects: [s1]
`)+`
`))
	if err != nil {
		t.Fatal(err)
	}
	c.Identity.OIDC.Scopes = []string{"openid", "profile", "groups"}
	c.Identity.OIDC.SigningAlgs = []string{"ES256"}
	c.Identity.OIDC.AllowPrivateNetwork = true
	c.Identity.OIDC.ExtraEndpointHosts = []string{"login.example.test"}
	c.Identity.OIDC.ClientAuth = weblogin.ClientSecretPost
	secret := func() (string, error) { return "", nil }
	hooks := nopHooks{}
	got := c.Identity.WebLoginConfig(secret, hooks)
	if got.ClientSecret == nil || got.Hooks != hooks {
		t.Fatal("secret reader or hooks not passed through")
	}
	got.ClientSecret, got.Hooks = nil, nil
	want := weblogin.Config{
		PublicBaseURL: "https://localhost:8787",
		Issuer:        "https://idp.example.test/realms/lr",
		ClientID:      "localrouter",
		ClientAuth:    weblogin.ClientSecretPost,
		Scopes:        []string{"openid", "profile", "groups"},
		SigningAlgs:   []string{"ES256"},
		Policy: weblogin.Policy{JIT: true, Claim: "roles", UserValues: []string{"u1", "u2"},
			AdminValues: []string{"a1"}, AdminSubjects: []string{"s1"}},
		Outbound: weblogin.Outbound{AllowPrivateNetwork: true,
			ExtraEndpointHosts: []string{"login.example.test"}, Timeout: 10 * time.Second},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WebLoginConfig = %+v\nwant %+v", got, want)
	}
	// The mapped slices are copies: mutating them cannot alter the config.
	got.Policy.UserValues[0] = "x"
	if c.Identity.Access.UserValues[0] != "u1" {
		t.Fatal("WebLoginConfig aliased access.user_values")
	}
	if _, err := weblogin.New(c.Identity.WebLoginConfig(secret, hooks)); err != nil {
		t.Fatalf("weblogin.New rejected a loaded config: %v", err)
	}
}
