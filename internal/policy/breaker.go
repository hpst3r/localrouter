package policy

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	defaultBreakerBase = 5 * time.Second
	defaultBreakerMax  = 60 * time.Second
)

// Breaker states reported by Policy.Breaker.
const (
	BreakerClosed   = "closed"
	BreakerOpen     = "open"
	BreakerHalfOpen = "half_open"
)

// BreakerOptions configures the per-account reachability breaker
// (policy.breaker). Threshold consecutive unreachable attempts (an Outcome
// with Unreachable set) open the breaker for Base; each re-open after a failed
// probe doubles the period up to Max. Threshold 0 disables the breaker.
// Base and Max default to 5s and 60s when the breaker is enabled.
type BreakerOptions struct {
	Threshold int
	Base      time.Duration
	Max       time.Duration
}

func (b BreakerOptions) enabled() bool { return b.Threshold > 0 }

// breakerState is the persistent reachability bookkeeping of one account. It
// lives in accountState, so it is shared by every configuration view and
// survives reloads like cooldowns do. It is the single source of truth for
// the breaker: admission, Breaker, Status and the open/close log lines all
// derive from it.
type breakerState struct {
	fails   int           // consecutive unreachable attempts
	tripped bool          // open or half-open (not closed)
	until   time.Time     // end of the current open period; half-open from then
	period  time.Duration // length of the current open period
	probe   bool          // the single half-open probe lease is in flight
	class   string        // error class of the last unreachable attempt
	// epoch counts transitions into open. It is never reset (not even when
	// a disabled breaker clears the rest), so a probe lease from an earlier
	// open period can never resolve a later one.
	epoch uint64
}

// breakerOpts returns the breaker knobs of the current generation (the view
// most recently activated). Every view applies them: breaker state is shared,
// so it must not depend on the generation that admitted a lease.
func (p *Policy) breakerOpts() BreakerOptions { return *p.brOpts.Load() }

// Activate makes this view's breaker knobs the current ones for every view
// sharing its runtime state (New activates its own). The app calls it when it
// installs the generation, so a view built by a reload that later fails never
// takes effect. Admission, Breaker, Status and lease outcomes all use the
// activated knobs, whichever view they go through.
func (p *Policy) Activate() {
	b := p.cur.Load().opts.Breaker
	p.brOpts.Store(&b)
}

// breakerBlocks reports whether the breaker refuses the account at now, the
// reason, and when it half-opens (already passed while a probe is in flight).
func breakerBlocks(br BreakerOptions, b *breakerState, now time.Time) (bool, string, time.Time) {
	if !br.enabled() || !b.tripped {
		return false, "", time.Time{}
	}
	if now.Before(b.until) {
		return true, "upstream unreachable (breaker open until " + b.until.UTC().Format(time.RFC3339) + ")", b.until
	}
	if b.probe {
		return true, "upstream unreachable (breaker half-open, probe in flight)", b.until
	}
	return false, "", time.Time{}
}

// breakerObserve applies one lease's reachability observation to b under the
// current knobs br: unreachable (an error class) or reached (response headers
// arrived and the answer is not request-scoped); neither is a neutral end.
// Only the current epoch's probe lease resolves a tripped breaker; any other
// lease changes b only while it is closed. A disabled breaker clears b,
// keeping the epoch (closing it if it was tripped). It reports whether the
// breaker opened (or re-opened) or closed. p.mu must be held.
func breakerObserve(br BreakerOptions, b *breakerState, l *lease, unreachable string, reached bool, now time.Time) (opened, closed bool) {
	if !br.enabled() {
		closed = b.tripped
		*b = breakerState{epoch: b.epoch}
		return false, closed
	}
	switch {
	case l.probe && b.tripped && b.probe && l.epoch == b.epoch:
		// Whatever happens, this probe no longer holds the half-open slot;
		// a neutral end (client cancellation, credential failure,
		// request-scoped answer) leaves the account half-open.
		b.probe = false
		switch {
		case unreachable != "":
			b.fails++
			b.class = unreachable
			b.period = min(max(b.period*2, br.Base), br.Max)
		case reached:
			*b = breakerState{epoch: b.epoch}
			return false, true
		default:
			return false, false
		}
	case b.tripped || l.probe:
		// A lease admitted before this open period, or the probe of an
		// earlier one, says nothing about the current period.
		return false, false
	case reached:
		b.fails = 0
		return false, false
	case unreachable != "":
		b.fails++
		b.class = unreachable
		if b.fails < br.Threshold {
			return false, false
		}
		b.tripped, b.period = true, br.Base
	default:
		return false, false
	}
	b.epoch++
	b.until = now.Add(b.period)
	return true, false
}

// breakerEvent is a breaker transition to log once p.mu is released.
type breakerEvent struct {
	opened, closed bool
	class          string // error class: of this failure, or the last one on close
	fails          int
	until          time.Time
	reason         string // why it closed, when not a probe ("disabled")
}

// observeLocked applies lease l's observation (see breakerObserve) to its
// account under the current knobs. A lease observes reachability at most
// once: after Reached, its release carries no breaker semantics. p.mu must be
// held.
func (p *Policy) observeLocked(l *lease, unreachable string, reached bool, now time.Time) breakerEvent {
	if l.reached {
		return breakerEvent{}
	}
	l.reached = reached
	b := &p.state[l.id].br
	ev := breakerEvent{class: b.class}
	br := p.breakerOpts()
	ev.opened, ev.closed = breakerObserve(br, b, l, unreachable, reached, now)
	if ev.closed && !br.enabled() {
		ev.reason = "disabled"
	}
	if ev.opened {
		ev.class, ev.fails, ev.until = b.class, b.fails, b.until
	}
	return ev
}

// log emits the open (warn) or close (info) line of a transition.
func (e breakerEvent) log(lg *slog.Logger, id string) {
	switch {
	case e.opened:
		lg.Warn("upstream breaker open", "account", id, "error_class", e.class,
			"consecutive_failures", e.fails, "until", e.until)
	case e.closed && e.reason != "":
		lg.Info("upstream breaker closed", "account", id, "error_class", e.class, "reason", e.reason)
	case e.closed:
		lg.Info("upstream breaker closed", "account", id, "error_class", e.class)
	}
}

// Reached records that the lease's upstream answered with response headers:
// reachability is proven. The proxy calls it as soon as the headers of a
// non-request-scoped answer arrive, before relaying the body, so the current
// half-open probe closes the breaker (freeing the account for other requests)
// without waiting for a long stream to end, and a lease on a closed breaker
// resets the consecutive count. The lease's later Release still ends it and
// applies cooldowns but no longer changes the breaker. Calls after the first,
// or after Release, are no-ops.
func (l *lease) Reached() {
	p := l.p
	p.mu.Lock()
	if l.released {
		p.mu.Unlock()
		return
	}
	// A reached observation never opens the breaker, so it needs no clock.
	ev := p.observeLocked(l, "", true, time.Time{})
	p.mu.Unlock()
	ev.log(p.cur.Load().opts.Logger, l.id)
}

// Breaker reports the reachability breaker of one account: its state
// (BreakerClosed, BreakerOpen or BreakerHalfOpen), consecutive unreachable
// attempts, and the end of the open period (zero when closed). enabled is
// false, and the other results are zero, when policy.breaker is disabled or
// the account is unknown.
func (p *Policy) Breaker(accountID string) (state string, consecutiveFailures int, openUntil time.Time, enabled bool) {
	c := p.cur.Load()
	if !p.breakerOpts().enabled() {
		return "", 0, time.Time{}, false
	}
	if _, ok := c.accounts[accountID]; !ok {
		return "", 0, time.Time{}, false
	}
	now := c.opts.Clock.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.state[accountID].br
	switch {
	case !b.tripped:
		return BreakerClosed, b.fails, time.Time{}, true
	case now.Before(b.until):
		return BreakerOpen, b.fails, b.until, true
	default:
		return BreakerHalfOpen, b.fails, b.until, true
	}
}

// AcquireWithBreaker is Acquire that also explains a breaker refusal. When
// the request is denied and every rejected candidate was refused by the
// reachability breaker, unreachable lists those accounts (in candidate order)
// and retryIn is the time until the earliest of them half-opens (zero or
// negative when one is already half-open with a probe in flight). Otherwise
// unreachable is nil, and a mixed denial (including one where a candidate is
// at its Options.MaxInflight cap) still names the breaker accounts in the
// decision reason.
func (p *Policy) AcquireWithBreaker(class core.Class, candidates []string, exclude map[string]bool) (l core.Lease, dec core.Decision, unreachable []string, retryIn time.Duration) {
	c := p.cur.Load()
	p.mu.Lock()
	defer p.mu.Unlock()
	sel := p.selectLocked(c, class, candidates, exclude)
	if !sel.dec.Allow {
		c.opts.Logger.Info("policy denied", "class", class, "reason", sel.dec.Reason)
		if sel.onlyBreaker() {
			return nil, sel.dec, sel.unreachable, sel.retryAt.Sub(sel.now)
		}
		return nil, sel.dec, nil, 0
	}
	return p.leaseLocked(sel.id, sel.br), sel.dec, nil, 0
}

// validateBreaker rejects negative breaker knobs and a cap below the base.
func validateBreaker(b BreakerOptions) error {
	if b.Threshold < 0 || b.Base < 0 || b.Max < 0 {
		return fmt.Errorf("%w: breaker threshold, base and max must not be negative", ErrInvalidConfig)
	}
	if b.Base > 0 && b.Max > 0 && b.Max < b.Base {
		return fmt.Errorf("%w: breaker max %v below base %v", ErrInvalidConfig, b.Max, b.Base)
	}
	return nil
}

// normalizeBreaker fills the enabled breaker's defaults.
func normalizeBreaker(b BreakerOptions) BreakerOptions {
	if !b.enabled() {
		return BreakerOptions{}
	}
	if b.Base <= 0 {
		b.Base = defaultBreakerBase
	}
	if b.Max <= 0 {
		b.Max = defaultBreakerMax
	}
	b.Max = max(b.Max, b.Base)
	return b
}
