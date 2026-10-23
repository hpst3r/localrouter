// Package modelcheck implements the backend model drift check (docs/SPEC.md,
// "Backend model check"). For every static route and eligible candidate
// account it compares the backend id the route sends (the resolved upstream
// model) against the account's GET {base_url}/models listing, fetched with
// internal/discovery's request and validation rules. It reports; it never
// changes routing.
//
// Public API:
//
//   - Expectations(accounts, routes) lists the expected (route, account, id)
//     triples; Evaluate(exps, listings) turns listings into Verdicts (ok,
//     missing with suggestions and single_model, unchecked with an error
//     class, ineligible); Suggest is the suggestion rule on its own.
//   - Probe(ctx, creds, accounts, routes, Options) fetches each eligible
//     account once (bounded concurrency) and returns a Result. It backs
//     `localrouter check -probe`.
//   - NewChecker(creds, shared, interval, logger, Options) builds the
//     App-level periodic checker: SetTargets re-targets it per generation
//     (startup and every accepted reload, which also triggers a re-check),
//     Start runs it in the background, Close stops it and waits. Accounts that
//     shared (the discovery Manager) already polls are read from it, never
//     fetched again. Diagnostics returns the diagnostics "model_check" block.
//     Verdict changes into missing/single_model log one warn line; a return to
//     ok logs one info line.
//
// Results carry account ids, route names and model ids only: never URLs,
// credentials or upstream bodies.
package modelcheck
