// Package control serves LocalRouter's read/admin API and the embedded
// status widget (SPEC "Control API").
//
// Wiring (cmd/localrouter):
//
//	srv := control.New(control.Deps{
//		Accounts:     accounts, // ordered as in config
//		Quota:        quotaSource,
//		Policy:       policy,
//		Ledger:       ledger,
//		Routes:       routes,
//		Authenticate: clientAuth, // bearer -> client; used when RequireAuth and for ingest
//		Ingester:     quotaManager, // core.SnapshotIngester for agent-pushed snapshots
//		IsSnapshotStale: func(err error) bool { return errors.Is(err, quota.ErrSnapshotStale) },
//		Clock:        core.SystemClock{},
//	}, control.Options{RequireAuth: cfg.Control.RequireAuth, StaleAfter: staleAfter})
//	mux.Handle("/", srv.Handler())
//
// Handler serves:
//
//	GET  /healthz            -> "ok"
//	GET  /control/v1/status  -> per-account quota/admission state
//	GET  /control/v1/usage   -> ledger summary (?since=24h|7d&group=account|model|class|client|host|route|task|agent)
//	GET  /control/v1/analytics -> core.AnalyticsResult time series
//	                            (?range=24h|7d|30d|90d or from/to RFC 3339,
//	                            bucket=hour|day, group=<dim>, filter.<dim>=v, top=1..50);
//	                            needs Deps.Ledger to implement core.AnalyticsLedger
//	                            (else 503). Ledger errors prefixed "analytics:" -> 400.
//	GET  /control/v1/analytics/dimensions -> top 50 values per dimension
//	                            in range (?range=30d, from/to, filter.<dim>)
//	POST /control/v1/admit   -> dry-run admission {class, model} or {class, account}
//	                            (exactly one of model/account); never creates a lease
//	POST /control/v1/ingest  -> agent push of claude usage records and quota
//	                            snapshots (core.IngestRequest); always needs a
//	                            client key with Ingest (401 no/bad key, 403 not
//	                            ingest); any invalid item -> 400, nothing written
//	GET  /                   -> embedded HTML status widget
//
// When Options.RequireAuth is set, /control/v1/* require a client bearer key;
// /healthz and / stay open and the widget prompts for a key kept in
// localStorage. No secret material is ever returned.
//
// Multi-user mode (Options.MultiUser or Deps.MultiUser) changes the route
// table; legacy mode is unchanged:
//
//   - Every /control/v1 route authenticates the bearer with
//     Deps.AuthenticatePrincipal only (nil seam or backend failure -> 503,
//     bad credential -> 401; cookies and the legacy Authenticate are never
//     used) and authorizes it with internal/authz (403). Usage, analytics and
//     dimensions are owner-scoped: a user key or user-owned static client sees
//     only its owner's rows, a service client sees all rows; the scope is
//     applied in SQL before aggregation and never taken from the URL. The
//     owner dimensions "key" (everyone) and "user" (global readers) are added.
//     Status, diagnostics and budgets are service-only on the bearer API;
//     ingest needs a service client with Ingest and forces record ownership
//     from the principal. Admit keeps its full answer for service clients; a
//     user bearer gets the decision with an empty account_id, the generic
//     reason "admitted" or "no_admissible_account", and a budget estimate over
//     its own client and user buckets only. The {class, account} form is 403
//     for a user bearer, before the account is looked up.
//   - /ui/v1/* is the browser-session API (session cookie only, an
//     Authorization header is refused; unsafe methods need the exact
//     PublicBaseURL Origin, Sec-Fetch-Site absent or same-origin and the
//     X-LocalRouter-CSRF token): /me, /me/keys, /me/usage, /me/analytics,
//     /me/analytics/dimensions (alias /me/dimensions), /me/budget, and for
//     admin sessions /admin/users[/{id}/disable|enable|delete|keys[/{kid}]],
//     /admin/audit and the global /admin/status, usage, analytics,
//     analytics/dimensions (alias /admin/dimensions), diagnostics, budgets.
//   - GET / redirects to /ui/; /readyz also requires the identity store.
package control
