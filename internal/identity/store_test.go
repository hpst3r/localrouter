package identity

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "identity.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), link, baseOptions(newFakeClock()))
	if err == nil {
		s.Close()
		t.Fatal("Open through a symlink succeeded, want error")
	}
}

func TestOpenTightensExistingDatabaseMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, baseOptions(newFakeClock()))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer s.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("db mode = %#o, want 0600", got)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	f := newFixture(t)
	if _, err := f.s.db.Exec(`UPDATE schema_version SET version = 99`); err != nil {
		t.Fatal(err)
	}
	f.s.Close()
	s, err := Open(context.Background(), f.path, f.opts)
	if err == nil {
		s.Close()
		t.Fatal("Open of a newer schema succeeded, want error")
	}
	// The refused open must not have rewritten the version.
	raw, err := sql.Open("sqlite", "file:"+f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var v int
	if err := raw.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 99 {
		t.Fatalf("schema version after refused open = %d, want 99", v)
	}
}

func TestOpenPinsIssuerAndClient(t *testing.T) {
	f := newFixture(t)
	f.s.Close()
	ctx := context.Background()

	same, err := Open(ctx, f.path, f.opts)
	if err != nil {
		t.Fatalf("reopen with the pinned binding = %v", err)
	}
	same.Close()

	for name, mutate := range map[string]func(*Options){
		"issuer":         func(o *Options) { o.Issuer = "https://other-idp.example.test/" },
		"issuer slash":   func(o *Options) { o.Issuer = strings.TrimSuffix(testIssuer, "/") },
		"client":         func(o *Options) { o.ClientID = "another-client" },
		"client padding": func(o *Options) { o.ClientID = testClientID + " " },
	} {
		o := f.opts
		mutate(&o)
		s, err := Open(ctx, f.path, o)
		if !errors.Is(err, ErrBindingMismatch) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("%s: Open = %v, want ErrBindingMismatch", name, err)
		}
		for _, v := range []string{o.Issuer, o.ClientID, testIssuer, testClientID} {
			if strings.Contains(err.Error(), strings.TrimSpace(v)) {
				t.Errorf("%s: error %q leaks a configured value", name, err)
			}
		}
	}
}

func TestOpenRejectsInvalidOptions(t *testing.T) {
	cases := map[string]func(*Options){
		"empty issuer":        func(o *Options) { o.Issuer = "" },
		"empty client":        func(o *Options) { o.ClientID = "" },
		"key ttl over 90d":    func(o *Options) { o.KeyMaxTTL = MaxKeyTTL + time.Second },
		"negative key ttl":    func(o *Options) { o.KeyMaxTTL = -time.Hour },
		"login age over 30d":  func(o *Options) { o.LoginMaxAge = MaxLoginAge + time.Second },
		"absolute over 24h":   func(o *Options) { o.SessionAbsoluteTTL = MaxSessionAbsolute + time.Second },
		"idle over 8h":        func(o *Options) { o.SessionIdleTTL = MaxSessionIdle + time.Second },
		"idle over absolute":  func(o *Options) { o.SessionAbsoluteTTL = time.Hour; o.SessionIdleTTL = 2 * time.Hour },
		"auth age over 1h":    func(o *Options) { o.MaxAuthAge = MaxAuthAgeCeiling + time.Second },
		"too many keys":       func(o *Options) { o.MaxKeysPerUser = MaxKeysPerUserCeiling + 1 },
		"negative keys":       func(o *Options) { o.MaxKeysPerUser = -1 },
		"negative op timeout": func(o *Options) { o.OpTimeout = -time.Second },
		"invalid utf8 issuer": func(o *Options) { o.Issuer = "https://idp.example.test/\xff" },
	}
	for name, mutate := range cases {
		o := baseOptions(newFakeClock())
		mutate(&o)
		path := filepath.Join(t.TempDir(), "identity.db")
		s, err := Open(context.Background(), path, o)
		if !errors.Is(err, ErrInvalid) {
			if s != nil {
				s.Close()
			}
			t.Errorf("%s: Open = %v, want ErrInvalid", name, err)
		}
	}
}

func TestOptionsDefaultToCeilings(t *testing.T) {
	f := newFixture(t)
	o := f.s.opts
	if o.KeyMaxTTL != MaxKeyTTL || o.LoginMaxAge != MaxLoginAge ||
		o.SessionIdleTTL != MaxSessionIdle || o.SessionAbsoluteTTL != MaxSessionAbsolute ||
		o.MaxAuthAge != DefaultMaxAuthAge || o.MaxKeysPerUser != DefaultMaxKeysPerUser ||
		o.OpTimeout != DefaultOpTimeout {
		t.Fatalf("normalized options = %+v", o)
	}
}
