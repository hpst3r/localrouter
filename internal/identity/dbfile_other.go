//go:build !unix

package identity

import (
	"errors"
	"fmt"
)

// createDBFile fails closed where the open cannot refuse a symlink (or
// reparse point) at the database path atomically or enforce 0600, so the
// package still compiles there but never opens a database.
func createDBFile(string) error {
	return fmt.Errorf("identity: open: %w: no symlink-safe database open on this platform", errors.ErrUnsupported)
}
