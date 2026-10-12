package identity

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestRawCredentialsNeverReachDisk(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var secrets []string
	for _, sub := range []string{"sub-d1", "sub-d2"} {
		c := f.userWithCreds(sub, core.RoleUser)
		secrets = append(secrets,
			c.key.Token, c.key.Token[len(c.key.Token)-secretChars:],
			c.session.Token, c.session.Token[len(sessionPrefix):], c.session.CSRF)
	}
	_ = ctx
	raw := fileBytes(t, f.path)
	if len(raw) == 0 {
		t.Fatal("no database bytes read")
	}
	for _, s := range secrets {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("raw credential material found in %s or its WAL", filepath.Base(f.path))
		}
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestRandomFailureFailsClosed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-rng", core.RoleUser)
	f.opts.Rand = failingReader{}
	s := f.second()
	if _, err := s.CreateKey(ctx, u.ID, "k", 0); err == nil {
		t.Fatal("CreateKey without entropy succeeded")
	}
	if _, err := s.CreateSession(ctx, u.ID); err == nil {
		t.Fatal("CreateSession without entropy succeeded")
	}
	other := f.login("sub-rng-new", core.RoleUser)
	if _, err := s.ResolveLogin(ctx, other); err == nil {
		t.Fatal("provisioning without entropy succeeded")
	}
	keys, _ := f.s.ListKeys(ctx, u.ID)
	users, _ := f.s.ListUsers(ctx, "", MaxListLimit)
	if len(keys) != 0 || len(users) != 1 {
		t.Fatalf("failed creations left rows: keys=%d users=%d", len(keys), len(users))
	}
}
