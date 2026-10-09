package policy

// Generation-scoped configuration views (hot reload). A view carries one
// immutable configuration generation (accounts + knobs) while sharing the
// receiver's runtime state: the inflight counters and cooldowns. The frozen
// API is Policy.WithConfig(accounts, opts) (*Policy, error); a view that
// violates topology or validation is refused and nothing is mutated.

import (
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func mustView(t *testing.T, p *Policy, accounts []core.Account, opts Options) *Policy {
	t.Helper()
	v, err := p.WithConfig(accounts, opts)
	if err != nil {
		t.Fatalf("WithConfig: %v", err)
	}
	if v == nil {
		t.Fatal("WithConfig returned nil view")
	}
	return v
}

// A new generation view must observe the old generation's live counters and
// cooldowns, because runtime state is persistent across reloads.
func TestWithConfigViewsShareRuntimeState(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0.10)}, Options{InflightEstimate: 0.01})

	l1, d1 := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d1.Allow {
		t.Fatalf("first acquire: %s", d1.Reason)
	}
	l2, d2 := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d2.Allow {
		t.Fatalf("second acquire: %s", d2.Reason)
	}

	nv := mustView(t, h.p, []core.Account{acct("a", 0.10)}, Options{InflightEstimate: 0.02})
	if got := nv.Status("a").Inflight; got != 2 {
		t.Fatalf("new view inflight = %d, want 2 (counters must be shared)", got)
	}
	if got := h.p.Status("a").Inflight; got != 2 {
		t.Fatalf("old view inflight = %d, want 2", got)
	}

	// A cooldown raised through the old view is visible through the new one.
	l1.Release(core.Outcome{Status: 429})
	want := t0.Add(time.Minute)
	if got := nv.Status("a").CooldownUntil; !got.Equal(want) {
		t.Fatalf("new view cooldown = %v, want %v (cooldowns must be shared)", got, want)
	}
	if got := h.p.Status("a").CooldownUntil; !got.Equal(want) {
		t.Fatalf("old view cooldown = %v, want %v", got, want)
	}
	l2.Release(core.Outcome{Status: 200})
}

// WithConfig must reject account topology changes and invalid knobs/reserves,
// returning an error and mutating nothing.
func TestWithConfigValidation(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0.1), acct("b", 0.1)}, Options{})

	cases := []struct {
		name     string
		accounts []core.Account
		opts     Options
	}{
		{"missing account", []core.Account{acct("a", 0.1)}, Options{}},
		{"extra account", []core.Account{acct("a", 0.1), acct("b", 0.1), acct("c", 0)}, Options{}},
		{"renamed account", []core.Account{acct("a", 0.1), acct("z", 0.1)}, Options{}},
		{"empty id", []core.Account{{ID: ""}, acct("b", 0.1)}, Options{}},
		{"duplicate id", []core.Account{acct("a", 0.1), acct("a", 0.1)}, Options{}},
		{"bad reserve key", []core.Account{{ID: "a", Reserve: map[string]float64{"1h": 0.1}}, acct("b", 0.1)}, Options{}},
		{"reserve at one", []core.Account{{ID: "a", Reserve: map[string]float64{core.Window5h: 1.0}}, acct("b", 0.1)}, Options{}},
		{"negative reserve", []core.Account{{ID: "a", Reserve: map[string]float64{core.Window5h: -0.1}}, acct("b", 0.1)}, Options{}},
		{"negative margin", []core.Account{acct("a", 0.1), acct("b", 0.1)}, Options{SafetyMargin: -0.1}},
		{"estimate at one", []core.Account{acct("a", 0.1), acct("b", 0.1)}, Options{InflightEstimate: 1.0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v, err := h.p.WithConfig(tc.accounts, tc.opts); err == nil {
				t.Fatalf("expected error, got view %v", v)
			} else if v != nil {
				t.Fatalf("view returned alongside error: %v", v)
			}
		})
	}

	// Unchanged topology with valid knobs succeeds.
	if _, err := h.p.WithConfig([]core.Account{acct("a", 0.1), acct("b", 0.1)}, Options{InflightEstimate: 0.01}); err != nil {
		t.Fatalf("valid same-topology WithConfig: %v", err)
	}
}

// A lease taken from an old view keeps its slot: releasing it decrements the
// shared counter exactly once, and the new view is the one that observes it.
func TestWithConfigOldLeaseReleaseSharedOnce(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0)}, Options{})
	l, dec := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !dec.Allow {
		t.Fatal(dec.Reason)
	}
	nv := mustView(t, h.p, []core.Account{acct("a", 0)}, Options{})
	if got := nv.Status("a").Inflight; got != 1 {
		t.Fatalf("new view inflight = %d, want 1", got)
	}

	l.Release(core.Outcome{Status: 200})
	l.Release(core.Outcome{Status: 200}) // idempotent across views
	if got := nv.Status("a").Inflight; got != 0 {
		t.Fatalf("after double release inflight = %d, want 0", got)
	}
	if got := h.p.Status("a").Inflight; got != 0 {
		t.Fatalf("old view inflight = %d, want 0", got)
	}
}

// New limits/reserves take effect on the new view while the old view keeps its
// own configuration, yet both see the same saturated in-flight counter.
func TestWithConfigLimitsDifferSharedActive(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0.10)}, Options{InflightEstimate: 0.01})
	h.q.Set(snap("a", t0, w5h(0.85)))

	l, dec := h.p.Acquire(core.ClassBackground, []string{"a"}, nil)
	if !dec.Allow {
		t.Fatalf("old view should admit one background request: %s", dec.Reason)
	}

	// A larger reserve makes the same in-flight load saturating.
	nv := mustView(t, h.p, []core.Account{acct("a", 0.20)}, Options{InflightEstimate: 0.01})
	if got := nv.Status("a").BackgroundAdmissible; got {
		t.Fatalf("new view admitted background while an old lease was active under a tighter reserve")
	}
	// The original view must be untouched: WithConfig never mutates it.
	if !h.p.Status("a").BackgroundAdmissible {
		t.Fatalf("original view was mutated by WithConfig: %s", h.p.Status("a").Reason)
	}
	l.Release(core.Outcome{Status: 200})
}

// The view must clone its inputs so later caller mutation cannot change it.
func TestWithConfigClonesInputs(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0.10)}, Options{InflightEstimate: 0.01})

	res := map[string]float64{core.Window5h: 0.20}
	accts := []core.Account{{ID: "a", Provider: core.ProviderCodex, Reserve: res}}
	nv := mustView(t, h.p, accts, Options{InflightEstimate: 0.01})

	// Mutate the caller's slice and map after the view was built.
	accts[0].ID = "mutated"
	res[core.Window5h] = 0.0

	h.q.Set(snap("a", t0, w5h(0.85)))
	if got := nv.Status("a").BackgroundAdmissible; got {
		t.Fatalf("view did not isolate its reserve from caller mutation: %s", nv.Status("a").Reason)
	}
	if got := nv.Status("mutated").Reason; got != "unknown account" {
		t.Fatalf("view kept a live reference to the caller slice: %q", got)
	}
}

// Many concurrent views over shared runtime state must not race and must
// balance to zero. Run with -race.
func TestWithConfigViewsConcurrent(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0), acct("b", 0)}, Options{InflightEstimate: 0.001})
	h.q.Set(snap("a", t0, w5h(0.1)))
	h.q.Set(snap("b", t0, w5h(0.1)))

	views := []*Policy{h.p}
	for i := 0; i < 3; i++ {
		views = append(views, mustView(t, h.p, []core.Account{acct("a", 0), acct("b", 0)}, Options{InflightEstimate: 0.001}))
	}

	var wg sync.WaitGroup
	for w := 0; w < 24; w++ {
		v := views[w%len(views)]
		wg.Add(1)
		go func(v *Policy) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l, dec := v.Acquire(core.ClassInteractive, []string{"a", "b"}, nil)
				if dec.Allow {
					_ = v.Status(l.AccountID())
					l.Release(core.Outcome{Status: 200})
					l.Release(core.Outcome{Status: 200})
				}
				_ = v.DryRun(core.ClassInteractive, []string{"a", "b"})
			}
		}(v)
	}
	wg.Wait()

	for i, v := range views {
		if a, b := v.Status("a").Inflight, v.Status("b").Inflight; a != 0 || b != 0 {
			t.Fatalf("view %d leaked inflight a=%d b=%d", i, a, b)
		}
	}
}
