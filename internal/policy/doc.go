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
// release-once. Releasing with status 429 (or 401/403) puts the account in
// cooldown; a newer snapshot showing headroom clears it early.
package policy
