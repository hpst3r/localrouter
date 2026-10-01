Package: internal/quota

Implement core.QuotaSource plus a poller.

- `New(accounts []core.Account, creds core.CredentialSource, opts Options) *Manager`
  where Options{PollInterval, MinRefreshGap (default 20s), HTTPClient *http.Client,
  Clock core.Clock, Logger *slog.Logger, CodexUsageURL, OllamaUsageURL string (defaults
  per SPEC; overridable for tests)}.
- `Start(ctx)` launches one goroutine per account polling every PollInterval (first
  poll immediately); stops on ctx cancel. `RequestRefresh(id, urgent)` triggers a
  refresh: non-urgent debounced by MinRefreshGap since last fetch; urgent ignores the
  gap but still coalesces concurrent requests (single in-flight fetch per account).
- `Latest` returns a copy (deep-copy Windows slice), thread-safe.
- Codex fetch per SPEC (/wham/usage). Credential headers from creds.Credential(ctx,id)
  (they include Authorization, ChatGPT-Account-Id); add Accept + User-Agent codex-cli.
  On 401: creds.Invalidate(id), retry once. Parse primary/secondary windows, plan_type,
  rate_limit.allowed. reset_at may be unix seconds (number) -> use it; else
  reset_after_seconds relative to fetch time.
- Ollama fetch per SPEC (/api/usage). The base URL for ollama accounts is the
  inference base (e.g. https://ollama.com/v1) — the usage URL is separate
  (OllamaUsageURL default https://ollama.com/api/usage). usage values are fractions.
  Clamp to [0,1]. Missing window => omit it.
- openai_compat accounts: no quota source; Latest returns ok=false; poller skips.
- ObserveHeaders for codex: parse x-codex-{primary,secondary}-used-percent,
  -reset-after-seconds, -window-minutes. If at least one used-percent present,
  merge into the snapshot (update matching window kinds, keep others), Source
  "headers", FetchedAt now. Do not clear Allowed unless a window shows <100% used
  (then set Allowed true only if it was false AND all observed windows < 100).
  Keep it conservative and documented.
- Fetch errors: keep last-good windows, set Err (sanitized: status code / error
  class only, never bodies with tokens), keep FetchedAt of last success.
- Tests: httptest fake wham + ollama servers; fake CredentialSource; fake clock.
  Cover acceptance test 8 (0.484), codex mapping incl. reset_at vs reset_after,
  401 retry with Invalidate, error keeps last-good, header observation merge,
  debounce/coalescing (count server hits), no secrets in Err.
