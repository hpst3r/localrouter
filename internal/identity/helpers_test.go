package identity

import (
	"context"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	testIssuer   = "https://idp.example.test/application/o/localrouter/"
	testClientID = "localrouter-client"
)

// fakeClock is a deterministic, settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// lockedRand is a seeded, goroutine-safe io.Reader for deterministic ids.
type lockedRand struct {
	mu sync.Mutex
	r  *rand.Rand
}

func (l *lockedRand) Read(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.Read(p)
}

type fixture struct {
	t     *testing.T
	dir   string
	path  string
	clock *fakeClock
	opts  Options
	s     *Store
}

func baseOptions(clock core.Clock) Options {
	return Options{
		Issuer:   testIssuer,
		ClientID: testClientID,
		Clock:    clock,
		Rand:     &lockedRand{r: rand.New(rand.NewSource(1))},
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	f := &fixture{t: t, dir: dir, path: filepath.Join(dir, "identity.db"), clock: newFakeClock()}
	f.opts = baseOptions(f.clock)
	s, err := Open(context.Background(), f.path, f.opts)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	f.s = s
	t.Cleanup(func() { s.Close() })
	return f
}

// second opens another *Store on the same file (simulates a CLI process).
func (f *fixture) second() *Store {
	f.t.Helper()
	s, err := Open(context.Background(), f.path, f.opts)
	if err != nil {
		f.t.Fatalf("second Open = %v", err)
	}
	f.t.Cleanup(func() { s.Close() })
	return s
}

func (f *fixture) login(subject string, role core.Role) Login {
	return Login{
		Issuer:      testIssuer,
		Subject:     subject,
		Role:        role,
		AuthTime:    f.clock.Now(),
		Email:       subject + "@example.test",
		DisplayName: "Display " + subject,
		Provision:   true,
	}
}

// user provisions and returns an active user with a fresh login.
func (f *fixture) user(subject string, role core.Role) User {
	f.t.Helper()
	u, err := f.s.ResolveLogin(context.Background(), f.login(subject, role))
	if err != nil {
		f.t.Fatalf("ResolveLogin(%s) = %v", subject, err)
	}
	return u
}
