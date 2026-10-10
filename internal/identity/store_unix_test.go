//go:build unix

package identity

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenCreatesPrivateDatabase(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	f := newFixture(t)
	ctx := context.Background()
	if err := f.s.Ping(ctx); err != nil {
		t.Fatalf("Ping = %v", err)
	}
	// Open's migration write leaves the WAL/SHM files in place while open.

	fi, err := os.Stat(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("data dir mode = %#o, want 0700", got)
	}
	for _, p := range []string{f.path, f.path + "-wal", f.path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(p), err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %#o, want 0600", filepath.Base(p), got)
		}
	}
}
