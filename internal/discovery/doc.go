// Package discovery implements route-level upstream model discovery
// (docs/SPEC.md, "Model discovery"). It polls GET {base_url}/models for every
// account a discovering route lists as a candidate and keeps the validated id
// listings as runtime state shared across configuration generations.
//
// Public API wired by internal/app:
//
//   - New(creds, Options) *Manager builds the App-level manager. It holds the
//     listings keyed by account id and is never rebuilt on reload.
//   - (*Manager).SetTargets(accounts, routes) re-targets polling to the
//     current generation: an account is polled while a discovering route lists
//     it, at the smallest discover_interval among those routes; an account no
//     longer referenced stops being polled and its listing is dropped.
//   - (*Manager).Start(ctx) enables background polling (non-blocking: the
//     first poll runs on the background goroutine, which exists only while
//     there is a target) and (*Manager).Close stops it and waits for every
//     goroutine the manager started.
//   - (*Manager).Resolver(routes) returns the per-generation *Resolver, or nil
//     when no route discovers (so callers keep the legacy path untouched).
//   - (*Resolver).Resolve(model) returns the one discovering route that
//     claims model, narrowed to the candidates that list it, or reports the
//     id ambiguous when several routes claim it; Models lists every
//     resolvable discovered id; Refresh(ctx, model) runs the single-flight,
//     rate-limited on-demand refresh and returns as soon as model resolves;
//     Snapshot/Diagnostics build the diagnostics "discovery" block.
//
// The package imports only core. A failed poll keeps the last good listing and
// records only a fixed error class; URLs, bodies and credentials are never
// logged or reported.
package discovery
