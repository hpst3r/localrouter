package discovery

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// snapshot returns an account's ids and last success time.
func (r *rig) snapshot(id string) ([]string, time.Time) {
	r.m.mu.RLock()
	defer r.m.mu.RUnlock()
	if e := r.m.targets[id]; e != nil {
		return e.sorted, e.lastSuccess
	}
	return nil, time.Time{}
}

// TestFMC1BaseURLChangeDropsStaleListing (Amendment R2 item 1): a reload that
// re-points an account at a different base_url drops the listing fetched from
// the old server and polls the new one on the next pass, so routing never
// narrows on ids the new server never listed.
func TestFMC1BaseURLChangeDropsStaleListing(t *testing.T) {
	r := newRig(t, Options{}, "lab")
	r.ups["lab"].set(listingBody("old-model"))
	rt := discRoute("lab", time.Hour, "lab")
	r.target(rt)
	r.pollDue()
	if got := r.listing("lab"); !slices.Equal(got, []string{"old-model"}) {
		t.Fatalf("setup: listing = %v", got)
	}

	// Reload: same account id, different server that lists only new-model.
	other := newUpstream(t)
	other.set(listingBody("new-model"))
	acct := r.accts["lab"]
	acct.BaseURL = other.srv.URL + "/v1"
	r.accts["lab"] = acct
	r.target(rt)
	if ids, last := r.snapshot("lab"); len(ids) != 0 || !last.IsZero() {
		t.Errorf("right after the retarget: ids %v lastSuccess %v, want an empty pending entry", ids, last)
	}
	r.clock.Add(time.Second) // next loop pass, well inside the interval
	r.pollDue()

	ids := r.listing("lab")
	if !slices.Equal(ids, []string{"new-model"}) {
		t.Errorf("after base_url change, listing = %v, want [new-model] (new server polled %d times)", ids, other.calls.Load())
	}
	res := r.m.Resolver([]core.Route{rt})
	if _, ok, _ := res.Resolve("old-model"); ok {
		t.Errorf("old-model still resolves to the re-pointed account")
	}

	// A change that does not affect the listing (reserve) keeps it and the
	// schedule.
	calls := other.calls.Load()
	acct.Reserve = map[string]float64{"5h": 0.2}
	r.accts["lab"] = acct
	r.target(rt)
	r.clock.Add(time.Second)
	r.pollDue()
	if ids := r.listing("lab"); !slices.Equal(ids, []string{"new-model"}) || other.calls.Load() != calls {
		t.Errorf("reserve change: listing %v, calls %d→%d; want listing kept and no poll", ids, calls, other.calls.Load())
	}
}

// TestFMC1InFlightPollAcrossBaseURLChangeDiscarded (Amendment R2 item 1): a
// poll of the old base_url still in flight when the reload re-points the
// account must not store its result.
func TestFMC1InFlightPollAcrossBaseURLChangeDiscarded(t *testing.T) {
	r := newRig(t, Options{}, "lab")
	started, gate := make(chan struct{}), make(chan struct{})
	r.ups["lab"].set(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-gate
		listingBody("old-model")(w, nil)
	})
	rt := discRoute("lab", time.Hour, "lab")
	r.target(rt)
	done := make(chan struct{})
	go func() { defer close(done); r.pollDue() }()
	<-started

	other := newUpstream(t)
	other.set(listingBody("new-model"))
	acct := r.accts["lab"]
	acct.BaseURL = other.srv.URL + "/v1"
	r.accts["lab"] = acct
	r.target(rt)
	close(gate)
	<-done

	if ids, last := r.snapshot("lab"); len(ids) != 0 || !last.IsZero() {
		t.Fatalf("stale in-flight poll stored its result: ids %v lastSuccess %v", ids, last)
	}
	r.m.PollDue(context.Background())
	if ids := r.listing("lab"); !slices.Equal(ids, []string{"new-model"}) {
		t.Errorf("after the stale poll, listing = %v, want [new-model]", ids)
	}
}
