package connlim

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// A global limit admits at most N concurrent requests and refuses further
// ones without ever going negative when the refusal path is exercised.
func TestGlobalLimit(t *testing.T) {
	c, err := New(2, nil)
	if err != nil {
		t.Fatal(err)
	}
	r1, ok := c.Acquire("alice")
	if !ok {
		t.Fatal("first acquire refused")
	}
	if _, ok := c.Acquire("alice"); !ok {
		t.Fatal("second acquire refused")
	}
	if rel, ok := c.Acquire("alice"); ok {
		rel()
		t.Fatal("third acquire admitted past global limit")
	}
	st := c.Stats()
	if st.GlobalLimit != 2 || st.GlobalActive != 2 || st.GlobalPeak != 2 {
		t.Fatalf("stats %+v", st)
	}
	r1()
	if got := c.Stats(); got.GlobalActive != 1 || got.GlobalPeak != 2 {
		t.Fatalf("after release %+v", got)
	}
}

// Zero means unlimited, so a zero global limit never refuses.
func TestZeroMeansUnlimited(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rels []func()
	for i := 0; i < 50; i++ {
		r, ok := c.Acquire("alice")
		if !ok {
			t.Fatalf("unlimited refused at %d", i)
		}
		rels = append(rels, r)
	}
	if st := c.Stats(); st.GlobalActive != 50 {
		t.Fatalf("stats %+v", st)
	}
	for _, r := range rels {
		r()
	}
}

// A per-client limit refuses only the over-limit client; other clients keep
// their own independent budget.
func TestPerClientIsolation(t *testing.T) {
	c, err := New(10, map[string]int{"vm1": 1})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := c.Acquire("vm1")
	if !ok {
		t.Fatal("vm1 first acquire refused")
	}
	if rel, ok := c.Acquire("vm1"); ok {
		rel()
		t.Fatal("vm1 admitted past its per-client limit")
	}
	if _, ok := c.Acquire("alice"); !ok {
		t.Fatal("alice refused because vm1 was saturated")
	}
	r()
	if _, ok := c.Acquire("vm1"); !ok {
		t.Fatal("vm1 still refused after release")
	}
}

// Release is idempotent: a double release must not free someone else's slot.
func TestReleaseIdempotent(t *testing.T) {
	c, err := New(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := c.Acquire("alice")
	if !ok {
		t.Fatal("acquire refused")
	}
	r()
	r()
	r()
	if st := c.Stats(); st.GlobalActive != 0 {
		t.Fatalf("active = %d after triple release", st.GlobalActive)
	}
	// The slot must be usable again — a stray decrement would let two through.
	if _, ok := c.Acquire("alice"); !ok {
		t.Fatal("slot lost")
	}
	if rel, ok := c.Acquire("alice"); ok {
		rel()
		t.Fatal("limit exceeded after reacquire")
	}
}

// Negative limits are invalid configuration.
func TestNegativeRejected(t *testing.T) {
	if _, err := New(-1, nil); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("global -1: %v", err)
	}
	if _, err := New(1, map[string]int{"vm1": -2}); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("per-client -2: %v", err)
	}
}

// Reconfigure is the reload seam: limits change while active counts survive.
func TestReconfigurePreservesActive(t *testing.T) {
	c, err := New(4, map[string]int{"vm1": 2})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := c.Acquire("vm1")
	_ = r
	if err := c.Reconfigure(1, map[string]int{"vm1": 0}); err != nil {
		t.Fatal(err)
	}
	st := c.Stats()
	if st.GlobalLimit != 1 || st.GlobalActive != 1 {
		t.Fatalf("stats %+v", st)
	}
	// Global limit 1 is now saturated by the carried-over active request.
	if rel, ok := c.Acquire("alice"); ok {
		rel()
		t.Fatal("reconfigured global limit not enforced")
	}
	if err := c.Reconfigure(-1, nil); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("reconfigure negative: %v", err)
	}
}

// Stats lists configured clients even at zero usage so diagnostics can show
// capacity, and sorts deterministically.
func TestStatsShape(t *testing.T) {
	c, err := New(3, map[string]int{"vm2": 1, "alice": 2})
	if err != nil {
		t.Fatal(err)
	}
	st := c.Stats()
	if st.Clients[0].Name != "alice" || st.Clients[0].Limit != 2 || st.Clients[1].Name != "vm2" {
		t.Fatalf("clients %+v", st.Clients)
	}
	var _ core.InflightStats = st
}

// A storm of concurrent Acquire/Release from many goroutines must never admit
// more than the global cap at a time, never corrupt the per-client or global
// counters, and leave every slot accounted for when it drains. Run with -race.
func TestConcurrentAcquireReleaseNeverExceeds(t *testing.T) {
	const (
		global  = 4
		workers = 64
		iters   = 200
	)
	c, err := New(global, map[string]int{"vm1": 2})
	if err != nil {
		t.Fatal(err)
	}

	var active, peak atomic.Int64
	var admitted, refused atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		client := "alice"
		if w%2 == 0 {
			client = "vm1"
		}
		go func(client string) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				release, ok := c.Acquire(client)
				if !ok {
					refused.Add(1)
					runtime.Gosched()
					continue
				}
				admitted.Add(1)
				n := active.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				runtime.Gosched()
				active.Add(-1)
				release()
				release() // idempotent by contract
			}
		}(client)
	}
	wg.Wait()

	if p := peak.Load(); p > global {
		t.Fatalf("observed %d concurrent requests, global cap is %d", p, global)
	}
	if admitted.Load()+refused.Load() != workers*iters {
		t.Fatalf("admitted+refused = %d, want %d", admitted.Load()+refused.Load(), workers*iters)
	}
	st := c.Stats()
	if st.GlobalActive != 0 {
		t.Fatalf("global active = %d after drain, want 0 (double release or leak)", st.GlobalActive)
	}
	if st.GlobalPeak > global {
		t.Fatalf("recorded peak %d exceeds global cap %d", st.GlobalPeak, global)
	}
	for _, cl := range st.Clients {
		if cl.Active != 0 {
			t.Fatalf("client %s active = %d after drain, want 0", cl.Name, cl.Active)
		}
	}
}

// A client over its own per-client limit is refused even when the global cap
// has room, and other clients are never starved by it.
func TestPerClientCapUnderGlobalHeadroom(t *testing.T) {
	c, err := New(10, map[string]int{"vm1": 1})
	if err != nil {
		t.Fatal(err)
	}
	r1, ok := c.Acquire("vm1")
	if !ok {
		t.Fatal("first vm1 acquire refused")
	}
	if r, ok := c.Acquire("vm1"); ok {
		r()
		t.Fatal("vm1 admitted past its per-client cap with global headroom")
	}
	// Nine other clients can still fill the rest of the global budget.
	var rels []func()
	for i := 0; i < 9; i++ {
		r, ok := c.Acquire("other")
		if !ok {
			t.Fatalf("other client %d refused: %+v", i, c.Stats())
		}
		rels = append(rels, r)
	}
	if st := c.Stats(); st.GlobalActive != 10 {
		t.Fatalf("stats %+v", st)
	}
	if r, ok := c.Acquire("other"); ok {
		r()
		t.Fatal("global cap not enforced")
	}
	r1()
	for _, r := range rels {
		r()
	}
	if st := c.Stats(); st.GlobalActive != 0 {
		t.Fatalf("global active = %d after release", st.GlobalActive)
	}
}

// Reconfigure while requests are active must not lose or duplicate slots: the
// carried-over active request keeps its slot and a fresh one becomes visible.
func TestReconfigureUnderLoad(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rels []func()
	for i := 0; i < 5; i++ {
		r, ok := c.Acquire("alice")
		if !ok {
			t.Fatalf("unlimited refused at %d", i)
		}
		rels = append(rels, r)
	}
	if err := c.Reconfigure(2, map[string]int{"alice": 1}); err != nil {
		t.Fatal(err)
	}
	if st := c.Stats(); st.GlobalActive != 5 || st.GlobalLimit != 2 {
		t.Fatalf("after reconfigure %+v", st)
	}
	// The new limits apply to subsequent admissions only.
	if r, ok := c.Acquire("alice"); ok {
		r()
		t.Fatal("new per-client limit not applied")
	}
	// Dropping three active requests leaves two over the new cap; no negative
	// accounting, and the limit still refuses.
	for i := 0; i < 3; i++ {
		rels[i]()
	}
	if st := c.Stats(); st.GlobalActive != 2 {
		t.Fatalf("after partial release %+v", st)
	}
	if r, ok := c.Acquire("bob"); ok {
		r()
		t.Fatal("global cap of 2 not enforced after releases")
	}
	for _, r := range rels[3:] {
		r()
	}
	if st := c.Stats(); st.GlobalActive != 0 {
		t.Fatalf("global active = %d after drain", st.GlobalActive)
	}
}
