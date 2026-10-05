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
//	GET  /control/v1/usage   -> ledger summary (?since=24h|7d&group=account|model|class|client|host)
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
package control
