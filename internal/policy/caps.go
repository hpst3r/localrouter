package policy

import (
	"fmt"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// AccountCaps derives Options.MaxInflight from the routes: every non-zero
// BalanceMaxInflight of a pooled route caps that account on every route.
// Config validation requires one value per account across balance blocks;
// should two differ anyway, the smaller wins. It returns nil when no route
// caps any account, so a configuration without balance blocks is unchanged.
func AccountCaps(routes []core.Route) map[string]int {
	var caps map[string]int
	for _, r := range routes {
		for id, n := range r.BalanceMaxInflight {
			if n <= 0 {
				continue
			}
			if caps == nil {
				caps = map[string]int{}
			}
			if cur, ok := caps[id]; !ok || n < cur {
				caps[id] = n
			}
		}
	}
	return caps
}

// capOf returns the account's max_inflight: the per-account Options cap, or
// else routeCap (a pooled route's own BalanceMaxInflight entry, 0 for an
// unpooled route). 0 means uncapped.
func (c *cfg) capOf(id string, routeCap int) int {
	if n := c.opts.MaxInflight[id]; n > 0 {
		return n
	}
	return max(routeCap, 0)
}

// saturated reports whether an account with inflight open leases is at its
// cap (see capOf), and the rejection reason when it is.
func (c *cfg) saturated(id string, inflight, routeCap int) (string, bool) {
	limit := c.capOf(id, routeCap)
	if limit == 0 || inflight < limit {
		return "", false
	}
	return fmt.Sprintf("%s: saturated (%d/%d in flight)", id, inflight, limit), true
}

// AcquireCapped is Acquire for an unpooled route that also classifies a
// denial with the PoolDeny* kinds, so the proxy answers a capped account
// exactly as it answers a saturated pool. Candidates are taken in order as in
// Acquire; one at its Options.MaxInflight cap is skipped. On denial, denial
// is PoolDenySaturated with the capped accounts when every rejection was the
// cap or the breaker (at least one the cap), PoolDenyBreaker with the
// breaker-refused accounts and retryIn when every rejection was the breaker
// (as AcquireWithBreaker), and PoolDenyPolicy otherwise.
func (p *Policy) AcquireCapped(class core.Class, candidates []string, exclude map[string]bool) (l core.Lease, dec core.Decision, denial string, accounts []string, retryIn time.Duration) {
	c := p.cur.Load()
	p.mu.Lock()
	defer p.mu.Unlock()
	sel := p.selectLocked(c, class, candidates, exclude)
	if sel.dec.Allow {
		return p.leaseLocked(sel.id), sel.dec, "", nil, 0
	}
	c.opts.Logger.Info("policy denied", "class", class, "reason", sel.dec.Reason)
	switch {
	case sel.onlySaturated():
		return nil, sel.dec, PoolDenySaturated, sel.saturated, 0
	case sel.onlyBreaker():
		return nil, sel.dec, PoolDenyBreaker, sel.unreachable, sel.retryAt.Sub(sel.now)
	}
	return nil, sel.dec, PoolDenyPolicy, nil, 0
}
