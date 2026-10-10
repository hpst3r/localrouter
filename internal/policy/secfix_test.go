package policy

import (
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// A request-scoped failure releases the lease without any cooldown and
// without an urgent (gap-bypassing) quota refresh, for every status that
// would otherwise cool the account down.
func TestSecRequestScopedNoCooldown(t *testing.T) {
	for _, tc := range []struct {
		a      core.Account
		status int
	}{
		{orAcct("x"), 402},
		{orAcct("x"), 403},
		{orAcct("x"), 429},
		{acct("x", 0), 401},
		{acct("x", 0), 429},
	} {
		h := newHarness([]core.Account{tc.a}, Options{})
		h.q.Set(orSnap("x", credits(5, t0), nil))
		l, d := h.p.Acquire(core.ClassBackground, []string{"x"}, nil)
		if !d.Allow {
			t.Fatalf("%s %d: denied %s", tc.a.Provider, tc.status, d.Reason)
		}
		if got := h.p.Status("x").Inflight; got != 1 {
			t.Fatalf("inflight %d", got)
		}
		l.Release(core.Outcome{Status: tc.status, ResetAt: t0.Add(3600e9), RequestScoped: true})
		st := h.p.Status("x")
		if !st.CooldownUntil.IsZero() || !st.InteractiveAdmissible || st.Inflight != 0 {
			t.Fatalf("%s %d: %+v", tc.a.Provider, tc.status, st)
		}
		for _, r := range h.q.Refreshes() {
			if r.urgent {
				t.Fatalf("%s %d: urgent refresh", tc.a.Provider, tc.status)
			}
		}
	}
}
