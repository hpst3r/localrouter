package policy

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Acceptance tests B3..B8 and B12 for the reachability breaker (docs/SPEC.md
// "Reachability breaker (policy.breaker)").

var (
	dead     = core.Outcome{Unreachable: "dns"}
	answered = core.Outcome{Status: 200}
)

func breakerHarness(threshold int, base, maxD time.Duration, accounts ...core.Account) *harness {
	if len(accounts) == 0 {
		accounts = []core.Account{acct("a", 0)}
	}
	return newHarness(accounts, Options{Breaker: BreakerOptions{Threshold: threshold, Base: base, Max: maxD}})
}

// attempt acquires id and releases it with o, failing the test on denial.
func (h *harness) attempt(t *testing.T, id string, o core.Outcome) {
	t.Helper()
	l, d := h.p.Acquire(core.ClassInteractive, []string{id}, nil)
	if !d.Allow {
		t.Fatalf("acquire %s denied: %s", id, d.Reason)
	}
	l.Release(o)
}

func (h *harness) breaker(t *testing.T, id string) (string, int, time.Time) {
	t.Helper()
	state, fails, until, enabled := h.p.Breaker(id)
	if !enabled {
		t.Fatal("breaker reported disabled")
	}
	return state, fails, until
}

func TestB3CountingRules(t *testing.T) {
	h := breakerHarness(3, 5*time.Second, time.Minute)
	h.attempt(t, "a", dead)
	h.attempt(t, "a", dead)
	// Neither count nor reset: request-scoped answers, client cancellation
	// and other neutral releases (Outcome{}).
	h.attempt(t, "a", core.Outcome{Status: 402, RequestScoped: true})
	h.attempt(t, "a", core.Outcome{})
	if state, fails, _ := h.breaker(t, "a"); state != BreakerClosed || fails != 2 {
		t.Fatalf("state=%s fails=%d, want closed/2", state, fails)
	}
	// Any received status (here a 503) is reachability success: reset.
	h.attempt(t, "a", core.Outcome{Status: 503})
	if _, fails, _ := h.breaker(t, "a"); fails != 0 {
		t.Fatalf("fails=%d after a 503 answer, want 0", fails)
	}
	h.attempt(t, "a", dead)
	h.attempt(t, "a", dead)
	h.attempt(t, "a", dead)
	state, fails, until := h.breaker(t, "a")
	if state != BreakerOpen || fails != 3 || !until.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("state=%s fails=%d until=%v, want open/3/t0+5s", state, fails, until)
	}
}

func TestB3BreakerDisabledIgnoresUnreachable(t *testing.T) {
	h := breakerHarness(0, 0, 0)
	for range 10 {
		h.attempt(t, "a", dead)
	}
	if _, _, _, enabled := h.p.Breaker("a"); enabled {
		t.Fatal("disabled breaker reported enabled")
	}
	if st := h.p.Status("a"); !st.InteractiveAdmissible || st.Reason != "" {
		t.Fatalf("disabled breaker affected status: %+v", st)
	}
}

func TestB4OpenSkipsEveryClassThenOneProbe(t *testing.T) {
	h := breakerHarness(1, 5*time.Second, time.Minute)
	h.attempt(t, "a", dead)
	for _, class := range []core.Class{core.ClassInteractive, core.ClassBackground} {
		l, d := h.p.Acquire(class, []string{"a"}, nil)
		if l != nil || d.Allow || !strings.Contains(d.Reason, "a: upstream unreachable (breaker open until 2026-10-01T12:00:05Z)") {
			t.Fatalf("%s: open account admitted or wrong reason: %+v", class, d)
		}
	}
	if st := h.p.Status("a"); st.InteractiveAdmissible || st.BackgroundAdmissible || !strings.Contains(st.Reason, "breaker open") {
		t.Fatalf("status while open: %+v", st)
	}

	h.clock.Advance(5 * time.Second)
	if state, _, _ := h.breaker(t, "a"); state != BreakerHalfOpen {
		t.Fatalf("state=%s at open-period end, want half_open", state)
	}
	// DryRun never claims the probe.
	if d := h.p.DryRun(core.ClassInteractive, []string{"a"}); !d.Allow {
		t.Fatalf("dry run while half-open: %+v", d)
	}
	probe, d := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow {
		t.Fatalf("probe denied: %s", d.Reason)
	}
	for _, class := range []core.Class{core.ClassInteractive, core.ClassBackground} {
		if _, d := h.p.Acquire(class, []string{"a"}, nil); d.Allow || !strings.Contains(d.Reason, "probe in flight") {
			t.Fatalf("%s: second request admitted while probe in flight: %+v", class, d)
		}
	}
	probe.Release(answered)
}

// Exactly one of many concurrent acquirers wins the half-open probe.
func TestB4ConcurrentProbeIsExclusive(t *testing.T) {
	h := breakerHarness(1, 5*time.Second, time.Minute)
	h.attempt(t, "a", dead)
	h.clock.Advance(5 * time.Second)
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		leases []core.Lease
	)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l, d := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil); d.Allow {
				mu.Lock()
				leases = append(leases, l)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(leases) != 1 {
		t.Fatalf("%d probes admitted, want exactly 1", len(leases))
	}
	leases[0].Release(answered)
}

func TestB5ProbeOutcomes(t *testing.T) {
	h := breakerHarness(1, 5*time.Second, 30*time.Second)
	h.attempt(t, "a", dead) // open 5s
	// Each failed probe re-opens with a doubled period, capped at max.
	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second} {
		_, _, until := h.breaker(t, "a")
		h.clock.Advance(until.Sub(h.clock.Now()))
		h.attempt(t, "a", dead)
		state, _, next := h.breaker(t, "a")
		if state != BreakerOpen || next.Sub(h.clock.Now()) != want {
			t.Fatalf("after failed probe: state=%s period=%v, want open/%v", state, next.Sub(h.clock.Now()), want)
		}
	}
	// A probe that receives headers closes the breaker and resets backoff.
	_, _, until := h.breaker(t, "a")
	h.clock.Advance(until.Sub(h.clock.Now()))
	h.attempt(t, "a", core.Outcome{Status: 500})
	if state, fails, until := h.breaker(t, "a"); state != BreakerClosed || fails != 0 || !until.IsZero() {
		t.Fatalf("after successful probe: %s/%d/%v, want closed", state, fails, until)
	}
	h.attempt(t, "a", dead)
	if _, _, until := h.breaker(t, "a"); until.Sub(h.clock.Now()) != 5*time.Second {
		t.Fatalf("re-open after close used %v, want base 5s", until.Sub(h.clock.Now()))
	}
}

func TestB6CancelledProbeReturnsToHalfOpen(t *testing.T) {
	h := breakerHarness(1, 5*time.Second, time.Minute)
	h.attempt(t, "a", dead)
	h.clock.Advance(5 * time.Second)
	for _, neutral := range []core.Outcome{{}, {Status: 403, RequestScoped: true}} {
		probe, d := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
		if !d.Allow {
			t.Fatalf("probe denied: %s", d.Reason)
		}
		probe.Release(neutral) // client cancelled (or a request-scoped answer)
		if state, _, _ := h.breaker(t, "a"); state != BreakerHalfOpen {
			t.Fatalf("state=%s after neutral probe release, want half_open", state)
		}
	}
	// The next request may probe.
	h.attempt(t, "a", answered)
	if state, _, _ := h.breaker(t, "a"); state != BreakerClosed {
		t.Fatalf("state=%s, want closed", state)
	}
}

// A lease admitted before the breaker opened is not the probe: its success
// does not close an open breaker.
func TestB6StaleLeaseDoesNotCloseOpenBreaker(t *testing.T) {
	h := breakerHarness(1, 5*time.Second, time.Minute)
	old, _ := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	h.attempt(t, "a", dead)
	old.Release(answered)
	if state, _, _ := h.breaker(t, "a"); state != BreakerOpen {
		t.Fatalf("state=%s, want open", state)
	}
}

func TestB7StateSurvivesReload(t *testing.T) {
	accts := []core.Account{acct("a", 0)}
	h := breakerHarness(2, 5*time.Second, time.Minute)
	h.attempt(t, "a", dead)
	h.attempt(t, "a", dead)
	v := mustView(t, h.p, accts, Options{Breaker: BreakerOptions{Threshold: 2, Base: 7 * time.Second, Max: time.Minute}})
	if state, _, until, _ := v.Breaker("a"); state != BreakerOpen || !until.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("new view lost breaker state: %s %v", state, until)
	}
	if _, d := v.Acquire(core.ClassInteractive, []string{"a"}, nil); d.Allow {
		t.Fatal("new view admitted an open account")
	}
	// The reloaded knobs apply to the next transition: a failed probe
	// doubles the current period (5s -> 10s), capped by the new max.
	h.clock.Advance(5 * time.Second)
	l, d := v.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow {
		t.Fatalf("probe denied: %s", d.Reason)
	}
	l.Release(dead)
	if _, _, until, _ := h.p.Breaker("a"); until.Sub(h.clock.Now()) != 10*time.Second {
		t.Fatalf("period %v, want 10s", until.Sub(h.clock.Now()))
	}
	// Disabling by reload makes the account admissible and clears state on
	// the next release.
	off := mustView(t, h.p, accts, Options{})
	l, d = off.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow {
		t.Fatalf("disabled breaker still refuses: %s", d.Reason)
	}
	l.Release(dead)
	on := mustView(t, h.p, accts, Options{Breaker: BreakerOptions{Threshold: 2}})
	if state, fails, _, _ := on.Breaker("a"); state != BreakerClosed || fails != 0 {
		t.Fatalf("re-enabled breaker state %s/%d, want closed/0", state, fails)
	}
}

func TestB7ValidatesBreakerKnobs(t *testing.T) {
	h := breakerHarness(0, 0, 0)
	for _, b := range []BreakerOptions{
		{Threshold: -1},
		{Threshold: 1, Base: -time.Second},
		{Threshold: 1, Max: -time.Second},
		{Threshold: 1, Base: 10 * time.Second, Max: 5 * time.Second},
	} {
		if _, err := h.p.WithConfig([]core.Account{acct("a", 0)}, Options{Breaker: b}); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%+v: err=%v, want ErrInvalidConfig", b, err)
		}
	}
}

// An open account is skipped like a cooled-down one and never adds a
// candidate; the decision names it, and AcquireWithBreaker reports an
// all-breaker denial only when every rejection is the breaker.
func TestB8SelectionSkipsOpenAccounts(t *testing.T) {
	h := breakerHarness(1, 5*time.Second, time.Minute, acct("a", 0), acct("b", 0), acct("c", 0))
	h.attempt(t, "a", dead)
	l, d := h.p.Acquire(core.ClassInteractive, []string{"a", "b"}, nil)
	if !d.Allow || d.AccountID != "b" || !strings.Contains(d.Reason, "skipped a: upstream unreachable") {
		t.Fatalf("decision %+v, want b after skipping a", d)
	}
	l.Release(answered)

	h.clock.Advance(time.Second)
	h.attempt(t, "b", dead) // b opens until t0+6s; a until t0+5s
	_, d, ids, retryIn := h.p.AcquireWithBreaker(core.ClassInteractive, []string{"b", "a"}, nil)
	if d.Allow || strings.Join(ids, ",") != "b,a" || retryIn != 4*time.Second {
		t.Fatalf("all-breaker denial: %+v ids=%v retryIn=%v", d, ids, retryIn)
	}
	// Mixed: c is cooled down by a 429.
	h.attempt(t, "c", core.Outcome{Status: 429})
	_, d, ids, _ = h.p.AcquireWithBreaker(core.ClassInteractive, []string{"a", "c"}, nil)
	if d.Allow || ids != nil || !strings.Contains(d.Reason, "a: upstream unreachable") || !strings.Contains(d.Reason, "c: cooldown") {
		t.Fatalf("mixed denial: %+v ids=%v", d, ids)
	}
	// An excluded candidate is a non-breaker rejection too.
	_, _, ids, _ = h.p.AcquireWithBreaker(core.ClassInteractive, []string{"a", "b"}, map[string]bool{"b": true})
	if ids != nil {
		t.Fatalf("exclude counted as breaker: %v", ids)
	}
}

func TestB12LogsOpenAndClose(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{now: t0}
	p := New([]core.Account{acct("a", 0)}, newFakeQuota(), Options{Clock: clock,
		Logger:  slog.New(slog.NewTextHandler(&buf, nil)),
		Breaker: BreakerOptions{Threshold: 2}})
	for range 2 {
		l, _ := p.Acquire(core.ClassInteractive, []string{"a"}, nil)
		l.Release(core.Outcome{Unreachable: "dns"})
	}
	clock.Advance(5 * time.Second)
	l, _ := p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	l.Release(core.Outcome{Status: 200})
	logs := buf.String()
	if n := strings.Count(logs, "upstream breaker open"); n != 1 ||
		!strings.Contains(logs, `level=WARN msg="upstream breaker open" account=a error_class=dns consecutive_failures=2`) {
		t.Fatalf("want one warn open line, got %d:\n%s", n, logs)
	}
	if n := strings.Count(logs, "upstream breaker closed"); n != 1 ||
		!strings.Contains(logs, `level=INFO msg="upstream breaker closed" account=a error_class=dns`) {
		t.Fatalf("want one info close line, got %d:\n%s", n, logs)
	}
}

// BR-1 (SPEC Amendment R1.1, R1.2): a lease admitted under a generation with
// the breaker disabled is released after a reload enabled it. Transitions use
// the current generation's knobs and the old lease is not the probe, so its
// release leaves the open/half-open breaker and the single probe intact.
func TestBRFixBR1StaleGenerationLeaseKeepsBreaker(t *testing.T) {
	for _, o := range []core.Outcome{{}, answered, dead} {
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
		l.Release(dead)
		old.Release(o) // while open
		if state, _, until, _ := on.Breaker("a"); state != BreakerOpen || !until.Equal(t0.Add(5*time.Second)) {
			t.Fatalf("%+v: breaker %s until %v after a stale release while open, want open until t0+5s", o, state, until)
		}
		h.clock.Advance(5 * time.Second)
		probe, d := on.Acquire(core.ClassInteractive, []string{"a"}, nil)
		if !d.Allow {
			t.Fatalf("probe denied: %s", d.Reason)
		}
		if state, _, _, _ := on.Breaker("a"); state != BreakerHalfOpen {
			t.Errorf("%+v: breaker state %q with a probe in flight, want half_open", o, state)
		}
		if l2, d := on.Acquire(core.ClassInteractive, []string{"a"}, nil); d.Allow {
			l2.Release(core.Outcome{})
			t.Errorf("%+v: second request admitted while the half-open probe is in flight: %s", o, d.Reason)
		}
		probe.Release(dead)
		if state, _, _, _ := on.Breaker("a"); state != BreakerOpen {
			t.Errorf("%+v: breaker state %q after a failed probe, want open", o, state)
		}
	}
}

// BR-1 (R1.5 at the policy level): the original reviewer scenario, where the
// stale lease ends while the probe is in flight.
func TestBRFixBR1StaleLeaseDuringProbe(t *testing.T) {
	accts := []core.Account{acct("a", 0)}
	h := newHarness(accts, Options{})
	old, _ := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	on := mustView(t, h.p, accts, Options{Breaker: BreakerOptions{Threshold: 1, Base: 5 * time.Second, Max: time.Minute}})
	l, _ := on.Acquire(core.ClassInteractive, []string{"a"}, nil)
	l.Release(dead)
	h.clock.Advance(5 * time.Second)
	probe, d := on.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow {
		t.Fatalf("probe denied: %s", d.Reason)
	}
	old.Release(answered)
	if state, _, _, _ := on.Breaker("a"); state != BreakerHalfOpen {
		t.Errorf("breaker state %q after a stale release, want half_open", state)
	}
	if _, d := on.Acquire(core.ClassInteractive, []string{"a"}, nil); d.Allow {
		t.Errorf("second request admitted while the half-open probe is in flight: %s", d.Reason)
	}
	probe.Release(dead)
	if state, _, _, _ := on.Breaker("a"); state != BreakerOpen {
		t.Errorf("breaker state %q after a failed probe, want open", state)
	}
}

// BR-1 (R1.1): a view built by a reload is inert for the breaker until it is
// activated, and then its knobs apply to leases of every view.
func TestBRFixBR1CurrentKnobsApplyToEveryView(t *testing.T) {
	accts := []core.Account{acct("a", 0)}
	h := newHarness(accts, Options{})
	on, err := h.p.WithConfig(accts, Options{Breaker: BreakerOptions{Threshold: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, enabled := on.Breaker("a"); enabled {
		t.Fatal("breaker enabled by a view that was never activated")
	}
	on.Activate()
	h.attempt(t, "a", dead) // through the old (disabled) view
	if state, _, _, _ := on.Breaker("a"); state != BreakerOpen {
		t.Fatalf("state %q after an unreachable release through the old view, want open", state)
	}
	if _, d := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil); d.Allow {
		t.Fatal("old view admitted an open account")
	}
	// Disabling clears state on the next release, but keeps the epoch: a
	// probe from before can never resolve a later open period.
	h.clock.Advance(5 * time.Second)
	probe, _ := on.Acquire(core.ClassInteractive, []string{"a"}, nil)
	mustView(t, h.p, accts, Options{})
	h.attempt(t, "a", core.Outcome{})
	mustView(t, h.p, accts, Options{Breaker: BreakerOptions{Threshold: 1}})
	h.attempt(t, "a", dead) // re-opens: a new epoch
	h.clock.Advance(5 * time.Second)
	probe.Release(answered)
	if state, _, _, _ := on.Breaker("a"); state != BreakerHalfOpen {
		t.Fatalf("state %q after a stale-epoch probe release, want half_open", state)
	}
}

// BR-2 (R1.3): Reached observes reachability when headers arrive. The current
// probe closes the breaker at once, so other requests are admitted while it
// streams; its later Release carries no breaker semantics.
func TestBRFixBR2ReachedClosesProbeBeforeRelease(t *testing.T) {
	h := breakerHarness(1, 5*time.Second, time.Minute)
	h.attempt(t, "a", dead)
	h.clock.Advance(5 * time.Second)
	probe, d := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow {
		t.Fatalf("probe denied: %s", d.Reason)
	}
	probe.(*lease).Reached()
	if state, fails, _ := h.breaker(t, "a"); state != BreakerClosed || fails != 0 {
		t.Fatalf("state=%s fails=%d after the probe reached the upstream, want closed/0", state, fails)
	}
	other, d := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow {
		t.Fatalf("request refused while the successful probe streams: %s", d.Reason)
	}
	other.Release(answered)
	probe.Release(dead) // e.g. the stream broke: not a reachability failure
	if state, fails, _ := h.breaker(t, "a"); state != BreakerClosed || fails != 0 {
		t.Fatalf("state=%s fails=%d, the reached lease's release counted", state, fails)
	}
}

// BR-2 (R1.2, R1.3): Reached on a closed breaker resets the count; on a
// non-probe lease while open it is ignored; after Release it is a no-op.
func TestBRFixBR2ReachedRules(t *testing.T) {
	h := breakerHarness(2, 5*time.Second, time.Minute)
	h.attempt(t, "a", dead)
	l, _ := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	l.(*lease).Reached()
	if _, fails, _ := h.breaker(t, "a"); fails != 0 {
		t.Fatalf("fails=%d after Reached on a closed breaker, want 0", fails)
	}
	l.Release(dead)
	if _, fails, _ := h.breaker(t, "a"); fails != 0 {
		t.Fatalf("fails=%d, a reached lease's release counted", fails)
	}
	early, _ := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	h.attempt(t, "a", dead)
	h.attempt(t, "a", dead)
	early.(*lease).Reached() // admitted before the breaker opened
	if state, _, _ := h.breaker(t, "a"); state != BreakerOpen {
		t.Fatalf("state=%s after Reached on a pre-open lease, want open", state)
	}
	early.Release(answered)
	h.clock.Advance(5 * time.Second)
	probe, _ := h.p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	probe.Release(core.Outcome{}) // cancelled: half-open again
	probe.(*lease).Reached()      // after Release: no-op
	if state, _, _ := h.breaker(t, "a"); state != BreakerHalfOpen {
		t.Fatalf("state=%s after Reached on a released probe, want half_open", state)
	}
}

// FBR-1 (R2.1): one Acquire uses one snapshot of the breaker knobs for both
// selection and lease creation. A reload that enables the breaker between the
// two (Activate does not take p.mu) must not make a lease admitted under the
// disabled knobs the probe of an account that is still open.
func TestFBR1AcquireUsesOneKnobSnapshot(t *testing.T) {
	accts := []core.Account{acct("a", 0)}
	h := breakerHarness(1, 5*time.Second, time.Minute)
	h.attempt(t, "a", dead) // open; epoch 1
	off := mustView(t, h.p, accts, Options{Clock: h.clock})
	on := mustView(t, h.p, accts, Options{Clock: h.clock, Breaker: BreakerOptions{Threshold: 1, Base: 5 * time.Second, Max: time.Minute}})
	off.Activate()
	// Acquire through the disabled view, with the enabling reload landing
	// between selection and lease creation.
	off.mu.Lock()
	sel := off.selectLocked(off.cur.Load(), core.ClassInteractive, []string{"a"}, nil)
	if !sel.dec.Allow {
		off.mu.Unlock()
		t.Fatalf("disabled breaker refused: %s", sel.dec.Reason)
	}
	on.Activate()
	l := off.leaseLocked(sel.id, sel.br)
	probe := h.p.state["a"].br.probe
	off.mu.Unlock()
	if l.probe || probe {
		t.Fatalf("lease admitted under disabled knobs became the probe (lease %v, state %v)", l.probe, probe)
	}
	if state, _, _, _ := on.Breaker("a"); state != BreakerOpen {
		t.Fatalf("state %q, want open", state)
	}
	l.Release(answered) // stale lease: ignored while open
	h.clock.Advance(5 * time.Second)
	p, d := on.Acquire(core.ClassInteractive, []string{"a"}, nil)
	if !d.Allow || !p.(*lease).probe {
		t.Fatalf("half-open account did not admit its probe: %s", d.Reason)
	}
	p.Release(answered)
	if state, _, _, _ := on.Breaker("a"); state != BreakerClosed {
		t.Fatalf("state %q after a successful probe, want closed", state)
	}
}

// FBR-2 (R2.2): a disabled breaker that clears a tripped account logs the
// close, so every open line has a matching close.
func TestFBR2DisabledClearLogsClose(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	clock := &fakeClock{now: t0}
	accts := []core.Account{acct("a", 0)}
	p := New(accts, newFakeQuota(), Options{Clock: clock, Logger: lg, Breaker: BreakerOptions{Threshold: 1}})
	l, _ := p.Acquire(core.ClassInteractive, []string{"a"}, nil)
	l.Release(dead)
	off := mustView(t, p, accts, Options{Clock: clock, Logger: lg})
	for range 2 { // only the clear of a tripped account logs
		l, d := off.Acquire(core.ClassInteractive, []string{"a"}, nil)
		if !d.Allow {
			t.Fatalf("disabled breaker refused: %s", d.Reason)
		}
		l.Release(answered)
	}
	logs := buf.String()
	if n := strings.Count(logs, "upstream breaker closed"); n != 1 ||
		!strings.Contains(logs, `level=INFO msg="upstream breaker closed" account=a error_class=dns reason=disabled`) {
		t.Fatalf("want one disabled close line, got %d:\n%s", n, logs)
	}
}
