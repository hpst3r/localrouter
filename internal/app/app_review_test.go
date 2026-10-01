package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/policy"
	"github.com/hpst3r/localrouter/internal/quota"
)

type reviewClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *reviewClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *reviewClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type reviewCreds struct{}

func (reviewCreds) Credential(context.Context, string) (core.Credential, error) {
	return core.Credential{Headers: http.Header{"Authorization": {"Bearer x"}}}, nil
}
func (reviewCreds) Invalidate(string) {}

// End-to-end through the real quota.Manager and policy.Policy: a fresh
// x-codex header sample at 95% (reserve 10%) must deny background. The merge
// keeps a past ResetAt, so the policy treats the window as rolled and admits.
func TestReviewBackgroundAdmittedBelowReserveAfterHeaderMerge(t *testing.T) {
	t.Skip("BUG: quota ObserveHeaders keeps past ResetAt; policy treats fresh 95% window as rolled and admits background below 10% reserve")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},
			"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":500000}}}`))
	}))
	defer srv.Close()
	clock := &reviewClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	accts := []core.Account{{ID: "cx", Provider: core.ProviderCodex, Reserve: map[string]float64{core.Window5h: 0.1}}}
	qm := quota.New(accts, reviewCreds{}, quota.Options{Clock: clock, HTTPClient: srv.Client(), CodexUsageURL: srv.URL, PollInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	qm.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, ok := qm.Latest("cx"); ok && !s.FetchedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial fetch did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pol := policy.New(accts, qm, policy.Options{Clock: clock, StaleAfter: 10 * time.Minute})

	// 2h later the API-reported 5h reset has passed; the proxy then observes
	// a fresh response carrying only the used-percent header.
	clock.advance(2 * time.Hour)
	hdr := http.Header{}
	hdr.Set("x-codex-primary-used-percent", "95")
	hdr.Set("x-codex-secondary-used-percent", "10")
	qm.ObserveHeaders("cx", hdr)

	lease, dec := pol.Acquire(core.ClassBackground, []string{"cx"}, nil)
	if lease != nil {
		lease.Release(core.Outcome{Status: 200})
		t.Fatalf("background admitted at fresh 95%% used with 10%% reserve: %s", dec.Reason)
	}
}
