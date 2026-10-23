package policy

import (
	"fmt"
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
// survives reloads like cooldowns do.
type breakerState struct {
	fails   int           // consecutive unreachable attempts
	tripped bool          // open or half-open (not closed)
	until   time.Time     // end of the current open period; half-open from then
	period  time.Duration // length of the current open period
	probe   bool          // the single half-open probe lease is in flight
	class   string        // error class of the last unreachable attempt
}

// breakerBlocks reports whether the breaker refuses the account at now, the
// reason, and when it half-opens (already passed while a probe is in flight).
func breakerBlocks(c *cfg, b *breakerState, now time.Time) (bool, string, time.Time) {
	if !c.opts.Breaker.enabled() || !b.tripped {
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

// breakerRelease applies one released lease's outcome to b. It reports
// whether the breaker opened (or re-opened) or closed. p.mu must be held.
func breakerRelease(c *cfg, b *breakerState, o core.Outcome, probe bool, now time.Time) (opened, closed bool) {
	br := c.opts.Breaker
	if !br.enabled() {
		*b = breakerState{}
		return false, false
	}
	if probe {
		// Whatever happens next, this probe no longer holds the half-open
		// slot; a neutral outcome (client cancellation, credential failure,
		// request-scoped answer) leaves the account half-open.
		b.probe = false
	}
	switch {
	case o.Unreachable != "":
		b.fails++
		b.class = o.Unreachable
		switch {
		case probe:
			b.period = min(max(b.period*2, br.Base), br.Max)
		case !b.tripped && b.fails >= br.Threshold:
			b.tripped, b.period = true, br.Base
		default:
			return false, false
		}
		b.until = now.Add(b.period)
		return true, false
	case o.Status != 0 && !o.RequestScoped:
		b.fails = 0
		if probe {
			*b = breakerState{}
			return false, true
		}
	}
	return false, false
}

// Breaker reports the reachability breaker of one account: its state
// (BreakerClosed, BreakerOpen or BreakerHalfOpen), consecutive unreachable
// attempts, and the end of the open period (zero when closed). enabled is
// false, and the other results are zero, when policy.breaker is disabled or
// the account is unknown.
func (p *Policy) Breaker(accountID string) (state string, consecutiveFailures int, openUntil time.Time, enabled bool) {
	c := p.cur.Load()
	if !c.opts.Breaker.enabled() {
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
// unreachable is nil, and a mixed denial still names the breaker accounts in
// the decision reason.
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
	return p.leaseLocked(c, sel.id), sel.dec, nil, 0
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
