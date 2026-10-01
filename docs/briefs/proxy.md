Package: internal/proxy

The inference HTTP surface. Depends only on core interfaces (inject fakes in tests).

- `New(deps Deps, opts Options) *Proxy` where
  Deps{Accounts map[string]core.Account, Routes []core.Route, Creds core.CredentialSource,
  Quota core.QuotaSource, Policy core.Policy, Ledger core.Ledger,
  Authenticate func(bearer string) (core.Client, bool), Clock core.Clock, Logger *slog.Logger}
  Options{MaxFailovers int, HTTPClient *http.Client (no overall timeout; streaming),
  MaxBodyBytes int64 (default 32 MiB)}.
- `(*Proxy) Handler() http.Handler` serving POST /v1/responses, POST /v1/chat/completions,
  GET /v1/models per SPEC. Client auth via Authenticate(bearer) -> 401 JSON error.
  Class: client's class; header X-LocalRouter-Class: background downgrades interactive
  only. Read body (limit), parse only `model` and `stream` (json.Decoder into small struct);
  route by exact model; unknown model -> 404 JSON error listing nothing secret.
  If route.UpstreamModel set, rewrite the `model` field only (preserve the rest of the
  JSON byte-for-byte as far as practical: use json.RawMessage map round-trip is fine).
  chat/completions + stream:true -> ensure stream_options.include_usage=true.
- Upstream URL: account.BaseURL + "/responses" or "/chat/completions".
  Codex accounts on chat/completions -> 400 per SPEC (before acquiring a lease).
- Flow per attempt: lease := Policy.Acquire(class, candidates, exclude); if denied ->
  429 JSON {"error":{"message":"localrouter: no admissible account: <reason>","type":"quota_reserve"}}
  with Retry-After 60 and NO upstream call. Credential (on error -> release, try next).
  Forward request with allowed headers + credential headers. On response: feed headers
  to Quota.ObserveHeaders. If status in {401,403} on first try for that account:
  Creds.Invalidate and retry the SAME account once. If 429/401/403/5xx and no bytes
  sent to client -> release lease with Outcome (parse Retry-After / x-codex reset hints
  into ResetAt when present), record a ledger row (status, failover chain), exclude
  account, try next (MaxFailovers). Otherwise stream through.
- Streaming: copy headers (strip hop-by-hop, Content-Length when re-streaming),
  WriteHeader, then copy body flushing per chunk while an incremental SSE parser
  extracts usage per SPEC (Responses: response.completed/incomplete/failed ->
  response.usage; chat: last chunk with usage). Non-SSE JSON body: parse top-level
  usage (cap 16 MiB buffered parse; still stream bytes through). Do NOT buffer the
  whole stream. Client disconnect (ctx canceled / write error) -> stop, usage_known
  false, release lease, record row.
- Ledger row per attempt: generate id (crypto/rand hex), client name, class, route,
  model (client-facing), provider, account, upstream identity (Credential.Identity),
  status, failover_of (previous attempt id), usage, latency, bytes, session/task/agent
  from X-LocalRouter-* headers (truncate to 128 chars), error (sanitized).
  Ledger errors are logged, never fail the request.
- GET /v1/models: {"object":"list","data":[{"id":m,"object":"model","owned_by":"localrouter"}]}
  for every route model, sorted.
- Tests with httptest fake upstream + fake Policy/Quota/Creds/Ledger: auth 401, unknown
  model, codex on chat -> 400, reserve deny -> 429 with no upstream hit (acceptance 1
  at proxy level via fake policy), failover 429 -> next account, two ledger rows,
  failover_of linked (acceptance 4), Responses SSE usage capture incl. cached+reasoning
  and multi-line data and CRLF (acceptance 5), client abort mid-stream -> usage_known
  false + lease released, chat stream include_usage injection preserves other
  stream_options, 401 -> Invalidate + same-account retry, client Authorization never
  forwarded, upstream model rewrite, X-LocalRouter-Class downgrade only, no secrets in
  logs (acceptance 9, sentinel key).
