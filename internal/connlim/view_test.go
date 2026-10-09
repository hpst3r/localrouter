package connlim

// Generation-scoped limiter views (hot reload). A View carries one immutable
// limit configuration generation while sharing the Controller's live counters
// and leases. Frozen API: Controller.View(global, clientLimits) (*View, error),
// View.Acquire(client), View.Stats().

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// Views carry different configured limits but one shared active count.
func TestViewSharesCountsAcrossConfigs(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := c.View(3, nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := c.View(2, nil)
	if err != nil {
		t.Fatal(err)
	}

	r1, ok := old.Acquire("alice")
	if !ok {
		t.Fatal("old view first acquire refused")
	}
	r2, ok := old.Acquire("alice")
	if !ok {
		t.Fatal("old view second acquire refused")
	}

	// Shared runtime counters, generation-specific limits.
	if st := fresh.Stats(); st.GlobalActive != 2 || st.GlobalLimit != 2 {
		t.Fatalf("fresh view stats %+v, want active=2 limit=2", st)
	}
	if st := old.Stats(); st.GlobalActive != 2 || st.GlobalLimit != 3 {
		t.Fatalf("old view stats %+v, want active=2 limit=3 (unchanged by the new view)", st)
	}

	// The tighter new view is saturated by the old view's live requests,
	// while the old view still has its own headroom.
	if rel, ok := fresh.Acquire("bob"); ok {
		rel()
		t.Fatal("fresh view did not see the old view's active requests")
	}
	if rel, ok := old.Acquire("bob"); !ok {
		t.Fatal("old view lost its own headroom")
	} else {
		rel()
	}
	r1()
	r2()
	if st := fresh.Stats(); st.GlobalActive != 0 {
		t.Fatalf("after releases fresh view active = %d, want 0", st.GlobalActive)
	}
}

// A view rejects negative limits and clones the caller's map.
func TestViewValidationAndClone(t *testing.T) {
	c, err := New(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.View(-1, nil); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("global -1: %v", err)
	}
	if _, err := c.View(1, map[string]int{"vm1": -2}); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("per-client -2: %v", err)
	}

	limits := map[string]int{"vm1": 1}
	v, err := c.View(10, limits)
	if err != nil {
		t.Fatal(err)
	}
	limits["vm1"] = 99 // must not affect the built view
	if _, ok := v.Acquire("vm1"); !ok {
		t.Fatal("vm1 first acquire refused")
	}
	if rel, ok := v.Acquire("vm1"); ok {
		rel()
		t.Fatal("view kept a live reference to the caller's limit map")
	}
}

// A release taken through an old view decrements the shared counter exactly
// once, and a newer view observes the result.
func TestViewOldLeaseReleaseSharedOnce(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := c.View(4, nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := c.View(4, nil)
	if err != nil {
		t.Fatal(err)
	}

	r, ok := old.Acquire("alice")
	if !ok {
		t.Fatal("old view acquire refused")
	}
	if st := fresh.Stats(); st.GlobalActive != 1 {
		t.Fatalf("fresh view active = %d, want 1", st.GlobalActive)
	}
	r()
	r() // idempotent
	if st := fresh.Stats(); st.GlobalActive != 0 {
		t.Fatalf("after double release fresh view active = %d, want 0", st.GlobalActive)
	}

	// The shared slot is usable again through the new view.
	if _, ok := fresh.Acquire("alice"); !ok {
		t.Fatal("fresh view lost the slot freed by the old lease")
	}
}

// A tighter new generation sees requests admitted under the old one and
// refuses past its own cap; the Controller's own config is not mutated.
func TestViewSaturatedNewLimitsSeesOldActive(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rels []func()
	for i := 0; i < 3; i++ {
		r, ok := c.Acquire("alice")
		if !ok {
			t.Fatalf("controller acquire %d refused: %+v", i, c.Stats())
		}
		rels = append(rels, r)
	}

	tight, err := c.View(2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rel, ok := tight.Acquire("bob"); ok {
		rel()
		t.Fatal("tight view admitted past its global limit of 2")
	}
	if st := tight.Stats(); st.GlobalActive != 3 || st.GlobalLimit != 2 {
		t.Fatalf("tight view stats %+v, want active=3 limit=2", st)
	}
	// The controller keeps enforcing its own original configuration.
	if st := c.Stats(); st.GlobalLimit != 0 || st.GlobalActive != 3 {
		t.Fatalf("controller stats mutated by View: %+v", st)
	}
	for _, r := range rels {
		r()
	}
}

// Concurrent acquisition and release across several views must never exceed a
// cap nor corrupt shared counters. Run with -race.
func TestViewsConcurrentRace(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	views := make([]*View, 0, 3)
	for i := 0; i < 3; i++ {
		v, err := c.View(4, map[string]int{"vm1": 2})
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, v)
	}

	const workers = 48
	var total, admitted, refused atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		v := views[w%len(views)]
		client := "alice"
		if w%2 == 0 {
			client = "vm1"
		}
		wg.Add(1)
		go func(v *View, client string) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				total.Add(1)
				release, ok := v.Acquire(client)
				if !ok {
					refused.Add(1)
					continue
				}
				admitted.Add(1)
				release()
				release() // idempotent by contract
			}
		}(v, client)
	}
	wg.Wait()

	if admitted.Load()+refused.Load() != total.Load() {
		t.Fatalf("admitted+refused = %d, want %d", admitted.Load()+refused.Load(), total.Load())
	}
	for i, v := range views {
		st := v.Stats()
		if st.GlobalActive != 0 {
			t.Fatalf("view %d global active = %d after drain", i, st.GlobalActive)
		}
		for _, cl := range st.Clients {
			if cl.Active != 0 {
				t.Fatalf("view %d client %s active = %d after drain", i, cl.Name, cl.Active)
			}
		}
	}
	var _ core.InflightStats = views[0].Stats()
}
