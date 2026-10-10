//go:build linux || darwin

package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestIdentityLockExclusive: identity.lock is held exclusively. A second
// acquisition — in this process too, since flock belongs to the open file
// description — is refused until the first owner releases it.
func TestIdentityLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.lock")
	first, err := acquireIdentityLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if second, err := acquireIdentityLock(path); !errors.Is(err, errIdentityLockHeld) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second acquire err = %v, want errIdentityLockHeld", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second release must be a no-op: %v", err)
	}
	again, err := acquireIdentityLock(path)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	_ = again.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestIdentityLockRefusesSymlink: a symlink planted at the lock path is never
// followed.
func TestIdentityLockRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "identity.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if l, err := acquireIdentityLock(path); err == nil {
		_ = l.Close()
		t.Fatal("symlinked lock path was accepted")
	}
}
