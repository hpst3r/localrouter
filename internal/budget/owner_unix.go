//go:build linux || darwin

// exclusivebudgetownership lives here: the primitive a caller uses to prove it
// is the single process allowed to act as the budget database's owner (notably
// to call ReconcileOrphans in store.go at startup).
//
// Scope. This file is standalone and additive. It never opens the budget
// database, never reconciles orphans itself, and is not wired into config, the
// app object graph, the proxy or the ledger: a caller acquires ownership, then
// does whatever it needs with the *Store, and holds the *Ownership for the life
// of the process. Ownership is an application-level guard; the Store cannot
// verify single-process ownership for the caller (see the Store doc comment).
//
// Mechanism. Ownership is an advisory whole-file lock held with flock(2) as
// LOCK_EX|LOCK_NB on a dedicated lock file. flock is chosen over fcntl/POSIX
// record locks because a flock belongs to the open file description, not the
// process: two independent opens of the same path within ONE process still
// contend, so a second AcquireOwnership in the same process is denied exactly
// as one from another process would be. The lock is non-blocking, so a
// second owner is refused promptly with ErrOwnershipHeld instead of stalling.
//
// Release. Close closes the descriptor, which releases the flock. The lock file
// is deliberately NEVER unlinked: unlinking it on release would race, letting a
// waiter lock a freshly created inode while a third party still holds the old
// one and producing two "owners". Leaving the (empty) file in place keeps every
// acquirer contending on one stable path and one stable inode.
//
// Permissions. The lock file is created 0600 and a missing parent directory
// 0700. An already existing file or parent is used exactly as found and is
// never chmod-ed: a caller-supplied parent may be shared or deliberately
// broader, and narrowing it as a side effect would be a surprise. A symlink at
// the lock path is refused.
//
// Portability. This file is built for the release platforms linux and darwin
// only; owner_windows.go provides the explicit unsupported stub for windows.
package budget

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// ErrOwnershipHeld reports that exclusive ownership of the lock path is already
// held by a live owner — another process, or another open file description in
// this same process.
var ErrOwnershipHeld = errors.New("budget: ownership held by another owner")

// Ownership is an exclusive, advisory whole-file lock on a caller-chosen lock
// path. It is safe for concurrent use by multiple goroutines: Close is
// serialized and idempotent, so releasing it more than once, or from two
// goroutines at once, releases the lock at most once and returns nil after the
// first successful release.
type Ownership struct {
	mu     sync.Mutex
	f      *os.File
	closed bool
}

// AcquireOwnership takes exclusive ownership of path, creating the lock file
// and any missing parent directories, and returns it locked. It never blocks:
// if another owner already holds the lock it returns ErrOwnershipHeld
// immediately.
//
// The lock file is created mode 0600 and a missing parent directory mode 0700;
// an existing file or parent keeps whatever mode it already had. A symlink at
// path is refused and never followed. The caller must Close the returned
// *Ownership to release the lock; the lock file itself is intentionally left in
// place (see the file comment).
func AcquireOwnership(path string) (*Ownership, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("budget: acquire ownership: empty lock path")
	}
	// MkdirAll creates only the missing components, each with 0700, and never
	// rewrites the mode of a directory that already exists, so a broader or
	// shared parent is left untouched.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("budget: acquire ownership: %w", err)
	}

	// Refuse an existing symlink up front for a clear error; openLockFile
	// additionally opens with O_NOFOLLOW so the refusal cannot be defeated by a
	// symlink swapped in between this check and the open.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("budget: acquire ownership: %s is a symlink", path)
	}

	f, created, err := openLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("budget: acquire ownership: %w", err)
	}
	if created {
		// Pin the mode even under a permissive umask. Applied only to a file
		// this call just created, never to a pre-existing one.
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			return nil, fmt.Errorf("budget: acquire ownership: %w", err)
		}
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrOwnershipHeld
		}
		return nil, fmt.Errorf("budget: acquire ownership: %w", err)
	}
	return &Ownership{f: f}, nil
}

// openLockFile opens the lock file without following a symlink. When the file
// is absent it is created with mode 0600 and created is true; when it already
// exists it is opened as-is, without altering its mode, and created is false.
func openLockFile(path string) (f *os.File, created bool, err error) {
	f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err == nil {
		return f, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}
	f, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, err
	}
	return f, false, nil
}

// Close releases the ownership lock. It is idempotent: the first call closes
// the lock file's descriptor — which releases the flock — and every later call
// is a no-op returning nil. The lock file is left on disk on purpose; see the
// file comment for why unlinking it would be a race.
func (o *Ownership) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	f := o.f
	o.f = nil
	if f == nil {
		return nil
	}
	return f.Close()
}
