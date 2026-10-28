package policy

import (
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Pool denial kinds returned by AcquireBalanced, so the proxy can map a
// pooled-route refusal to its answer.
const (
	// PoolDenyBreaker: every member was refused by the reachability breaker.
	PoolDenyBreaker = "breaker"
	// PoolDenySaturated: at least one member is at its max_inflight and every
	// other member is saturated or held open by the breaker.
	PoolDenySaturated = "saturated"
	// PoolDenyPolicy: some member was refused by admission (quota, cooldown,
	// stale reserve), was excluded by the caller, or is unknown.
	PoolDenyPolicy = "policy"
)

// Pool pick reasons reported by a pooled lease.
const (
	PoolPickSticky        = "sticky"
	PoolPickLeastInflight = "least_inflight"
)

// stickySlack is how far a bound, uncapped member's in-flight count may
// exceed the least-loaded eligible member before affinity yields.
const stickySlack = 2

// PoolLease is the lease AcquireBalanced returns. Pick reports how the member
// was chosen (PoolPickSticky or PoolPickLeastInflight), its in-flight count
// at pick time including this lease, and its cap (0 = uncapped).
type PoolLease struct {
	*lease
	pick     string
	inflight int
	cap      int
}

// Pick reports how the member was chosen, its in-flight count including this
// lease, and its max_inflight (0 = uncapped).
func (l *PoolLease) Pick() (pick string, inflight, maxInflight int) {
	return l.pick, l.inflight, l.cap
}

// poolScan is the result of one scanPoolLocked pass.
type poolScan struct {
	now      time.Time
	id       string // picked member; "" on denial
	pick     string
	cap      int
	key      string // affinity key; "" when affinity is off for the request
	dec      core.Decision
	denial   string
	accounts []string // breaker or saturated members, per denial
	retryAt  time.Time
	// br is the breaker-knob snapshot used for both selection and the lease
	// (breaker Amendment R2.1).
	br BreakerOptions
}

// AcquireBalanced selects a member of a pooled route (route.BalanceStrategy
// != "") and creates a lease on it, atomically under the admission mutex.
//
// Each candidate not in exclude is checked like Acquire (reachability breaker,
// then admission) and then against its max_inflight (Options.MaxInflight,
// else route.BalanceMaxInflight). Among the
// eligible members a live session binding wins (when route.BalanceStickyTTL >
// 0 and session != "") unless the bound member is uncapped and more than two
// leases above the least-loaded eligible member; otherwise the member with the
// lowest (inflight+1)/weight wins, ties going to candidate order. The binding
// is (re)written with a sliding TTL. A half-open member's lease becomes its
// breaker probe exactly as in Acquire.
//
// On denial the lease is nil, denial is one of the PoolDeny* kinds, accounts
// lists the breaker-refused members (PoolDenyBreaker) or the saturated members
// (PoolDenySaturated), and retryIn is the time until the earliest breaker
// half-open (PoolDenyBreaker only). On success the lease is a *PoolLease.
func (p *Policy) AcquireBalanced(class core.Class, candidates []string, exclude map[string]bool, route core.Route, session string) (l core.Lease, dec core.Decision, denial string, accounts []string, retryIn time.Duration) {
	c := p.cur.Load()
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.scanPoolLocked(c, class, candidates, exclude, route, session)
	if !s.dec.Allow {
		c.opts.Logger.Info("policy denied", "class", class, "reason", s.dec.Reason)
		if s.denial == PoolDenyBreaker {
			retryIn = s.retryAt.Sub(s.now)
		}
		return nil, s.dec, s.denial, s.accounts, retryIn
	}
	if s.key != "" {
		p.aff.put(s.key, s.id, s.now.Add(route.BalanceStickyTTL))
	}
	base := p.leaseLocked(s.id, s.br)
	return &PoolLease{lease: base, pick: s.pick, inflight: p.state[s.id].inflight, cap: s.cap}, s.dec, "", nil, 0
}

// DryRunBalanced evaluates a pooled route like AcquireBalanced (including a
// live session binding) without creating a lease, writing an affinity entry
// or claiming a breaker probe.
func (p *Policy) DryRunBalanced(class core.Class, candidates []string, route core.Route, session string) core.Decision {
	c := p.cur.Load()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scanPoolLocked(c, class, candidates, nil, route, session).dec
}

// AffinityEntries reports the live session bindings of one pooled route.
func (p *Policy) AffinityEntries(route string) int {
	now := p.cur.Load().opts.Clock.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.aff.count(route, now)
}

// scanPoolLocked is the pooled selection pass. It mutates nothing but what
// admitLocked may (clearing an expired cooldown). p.mu must be held.
func (p *Policy) scanPoolLocked(c *cfg, class core.Class, candidates []string, exclude map[string]bool, route core.Route, session string) poolScan {
	s := poolScan{now: c.opts.Clock.Now(), br: p.breakerOpts()}
	type member struct {
		id                 string
		inflight, w, limit int
	}
	var (
		eligible               []member
		reasons                []string
		unreachable, saturated []string
		others                 int
	)
	for _, id := range candidates {
		if exclude[id] {
			reasons = append(reasons, id+": excluded")
			others++
			continue
		}
		acct, ok := c.accounts[id]
		if !ok {
			reasons = append(reasons, id+": unknown account")
			others++
			continue
		}
		st := p.state[id]
		if blocked, why, at := breakerBlocks(s.br, &st.br, s.now); blocked {
			reasons = append(reasons, id+": "+why)
			unreachable = append(unreachable, id)
			if s.retryAt.IsZero() || at.Before(s.retryAt) {
				s.retryAt = at
			}
			continue
		}
		if ok, why := p.admitLocked(c, acct, class, s.now); !ok {
			reasons = append(reasons, id+": "+why)
			others++
			continue
		}
		limit := c.capOf(id, route.BalanceMaxInflight[id])
		if why, full := c.saturated(id, st.inflight, limit); full {
			reasons = append(reasons, why)
			saturated = append(saturated, id)
			continue
		}
		eligible = append(eligible, member{id: id, inflight: st.inflight, w: max(route.BalanceWeight[id], 1), limit: limit})
	}
	if len(eligible) == 0 {
		switch {
		case others > 0 || len(reasons) == 0:
			s.denial = PoolDenyPolicy
		case len(saturated) > 0:
			s.denial, s.accounts = PoolDenySaturated, saturated
		default:
			s.denial, s.accounts = PoolDenyBreaker, unreachable
		}
		s.dec = core.Decision{Reason: strings.Join(reasons, "; ")}
		if len(reasons) == 0 {
			s.dec.Reason = "no candidate accounts"
		}
		return s
	}

	best := eligible[0]
	least := best.inflight
	for _, m := range eligible[1:] {
		// (m.inflight+1)/m.w < (best.inflight+1)/best.w, strictly: ties keep
		// the earlier candidate.
		if (m.inflight+1)*best.w < (best.inflight+1)*m.w {
			best = m
		}
		least = min(least, m.inflight)
	}
	s.pick = PoolPickLeastInflight
	if session != "" && route.BalanceStickyTTL > 0 {
		s.key = affinityKey(route.Name, session)
		if id, ok := p.aff.get(s.key, s.now); ok {
			for _, m := range eligible {
				if m.id == id && (m.limit > 0 || m.inflight <= least+stickySlack) {
					best, s.pick = m, PoolPickSticky
					break
				}
			}
		}
	}
	s.id, s.cap = best.id, best.limit
	reason := "admitted " + best.id + " (pool " + s.pick
	if len(reasons) > 0 {
		reason += "; skipped " + strings.Join(reasons, "; ")
	}
	s.dec = core.Decision{Allow: true, AccountID: best.id, Reason: reason + ")"}
	return s
}
