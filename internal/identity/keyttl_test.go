package identity

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// reopenWithKeyMaxTTL closes the fixture's store and reopens the same file
// with a different KeyMaxTTL, as an operator restart after editing
// keys.max_ttl would.
func (f *fixture) reopenWithKeyMaxTTL(ttl time.Duration) *Store {
	f.t.Helper()
	f.s.Close()
	f.opts.KeyMaxTTL = ttl
	f.opts.Rand = &lockedRand{r: rand.New(rand.NewSource(2))}
	s, err := Open(context.Background(), f.path, f.opts)
	if err != nil {
		f.t.Fatalf("reopen = %v", err)
	}
	f.t.Cleanup(func() { s.Close() })
	f.s = s
	return s
}

// A key issued under a longer ceiling is bounded by a tightened KeyMaxTTL
// after restart: it expires at min(stored expiry, created + current ceiling).
func TestTightenedKeyMaxTTLBoundsExistingKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-ttl", core.RoleUser)
	long, err := f.s.CreateKey(ctx, u.ID, "long", 0) // 90d under the default ceiling
	if err != nil {
		t.Fatal(err)
	}
	short, err := f.s.CreateKey(ctx, u.ID, "short", 2*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s := f.reopenWithKeyMaxTTL(7 * 24 * time.Hour)

	// The stored expiry is still the tighter bound for the 2d key.
	f.clock.Advance(2 * 24 * time.Hour)
	if _, err := s.AuthenticateKey(ctx, short.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("2d key at +2d = %v, want ErrUnauthenticated", err)
	}
	// Keep the 30-day login window fresh so only the TTL is under test.
	f.clock.Advance(5*24*time.Hour - time.Millisecond)
	if _, err := s.ResolveLogin(ctx, f.login("sub-ttl", core.RoleUser)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateKey(ctx, long.Token); err != nil {
		t.Fatalf("90d key 1ms before the tightened 7d ceiling = %v, want ok", err)
	}
	f.clock.Advance(time.Millisecond)
	if _, err := s.AuthenticateKey(ctx, long.Token); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("90d key at created+7d after max_ttl was tightened to 7d = %v, want ErrUnauthenticated", err)
	}
}

// ListKeys reports the effective expiry under the current ceiling, so the
// key list shows the same expiry AuthenticateKey enforces.
func TestListKeysReportsTightenedExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-ttl-list", core.RoleUser)
	long, err := f.s.CreateKey(ctx, u.ID, "long", 0)
	if err != nil {
		t.Fatal(err)
	}
	short, err := f.s.CreateKey(ctx, u.ID, "short", 2*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s := f.reopenWithKeyMaxTTL(7 * 24 * time.Hour)
	keys, err := s.ListKeys(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{
		long.ID:  long.CreatedAt.Add(7 * 24 * time.Hour),
		short.ID: short.ExpiresAt,
	}
	if len(keys) != len(want) {
		t.Fatalf("ListKeys = %d keys, want %d", len(keys), len(want))
	}
	for _, k := range keys {
		if !k.ExpiresAt.Equal(want[k.ID]) {
			t.Errorf("key %s ExpiresAt = %s, want %s", k.ID, k.ExpiresAt, want[k.ID])
		}
	}
}

// A key past its tightened ceiling no longer counts toward MaxKeysPerUser.
func TestTightenedKeyMaxTTLFreesKeyLimit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.s.Close()
	f.opts.MaxKeysPerUser = 1
	s, err := Open(ctx, f.path, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	u := f.user("sub-ttl-limit", core.RoleUser)
	if _, err := f.s.CreateKey(ctx, u.ID, "old", 0); err != nil {
		t.Fatal(err)
	}
	s = f.reopenWithKeyMaxTTL(7 * 24 * time.Hour)
	f.clock.Advance(7 * 24 * time.Hour)
	if _, err := s.ResolveLogin(ctx, f.login("sub-ttl-limit", core.RoleUser)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateKey(ctx, u.ID, "new", 0); err != nil {
		t.Fatalf("CreateKey with only an effectively expired key = %v, want ok", err)
	}
}
