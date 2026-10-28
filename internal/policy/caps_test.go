package policy

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Pooled backends Amendment R1.1 (PL-1): max_inflight is a per-account cap
// enforced on every route, pooled or not.

func TestR1AccountCaps(t *testing.T) {
	if caps := AccountCaps([]core.Route{{Name: "plain"}, poolRoute(nil, map[string]int{"a": 0}, 0)}); caps != nil {
		t.Fatalf("no non-zero cap: %v", caps)
	}
	caps := AccountCaps([]core.Route{poolRoute(nil, map[string]int{"a": 2, "b": 0}, 0), poolRoute(nil, map[string]int{"a": 3, "c": 1}, 0)})
	if len(caps) != 2 || caps["a"] != 2 || caps["c"] != 1 {
		t.Fatalf("caps = %v", caps)
	}
}

func TestR1UnpooledRouteHonorsAccountCap(t *testing.T) {
	h := poolHarness(Options{MaxInflight: map[string]int{"a": 1}, Breaker: BreakerOptions{Threshold: 1, Base: 10 * time.Second}}, "a", "b", "c", "d")
	held, d := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow {
		t.Fatal(d.Reason)
	}
	defer held.Release(core.Outcome{})

	// The next candidate serves; a capped account is skipped like a full pool member.
	l, d := h.p.Acquire(core.ClassInteractive, []string{"a", "d"}, nil)
	if !d.Allow || l.AccountID() != "d" || d.Reason != "admitted d (skipped a: saturated (1/1 in flight))" {
		t.Fatalf("acquire: %+v", d)
	}
	l.Release(core.Outcome{Status: 200})
	if d := h.p.DryRun(core.ClassInteractive, []string{"a"}); d.Allow {
		t.Fatal("dry run admitted a capped account at its cap")
	}

	h.attempt(t, "b", dead) // b's breaker opens
	h.attempt(t, "c", core.Outcome{Status: 429})
	cases := []struct {
		cands []string
		want  string
		accts string
	}{
		{[]string{"a"}, PoolDenySaturated, "a"},
		{[]string{"a", "b"}, PoolDenySaturated, "a"},
		{[]string{"b"}, PoolDenyBreaker, "b"},
		{[]string{"a", "c"}, PoolDenyPolicy, ""},
		{nil, PoolDenyPolicy, ""},
	}
	for _, tc := range cases {
		l, d, denial, accts, retryIn := h.p.AcquireCapped(core.ClassInteractive, tc.cands, nil)
		if l != nil || d.Allow || denial != tc.want || strings.Join(accts, ",") != tc.accts {
			t.Fatalf("%v: %q %v (%s)", tc.cands, denial, accts, d.Reason)
		}
		if denial == PoolDenyBreaker && retryIn != 10*time.Second {
			t.Fatalf("retryIn = %v", retryIn)
		}
	}
	// AcquireWithBreaker's all-breaker report excludes a cap rejection.
	if _, _, unreachable, _ := h.p.AcquireWithBreaker(core.ClassInteractive, []string{"a", "b"}, nil); unreachable != nil {
		t.Fatalf("saturated + breaker reported as breaker-only: %v", unreachable)
	}
	if l, d, denial, _, _ := h.p.AcquireCapped(core.ClassInteractive, []string{"a", "d"}, nil); !d.Allow || denial != "" || l.AccountID() != "d" {
		t.Fatalf("capped admit: %q %+v", denial, d)
	} else {
		l.Release(core.Outcome{Status: 200})
	}
}

// TestR1PoolUsesAccountCap: the per-account cap applies to a pooled route even
// when its own block leaves the member uncapped, and member_cap reports it.
func TestR1PoolUsesAccountCap(t *testing.T) {
	h := poolHarness(Options{MaxInflight: map[string]int{"a": 1}}, "a", "b")
	r := poolRoute(nil, nil, 0)
	la := h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, "")
	if _, _, limit := la.Pick(); la.AccountID() != "a" || limit != 1 {
		t.Fatalf("first pick %s cap %d", la.AccountID(), limit)
	}
	for range 3 {
		if l := h.acquireB(t, core.ClassInteractive, []string{"a", "b"}, r, ""); l.AccountID() != "b" {
			t.Fatalf("pick past a's cap: %s", l.AccountID())
		}
	}
}

// TestR1SharedCapRace: an account in a pooled route and an unpooled route
// never exceeds its cap under 200 concurrent requests across both.
func TestR1SharedCapRace(t *testing.T) {
	routes := []core.Route{poolRoute(nil, map[string]int{"a": 2, "b": 2}, 0), {Name: "plain"}}
	h := poolHarness(Options{MaxInflight: AccountCaps(routes)}, "a", "b", "c")
	var (
		mu   sync.Mutex
		cur  = map[string]int{}
		peak = map[string]int{}
		wg   sync.WaitGroup
	)
	for i := range 200 {
		wg.Go(func() {
			var l core.Lease
			var d core.Decision
			if i%2 == 0 {
				l, d, _, _, _ = h.p.AcquireBalanced(core.ClassInteractive, []string{"a", "b"}, nil, routes[0], "")
			} else {
				l, d, _, _, _ = h.p.AcquireCapped(core.ClassInteractive, []string{"a", "c"}, nil)
			}
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
	if peak["a"] > 2 || peak["b"] > 2 || peak["a"] == 0 {
		t.Fatalf("peak = %v", peak)
	}
	if st := h.p.Status("a"); st.Inflight != 0 {
		t.Fatalf("leaked leases: %d", st.Inflight)
	}
}

func TestR1MaxInflightReloadAndValidation(t *testing.T) {
	h := poolHarness(Options{}, "a")
	if _, err := h.p.WithConfig([]core.Account{localAcct("a")}, Options{MaxInflight: map[string]int{"a": -1}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative cap: %v", err)
	}
	held, _ := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	defer held.Release(core.Outcome{})
	caps := map[string]int{"a": 1}
	v, err := h.p.WithConfig([]core.Account{localAcct("a")}, Options{MaxInflight: caps})
	if err != nil {
		t.Fatal(err)
	}
	caps["a"] = 5 // the view cloned the map
	// The old generation's lease counts against the new generation's cap.
	if _, d, denial, _, _ := v.AcquireCapped(core.ClassInteractive, []string{"a"}, nil); d.Allow || denial != PoolDenySaturated {
		t.Fatalf("reloaded cap: %q %+v", denial, d)
	}
}
