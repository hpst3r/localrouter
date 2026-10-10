package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
)

const (
	testIssuer   = "https://idp.example.test/application/o/localrouter/"
	testClientID = "localrouter-client"
)

func testIdentityOptions() identity.Options {
	return identity.Options{Issuer: testIssuer, ClientID: testClientID}
}

func TestUsersUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"grant"}} {
		err := runUsers(args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "usage: localrouter users") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if !strings.Contains(usage, "localrouter users") {
		t.Error("root usage does not mention the users command")
	}
}

// The CLI must never create an empty identity database: a typo in data_dir
// or running before the server initialized it is a clear error.
func TestOpenExistingIdentityMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "identity.db")
	st, err := openExistingIdentity(context.Background(), path, testIdentityOptions())
	if err == nil {
		st.Close()
		t.Fatal("opened a missing identity database")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("identity database created: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("data_dir created: %v", err)
	}
}

// A zero-length file is not an initialized database; identity.Open would
// migrate and pin it.
func TestOpenExistingIdentityEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := openExistingIdentity(context.Background(), path, testIdentityOptions())
	if err == nil {
		st.Close()
		t.Fatal("opened an empty identity database")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("error = %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		t.Errorf("empty database modified: %v %v", fi, err)
	}
}

// seedIdentity creates an initialized identity database at path and returns
// the still-open store, standing in for a running server.
func seedIdentity(t *testing.T, path string) *identity.Store {
	t.Helper()
	st, err := identity.Open(context.Background(), path, testIdentityOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedUser(t *testing.T, st *identity.Store, sub string, role core.Role, email, name string) identity.User {
	t.Helper()
	u, err := st.ResolveLogin(context.Background(), identity.Login{
		Issuer: testIssuer, Subject: sub, Role: role, AuthTime: time.Now(),
		Email: email, DisplayName: name, Provision: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func seedKey(t *testing.T, st *identity.Store, uid, name string) identity.NewAPIKey {
	t.Helper()
	k, err := st.CreateKey(context.Background(), uid, name, 0)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// newSeeded returns a server-side store with an admin and a user (with one
// key) plus a second, CLI-side store on the same file.
func newSeeded(t *testing.T) (srv, cli *identity.Store, admin, user identity.User, key identity.NewAPIKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "identity.db")
	srv = seedIdentity(t, path)
	admin = seedUser(t, srv, "sub-admin", core.RoleAdmin, "admin@example.test", "Ada Admin")
	user = seedUser(t, srv, "sub-user", core.RoleUser, "user@example.test", "Uma User")
	key = seedKey(t, srv, user.ID, "laptop")
	cli, err := openExistingIdentity(context.Background(), path, testIdentityOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return srv, cli, admin, user, key
}

func assertNoSecrets(t *testing.T, out string, key identity.NewAPIKey) {
	t.Helper()
	for _, s := range []string{key.Token, testIssuer, testClientID, "sub-admin", "sub-user", "lrk_", "lrs_"} {
		if strings.Contains(out, s) {
			t.Errorf("output contains %q:\n%s", s, out)
		}
	}
}

func TestUsersList(t *testing.T) {
	_, cli, admin, user, key := newSeeded(t)
	var out strings.Builder
	if err := usersList(context.Background(), cli, "", 0, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{admin.ID, user.ID, "active", "admin", "user",
		"admin@example.test", "Uma User"} {
		if !strings.Contains(got, want) {
			t.Errorf("list missing %q:\n%s", want, got)
		}
	}
	assertNoSecrets(t, got, key)
	if strings.Contains(got, "-after") {
		t.Errorf("cursor hint on a short page:\n%s", got)
	}

	// A full page points at the next one; the cursor resumes after it.
	out.Reset()
	if err := usersList(context.Background(), cli, "", 1, &out); err != nil {
		t.Fatal(err)
	}
	first := min(admin.ID, user.ID)
	second := max(admin.ID, user.ID)
	if !strings.Contains(out.String(), "-after "+first) || strings.Contains(out.String(), second) {
		t.Errorf("page 1:\n%s", out.String())
	}
	out.Reset()
	if err := usersList(context.Background(), cli, first, 1, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), second) || strings.Contains(out.String(), first) {
		t.Errorf("page 2:\n%s", out.String())
	}
}

func lastAudit(t *testing.T, st *identity.Store) identity.AuditEvent {
	t.Helper()
	ev, err := st.AuditEvents(context.Background(), 0, identity.MaxListLimit)
	if err != nil || len(ev) == 0 {
		t.Fatalf("audit: %v %v", ev, err)
	}
	return ev[len(ev)-1]
}

func TestUsersDisable(t *testing.T) {
	srv, cli, _, user, key := newSeeded(t)
	ctx := context.Background()
	var out strings.Builder
	if err := usersDisable(ctx, cli, user.ID, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), user.ID) || !strings.Contains(out.String(), "disabled") {
		t.Errorf("output: %q", out.String())
	}
	// The server's own store sees it on its next read.
	if u, err := srv.User(ctx, user.ID); err != nil || u.Status != identity.StatusDisabled {
		t.Fatalf("status = %v %v", u.Status, err)
	}
	if _, err := srv.AuthenticateKey(ctx, key.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("key still authenticates: %v", err)
	}
	if ev := lastAudit(t, srv); ev.Action != identity.AuditUserDisabled || ev.ActorKind != identity.ActorCLI {
		t.Errorf("audit = %+v", ev)
	}
	assertNoSecrets(t, out.String(), key)
}

const unknownUserID = "u_aaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestUsersUnknownID(t *testing.T) {
	_, cli, _, _, _ := newSeeded(t)
	err := usersDisable(context.Background(), cli, unknownUserID, io.Discard)
	if err == nil || err.Error() != "user "+unknownUserID+" not found" {
		t.Errorf("disable unknown: %v", err)
	}
}

func TestUsersMalformedID(t *testing.T) {
	_, cli, _, user, _ := newSeeded(t)
	// One fixed message for every bad input: nothing is echoed.
	var first string
	for _, id := range []string{"", "u_", "\x1b[31mu_x", user.ID + " ", strings.ToUpper(user.ID), "k" + user.ID[1:]} {
		err := usersDisable(context.Background(), cli, id, io.Discard)
		if err == nil || !strings.HasPrefix(err.Error(), "invalid user id") {
			t.Fatalf("%q: %v", id, err)
		}
		if first == "" {
			first = err.Error()
		}
		if err.Error() != first {
			t.Errorf("%q: message varies with input: %v", id, err)
		}
	}
}

func TestUsersEnable(t *testing.T) {
	srv, cli, _, user, key := newSeeded(t)
	ctx := context.Background()
	if err := usersDisable(ctx, cli, user.ID, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := usersEnable(ctx, cli, user.ID, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), user.ID) || !strings.Contains(out.String(), "enabled") {
		t.Errorf("output: %q", out.String())
	}
	if u, err := srv.User(ctx, user.ID); err != nil || u.Status != identity.StatusActive {
		t.Fatalf("status = %v %v", u.Status, err)
	}
	// Enabling never resurrects credentials.
	if _, err := srv.AuthenticateKey(ctx, key.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("revoked key authenticates after enable: %v", err)
	}
	if ev := lastAudit(t, srv); ev.Action != identity.AuditUserEnabled || ev.ActorKind != identity.ActorCLI {
		t.Errorf("audit = %+v", ev)
	}
	if err := usersEnable(ctx, cli, unknownUserID, io.Discard); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("enable unknown: %v", err)
	}
	if err := usersEnable(ctx, cli, "bogus", io.Discard); err == nil || !strings.HasPrefix(err.Error(), "invalid user id") {
		t.Errorf("enable malformed: %v", err)
	}
}

func TestUsersDelete(t *testing.T) {
	srv, cli, _, user, key := newSeeded(t)
	ctx := context.Background()
	var out strings.Builder
	if err := usersDelete(ctx, cli, user.ID, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), user.ID) || !strings.Contains(out.String(), "deleted") {
		t.Errorf("output: %q", out.String())
	}
	u, err := srv.User(ctx, user.ID)
	if err != nil || u.Status != identity.StatusDeleted || u.Email != "" || u.DisplayName != "" {
		t.Fatalf("user = %+v %v", u, err)
	}
	if _, err := srv.AuthenticateKey(ctx, key.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("key authenticates after delete: %v", err)
	}
	if ev := lastAudit(t, srv); ev.Action != identity.AuditUserDeleted || ev.ActorKind != identity.ActorCLI {
		t.Errorf("audit = %+v", ev)
	}
	// Every lifecycle command on a deleted user is a clean error.
	want := "user " + user.ID + " is deleted"
	for name, op := range map[string]func(context.Context, *identity.Store, string, io.Writer) error{
		"delete": usersDelete, "disable": usersDisable, "enable": usersEnable,
	} {
		if err := op(ctx, cli, user.ID, io.Discard); err == nil || err.Error() != want {
			t.Errorf("%s deleted user: %v", name, err)
		}
	}
	if err := usersDelete(ctx, cli, unknownUserID, io.Discard); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("delete unknown: %v", err)
	}
	if err := usersDelete(ctx, cli, "bogus", io.Discard); err == nil || !strings.HasPrefix(err.Error(), "invalid user id") {
		t.Errorf("delete malformed: %v", err)
	}
}

func TestUsersKeys(t *testing.T) {
	srv, cli, admin, user, key := newSeeded(t)
	ctx := context.Background()
	other := seedKey(t, srv, user.ID, "ci runner")
	if err := srv.RevokeKey(ctx, identity.Actor{Kind: identity.ActorUser, UserID: user.ID}, user.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := usersKeys(ctx, cli, user.ID, time.Now(), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{key.ID, "laptop", other.ID, "ci runner", "active", "revoked (user)",
		key.ExpiresAt.UTC().Format(time.RFC3339)} {
		if !strings.Contains(got, want) {
			t.Errorf("keys missing %q:\n%s", want, got)
		}
	}
	assertNoSecrets(t, got, key)
	assertNoSecrets(t, got, other)

	// After the clock passes the expiry the key shows as expired.
	out.Reset()
	if err := usersKeys(ctx, cli, user.ID, key.ExpiresAt.Add(time.Second), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "expired") {
		t.Errorf("expired key:\n%s", out.String())
	}

	// A user with no keys is not an error; an unknown user is.
	out.Reset()
	if err := usersKeys(ctx, cli, admin.ID, time.Now(), &out); err != nil || !strings.Contains(out.String(), "no API keys") {
		t.Errorf("no keys: %v %q", err, out.String())
	}
	if err := usersKeys(ctx, cli, unknownUserID, time.Now(), io.Discard); err == nil || err.Error() != "user "+unknownUserID+" not found" {
		t.Errorf("keys unknown: %v", err)
	}
	if err := usersKeys(ctx, cli, "bogus", time.Now(), io.Discard); err == nil || !strings.HasPrefix(err.Error(), "invalid user id") {
		t.Errorf("keys malformed: %v", err)
	}
}

func TestUsersRevoke(t *testing.T) {
	srv, cli, admin, user, key := newSeeded(t)
	ctx := context.Background()
	adminKey := seedKey(t, srv, admin.ID, "admin laptop")

	// The key must belong to the named user: no cross-owner revocation.
	want := "key " + adminKey.ID + " not found for user " + user.ID
	if err := usersRevoke(ctx, cli, user.ID, adminKey.ID, io.Discard); err == nil || err.Error() != want {
		t.Errorf("cross-owner revoke: %v", err)
	}
	if _, err := srv.AuthenticateKey(ctx, adminKey.Token); err != nil {
		t.Fatalf("cross-owner revoke touched the key: %v", err)
	}

	var out strings.Builder
	if err := usersRevoke(ctx, cli, user.ID, key.ID, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), key.ID) || !strings.Contains(out.String(), "revoked") {
		t.Errorf("output: %q", out.String())
	}
	assertNoSecrets(t, out.String(), key)
	if _, err := srv.AuthenticateKey(ctx, key.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("revoked key authenticates: %v", err)
	}
	keys, err := srv.ListKeys(ctx, user.ID)
	if err != nil || len(keys) != 1 || keys[0].RevokeReason != identity.RevokeCLI {
		t.Fatalf("keys = %+v %v", keys, err)
	}
	if ev := lastAudit(t, srv); ev.Action != identity.AuditKeyRevoked || ev.ActorKind != identity.ActorCLI || ev.TargetKeyID != key.ID {
		t.Errorf("audit = %+v", ev)
	}
	// Idempotent.
	if err := usersRevoke(ctx, cli, user.ID, key.ID, io.Discard); err != nil {
		t.Errorf("second revoke: %v", err)
	}

	if err := usersRevoke(ctx, cli, unknownUserID, key.ID, io.Discard); err == nil || err.Error() != "user "+unknownUserID+" not found" {
		t.Errorf("unknown user: %v", err)
	}
	if err := usersRevoke(ctx, cli, "bogus", key.ID, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "invalid user id") {
		t.Errorf("malformed user: %v", err)
	}
	for _, kid := range []string{"", key.Token, "u" + key.ID[1:], "\x1b]0;k_"} {
		err := usersRevoke(ctx, cli, user.ID, kid, io.Discard)
		if err == nil || !strings.HasPrefix(err.Error(), "invalid key id") || (len(kid) > 2 && strings.Contains(err.Error(), kid)) {
			t.Errorf("malformed key %q: %v", kid, err)
		}
	}
}

// usersConfig writes a config into a fresh directory and returns its path
// and data_dir. identityBlock false writes a legacy (single-user) config.
func usersConfig(t *testing.T, identityBlock bool, issuer string) (cfgPath, dataDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits not enforced on windows")
	}
	dir := t.TempDir()
	body := "data_dir: data\ncontrol: {require_auth: true}\n"
	if identityBlock {
		if err := os.WriteFile(filepath.Join(dir, "oidc.secret"), []byte("not-a-real-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		body += `identity:
  public_base_url: https://localhost:8787
  oidc:
    issuer: ` + issuer + `
    client_id: ` + testClientID + `
    client_secret_file: oidc.secret
  access:
    claim: groups
    user_values: [lr-users]
    admin_values: [lr-admins]
`
	} else {
		if err := os.WriteFile(filepath.Join(dir, "c.key"), []byte("lr-client-key-0123456789abcdef\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		body += "clients:\n  - {name: c, class: interactive, key_file: c.key}\n"
	}
	cfgPath = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, filepath.Join(dir, "data")
}

func TestRunUsersLegacyConfig(t *testing.T) {
	cfg, data := usersConfig(t, false, "")
	for _, args := range [][]string{
		{"list", "-config", cfg},
		{"disable", "-config", cfg, unknownUserID},
		{"delete", "-config", cfg, "--yes", unknownUserID},
	} {
		err := runUsers(args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "requires the identity block") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := os.Lstat(data); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("data_dir created in legacy mode: %v", err)
	}
}

func TestRunUsersMissingDatabase(t *testing.T) {
	cfg, data := usersConfig(t, true, testIssuer)
	for _, args := range [][]string{
		{"list", "-config", cfg},
		{"disable", "-config", cfg, unknownUserID},
	} {
		err := runUsers(args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(data, "identity.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("identity.db created: %v", err)
	}
}

// End to end through the config file while a server-side store stays open on
// the same database.
func TestRunUsersLifecycle(t *testing.T) {
	cfg, data := usersConfig(t, true, testIssuer)
	srv := seedIdentity(t, filepath.Join(data, "identity.db"))
	ctx := context.Background()
	user := seedUser(t, srv, "sub-user", core.RoleUser, "user@example.test", "Uma User")
	key := seedKey(t, srv, user.ID, "laptop")
	key2 := seedKey(t, srv, user.ID, "desktop")

	run := func(args ...string) string {
		t.Helper()
		var out strings.Builder
		if err := runUsers(append(args[:1:1], append([]string{"-config", cfg}, args[1:]...)...), &out); err != nil {
			t.Fatalf("users %v: %v", args, err)
		}
		assertNoSecrets(t, out.String(), key)
		assertNoSecrets(t, out.String(), key2)
		return out.String()
	}
	if got := run("list"); !strings.Contains(got, user.ID) || !strings.Contains(got, "user@example.test") {
		t.Errorf("list:\n%s", got)
	}
	if got := run("list", "-limit", "1"); !strings.Contains(got, "-after "+user.ID) {
		t.Errorf("list -limit 1:\n%s", got)
	}
	if got := run("keys", user.ID); !strings.Contains(got, key.ID) || !strings.Contains(got, key2.ID) {
		t.Errorf("keys:\n%s", got)
	}
	run("revoke", user.ID, key.ID)
	if _, err := srv.AuthenticateKey(ctx, key.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("revoked key authenticates: %v", err)
	}
	if _, err := srv.AuthenticateKey(ctx, key2.Token); err != nil {
		t.Errorf("other key revoked too: %v", err)
	}
	run("disable", user.ID)
	if _, err := srv.AuthenticateKey(ctx, key2.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("key authenticates after disable: %v", err)
	}
	run("enable", user.ID)
	run("delete", "--yes", user.ID)
	if u, err := srv.User(ctx, user.ID); err != nil || u.Status != identity.StatusDeleted {
		t.Fatalf("after delete: %+v %v", u, err)
	}
	err := runUsers([]string{"delete", "-config", cfg, "--yes", user.ID}, io.Discard)
	if err == nil || err.Error() != "user "+user.ID+" is deleted" {
		t.Errorf("second delete: %v", err)
	}
}

func TestRunUsersDeleteRequiresYes(t *testing.T) {
	cfg, data := usersConfig(t, true, testIssuer)
	srv := seedIdentity(t, filepath.Join(data, "identity.db"))
	user := seedUser(t, srv, "sub-user", core.RoleUser, "user@example.test", "Uma User")
	for _, args := range [][]string{
		{"delete", "-config", cfg, user.ID},
		{"delete", "-config", cfg, "-yes=false", user.ID},
		{"delete", "-config", cfg, user.ID, "--yes"}, // flags must precede the ID
	} {
		err := runUsers(args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if u, err := srv.User(context.Background(), user.ID); err != nil || u.Status != identity.StatusActive || u.Email == "" {
		t.Fatalf("user changed without --yes: %+v %v", u, err)
	}
}

// The CLI opens with the configured issuer and client pin; a config for a
// different issuer fails closed and leaves the database as it was.
func TestRunUsersBindingMismatch(t *testing.T) {
	const otherIssuer = "https://other-idp.example.test/realms/lr"
	cfg, data := usersConfig(t, true, otherIssuer)
	srv := seedIdentity(t, filepath.Join(data, "identity.db"))
	user := seedUser(t, srv, "sub-user", core.RoleUser, "user@example.test", "Uma User")
	err := runUsers([]string{"disable", "-config", cfg, user.ID}, io.Discard)
	if !errors.Is(err, identity.ErrBindingMismatch) || !strings.Contains(err.Error(), "identity.oidc.issuer") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), otherIssuer) || strings.Contains(err.Error(), testIssuer) {
		t.Errorf("issuer value in error: %v", err)
	}
	if u, err := srv.User(context.Background(), user.ID); err != nil || u.Status != identity.StatusActive {
		t.Errorf("user changed: %+v %v", u, err)
	}
}

func TestRunUsersArguments(t *testing.T) {
	cfg, data := usersConfig(t, true, testIssuer)
	srv := seedIdentity(t, filepath.Join(data, "identity.db"))
	user := seedUser(t, srv, "sub-user", core.RoleUser, "user@example.test", "Uma User")
	other := seedUser(t, srv, "sub-other", core.RoleUser, "other@example.test", "Otto Other")
	for _, args := range [][]string{
		{"list", "-config", cfg, user.ID},
		{"disable", "-config", cfg},
		{"disable", "-config", cfg, user.ID, other.ID}, // no bulk forms
		{"enable", "-config", cfg, user.ID, other.ID},
		{"keys", "-config", cfg},
		{"revoke", "-config", cfg, user.ID},
	} {
		if err := runUsers(args, io.Discard); err == nil || !strings.Contains(err.Error(), "usage: localrouter users") {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, n := range []string{"0", "-1", "201"} {
		if err := runUsers([]string{"list", "-config", cfg, "-limit", n}, io.Discard); err == nil || !strings.Contains(err.Error(), "-limit") {
			t.Errorf("-limit %s: %v", n, err)
		}
	}
	for _, id := range []string{user.ID, other.ID} {
		if u, err := srv.User(context.Background(), id); err != nil || u.Status != identity.StatusActive {
			t.Errorf("user changed by a rejected command: %+v %v", u, err)
		}
	}
}

// Like serve and check, the command refuses a config that others could
// rewrite to point it at a different database.
func TestRunUsersChecksFiles(t *testing.T) {
	cfg, data := usersConfig(t, true, testIssuer)
	srv := seedIdentity(t, filepath.Join(data, "identity.db"))
	user := seedUser(t, srv, "sub-user", core.RoleUser, "user@example.test", "Uma User")
	if err := os.Chmod(cfg, 0o666); err != nil {
		t.Fatal(err)
	}
	err := runUsers([]string{"disable", "-config", cfg, user.ID}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "file permissions") {
		t.Fatalf("err = %v", err)
	}
	if u, err := srv.User(context.Background(), user.ID); err != nil || u.Status != identity.StatusActive {
		t.Errorf("user changed: %+v %v", u, err)
	}
}

// Break-glass: unlike the browser admin API, the CLI may disable the only
// admin.
func TestUsersDisableOnlyAdmin(t *testing.T) {
	srv, cli, admin, _, _ := newSeeded(t)
	if err := usersDisable(context.Background(), cli, admin.ID, io.Discard); err != nil {
		t.Fatal(err)
	}
	if u, err := srv.User(context.Background(), admin.ID); err != nil || u.Status != identity.StatusDisabled {
		t.Errorf("admin = %+v %v", u, err)
	}
}

func TestRunUsersHelp(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		var out strings.Builder
		if err := runUsers(args, &out); err != nil || !strings.Contains(out.String(), "localrouter users revoke") {
			t.Errorf("%v: %v %q", args, err, out.String())
		}
	}
	// Subcommand flag help is not an error either (flag prints it to stderr).
	if err := runUsers([]string{"list", "-h"}, io.Discard); err != nil {
		t.Errorf("list -h: %v", err)
	}
}
