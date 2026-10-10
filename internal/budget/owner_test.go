//go:build linux || darwin

package budget

// Tests for exclusive budget ownership (owner_unix.go).
//
// Contract under test:
//
//	AcquireOwnership(path string) (*Ownership, error)
//	(*Ownership).Close() error           // idempotent
//	ErrOwnershipHeld                     // second owner denied
//
// The lock is a flock, so a second open file description contends even inside
// this one test process — which is exactly how "another process" is simulated
// here without forking. The lock file must never be unlinked on release (its
// identity must be stable across Close/reacquire), newly created paths are
// 0600 in a 0700 parent while pre-existing permissions are preserved, and a
// symlink at the lock path is refused.

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func lockTestPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "data", "budget.lock")
}

// heldByRawFlock reports whether an independent descriptor sees the lock held.
func heldByRawFlock(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open lock path: %v", err)
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		// We acquired it; drop it so the caller's expectations stay simple.
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("raw flock = %v, want EWOULDBLOCK/EAGAIN", err)
	}
	return true
}

func TestAcquireOwnershipSecondOwnerDenied(t *testing.T) {
	path := lockTestPath(t)

	first, err := AcquireOwnership(path)
	if err != nil {
		t.Fatalf("first AcquireOwnership = %v, want nil", err)
	}

	// Same process, separate open file description: flock contends, so this is
	// a genuine second owner and must be denied.
	second, err := AcquireOwnership(path)
	if second != nil {
		t.Fatalf("second AcquireOwnership returned %v, want nil Ownership", second)
	}
	if !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("second AcquireOwnership = %v, want ErrOwnershipHeld", err)
	}
	if !heldByRawFlock(t, path) {
		t.Fatal("independent descriptor did not observe the lock as held")
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if heldByRawFlock(t, path) {
		t.Fatal("lock still held after Close")
	}
	if _, err := AcquireOwnership(path); err != nil {
		t.Fatalf("AcquireOwnership after release = %v, want nil", err)
	}
}

func TestOwnershipCloseIdempotentReacquirePathStable(t *testing.T) {
	path := lockTestPath(t)

	first, err := AcquireOwnership(path)
	if err != nil {
		t.Fatalf("AcquireOwnership = %v, want nil", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before Close: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close = %v, want nil", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil (Close must be idempotent)", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("third Close = %v, want nil", err)
	}

	// The lock file must survive Close: unlinking it would let two owners
	// contend on different inodes.
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after Close: %v (lock file must not be unlinked)", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("lock file identity changed across Close; path is not stable")
	}

	second, err := AcquireOwnership(path)
	if err != nil {
		t.Fatalf("reacquire after Close = %v, want nil", err)
	}
	defer second.Close()
	if !heldByRawFlock(t, path) {
		t.Fatal("reacquired lock is not held")
	}
	if _, err := AcquireOwnership(path); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("contend reacquired lock = %v, want ErrOwnershipHeld", err)
	}
}

func TestOwnershipNewFileAndParentPerms(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	path := filepath.Join(dir, "budget.lock")

	o, err := AcquireOwnership(path)
	if err != nil {
		t.Fatalf("AcquireOwnership = %v, want nil", err)
	}
	defer o.Close()

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("created parent mode = %04o, want 0700", got)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat lock file: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("created lock file mode = %04o, want 0600", got)
	}
}

func TestOwnershipPreservesExistingPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // defeat umask
		t.Fatalf("chmod dir: %v", err)
	}
	path := filepath.Join(dir, "budget.lock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod lock file: %v", err)
	}

	o, err := AcquireOwnership(path)
	if err != nil {
		t.Fatalf("AcquireOwnership = %v, want nil", err)
	}
	defer o.Close()

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o755 {
		t.Errorf("existing parent mode changed to %04o, want 0755 left alone", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("existing lock file mode changed to %04o, want 0644 left alone", got)
	}
}

func TestOwnershipRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("do not touch"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "budget.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	o, err := AcquireOwnership(link)
	if o != nil {
		o.Close()
		t.Fatal("AcquireOwnership on symlink returned an Ownership, want refusal")
	}
	if err == nil {
		t.Fatal("AcquireOwnership on symlink = nil, want error")
	}

	// The symlink target must be untouched and must not have been locked.
	f, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("target was locked through the symlink: %v", err)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	// A dangling symlink is refused too.
	dangling := filepath.Join(dir, "dangling.lock")
	if err := os.Symlink(filepath.Join(dir, "missing"), dangling); err != nil {
		t.Fatalf("symlink dangling: %v", err)
	}
	if _, err := AcquireOwnership(dangling); err == nil {
		t.Fatal("AcquireOwnership on dangling symlink = nil, want error")
	}
}

func TestOwnershipConcurrentAcquireSingleWinner(t *testing.T) {
	path := lockTestPath(t)
	const n = 8

	start := make(chan struct{})
	var wg sync.WaitGroup
	owners := make([]*Ownership, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			owners[i], errs[i] = AcquireOwnership(path)
		}(i)
	}
	close(start)
	wg.Wait()

	winners, denied := 0, 0
	for i := 0; i < n; i++ {
		switch {
		case errs[i] == nil:
			winners++
			if owners[i] == nil {
				t.Errorf("goroutine %d: nil error but nil Ownership", i)
				continue
			}
			if err := owners[i].Close(); err != nil {
				t.Errorf("goroutine %d: Close = %v, want nil", i, err)
			}
		case errors.Is(errs[i], ErrOwnershipHeld):
			denied++
		default:
			t.Errorf("goroutine %d: unexpected error %v", i, errs[i])
		}
	}
	if winners != 1 || denied != n-1 {
		t.Fatalf("winners=%d denied=%d, want 1 winner and %d denied", winners, denied, n-1)
	}

	// Every winner released: a fresh acquire must now succeed.
	final, err := AcquireOwnership(path)
	if err != nil {
		t.Fatalf("AcquireOwnership after all released = %v, want nil", err)
	}
	if err := final.Close(); err != nil {
		t.Fatalf("final Close = %v, want nil", err)
	}
	if err := final.Close(); err != nil {
		t.Fatalf("final repeated Close = %v, want nil", err)
	}
}
