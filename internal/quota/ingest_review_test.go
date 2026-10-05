package quota

// Adversarial review tests; skipped until fixed. Run with LOCALROUTER_REVIEW=1
// to see them fail.

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func reviewBug(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("LOCALROUTER_REVIEW") == "" {
		t.Skip("BUG: " + msg)
	}
}

// Polled snapshots pass every fraction through clampFrac; ingested ones are
// stored verbatim. A pushed used_frac of -3 makes policy compute far more
// headroom than exists (background admitted on a reserved account), and 7
// shows as -600% remaining in status.
func TestReviewIngestSnapshotClampsFractions(t *testing.T) {
	reviewBug(t, "IngestSnapshot stores UsedFrac outside [0,1] unclamped")
	m := New([]core.Account{{ID: "cl", Provider: core.ProviderClaude, QuotaSource: QuotaSourceAgent}}, nil, Options{})
	err := m.IngestSnapshot(core.Snapshot{AccountID: "cl", FetchedAt: t0, Windows: []core.Window{
		{Kind: core.Window5h, UsedFrac: -3}, {Kind: core.WindowWeekly, UsedFrac: 7},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := m.Latest("cl")
	for _, w := range got.Windows {
		if w.UsedFrac < 0 || w.UsedFrac > 1 {
			t.Errorf("window %s stored used_frac %v", w.Kind, w.UsedFrac)
		}
	}
}

// Race probe (passes today; kept with the review tests so it runs under
// LOCALROUTER_REVIEW=1 -race): concurrent ingest / Latest / refresh paths on
// agent and polled accounts.
func TestReviewIngestConcurrentWithLatestAndRefresh(t *testing.T) {
	if os.Getenv("LOCALROUTER_REVIEW") == "" {
		t.Skip("review probe (no bug found); run with LOCALROUTER_REVIEW=1 -race")
	}
	m := New([]core.Account{
		{ID: "cl", Provider: core.ProviderClaude, QuotaSource: QuotaSourceAgent},
		{ID: "cx", Provider: core.ProviderCodex},
	}, nil, Options{Clock: &fakeClock{t: t0}})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				_ = m.IngestSnapshot(core.Snapshot{AccountID: "cl", FetchedAt: t0.Add(time.Duration(i*1000+j) * time.Millisecond),
					Windows: []core.Window{{Kind: core.Window5h, UsedFrac: 0.5}}, ModelRequests: map[string][]core.ModelCount{"5h": {{Model: "m", Requests: 1}}}})
				if s, ok := m.Latest("cl"); ok {
					s.Windows[0].UsedFrac = 9 // must not affect stored copy
					s.ModelRequests["5h"][0].Requests = 99
				}
				m.RequestRefresh("cl", true)
				m.ObserveHeaders("cl", nil)
			}
		})
	}
	wg.Wait()
	m.wait()
	s, _ := m.Latest("cl")
	if s.Windows[0].UsedFrac != 0.5 || s.ModelRequests["5h"][0].Requests != 1 {
		t.Fatalf("Latest returned shared state: %+v", s)
	}
}
