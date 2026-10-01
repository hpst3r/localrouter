package quota

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// A header observation that updates UsedFrac without a reset-after header
// keeps the window's old ResetAt. If that ResetAt is already in the past the
// policy treats the window as rolled (used = 0), discarding the fresh sample.
func TestReviewHeaderMergeKeepsPastResetAt(t *testing.T) {
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`))
	})
	h.m.refresh(context.Background(), "cx", true)
	h.clock.Advance(2 * time.Hour) // primary ResetAt (t0+1h) is now in the past
	hdr := http.Header{}
	hdr.Set("x-codex-primary-used-percent", "95")
	h.m.ObserveHeaders("cx", hdr)

	s, _ := h.m.Latest("cx")
	w := findWindow(t, s, core.Window5h)
	now := h.clock.Now()
	if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
		t.Fatalf("fresh used=%.2f observed at %s but ResetAt=%s is past: window looks rolled", w.UsedFrac, now, w.ResetAt)
	}
}

// A usage-API fetch that started before a header observation must not
// overwrite the newer header data when it completes.
func TestReviewSlowFetchOverwritesNewerHeaders(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":50,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`))
	})
	done := make(chan struct{})
	go func() { h.m.refresh(context.Background(), "cx", true); close(done) }()
	<-entered
	h.clock.Advance(time.Second)
	hdr := http.Header{}
	hdr.Set("x-codex-primary-used-percent", "95")
	hdr.Set("x-codex-primary-reset-after-seconds", "3500")
	h.m.ObserveHeaders("cx", hdr)
	close(release)
	<-done

	s, _ := h.m.Latest("cx")
	if w := findWindow(t, s, core.Window5h); w.UsedFrac < 0.95 {
		t.Fatalf("newer header sample (0.95) replaced by older fetch: used=%.2f fetched_at=%s", w.UsedFrac, s.FetchedAt)
	}
}

// When the usage API has never succeeded, a primary-only header sample makes
// the snapshot look fresh even though the weekly window is entirely unknown,
// so the policy stops treating the reserved account as stale.
func TestReviewHeadersFreshenSnapshotWithUnknownWeekly(t *testing.T) {
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h.m.refresh(context.Background(), "cx", true)
	hdr := http.Header{}
	hdr.Set("x-codex-primary-used-percent", "10")
	h.m.ObserveHeaders("cx", hdr)

	s, _ := h.m.Latest("cx")
	hasWeekly := false
	for _, w := range s.Windows {
		hasWeekly = hasWeekly || w.Kind == core.WindowWeekly
	}
	if !s.FetchedAt.IsZero() && !hasWeekly {
		t.Fatalf("snapshot fresh (fetched_at=%s) but weekly window unknown: %+v", s.FetchedAt, s.Windows)
	}
}
