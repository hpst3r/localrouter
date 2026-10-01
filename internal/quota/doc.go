// Package quota implements core.QuotaSource: it fetches per-account quota
// snapshots from the Codex (/wham/usage) and Ollama Cloud (/api/usage) usage
// APIs, polls them periodically, and merges passive observations from Codex
// response headers.
//
// Public API wired by cmd/localrouter:
//
//	m := quota.New(accounts, creds, quota.Options{PollInterval: cfg.Quota.PollInterval, Logger: log})
//	m.Start(ctx)             // one poller goroutine per codex/ollama account; stop by cancelling ctx
//	var _ core.QuotaSource = m
//
// Latest returns deep copies and is safe for concurrent use. RequestRefresh
// is asynchronous: non-urgent requests are debounced by Options.MinRefreshGap
// since the last fetch; urgent ones ignore the gap. Either way at most one
// fetch per account is in flight at a time.
//
// openai_compat accounts have no quota source: Latest reports ok=false and
// the poller skips them.
//
// Fetch errors keep the last-good windows and FetchedAt and set Snapshot.Err
// to a sanitized description (status code or error class only; never bodies
// or credentials). If the very first fetch fails, Latest returns ok=true with
// a snapshot that has no windows, zero FetchedAt (thus stale) and Err set, so
// the error is visible in the control API.
package quota
