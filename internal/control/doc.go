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
//		Authenticate: clientAuth, // bearer -> client; used when RequireAuth
//		Clock:        core.SystemClock{},
//	}, control.Options{RequireAuth: cfg.Control.RequireAuth, StaleAfter: staleAfter})
//	mux.Handle("/", srv.Handler())
//
// Handler serves:
//
//	GET  /healthz            -> "ok"
//	GET  /control/v1/status  -> per-account quota/admission state
//	GET  /control/v1/usage   -> ledger summary (?since=24h|7d&group=account|model|class|client)
//	POST /control/v1/admit   -> dry-run admission {class, model}; never creates a lease
//	GET  /                   -> embedded HTML status widget
//
// When Options.RequireAuth is set, /control/v1/* require a client bearer key;
// /healthz and / stay open and the widget prompts for a key kept in
// localStorage. No secret material is ever returned.
package control
