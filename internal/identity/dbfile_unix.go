//go:build unix

package identity

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// createDBFile creates path's parent directory 0700 if missing and the
// database file 0600, tightening an existing file to 0600. A symlink at path
// is refused, and O_NOFOLLOW closes the race between that check and the open.
func createDBFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("identity: open: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("identity: open: database path is a symlink")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("identity: open: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("identity: open: %w", err)
	}
	return nil
}
