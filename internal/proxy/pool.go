package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// denialPoolSaturated is the "denial" attribute of the policy denied log line
// and the error type of a pooled route whose members are all at max_inflight.
const denialPoolSaturated = "pool_saturated"

// Pool denial kinds reported by balancedPolicy (internal/policy.PoolDeny*).
const (
	poolDenyBreaker   = "breaker"
	poolDenySaturated = "saturated"
)

// poolRetryAfter mirrors concurrencyRetryAfter: a member frees as soon as
// any of its in-flight requests finishes.
const poolRetryAfter = "1"

// balancedPolicy is implemented by policies that can pool a route's members
// (internal/policy.Policy). AcquireBalanced is called only for a route with a
// balance block; denial is "breaker", "saturated", "policy" or "" and accounts
// names the breaker-refused or saturated members.
type balancedPolicy interface {
	AcquireBalanced(class core.Class, candidates []string, exclude map[string]bool, route core.Route, session string) (l core.Lease, dec core.Decision, denial string, accounts []string, retryIn time.Duration)
}

// cappedPolicy is implemented by policies that enforce a per-account
// max_inflight on unpooled routes too (internal/policy.Policy).
// AcquireCapped is Acquire with the denial kinds and accounts of
// balancedPolicy.
type cappedPolicy interface {
	AcquireCapped(class core.Class, candidates []string, exclude map[string]bool) (l core.Lease, dec core.Decision, denial string, accounts []string, retryIn time.Duration)
}

// pooledLease is implemented by the leases AcquireBalanced returns.
type pooledLease interface {
	Pick() (pick string, inflight, maxInflight int)
}

// poolPick is the pooled-route part of an attempt event.
type poolPick struct {
	pick        string
	inflight    int
	maxInflight int
}

// acquireFor admits one attempt of req. A pooled route asks AcquireBalanced;
// an unpooled route asks AcquireCapped (so a per-account max_inflight holds
// there too), or takes the plain acquire path with a policy that has
// neither. The denial maps as: breaker -> unreachable/retryIn (the breaker's
// 503), saturated -> saturated accounts (429 pool_saturated), anything else
// -> neither (429 quota_reserve).
func (p *Proxy) acquireFor(req *request, exclude map[string]bool) (l core.Lease, dec core.Decision, unreachable []string, retryIn time.Duration, saturated []string) {
	var (
		denial   string
		accounts []string
	)
	bp, pooled := p.deps.Policy.(balancedPolicy)
	cp, capped := p.deps.Policy.(cappedPolicy)
	switch {
	case req.route.BalanceStrategy != "" && pooled:
		l, dec, denial, accounts, retryIn = bp.AcquireBalanced(req.class, req.candidates, exclude, req.route, req.session)
	case capped:
		l, dec, denial, accounts, retryIn = cp.AcquireCapped(req.class, req.candidates, exclude)
	default:
		l, dec, unreachable, retryIn = p.acquire(req.class, req.candidates, exclude)
		return l, dec, unreachable, retryIn, nil
	}
	switch denial {
	case poolDenyBreaker:
		return l, dec, accounts, retryIn, nil
	case poolDenySaturated:
		return l, dec, nil, 0, accounts
	}
	return l, dec, nil, 0, nil
}

// writePoolSaturated answers a request whose candidates are all at their
// max_inflight (the rest, if any, held open by the breaker), on a pooled or
// an unpooled route.
func writePoolSaturated(w http.ResponseWriter, accounts []string) {
	w.Header().Set("Retry-After", poolRetryAfter)
	writeError(w, http.StatusTooManyRequests,
		"localrouter: pool saturated (every member at max_inflight): "+strings.Join(accounts, ", "), denialPoolSaturated)
}

// poolPickOf returns the pick of a pooled lease, or nil for any other lease.
func poolPickOf(l core.Lease) *poolPick {
	if ol, ok := l.(*onceLease); ok {
		l = ol.Lease
	}
	pl, ok := l.(pooledLease)
	if !ok {
		return nil
	}
	pick, inflight, limit := pl.Pick()
	return &poolPick{pick: pick, inflight: inflight, maxInflight: limit}
}
