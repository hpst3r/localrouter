Package: internal/quota (extend the existing package).

Implement the "Claude quota" section of docs/SPEC.md for accounts with
Provider == core.ProviderClaude. Read the existing fetch.go/quota.go first and
follow their patterns (sanitized Err strings, last-good retention, debounce,
coalescing, fake clock).

- Options gains: ClaudeUsageURL (default https://api.anthropic.com/api/oauth/usage),
  ClaudeUserAgent (default "claude-code/2.1.0"), and
  ClaudeCredentialsFile func(accountID string) string (the architect wires it from
  config; tests inject temp files). Claude accounts do NOT use core.CredentialSource
  (creds may be nil for them) — read the token from the credentials file.
- Never write the credentials file; never call any token/refresh endpoint.
- Expired token (expiresAt unix ms <= now) → skip HTTP, Err per SPEC.
- 401 → Err per SPEC, no Invalidate.
- Map windows per SPEC including weekly_<name> extras and limits[] weekly_scoped.
  utilization is 0–100 PERCENT → /100, clamp [0,1].
- ObserveHeaders: no-op for claude accounts.
- Tests (httptest + temp credentials file, use the REAL response shape below):
  mapping 5h/weekly/extra, percent→fraction (27.0 → 0.27), resets_at parsing,
  expired token skips network (assert zero hits), 401 message, credentials file
  never modified (compare bytes+mtime before/after), token never in Err/logs,
  missing file → sanitized Err, plan from subscriptionType.

Real /api/oauth/usage response (sanitized):
{"five_hour":{"utilization":27.0,"resets_at":"2026-10-01T23:00:00.506990+00:00"},
 "seven_day":{"utilization":7.0,"resets_at":"2026-10-02T20:00:00.507011+00:00"},
 "seven_day_oauth_apps":null,"seven_day_opus":null,"seven_day_sonnet":null,
 "extra_usage":{"is_enabled":false},
 "limits":[{"kind":"session","group":"session","percent":27,"resets_at":"2026-10-01T23:00:00.506990+00:00"},
  {"kind":"weekly_all","group":"weekly","percent":7,"resets_at":"2026-10-02T20:00:00.507011+00:00"},
  {"kind":"weekly_scoped","group":"weekly","percent":0,"resets_at":"2026-10-02T20:00:00.507187+00:00",
   "scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}}]}
Note fractional-second timestamps with +00:00 offset — parse with time.RFC3339Nano.
Expected windows: 5h 0.27, weekly 0.07, weekly_fable 0.00.
