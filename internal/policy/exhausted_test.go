package policy

import (
	"net/http"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Interactive must not be admitted on an exhausted window even when the
// provider gate is bypassed because another window has rolled.
func TestExhaustedWindowDeniesInteractive(t *testing.T) {
	no := false
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	q := &staticQuota{snap: core.Snapshot{
		AccountID: "a", FetchedAt: now, Allowed: &no,
		Windows: []core.Window{
			{Kind: core.Window5h, UsedFrac: 1.0, ResetAt: now.Add(-time.Minute)}, // rolled
			{Kind: core.WindowWeekly, UsedFrac: 1.0, ResetAt: now.Add(48 * time.Hour)},
		},
	}}
	p := New([]core.Account{{ID: "a", Provider: core.ProviderCodex}}, q, Options{Clock: fixedClock{now}})
	if d := p.DryRun(core.ClassInteractive, []string{"a"}); d.Allow {
		t.Fatalf("interactive admitted on exhausted weekly window: %+v", d)
	}
}

type staticQuota struct{ snap core.Snapshot }

func (s *staticQuota) Latest(string) (core.Snapshot, bool) { return s.snap, true }
func (s *staticQuota) ObserveHeaders(string, http.Header)  {}
func (s *staticQuota) RequestRefresh(string, bool)         {}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }
