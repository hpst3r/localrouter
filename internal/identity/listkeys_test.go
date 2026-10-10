package identity

import (
	"context"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Every live key is listed, whatever the history behind it: live keys come
// first (newest first), then the newest non-live keys up to MaxListLimit.
func TestListKeysAlwaysListsLiveKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("victim", core.RoleUser)
	self := Actor{Kind: ActorUser, UserID: u.ID}
	live, err := f.s.CreateKey(ctx, u.ID, "persist", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Second)
	expiring, err := f.s.CreateKey(ctx, u.ID, "short", MinKeyTTL)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Second)
	for i := 0; i < MaxListLimit; i++ {
		k, err := f.s.CreateKey(ctx, u.ID, "churn", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.RevokeKey(ctx, self, u.ID, k.ID); err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(time.Second)
	}
	second, err := f.s.CreateKey(ctx, u.ID, "second", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(MinKeyTTL) // "short" is now expired: history, not live

	keys, err := f.s.ListKeys(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != MaxListLimit {
		t.Fatalf("listed %d keys, want %d", len(keys), MaxListLimit)
	}
	if keys[0].ID != second.ID || keys[1].ID != live.ID {
		t.Fatalf("live keys not listed first: %s, %s", keys[0].ID, keys[1].ID)
	}
	for i, k := range keys[2:] {
		if k.RevokedAt.IsZero() {
			t.Fatalf("history entry %d is not revoked", i)
		}
		if k.ID == expiring.ID {
			t.Fatal("oldest expired key listed ahead of newer history")
		}
		if i > 0 && k.CreatedAt.After(keys[i+1].CreatedAt) {
			t.Fatalf("history not newest first at %d", i)
		}
	}
}
