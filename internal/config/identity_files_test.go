package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const testSecret = "oidc-Secret_value~0123456789"

// identityFixture writes a multi-user config, a 0600 client secret and the
// given static clients' 0600 key files, and loads it.
func identityFixture(t *testing.T, clients string, keys map[string]string) (string, *Config) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits not enforced on windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "oidc.secret"), []byte(testSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range keys {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(identityBase+clients), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, c
}

func writeSecret(t *testing.T, c *Config, body []byte, mode os.FileMode) {
	t.Helper()
	p := c.Identity.OIDC.ClientSecretFile
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityClientSecretFile(t *testing.T) {
	p, c := identityFixture(t, "", nil)
	if err := c.CheckFiles(p); err != nil {
		t.Fatalf("private secret rejected: %v", err)
	}
	got, err := c.Identity.OIDC.ReadClientSecret()
	if err != nil || got != testSecret {
		t.Fatalf("ReadClientSecret = %q, %v", got, err)
	}

	for name, tc := range map[string]struct {
		body []byte
		mode os.FileMode
		want string
	}{
		"group readable": {[]byte(testSecret), 0o640, "must not be group/world accessible"},
		"world readable": {[]byte(testSecret), 0o604, "must not be group/world accessible"},
		"empty":          {[]byte(" \n\t"), 0o600, "is empty"},
		"too large":      {[]byte(strings.Repeat("s", MaxClientSecretBytes+1)), 0o600, "larger than"},
		"invalid utf8":   {[]byte(testSecret + "\xff"), 0o600, "printable UTF-8"},
		"two lines":      {[]byte(testSecret + "\n" + testSecret + "\n"), 0o600, "single value"},
		"inner space":    {[]byte(testSecret + " " + testSecret), 0o600, "single value"},
		"control char":   {[]byte(testSecret + "\x00"), 0o600, "printable UTF-8"},
	} {
		t.Run(name, func(t *testing.T) {
			writeSecret(t, c, tc.body, tc.mode)
			for what, err := range map[string]error{
				"CheckFiles":       c.CheckFiles(p),
				"ReadClientSecret": func() error { _, err := c.Identity.OIDC.ReadClientSecret(); return err }(),
			} {
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "identity.oidc.client_secret_file") {
					t.Fatalf("%s: want error mentioning %q, got %v", what, tc.want, err)
				}
				if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "sss") {
					t.Fatalf("%s: error leaks the secret: %v", what, err)
				}
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		if err := os.Remove(c.Identity.OIDC.ClientSecretFile); err != nil {
			t.Fatal(err)
		}
		if err := c.CheckFiles(p); err == nil || !strings.Contains(err.Error(), "identity.oidc.client_secret_file") {
			t.Fatalf("missing secret accepted: %v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		if err := os.Mkdir(c.Identity.OIDC.ClientSecretFile, 0o700); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(c.Identity.OIDC.ClientSecretFile)
		if err := c.CheckFiles(p); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("directory secret accepted: %v", err)
		}
	})
}

// A static key that starts with the user-key prefix would be routed to the
// identity store and never match; reject it before serving. Reading key
// content is I/O, so it is a CheckFiles (not Validate) rule.
func TestIdentityStaticKeyReservedPrefix(t *testing.T) {
	const userLike = "lrk_abcdefghijklmnopqrstuvwxyz_0123456789012345678901234567890123456"
	p, c := identityFixture(t, `
clients:
  - {name: svc, class: background, key_file: svc.key, role: service, ingest: true}
  - {name: bad, class: interactive, key_files: [ok.key, bad.key], role: user, owner: `+validOwner+`}
`, map[string]string{
		"svc.key": "lr-static-service-key-0123456789\n",
		"ok.key":  "lr-static-user-key-0123456789\n",
		"bad.key": "  " + userLike + "\n",
	})
	err := c.CheckFiles(p)
	if err == nil || !strings.Contains(err.Error(), "client bad key file") || !strings.Contains(err.Error(), "lrk_") {
		t.Fatalf("reserved-prefix static key accepted: %v", err)
	}
	if strings.Contains(err.Error(), userLike) || strings.Contains(err.Error(), "static-") {
		t.Fatalf("error leaks key material: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(p), "bad.key"), []byte("lr-fixed-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(p); err != nil {
		t.Fatalf("fixed keys rejected: %v", err)
	}
}

// Legacy mode never reads key content in CheckFiles: behavior unchanged.
func TestLegacyCheckFilesIgnoresKeyContent(t *testing.T) {
	p, c := secFixture(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(p), "c.key"), []byte("lrk_legacy-key-content-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(p); err != nil {
		t.Fatalf("legacy CheckFiles changed: %v", err)
	}
}

// The OIDC client secret must be its own file: sharing a path with a
// bearer key, upstream key or TLS key would turn one leak into two.
func TestIdentityClientSecretFileCollisions(t *testing.T) {
	for name, extra := range map[string]string{
		"client key_file":     "clients: [{name: s, class: interactive, key_file: oidc.secret, role: service}]\n",
		"client key_files":    "clients: [{name: s, class: interactive, key_files: [a.key, ./oidc.secret], role: service}]\n",
		"api_key_file":        "accounts: [{id: o, provider: openrouter, api_key_file: oidc.secret}]\n",
		"management_key_file": "accounts: [{id: o, provider: openrouter, api_key_env: K, management_key_file: oidc.secret}]\n",
		"tls_key_file":        "tls_cert_file: tls.crt\ntls_key_file: oidc.secret\n",
	} {
		t.Run(name, func(t *testing.T) {
			wantErrs(t, identityBase+extra, "identity.oidc.client_secret_file must not be the same file as")
		})
	}
}

// Validate compares paths; CheckFiles also catches another name for the
// same file (a hard link or symlink).
func TestIdentityClientSecretSameFileOnDisk(t *testing.T) {
	p, c := identityFixture(t, `
clients:
  - {name: svc, class: background, key_file: svc.key, role: service}
`, map[string]string{"svc.key": "lr-static-service-key-0123456789\n"})
	dir := filepath.Dir(p)
	if err := os.Remove(filepath.Join(dir, "svc.key")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "oidc.secret"), filepath.Join(dir, "svc.key")); err != nil {
		t.Fatal(err)
	}
	err := c.CheckFiles(p)
	if err == nil || !strings.Contains(err.Error(), "identity.oidc.client_secret_file must not be the same file as client svc key file") {
		t.Fatalf("hard-linked secret accepted: %v", err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatalf("error leaks the secret: %v", err)
	}
}

// Test seams (dial override, CA pool, clock), inline or environment secrets
// and policy switches that are fixed in v1 have no YAML spelling.
func TestIdentityUnknownFieldsRejected(t *testing.T) {
	for name, tc := range map[string]struct{ old, new string }{
		"test dial":        {"  oidc:\n", "  oidc:\n    test_dial_context: 127.0.0.1:1\n"},
		"root cas":         {"  oidc:\n", "  oidc:\n    root_cas: /etc/ssl/ca.pem\n"},
		"inline secret":    {"  oidc:\n", "  oidc:\n    client_secret: hunter2\n"},
		"env secret":       {"  oidc:\n", "  oidc:\n    client_secret_env: OIDC_SECRET\n"},
		"jit switch":       {"  access:\n", "  access:\n    jit: false\n"},
		"clock":            {"identity:\n", "identity:\n  clock: fixed\n"},
		"max auth age":     {"identity:\n", "identity:\n  max_auth_age: 1h\n"},
		"session typo":     {"identity:\n", "identity:\n  sessions: {idle_ttl: 1h}\n"},
		"user budget typo": {"identity:\n", "budgets: {reserve_usd: \"1\", user: {daily_usd: \"1\"}}\nidentity:\n"},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(identityBase, tc.old, tc.new, 1)
			_, err := Load(write(t, body))
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("unknown field accepted: %v", err)
			}
		})
	}
}

// The app's reload clone and restart-only comparison go through JSON, so
// every identity field must survive a round trip, and the encoding carries
// the secret file's path only.
func TestIdentityJSONRoundTrip(t *testing.T) {
	_, c := identityFixture(t, `
clients:
  - {name: u, class: interactive, key_file: u.key, role: user, owner: `+validOwner+`}
limits: {max_concurrent_per_user: 2}
budgets: {reserve_usd: "0.1", users: {daily_usd: "3"}}
`, map[string]string{"u.key": "lr-static-user-key-0123456789\n"})
	c.Identity.OIDC.Scopes = []string{"openid", "groups"}
	c.Identity.OIDC.SigningAlgs = []string{"ES256"}
	c.Identity.OIDC.ExtraEndpointHosts = []string{"login.example.test"}
	c.Identity.OIDC.AllowPrivateNetwork = true
	c.Identity.Access.AdminSubjects = []string{"s1"}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), testSecret) {
		t.Fatal("JSON encoding contains the client secret")
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Identity, c.Identity) {
		t.Fatalf("identity changed in round trip:\n got %+v\nwant %+v", out.Identity, c.Identity)
	}
	if !reflect.DeepEqual(out.Clients, c.Clients) || out.Limits.MaxConcurrentPerUser != 2 ||
		out.Budgets.Users == nil || *out.Budgets.Users.DailyUSD != "3" {
		t.Fatalf("role/owner/per-user fields lost: %+v %+v %+v", out.Clients, out.Limits, out.Budgets)
	}
	var legacy Config
	lb, _ := json.Marshal(&legacy)
	var legacyOut Config
	if err := json.Unmarshal(lb, &legacyOut); err != nil || legacyOut.Identity != nil {
		t.Fatalf("nil identity must round-trip nil: %+v, %v", legacyOut.Identity, err)
	}
}
