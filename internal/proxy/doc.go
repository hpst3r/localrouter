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
// request-scoped so they never cool the account down.
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
//		// Multi-user mode instead (identity enabled):
//		// MultiUser: true, AuthenticatePrincipal: bearerFn, // core.BearerAuthenticator
//		Clock:        core.SystemClock{},
//		Logger:       logger,
//	}, proxy.Options{MaxFailovers: 2}) // timeouts default to 180s headers / 300s stream idle
//	mux.Handle("/v1/", p.Handler())
//
// Handler serves POST /v1/responses, POST /v1/chat/completions and
// GET /v1/models. Secrets (client keys, upstream credentials) and request or
// response bodies are never logged or recorded.
//
// In multi-user mode only the Authorization bearer header authenticates (never
// a cookie); the principal's opaque UserID and KeyID are the only source of a
// row's owner, a user key's client identity is its KeyID, per-user concurrency
// is counted through PrincipalLimiter, and policy denials do not disclose the
// policy's reason. Because upstream accounts are shared, every principal gets
// only the Content-Type and Content-Encoding of a relayed response, and an
// upstream status >= 400 is answered with that status, a validated
// Retry-After and the generic upstream_error envelope instead of the
// upstream's body and headers.
package proxy
