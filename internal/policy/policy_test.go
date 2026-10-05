package policy

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type refreshCall struct {
	id     string
	urgent bool
}

type fakeQuota struct {
	mu        sync.Mutex
	snaps     map[string]core.Snapshot
	refreshes []refreshCall
}

func newFakeQuota() *fakeQuota { return &fakeQuota{snaps: map[string]core.Snapshot{}} }

func (q *fakeQuota) Set(s core.Snapshot) {
	q.mu.Lock()
	q.snaps[s.AccountID] = s
	q.mu.Unlock()
}

func (q *fakeQuota) Latest(id string) (core.Snapshot, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	s, ok := q.snaps[id]
	return s, ok
}

func (q *fakeQuota) ObserveHeaders(string, http.Header) {}

func (q *fakeQuota) RequestRefresh(id string, urgent bool) {
	q.mu.Lock()
	q.refreshes = append(q.refreshes, refreshCall{id, urgent})
	q.mu.Unlock()
}

func (q *fakeQuota) Refreshes() []refreshCall {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]refreshCall(nil), q.refreshes...)
}

func snap(id string, fetched time.Time, ws ...core.Window) core.Snapshot {
	return core.Snapshot{AccountID: id, FetchedAt: fetched, Source: "usage_api", Windows: ws}
}

func w5h(used float64) core.Window {
	return core.Window{Kind: core.Window5h, UsedFrac: used, ResetAt: t0.Add(2 * time.Hour), WindowSeconds: 18000}
}

func acct(id string, reserve5h float64) core.Account {
	a := core.Account{ID: id, Provider: core.ProviderCodex}
	if reserve5h > 0 {
		a.Reserve = map[string]float64{core.Window5h: reserve5h}
	}
	return a
}

func boolp(b bool) *bool { return &b }

type harness struct {
	p     *Policy
	q     *fakeQuota
	clock *fakeClock
}

func newHarness(accounts []core.Account, opts Options) *harness {
	h := &harness{q: newFakeQuota(), clock: &fakeClock{now: t0}}
	opts.Clock = h.clock
	h.p = New(accounts, h.q, opts)
	return h
}

func TestAdmission(t *testing.T) {
	tests := []struct {
		name     string
		account  core.Account
		snap     *core.Snapshot
		opts     Options
		inflight int
		class    core.Class
		allow    bool
		reason   string
	}{
		{
			name:    "AT1 background denied above reserve",
			account: acct("codex-primary", 0.10),
			snap:    ptr(snap("codex-primary", t0, w5h(0.91))),
			opts:    Options{InflightEstimate: 0.01},
			class:   core.ClassBackground,
			allow:   false,
			reason:  "codex-primary: 5h: 9% left; background stops at 11% (reserve 10% + margin 0% + in-flight 1%)",
		},
		{
			name:    "AT1 background allowed below reserve",
			account: acct("codex-primary", 0.10),
			snap:    ptr(snap("codex-primary", t0, w5h(0.85))),
			opts:    Options{InflightEstimate: 0.01},
			class:   core.ClassBackground,
			allow:   true,
		},
		{
			name:    "interactive ignores reserve",
			account: acct("a", 0.10),
			snap:    ptr(snap("a", t0, w5h(0.98))),
			opts:    Options{InflightEstimate: 0.01, SafetyMargin: 0.05},
			class:   core.ClassInteractive,
			allow:   true,
		},
		{
			name:     "interactive denied when exhausted",
			account:  acct("a", 0),
			snap:     ptr(snap("a", t0, w5h(1.0))),
			opts:     Options{InflightEstimate: 0.01},
			inflight: 1,
			class:    core.ClassInteractive,
			allow:    false,
			reason:   "5h window exhausted",
		},
		{
			name:    "safety margin applies to background",
			account: acct("a", 0.10),
			snap:    ptr(snap("a", t0, w5h(0.86))),
			opts:    Options{SafetyMargin: 0.05},
			class:   core.ClassBackground,
			allow:   false,
		},
		{
			name:     "inflight counts against interactive",
			account:  acct("a", 0),
			snap:     ptr(snap("a", t0, w5h(0.95))),
			opts:     Options{InflightEstimate: 0.04},
			inflight: 2,
			class:    core.ClassInteractive,
			allow:    false,
		},
		{
			name:    "weekly reserve checked too",
			account: core.Account{ID: "a", Reserve: map[string]float64{core.WindowWeekly: 0.2}},
			snap: ptr(snap("a", t0, w5h(0.1),
				core.Window{Kind: core.WindowWeekly, UsedFrac: 0.85, ResetAt: t0.Add(72 * time.Hour)})),
			class:  core.ClassBackground,
			allow:  false,
			reason: "weekly: 15% left; background stops at 20%",
		},
		{
			name:    "AT6 reset boundary: rolled window admissible",
			account: acct("a", 0.10),
			snap: ptr(snap("a", t0, core.Window{Kind: core.Window5h, UsedFrac: 0.99,
				ResetAt: t0.Add(-time.Second)})),
			opts:  Options{InflightEstimate: 0.01},
			class: core.ClassBackground,
			allow: true,
		},
		{
			name:    "reset exactly now counts as rolled",
			account: acct("a", 0.10),
			snap:    ptr(snap("a", t0, core.Window{Kind: core.Window5h, UsedFrac: 0.99, ResetAt: t0})),
			class:   core.ClassBackground,
			allow:   true,
		},
		{
			name:    "reset in future not rolled",
			account: acct("a", 0.10),
			snap:    ptr(snap("a", t0, core.Window{Kind: core.Window5h, UsedFrac: 0.99, ResetAt: t0.Add(time.Second)})),
			class:   core.ClassBackground,
			allow:   false,
		},
		{
			name:    "Allowed=false denies interactive",
			account: acct("a", 0),
			snap:    ptr(withAllowed(snap("a", t0, w5h(0.2)), false)),
			class:   core.ClassInteractive,
			allow:   false,
			reason:  "limit reached",
		},
		{
			name:    "Allowed=false ignored once a window rolled",
			account: acct("a", 0),
			snap: ptr(withAllowed(snap("a", t0, core.Window{Kind: core.Window5h, UsedFrac: 1,
				ResetAt: t0.Add(-time.Minute)}), false)),
			class: core.ClassInteractive,
			allow: true,
		},
		{
			name:    "stale reserved denies background",
			account: acct("a", 0.10),
			snap:    ptr(snap("a", t0.Add(-11*time.Minute), w5h(0.1))),
			class:   core.ClassBackground,
			allow:   false,
			reason:  "stale",
		},
		{
			name:    "stale reserved allows interactive",
			account: acct("a", 0.10),
			snap:    ptr(snap("a", t0.Add(-11*time.Minute), w5h(1.0))),
			class:   core.ClassInteractive,
			allow:   true,
		},
		{
			name:    "absent reserved denies background",
			account: acct("a", 0.10),
			class:   core.ClassBackground,
			allow:   false,
		},
		{
			name:    "stale unreserved allows background",
			account: acct("a", 0),
			snap:    ptr(snap("a", t0.Add(-time.Hour), w5h(1.0))),
			class:   core.ClassBackground,
			allow:   true,
		},
		{
			name:    "openai_compat with no snapshot and no reserve allows",
			account: core.Account{ID: "local", Provider: core.ProviderOpenAICompat},
			class:   core.ClassBackground,
			allow:   true,
		},
		{
			name:    "custom StaleAfter respected",
			account: acct("a", 0.10),
			snap:    ptr(snap("a", t0.Add(-2*time.Minute), w5h(0.1))),
			opts:    Options{StaleAfter: time.Minute},
			class:   core.ClassBackground,
			allow:   false,
			reason:  "stale",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness([]core.Account{tc.account}, tc.opts)
			if tc.snap != nil {
				h.q.Set(*tc.snap)
			}
			for i := 0; i < tc.inflight; i++ {
				h.p.state[tc.account.ID].inflight++
			}
			dry := h.p.DryRun(tc.class, []string{tc.account.ID})
			lease, dec := h.p.Acquire(tc.class, []string{tc.account.ID}, nil)
			if dec.Allow != tc.allow || dry.Allow != tc.allow {
				t.Fatalf("allow=%v dry=%v want %v (reason %q)", dec.Allow, dry.Allow, tc.allow, dec.Reason)
			}
			if tc.allow {
				if lease == nil || lease.AccountID() != tc.account.ID || dec.AccountID != tc.account.ID {
					t.Fatalf("bad lease/decision: %v %+v", lease, dec)
				}
			} else if lease != nil {
				t.Fatal("lease returned on deny")
			}
			if tc.reason != "" && !strings.Contains(dec.Reason, tc.reason) {
				t.Errorf("reason %q missing %q", dec.Reason, tc.reason)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func withAllowed(s core.Snapshot, b bool) core.Snapshot {
	s.Allowed = boolp(b)
	return s
}

// Acceptance test 2: 3 concurrent background admits near the floor → exactly 1.
func TestConcurrentAdmitsNearFloor(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		h := newHarness([]core.Account{acct("a", 0.10)}, Options{InflightEstimate: 0.04})
		h.q.Set(snap("a", t0, w5h(0.85)))

		const n = 3
		var (
			start   = make(chan struct{})
			wg      sync.WaitGroup
			mu      sync.Mutex
			allowed int
		)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, dec := h.p.Acquire(core.ClassBackground, []string{"a"}, nil); dec.Allow {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		if allowed != 1 {
			t.Fatalf("iteration %d: allowed=%d want 1", iter, allowed)
		}
		if got := h.p.Status("a").Inflight; got != 1 {
			t.Fatalf("inflight=%d want 1", got)
		}
	}
}

// Acceptance test 3: primary over reserve → background routed to secondary.
func TestSelectionPicksSecondary(t *testing.T) {
	h := newHarness([]core.Account{acct("codex-primary", 0.1), acct("codex-secondary", 0)},
		Options{InflightEstimate: 0.01})
	h.q.Set(snap("codex-primary", t0, w5h(0.91)))
	h.q.Set(snap("codex-secondary", t0, w5h(0.54)))

	lease, dec := h.p.Acquire(core.ClassBackground, []string{"ghost", "codex-primary", "codex-secondary"}, nil)
	if !dec.Allow || lease.AccountID() != "codex-secondary" || dec.AccountID != "codex-secondary" {
		t.Fatalf("got %+v", dec)
	}
	for _, want := range []string{"ghost: unknown account", "codex-primary: 5h: 9% left; background stops at 11%"} {
		if !strings.Contains(dec.Reason, want) {
			t.Errorf("reason %q missing %q", dec.Reason, want)
		}
	}

	// Interactive still prefers primary.
	_, dec = h.p.Acquire(core.ClassInteractive, []string{"codex-primary", "codex-secondary"}, nil)
	if dec.AccountID != "codex-primary" {
		t.Fatalf("interactive got %+v", dec)
	}

	// Exclude skips.
	_, dec = h.p.Acquire(core.ClassInteractive, []string{"codex-primary", "codex-secondary"},
		map[string]bool{"codex-primary": true})
	if dec.AccountID != "codex-secondary" {
		t.Fatalf("exclude got %+v", dec)
	}
}

func TestDenyReasonNamesEachCandidate(t *testing.T) {
	h := newHarness([]core.Account{acct("p", 0.1), acct("s", 0.1)}, Options{})
	h.q.Set(snap("p", t0, w5h(0.95)))
	_, dec := h.p.Acquire(core.ClassBackground, []string{"p", "s", "x"}, nil)
	if dec.Allow {
		t.Fatal("expected deny")
	}
	for _, want := range []string{"p: 5h: 5% left; background stops at 10% (reserve 10%", "s: quota snapshot stale", "x: unknown account"} {
		if !strings.Contains(dec.Reason, want) {
			t.Errorf("reason %q missing %q", dec.Reason, want)
		}
	}
	if dec := h.p.DryRun(core.ClassBackground, nil); dec.Allow || dec.Reason == "" {
		t.Errorf("empty candidates: %+v", dec)
	}
}

func TestLeaseInflightAndDoubleRelease(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0)}, Options{})
	h.q.Set(snap("a", t0, w5h(0.1)))

	l1, _ := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	l2, _ := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if got := h.p.Status("a").Inflight; got != 2 {
		t.Fatalf("inflight=%d want 2", got)
	}
	// DryRun creates no lease.
	h.p.DryRun(core.ClassInteractive, []string{"a"})
	if got := h.p.Status("a").Inflight; got != 2 {
		t.Fatalf("inflight after dry run=%d want 2", got)
	}
	l1.Release(core.Outcome{Status: 200})
	l1.Release(core.Outcome{Status: 200})
	if got := h.p.Status("a").Inflight; got != 1 {
		t.Fatalf("inflight after double release=%d want 1", got)
	}
	l2.Release(core.Outcome{Status: 200})
	if got := h.p.Status("a").Inflight; got != 0 {
		t.Fatalf("inflight=%d want 0", got)
	}
	refs := h.q.Refreshes()
	if len(refs) != 2 || refs[0] != (refreshCall{"a", false}) {
		t.Fatalf("refreshes=%v", refs)
	}
}

func TestReleaseFreesCapacity(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0.10)}, Options{InflightEstimate: 0.04})
	h.q.Set(snap("a", t0, w5h(0.85)))
	l, dec := h.p.Acquire(core.ClassBackground, []string{"a"}, nil)
	if !dec.Allow {
		t.Fatal(dec.Reason)
	}
	if _, dec := h.p.Acquire(core.ClassBackground, []string{"a"}, nil); dec.Allow {
		t.Fatal("second admit should be denied")
	}
	l.Release(core.Outcome{Status: 200})
	if _, dec := h.p.Acquire(core.ClassBackground, []string{"a"}, nil); !dec.Allow {
		t.Fatalf("after release: %s", dec.Reason)
	}
}

func TestCooldown429(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0), acct("b", 0)}, Options{})
	h.q.Set(snap("a", t0, w5h(0.5)))
	h.q.Set(snap("b", t0, w5h(0.5)))

	l, _ := h.p.Acquire(core.ClassInteractive, []string{"a", "b"}, nil)
	l.Release(core.Outcome{Status: 429})

	st := h.p.Status("a")
	if !st.CooldownUntil.Equal(t0.Add(60*time.Second)) || st.InteractiveAdmissible || st.BackgroundAdmissible {
		t.Fatalf("status=%+v", st)
	}
	if !strings.Contains(st.Reason, "cooldown") {
		t.Errorf("reason=%q", st.Reason)
	}
	if refs := h.q.Refreshes(); len(refs) != 1 || refs[0] != (refreshCall{"a", true}) {
		t.Fatalf("refreshes=%v", refs)
	}
	_, dec := h.p.Acquire(core.ClassInteractive, []string{"a", "b"}, nil)
	if dec.AccountID != "b" || !strings.Contains(dec.Reason, "a: cooldown until") {
		t.Fatalf("got %+v", dec)
	}

	// Expires after 60s.
	h.clock.Advance(61 * time.Second)
	if _, dec := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil); !dec.Allow {
		t.Fatalf("after expiry: %s", dec.Reason)
	}
}

func TestCooldownUntilExhaustedReset(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0)}, Options{})
	reset := t0.Add(30 * time.Minute)
	h.q.Set(snap("a", t0, core.Window{Kind: core.Window5h, UsedFrac: 1, ResetAt: reset},
		core.Window{Kind: core.WindowWeekly, UsedFrac: 0.5, ResetAt: t0.Add(5 * time.Minute)}))
	h.p.state["a"].inflight++ // admit bypass: account is exhausted
	l := &lease{p: h.p, id: "a"}
	l.Release(core.Outcome{Status: 429, ResetAt: t0.Add(10 * time.Minute)})
	if got := h.p.Status("a").CooldownUntil; !got.Equal(reset) {
		t.Fatalf("cooldown=%v want %v", got, reset)
	}

	// Outcome.ResetAt wins when later.
	h2 := newHarness([]core.Account{acct("a", 0)}, Options{})
	l2, _ := h2.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	l2.Release(core.Outcome{Status: 429, ResetAt: t0.Add(time.Hour)})
	if got := h2.p.Status("a").CooldownUntil; !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("cooldown=%v", got)
	}
}

func TestCooldownEarlyClear(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0)}, Options{})
	h.q.Set(snap("a", t0, w5h(1.0)))
	h.p.state["a"].inflight++
	(&lease{p: h.p, id: "a"}).Release(core.Outcome{Status: 429, ResetAt: t0.Add(time.Hour)})

	h.clock.Advance(10 * time.Second)
	// Snapshot older than cooldown start: no clear even with headroom.
	h.q.Set(snap("a", t0.Add(-time.Second), w5h(0.2)))
	if h.p.Status("a").InteractiveAdmissible {
		t.Fatal("old snapshot must not clear cooldown")
	}
	// Newer snapshot but still exhausted: no clear.
	h.q.Set(snap("a", t0.Add(5*time.Second), w5h(1.0)))
	if h.p.Status("a").InteractiveAdmissible {
		t.Fatal("exhausted snapshot must not clear cooldown")
	}
	// Newer snapshot with Allowed=false: no clear.
	h.q.Set(withAllowed(snap("a", t0.Add(5*time.Second), w5h(0.2)), false))
	if h.p.Status("a").InteractiveAdmissible {
		t.Fatal("Allowed=false must not clear cooldown")
	}
	// Newer snapshot showing headroom clears.
	h.q.Set(snap("a", t0.Add(5*time.Second), w5h(0.2)))
	st := h.p.Status("a")
	if !st.InteractiveAdmissible || !st.CooldownUntil.IsZero() {
		t.Fatalf("expected cleared: %+v", st)
	}
}

func TestCooldownAuthErrors(t *testing.T) {
	for _, code := range []int{401, 403} {
		h := newHarness([]core.Account{acct("a", 0)}, Options{})
		l, _ := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
		l.Release(core.Outcome{Status: code})
		if got := h.p.Status("a").CooldownUntil; !got.Equal(t0.Add(time.Minute)) {
			t.Fatalf("%d: cooldown=%v", code, got)
		}
		if refs := h.q.Refreshes(); len(refs) != 1 || refs[0].urgent {
			t.Fatalf("%d: refreshes=%v", code, refs)
		}
	}
	// 5xx does not cool down.
	h := newHarness([]core.Account{acct("a", 0)}, Options{})
	l, _ := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	l.Release(core.Outcome{Status: 502})
	if got := h.p.Status("a").CooldownUntil; !got.IsZero() {
		t.Fatalf("502 cooldown=%v", got)
	}
}

func TestStatus(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0.1), acct("b", 0.1)}, Options{InflightEstimate: 0.01})
	h.q.Set(snap("a", t0, w5h(0.95)))

	st := h.p.Status("a")
	if st.Stale || st.BackgroundAdmissible || !st.InteractiveAdmissible || !strings.Contains(st.Reason, "5h: 5% left; background stops at 11%") {
		t.Fatalf("a=%+v", st)
	}
	st = h.p.Status("b")
	if !st.Stale || st.BackgroundAdmissible || !st.InteractiveAdmissible {
		t.Fatalf("b=%+v", st)
	}
	if st := h.p.Status("nope"); st.Reason != "unknown account" || st.InteractiveAdmissible {
		t.Fatalf("unknown=%+v", st)
	}
}

func TestConcurrentAcquireRelease(t *testing.T) {
	h := newHarness([]core.Account{acct("a", 0), acct("b", 0)}, Options{InflightEstimate: 0.001})
	h.q.Set(snap("a", t0, w5h(0.1)))
	h.q.Set(snap("b", t0, w5h(0.1)))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l, dec := h.p.Acquire(core.ClassInteractive, []string{"a", "b"}, nil)
				if dec.Allow {
					_ = h.p.Status(l.AccountID())
					l.Release(core.Outcome{Status: 200})
					l.Release(core.Outcome{Status: 200})
				}
			}
		}()
	}
	wg.Wait()
	if a, b := h.p.Status("a").Inflight, h.p.Status("b").Inflight; a != 0 || b != 0 {
		t.Fatalf("inflight a=%d b=%d", a, b)
	}
}
