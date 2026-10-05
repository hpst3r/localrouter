// Package quota implements core.QuotaSource: it fetches per-account quota
// snapshots from the Codex (/wham/usage), Ollama Cloud (/api/usage) and
// Claude (/api/oauth/usage) usage APIs, polls them periodically, and merges
// passive observations from Codex response headers.
//
// Public API wired by cmd/localrouter:
//
//	m := quota.New(accounts, creds, quota.Options{
//		PollInterval:          cfg.Quota.PollInterval,
//		Logger:                log,
//		ClaudeCredentialsFile: credentialsFileFor, // func(accountID string) string
//	})
//	m.Start(ctx)             // one poller goroutine per codex/ollama/claude account; stop by cancelling ctx
//	var _ core.QuotaSource = m
//
// Latest returns deep copies and is safe for concurrent use. RequestRefresh
// is asynchronous: non-urgent requests are debounced by Options.MinRefreshGap
// since the last fetch; urgent ones ignore the gap. Either way at most one
// fetch per account is in flight at a time.
//
// Claude accounts do not use the CredentialSource: each fetch reads the
// access token from the Claude Code credentials file (cached by mtime/size),
// never writes it and never refreshes. An expired token skips the request;
// a 401 sets Err without invalidating anything. Utilization percentages map
// to five_hour → 5h, seven_day → weekly, seven_day_<name> → weekly_<name>,
// and limits[] weekly_scoped models → weekly_<lowercase display name>.
// ObserveHeaders is a no-op for claude accounts.
//
// Claude accounts with QuotaSource == QuotaSourceAgent ("agent") are never
// polled (Start, RequestRefresh and refresh skip them: no credential read, no
// HTTP). Their snapshots arrive only through IngestSnapshot (core.SnapshotIngester),
// which stores a deep copy if FetchedAt is newer than the stored one and
// otherwise returns ErrSnapshotStale; unknown or non-claude+agent accounts
// are rejected. Latest reports ok=false until the first ingest.
//
// The per-host agent reuses the server's fetch path:
//
//	cred, err := quota.ParseClaudeCredentials(fileOrKeychainJSON)
//	snap, err := quota.FetchClaudeSnapshot(ctx, httpClient, quota.DefaultClaudeUsageURL,
//		quota.DefaultClaudeUserAgent, cred, accountID, time.Now())
//	// errors.Is(err, quota.ErrClaudeTokenExpired): no request was made; push nothing.
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
