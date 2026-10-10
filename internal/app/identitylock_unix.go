//go:build linux || darwin

package app

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
)

// errIdentityLockHeld reports that another server (or another open of the
// same file in this process) owns identity.db.
var errIdentityLockHeld = errors.New("identity: identity.db is in use by another LocalRouter server")

// identityLock makes this process the only LocalRouter server on identity.db:
// an advisory flock(2) LOCK_EX|LOCK_NB on <data_dir>/identity.lock, held from
// before the store is opened until after it is closed. flock belongs to the
// open file description, so a second acquisition inside this process is
// refused exactly like one from another server. It does not make the server
// the database's only user: the `localrouter users` CLI deliberately opens
// identity.db next to a running server without it. The lock file is never
// unlinked (unlinking would let a waiter lock a fresh inode while the old one
// is still held). Close is idempotent and safe for concurrent use.
type identityLock struct {
	mu sync.Mutex
	f  *os.File
}

// acquireIdentityLock takes ownership of path without blocking. The file is
// created 0600 and a symlink at path is refused (O_NOFOLLOW).
func acquireIdentityLock(path string) (*identityLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("identity lock: %w", err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("identity lock: not a regular file")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("identity lock: %w", err)
		}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errIdentityLockHeld
		}
		return nil, fmt.Errorf("identity lock: %w", err)
	}
	return &identityLock{f: f}, nil
}

// Close releases the lock (closing the descriptor drops the flock).
func (l *identityLock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.f
	l.f = nil
	if f == nil {
		return nil
	}
	return f.Close()
}
