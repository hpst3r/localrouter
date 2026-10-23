package policy

import (
	"os"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// TestReviewBR1StaleGenerationLeaseWipesBreaker: a lease admitted under a
// generation with the breaker disabled is released with that generation's
// cfg (lease.p is the old view), so breakerRelease's "disabled" branch wipes
// the breaker that the current, enabled generation opened. That closes an
// open breaker from a lease admitted before it opened (contradicts SPEC B6)
// and, with a probe in flight, lets a second request through half-open.
func TestReviewBR1StaleGenerationLeaseWipesBreaker(t *testing.T) {
	if os.Getenv("LR_REVIEW") == "" {
		t.Skip("BUG BR-1: lease from a breaker-disabled generation wipes the enabled breaker on release")
	}
	accts := []core.Account{acct("a", 0)}
	h := newHarness(accts, Options{}) // generation 1: breaker disabled
	// A long-running request (e.g. a multi-minute stream) admitted before
	// the reload that enables the breaker.
	old, d := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow {
		t.Fatalf("acquire: %s", d.Reason)
	}
	on := mustView(t, h.p, accts, Options{Breaker: BreakerOptions{Threshold: 1, Base: 5 * time.Second, Max: time.Minute}})
	l, d := on.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow {
		t.Fatalf("acquire: %s", d.Reason)
	}
	l.Release(core.Outcome{Unreachable: "dial"})
	h.clock.Advance(5 * time.Second)
	probe, d := on.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow {
		t.Fatalf("probe denied: %s", d.Reason)
	}
	// The pre-reload lease ends neutrally (client cancelled).
	old.Release(core.Outcome{})
	if state, _, _, _ := on.Breaker("a"); state != BreakerHalfOpen {
		t.Errorf("breaker state %q after a stale neutral release, want half_open", state)
	}
	if l2, d := on.Acquire(core.ClassInteractive, []string{"a"}, nil); d.Allow {
		l2.Release(core.Outcome{})
		t.Errorf("second request admitted while the half-open probe is in flight: %s", d.Reason)
	}
	probe.Release(core.Outcome{Unreachable: "dial"})
	if state, _, _, _ := on.Breaker("a"); state != BreakerOpen {
		t.Errorf("breaker state %q after a failed probe, want open", state)
	}
}
