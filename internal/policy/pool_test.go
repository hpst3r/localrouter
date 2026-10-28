package policy

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Pooled-backend acceptance tests at the policy level (docs/SPEC.md "Pooled
// backends (routes[].balance)").

func localAcct(id string) core.Account {
	return core.Account{ID: id, Provider: core.ProviderOpenAICompat, CostBasis: "metered"}
}

func poolRoute(weights, caps map[string]int, ttl time.Duration) core.Route {
	return core.Route{Name: "pool", BalanceStrategy: "least_inflight", BalanceIdentity: "x",
		BalanceWeight: weights, BalanceMaxInflight: caps, BalanceStickyTTL: ttl}
}

func poolHarness(opts Options, ids ...string) *harness {
	var accts []core.Account
	for _, id := range ids {
		accts = append(accts, localAcct(id))
	}
	return newHarness(accts, opts)
}

// acquireB acquires on the pool and fails the test on denial.
func (h *harness) acquireB(t *testing.T, class core.Class, cands []string, r core.Route, session string) *PoolLease {
	t.Helper()
	l, d, denial, _, _ := h.p.AcquireBalanced(class, cands, nil, r, session)
	if !d.Allow || l == nil || denial != "" {
		t.Fatalf("pool acquire denied: %s (%s)", d.Reason, denial)
	}
	return l.(*PoolLease)
}

func TestP3SpreadByWeight(t *testing.T) {
	for _, tc := range []struct {
		wa, wb, n   int
		wantA, want int
	}{
		{1, 1, 4, 2, 2},
		{2, 1, 6, 4, 2},
	} {
		h := poolHarness(Options{}, "a", "b")
		r := poolRoute(map[string]int{"a": tc.wa, "b": tc.wb}, nil, 0)
		got := map[string]int{}
		for range tc.n {
			got[h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, "").AccountID()]++
		}
		if got["a"] != tc.wantA || got["b"] != tc.want {
			t.Fatalf("weights %d/%d: split %v", tc.wa, tc.wb, got)
		}
	}
}

func TestP4TieBreakFollowsClassListOrder(t *testing.T) {
	h := poolHarness(Options{}, "a", "b")
	r := poolRoute(map[string]int{"a": 1, "b": 1}, nil, 0)
	for range 10 {
		l := h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, "")
		if l.AccountID() != "a" {
			t.Fatalf("idle interactive pick = %s", l.AccountID())
		}
		if pick, inflight, limit := l.Pick(); pick != PoolPickLeastInflight || inflight != 1 || limit != 0 {
			t.Fatalf("pick = %s %d %d", pick, inflight, limit)
		}
		l.Release(core.Outcome{Status: 200})
	}
	l := h.acquireB(t, core.ClassBackground, []string{"b", "a"}, r, "")
	if l.AccountID() != "b" {
		t.Fatalf("idle background pick = %s, want the background list's first", l.AccountID())
	}
}

func TestP5SaturationDenies(t *testing.T) {
	h := poolHarness(Options{}, "a", "b")
	r := poolRoute(map[string]int{"a": 1, "b": 1}, map[string]int{"a": 1, "b": 1}, 0)
	la := h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, "")
	lb := h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, "")
	if la.AccountID() == lb.AccountID() {
		t.Fatal("both leases on one capped member")
	}
	if _, inflight, limit := lb.Pick(); inflight != 1 || limit != 1 {
		t.Fatalf("pick inflight/cap = %d/%d", inflight, limit)
	}
	l, d, denial, accts, _ := h.p.AcquireBalanced(core.ClassInteractive, []string{"a", "b"}, nil, r, "")
	if l != nil || d.Allow || denial != PoolDenySaturated || strings.Join(accts, ",") != "a,b" {
		t.Fatalf("third: %v %+v %q %v", l, d, denial, accts)
	}
	if d.Reason != "a: saturated (1/1 in flight); b: saturated (1/1 in flight)" {
		t.Fatalf("reason = %q", d.Reason)
	}
	la.Release(core.Outcome{Status: 200})
	if got := h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, "").AccountID(); got != "a" {
		t.Fatalf("after release pick = %s", got)
	}
	lb.Release(core.Outcome{Status: 200})
}

// TestP5DenialKinds pins the kind precedence: any policy (or excluded)
// rejection wins, then saturated, and only an all-breaker denial is breaker.
func TestP5DenialKinds(t *testing.T) {
	h := poolHarness(Options{Breaker: BreakerOptions{Threshold: 1, Base: 10 * time.Second}}, "a", "b", "c")
	r := poolRoute(nil, map[string]int{"a": 1}, 0)
	held := h.acquireB(t, core.ClassInteractive, []string{"a"}, r, "")
	defer held.Release(core.Outcome{})
	h.attempt(t, "b", dead) // b's breaker opens
	cases := []struct {
		cands   []string
		exclude map[string]bool
		want    string
	}{
		{[]string{"b"}, nil, PoolDenyBreaker},
		{[]string{"a", "b"}, nil, PoolDenySaturated},
		{[]string{"a", "b", "c"}, map[string]bool{"c": true}, PoolDenyPolicy},
		{nil, nil, PoolDenyPolicy},
	}
	for _, tc := range cases {
		_, d, denial, _, retryIn := h.p.AcquireBalanced(core.ClassInteractive, tc.cands, tc.exclude, r, "")
		if d.Allow || denial != tc.want {
			t.Fatalf("%v: %q (%s)", tc.cands, denial, d.Reason)
		}
		if denial == PoolDenyBreaker && retryIn != 10*time.Second {
			t.Fatalf("retryIn = %v", retryIn)
		}
	}
	// A quota/cooldown rejection is policy even next to a saturated member.
	h.attempt(t, "c", core.Outcome{Status: 429})
	if _, _, denial, _, _ := h.p.AcquireBalanced(core.ClassInteractive, []string{"a", "c"}, nil, r, ""); denial != PoolDenyPolicy {
		t.Fatalf("cooldown + saturated = %q", denial)
	}
}

func TestP6CapRace(t *testing.T) {
	h := poolHarness(Options{}, "a", "b")
	r := poolRoute(map[string]int{"a": 1, "b": 1}, map[string]int{"a": 2, "b": 2}, 0)
	var (
		mu   sync.Mutex
		cur  = map[string]int{}
		peak = map[string]int{}
		wg   sync.WaitGroup
	)
	for range 200 {
		wg.Go(func() {
			l, d, _, _, _ := h.p.AcquireBalanced(core.ClassInteractive, []string{"a", "b"}, nil, r, "")
			if !d.Allow {
				return
			}
			id := l.AccountID()
			mu.Lock()
			cur[id]++
			peak[id] = max(peak[id], cur[id])
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			cur[id]--
			mu.Unlock()
			l.Release(core.Outcome{Status: 200})
		})
	}
	wg.Wait()
	if peak["a"] > 2 || peak["b"] > 2 {
		t.Fatalf("peak = %v", peak)
	}
	if st := h.p.Status("a"); st.Inflight != 0 {
		t.Fatalf("leaked leases: %d", st.Inflight)
	}
}

// TestP7BreakerSkipAndSingleProbe: an open member gets no traffic until it
// half-opens, then exactly one pooled lease probes it.
func TestP7BreakerSkipAndSingleProbe(t *testing.T) {
	h := poolHarness(Options{Breaker: BreakerOptions{Threshold: 2, Base: 5 * time.Second}}, "a", "b")
	r := poolRoute(nil, nil, 0)
	cands := []string{"a", "b"}
	for range 2 {
		l := h.acquireB(t, core.ClassInteractive, cands, r, "")
		if l.AccountID() != "a" {
			t.Fatalf("pick = %s", l.AccountID())
		}
		l.Release(dead)
	}
	for range 5 {
		l := h.acquireB(t, core.ClassInteractive, cands, r, "")
		if l.AccountID() != "b" {
			t.Fatalf("open a picked")
		}
		l.Release(answered)
	}
	h.clock.Advance(5 * time.Second)
	probe := h.acquireB(t, core.ClassInteractive, cands, r, "")
	if probe.AccountID() != "a" || !probe.probe {
		t.Fatalf("half-open pick = %s probe=%v", probe.AccountID(), probe.probe)
	}
	if other := h.acquireB(t, core.ClassInteractive, cands, r, ""); other.AccountID() != "b" {
		t.Fatal("second lease probed a while the probe is in flight")
	} else {
		other.Release(answered)
	}
	probe.Release(answered)
	if state, _, _ := h.breaker(t, "a"); state != BreakerClosed {
		t.Fatalf("after probe success = %s", state)
	}
	if l := h.acquireB(t, core.ClassInteractive, cands, r, ""); l.AccountID() != "a" {
		t.Fatal("a not back in rotation")
	}
}

func TestP11Sticky(t *testing.T) {
	h := poolHarness(Options{}, "a", "b")
	r := poolRoute(nil, map[string]int{"a": 0, "b": 1}, 15*time.Minute)
	cands := []string{"a", "b"}
	// Bind S to b: a is busy at the first pick.
	busy := h.acquireB(t, core.ClassInteractive, cands, r, "")
	first := h.acquireB(t, core.ClassInteractive, cands, r, "S")
	busy.Release(answered)
	if first.AccountID() != "b" {
		t.Fatalf("first pick = %s", first.AccountID())
	}
	first.Release(answered)
	for range 5 {
		l := h.acquireB(t, core.ClassInteractive, cands, r, "S")
		if pick, _, _ := l.Pick(); l.AccountID() != "b" || pick != PoolPickSticky {
			t.Fatalf("sticky pick = %s %s", l.AccountID(), pick)
		}
		l.Release(answered)
	}
	// b saturated: spill to a and rebind.
	hold := h.acquireB(t, core.ClassInteractive, []string{"b"}, r, "")
	spill := h.acquireB(t, core.ClassInteractive, cands, r, "S")
	if spill.AccountID() != "a" {
		t.Fatalf("spill = %s", spill.AccountID())
	}
	spill.Release(answered)
	hold.Release(answered)
	for range 3 {
		l := h.acquireB(t, core.ClassInteractive, cands, r, "S")
		if l.AccountID() != "a" {
			t.Fatalf("after rebind = %s", l.AccountID())
		}
		l.Release(answered)
	}
	// Bind T to b, let the TTL elapse: the pick reverts to strategy (a).
	hold = h.acquireB(t, core.ClassInteractive, cands, r, "")
	if l := h.acquireB(t, core.ClassInteractive, cands, r, "T"); l.AccountID() != "b" {
		t.Fatal("T not bound to b")
	} else {
		l.Release(answered)
	}
	hold.Release(answered)
	h.clock.Advance(14 * time.Minute)
	if l := h.acquireB(t, core.ClassInteractive, cands, r, "T"); l.AccountID() != "b" {
		t.Fatal("binding expired early (TTL slides on use)")
	} else {
		l.Release(answered)
	}
	h.clock.Advance(15 * time.Minute)
	if l := h.acquireB(t, core.ClassInteractive, cands, r, "T"); l.AccountID() != "a" {
		t.Fatalf("after TTL pick = %s", l.AccountID())
	} else if pick, _, _ := l.Pick(); pick != PoolPickLeastInflight {
		t.Fatalf("after TTL pick reason = %s", pick)
	} else {
		l.Release(answered)
	}
}

// TestP11StickyYieldsToLoad: an uncapped bound member more than two leases
// above the least-loaded eligible member yields.
func TestP11StickyYieldsToLoad(t *testing.T) {
	h := poolHarness(Options{}, "a", "b")
	r := poolRoute(nil, nil, time.Hour)
	if l := h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, "S"); l.AccountID() != "a" {
		t.Fatal("S not bound to a")
	} else {
		l.Release(answered)
	}
	var held []*PoolLease
	for i := range 3 {
		held = append(held, h.acquireB(t, core.ClassInteractive, []string{"a"}, r, ""))
		l := h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, "S")
		want := "a"
		if i == 2 { // a at 3 > b at 0 + 2
			want = "b"
		}
		if l.AccountID() != want {
			t.Fatalf("with a at %d: pick %s, want %s", i+1, l.AccountID(), want)
		}
		l.Release(answered)
	}
	for _, l := range held {
		l.Release(answered)
	}
}

func TestP11NoHeaderNoBindingAndLRUBound(t *testing.T) {
	h := poolHarness(Options{}, "a")
	r := poolRoute(nil, nil, time.Hour)
	for range 3 {
		h.acquireB(t, core.ClassInteractive, []string{"a"}, r, "").Release(answered)
	}
	if n := h.p.AffinityEntries(""); n != 0 {
		t.Fatalf("no-header bindings = %d", n)
	}
	noTTL := poolRoute(nil, nil, 0)
	h.acquireB(t, core.ClassInteractive, []string{"a"}, noTTL, "S").Release(answered)
	if n := h.p.AffinityEntries(""); n != 0 {
		t.Fatalf("sticky_ttl 0 bindings = %d", n)
	}
	for i := range affinityCap + 50 {
		h.acquireB(t, core.ClassInteractive, []string{"a"}, r, fmt.Sprintf("s%d", i)).Release(answered)
	}
	if n, ll := h.p.AffinityEntries("pool"), h.p.aff.ll.Len(); n != affinityCap || ll != affinityCap || len(h.p.aff.items) != affinityCap {
		t.Fatalf("LRU size = %d/%d/%d", n, ll, len(h.p.aff.items))
	}
	// The oldest bindings were evicted, the newest kept.
	if _, ok := h.p.aff.get(affinityKey("pool", "s0"), t0); ok {
		t.Fatal("oldest binding not evicted")
	}
	if _, ok := h.p.aff.get(affinityKey("pool", fmt.Sprintf("s%d", affinityCap+49)), t0); !ok {
		t.Fatal("newest binding missing")
	}
}

func TestP12DryRunBalancedHasNoSideEffects(t *testing.T) {
	h := poolHarness(Options{Breaker: BreakerOptions{Threshold: 1, Base: 5 * time.Second}}, "a", "b")
	r := poolRoute(map[string]int{"a": 1, "b": 1}, map[string]int{"a": 1, "b": 1}, time.Hour)
	cands := []string{"a", "b"}
	h.attempt(t, "a", dead)
	h.clock.Advance(5 * time.Second) // a half-open
	held := h.acquireB(t, core.ClassInteractive, []string{"b"}, r, "")
	d := h.p.DryRunBalanced(core.ClassInteractive, cands, r, "S")
	if !d.Allow || d.AccountID != "a" || d.Reason != "admitted a (pool least_inflight; skipped b: saturated (1/1 in flight))" {
		t.Fatalf("dry run = %+v", d)
	}
	if st := h.p.Status("a"); st.Inflight != 0 {
		t.Fatal("dry run created a lease")
	}
	if n := h.p.AffinityEntries(""); n != 0 {
		t.Fatal("dry run wrote an affinity entry")
	}
	if probe := h.acquireB(t, core.ClassInteractive, cands, r, ""); !probe.probe {
		t.Fatal("dry run claimed the half-open probe")
	} else {
		probe.Release(answered)
	}
	held.Release(answered)
	// Dry run follows a live binding.
	h.acquireB(t, core.ClassInteractive, []string{"b"}, r, "S").Release(answered)
	if d := h.p.DryRunBalanced(core.ClassInteractive, cands, r, "S"); d.AccountID != "b" || !strings.Contains(d.Reason, "pool sticky") {
		t.Fatalf("sticky dry run = %+v", d)
	}
}

func TestP13ReloadSharesLeasesAndAffinity(t *testing.T) {
	h := poolHarness(Options{}, "a", "b")
	r := poolRoute(nil, map[string]int{"a": 1, "b": 1}, time.Hour)
	la := h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, "S")
	lb := h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, "")
	v, err := h.p.WithConfig([]core.Account{localAcct("a"), localAcct("b")}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Caps count the old generation's leases.
	if _, _, denial, _, _ := v.AcquireBalanced(core.ClassInteractive, []string{"a", "b"}, nil, r, ""); denial != PoolDenySaturated {
		t.Fatalf("new view ignores old leases: %q", denial)
	}
	la.Release(answered)
	// The binding survives the view swap.
	if l, _, _, _, _ := v.AcquireBalanced(core.ClassInteractive, []string{"a", "b"}, nil, r, "S"); l.AccountID() != "a" {
		t.Fatal("binding lost across reload")
	} else if pick, _, _ := l.(*PoolLease).Pick(); pick != PoolPickSticky {
		t.Fatalf("pick = %s", pick)
	} else {
		l.Release(answered)
	}
	// Removing b from the route: b is never picked, and its old lease releases
	// without underflow.
	onlyA := poolRoute(nil, map[string]int{"a": 1}, time.Hour)
	for range 3 {
		l, _, _, _, _ := v.AcquireBalanced(core.ClassInteractive, []string{"a"}, nil, onlyA, "S")
		if l.AccountID() != "a" {
			t.Fatal("removed member picked")
		}
		l.Release(answered)
	}
	lb.Release(answered)
	lb.Release(answered)
	if st := v.Status("b"); st.Inflight != 0 {
		t.Fatalf("b inflight = %d", st.Inflight)
	}
	// Adding an account stays restart-only.
	if _, err := v.WithConfig([]core.Account{localAcct("a"), localAcct("b"), localAcct("c")}, Options{}); err == nil {
		t.Fatal("added account accepted")
	}
}
