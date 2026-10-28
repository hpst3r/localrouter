// Package proxy implements the LocalRouter inference HTTP surface.
//
// It authenticates downstream clients, routes requests by exact model name,
// asks a core.Policy for an admissible upstream account (lease), attaches
// upstream credentials from a core.CredentialSource, forwards the request,
// streams the response back while extracting token usage from SSE or JSON
// bodies, fails over to the next account on 429/401/403/5xx before any byte
// reaches the client, and records one core.RequestRecord per attempt.
// OpenRouter failures caused by the request itself (moderation 403, a 402
// while funds are known, a non-exhaustion 429) are reported to policy as
// request-scoped so they never cool the account down. A send that got no
// response headers (DNS, dial, TLS, reset, header timeout) is reported as
// core.Outcome.Unreachable for the reachability breaker; when a policy
// implementing AcquireWithBreaker (internal/policy) refuses every candidate
// only because of the breaker, the answer is 503 upstream_unavailable. A
// lease implementing Reached() is told as soon as the headers of an answer
// that is not request-scoped arrive, before the body is relayed.
// A pooled route (core.Route.BalanceStrategy set) is admitted through a
// policy implementing AcquireBalanced; when every member is at max_inflight
// (or breaker-open) the answer is 429 pool_saturated with Retry-After 1, and
// pooled attempt events add pool_pick, member_inflight and member_cap. A
// member's max_inflight caps that account on every route: an unpooled route
// is admitted through AcquireCapped when the policy implements it, and a
// denial only by caps (or caps and the breaker) is answered the same way.
//
// Public API wired by cmd/localrouter:
//
//	p := proxy.New(proxy.Deps{
//		Accounts:     accounts,     // map[string]core.Account keyed by ID
//		Routes:       routes,       // []core.Route
//		Creds:        creds,        // core.CredentialSource
//		Quota:        quota,        // core.QuotaSource (ObserveHeaders, RequestRefresh)
//		Policy:       policy,       // core.Policy (Acquire)
//		Ledger:       ledger,       // core.Ledger (Record)
//		Authenticate: authFn,       // func(bearer string) (core.Client, bool)
//		Clock:        core.SystemClock{},
//		Logger:       logger,
//	}, proxy.Options{MaxFailovers: 2}) // timeouts default to 180s headers / 300s stream idle
//	mux.Handle("/v1/", p.Handler())
//
// Handler serves POST /v1/responses, POST /v1/chat/completions and
// GET /v1/models. When a route declares a typed-decision protocol it also
// serves POST /v1/systemone (TypeSafe System One shape, upstream
// <base_url>/systemone) and POST /v1/decisions (OpenRouter Decisions API,
// upstream <base_url minus /v1>/alpha/decisions): native passthroughs with
// only the pinned model rewritten, strictly classified with
// Options.DecisionBounds (routing.InferDecision). Secrets (client keys,
// upstream credentials) and request or response bodies are never logged or
// recorded.
package proxy
