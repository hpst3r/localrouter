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

// acquireFor admits one attempt of req. An unpooled route (or a policy
// without pooling) takes the unchanged acquire path; a pooled route asks
// AcquireBalanced and maps its denial: breaker -> unreachable/retryIn (the
// breaker's 503), saturated -> saturated members (429 pool_saturated),
// anything else -> neither (429 quota_reserve).
func (p *Proxy) acquireFor(req *request, exclude map[string]bool) (l core.Lease, dec core.Decision, unreachable []string, retryIn time.Duration, saturated []string) {
	bp, ok := p.deps.Policy.(balancedPolicy)
	if req.route.BalanceStrategy == "" || !ok {
		l, dec, unreachable, retryIn = p.acquire(req.class, req.candidates, exclude)
		return l, dec, unreachable, retryIn, nil
	}
	l, dec, denial, accounts, retryIn := bp.AcquireBalanced(req.class, req.candidates, exclude, req.route, req.session)
	switch denial {
	case poolDenyBreaker:
		return l, dec, accounts, retryIn, nil
	case poolDenySaturated:
		return l, dec, nil, 0, accounts
	}
	return l, dec, nil, 0, nil
}

// writePoolSaturated answers a pooled route whose members are all at their
// max_inflight (the rest, if any, held open by the breaker).
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
