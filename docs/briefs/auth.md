Package: internal/auth

Implement core.CredentialSource for codex (OAuth) and static-key providers, plus
the device login flow. Constants per SPEC (client id app_EMoamEEZ73f0CkXaXp7hrann,
auth issuer https://auth.openai.com) — make issuer overridable for tests.

- `NewStore(dir string) (*Store, error)` token files `<dir>/<account>.json` (dir 0700,
  files 0600) fields {access_token, refresh_token, id_token, account_id, expires_at,
  last_refresh}. Atomic write: temp in same dir, fsync, rename, fsync dir.
- `New(accounts []core.Account, keys map[string]StaticKey, store *Store, opts Options) *Manager`
  where StaticKey{File, Env string}; Options{Issuer, HTTPClient, Clock, Logger,
  RefreshSkew (default 5m)}. Implements core.CredentialSource.
- Codex Credential: load from store (cache in memory), refresh if JWT exp within
  RefreshSkew or after Invalidate. Single-flight per account via
  golang.org/x/sync/singleflight AND a per-account mutex so the rotated refresh token
  is persisted BEFORE any caller receives the new access token. Refresh failure with
  invalid_grant/401 => error clearly saying "run: localrouter login <id>"; do not
  delete the token file. Headers: Authorization Bearer, ChatGPT-Account-Id,
  originator codex_cli_rs. Identity = chatgpt_account_id.
  account_id from JWT claim https://api.openai.com/auth -> chatgpt_account_id
  (id_token first, then access_token). JWT parsing: decode payload only (no
  signature verification; document why — tokens come from the issuer over TLS and
  are only replayed to it).
- Static: read key from File (trim whitespace) or Env at Credential time (cache with
  file mtime). Identity = account id. Header Authorization Bearer.
- Device login: `Login(ctx, accountID string, out io.Writer) error` per SPEC: usercode,
  print verification URL + code to out, poll at interval (403/404 pending, honor ctx,
  15 min limit), exchange code, extract account_id, persist. Refuse to overwrite a
  token whose chatgpt_account_id differs from an existing one unless
  `LoginOptions{Force bool}` — implement as `LoginWithOptions`.
  Also refuse if the new chatgpt_account_id is already stored under a DIFFERENT
  account id (prevents the same ChatGPT account being configured twice).
- Client keys: `LoadClientKeys(map[name]keyFile) (*ClientKeys, error)`;
  `(*ClientKeys).Lookup(bearer string) (name string, ok bool)` using
  crypto/subtle constant-time compare over all keys; reject empty/short (<16 char)
  keys at load; files must not be group/world readable (error if mode & 0o077).
  `GenerateKey() (string, error)` 32 random bytes base64url with prefix "lr-".
- Tests: fake issuer httptest. Acceptance test 7 (10 concurrent Credential with an
  expiring token -> exactly 1 refresh request; all get new token; store has rotated
  refresh token). Device flow pending->success. invalid_grant message. Duplicate
  account guard. File modes. Client key constant-time lookup + perms check.
  Acceptance 9: errors and logs never contain token strings (test with a sentinel
  token and a captured slog handler).
