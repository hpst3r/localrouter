package identity

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// fileBytes returns the database file and its WAL as they are on disk now.
func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	var all []byte
	for _, p := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	return all
}

func TestDeleteUserTombstonesAndScrubs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const (
		subject = "sub-deleted-PRIVATESUBJECT"
		email   = "PRIVATEMAIL@example.test"
		display = "PRIVATEDISPLAY Person"
		keyName = "PRIVATEKEYNAME"
	)
	l := f.login(subject, core.RoleUser)
	l.Email, l.DisplayName = email, display
	u, err := f.s.ResolveLogin(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	k, err := f.s.CreateKey(ctx, u.ID, keyName, 0)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := f.s.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	bystander := f.login("sub-bystander", core.RoleUser)
	bystander.Email = "BYSTANDERMAIL@example.test"
	if _, err := f.s.ResolveLogin(ctx, bystander); err != nil {
		t.Fatal(err)
	}

	cli := f.second()
	if err := cli.DeleteUser(ctx, Actor{Kind: ActorCLI}, u.ID); err != nil {
		t.Fatalf("DeleteUser = %v", err)
	}
	f.assertCredsDenied(f.s, userCreds{user: u, key: k, session: ns})

	got, err := f.s.User(ctx, u.ID)
	if err != nil {
		t.Fatalf("deleted user row must be retained for attribution: %v", err)
	}
	if got.Status != StatusDeleted || got.Email != "" || got.DisplayName != "" || !got.LastLoginAt.IsZero() {
		t.Fatalf("deleted user = %+v", got)
	}
	keys, _ := f.s.ListKeys(ctx, u.ID)
	if len(keys) != 1 || keys[0].Name != "" || keys[0].RevokeReason != RevokeUserDeleted {
		t.Fatalf("keys after delete = %+v", keys)
	}
	var hash []byte
	f.s.db.QueryRow(`SELECT token_hash FROM api_keys WHERE id = ?`, k.ID).Scan(&hash)
	if bytes.Equal(hash, tokenDigest(k.Token)) {
		t.Fatal("key digest retained after delete")
	}

	// The subject cannot come back, even with JIT allowed.
	before, _ := f.s.ListUsers(ctx, "", MaxListLimit)
	if _, err := f.s.ResolveLogin(ctx, f.login(subject, core.RoleAdmin)); !errors.Is(err, ErrUserDeleted) {
		t.Fatalf("re-login of deleted subject = %v, want ErrUserDeleted", err)
	}
	after, _ := f.s.ListUsers(ctx, "", MaxListLimit)
	if len(after) != len(before) {
		t.Fatal("deleted subject was re-provisioned")
	}

	for name, op := range map[string]func(context.Context, Actor, string) error{
		"delete": f.s.DeleteUser, "disable": f.s.DisableUser, "enable": f.s.EnableUser,
	} {
		if err := op(ctx, Actor{Kind: ActorCLI}, u.ID); !errors.Is(err, ErrUserDeleted) {
			t.Errorf("%s deleted user = %v, want ErrUserDeleted", name, err)
		}
	}
	if err := f.s.DeleteUser(ctx, Actor{Kind: ActorCLI}, "u_unknown"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete unknown = %v, want ErrNotFound", err)
	}

	assertNoValueInTables(t, f.s, subject, email, display, keyName)
	raw := fileBytes(t, f.path)
	if !bytes.Contains(raw, []byte("BYSTANDERMAIL")) {
		t.Fatal("positive control: live user's email not found in the database bytes")
	}
	for _, secret := range []string{subject, email, display, keyName} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("%q still present in database/WAL bytes after delete", secret)
		}
	}
}
