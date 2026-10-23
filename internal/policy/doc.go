// Package policy implements core.Policy: the admission gate that decides
// whether a request of a given workload class may run on an upstream account,
// selects the first admissible account from an ordered candidate list, and
// tracks in-flight leases and cooldowns.
//
// Public API wired by cmd/localrouter:
//
//	p := policy.New(accounts, quotaSource, policy.Options{
//		StaleAfter:       cfg.Policy.StaleAfter,
//		SafetyMargin:     cfg.Policy.SafetyMargin,
//		InflightEstimate: cfg.Policy.InflightEstimate,
//		Clock:            core.SystemClock{},
//		Logger:           logger,
//	})
//	lease, dec := p.Acquire(core.ClassBackground, route.Background, nil)
//	defer lease.Release(core.Outcome{Status: 200})
//
// Admission for account A and class C passes iff, for every window W of A's
// latest quota snapshot,
//
//	used(W) + load + margin <= 1 - floor(W)
//
// where used(W) is 0 once W.ResetAt has passed, floor is A's reserve for
// W.Kind (background only), margin is SafetyMargin (background only), and
// load is InflightEstimate times the number of open leases on A. For the
// background class the request being admitted is counted as in flight too,
// so N concurrent background requests near the floor cannot all pass.
//
// Acquire evaluates and creates the lease under a single mutex. Leases are
// release-once. Releasing with status 429 (or 401/403, or 402 on OpenRouter)
// puts the account in cooldown; a newer snapshot showing headroom clears it
// early. An Outcome with RequestScoped set never starts a cooldown.
//
// Options.Breaker enables the per-account reachability breaker
// (policy.breaker): Threshold consecutive releases with Outcome.Unreachable
// open it for Base (doubling per failed probe up to Max); when the period
// ends exactly one lease is admitted as a probe. Breaker reports its state for
// the control API, and AcquireWithBreaker tells the proxy when a denial was
// caused by the breaker alone:
//
//	lease, dec, unreachable, retryIn := p.AcquireWithBreaker(class, candidates, exclude)
//	state, fails, openUntil, enabled := p.Breaker(accountID)
//
// A pooled route (core.Route.BalanceStrategy != "", routes[].balance) uses
// AcquireBalanced instead: among members that pass the breaker, admission and
// their BalanceMaxInflight cap it picks the lowest (inflight+1)/weight (ties
// to candidate order), or a live X-LocalRouter-Session binding kept in a
// 4,096-entry LRU shared across views. A denial carries a PoolDeny* kind;
// DryRunBalanced evaluates without side effects and AffinityEntries counts
// bindings for diagnostics:
//
//	lease, dec, denial, accounts, retryIn := p.AcquireBalanced(class, candidates, exclude, route, session)
//	pick, inflight, maxInflight := lease.(*policy.PoolLease).Pick()
package policy
