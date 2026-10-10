//go:build !unix

package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Without symlink-safe file creation Open fails closed and creates nothing.
func TestOpenFailsClosedWithoutNoFollow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	s, err := Open(context.Background(), path, baseOptions(newFakeClock()))
	if err == nil {
		s.Close()
		t.Fatal("Open succeeded on a platform without O_NOFOLLOW")
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Open = %v, want errors.ErrUnsupported", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("Open created the database file (lstat err = %v)", err)
	}
}
