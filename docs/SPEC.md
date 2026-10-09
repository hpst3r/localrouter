# LocalRouter MVP — Specification (contract for all packages)

LocalRouter is a single Go binary: an OpenAI-compatible proxy that selects an
upstream subscription account per request, enforces per-account quota reserves
by workload class, records token usage + estimated cost in SQLite, and exposes
a small read API + analytics dashboard. It listens on loopback by default; an
optional network mode (`allow_non_loopback`) serves trusted remote clients.
HTTPS is the recommended transport for all networked use.

For deployment, security assumptions, and maintenance procedures, see
[Network deployment and operations](NETWORK.md).

Non-goals for MVP: context composition, local tokenization/estimation,
forecasting engine, Prometheus, trace explorer, multi-user auth,
chat-completions→Responses translation for Codex.

## Packages and ownership

| Package | Owns | Depends on |
|---|---|---|
| `internal/core` | Shared types + interfaces (frozen; architect-owned) | stdlib |
| `internal/config` | YAML config load/validate (architect-owned) | core |
| `internal/quota` | Codex + Ollama + Claude + OpenRouter quota fetchers, poller, passive header observation | core |
| `internal/policy` | Admission gate, reserves, in-flight leases, cooldowns, account selection | core |
| `internal/ledger` | SQLite request ledger, summaries, pricing table + cost | core |
| `internal/auth` | Codex OAuth device login, token store, single-flight refresh; static API keys | core |
| `internal/proxy` | HTTP inference surface, client auth, forwarding, SSE usage capture, failover | core |
| `internal/control` | `/control/v1/*` JSON API + embedded HTML widget | core |
| `internal/claudelog` | Claude Code transcript collector (local, and on each host via the agent) | core |
| `internal/hermeslog` | Hermes `state.db` per-model usage importer (read-only) | core |
| `internal/agent` | `localrouter agent` push loops (transcript usage + Claude quota) | core |
| `internal/app` | Wiring: builds every component, Host guard, self-host detection | all |
| `cmd/localrouter` | Wiring + CLI (architect-owned) | all |

Library packages import only `core`; they never import each other. The
wiring/CLI layer is the exception: `internal/app` imports every library package
to build the object graph and is imported by `cmd/localrouter`, which also
wires `internal/agent`, `internal/claudelog`, and `internal/quota`. Tests may
import sibling packages (e.g. the `agent` tests use `claudelog` and `control`).
Use fakes of `core` interfaces where isolation is needed.

## Wire surfaces

Listen default `127.0.0.1:8787`. Refuse non-loopback bind unless
`allow_non_loopback: true`.

Inference requires the client key in the `Authorization` header, using the
`Bearer` scheme:

- `POST /v1/responses` — Responses API. For `provider: codex` upstream is
  `https://chatgpt.com/backend-api/codex/responses`. For `provider: ollama`
  / `openai_compat` upstream is `<base_url>/responses`.
- `POST /v1/chat/completions` — forwarded to `openai_compat`/`ollama`
  accounts only. A route resolving to a codex account on this path returns
  400 `{"error":{"message":"codex accounts only serve /v1/responses"}}`.
- `GET /v1/models` — synthesized from config `routes[].models` (exact names).

The body is parsed once, with the upstream's exact-key, case-sensitive
semantics, and rejected with 400 `invalid_request_error` (never forwarded)
unless it is a single UTF-8 JSON object (no trailing data) with no duplicate
top-level keys and no top-level key equal to `model`/`stream` only
case-insensitively (e.g. `MODEL`, `Stream`). `model` must be a non-empty
string; `stream`, if present, a boolean. Nested values are not inspected
beyond syntax. The route is chosen from that `model`, so the upstream reads
the same model and stream mode that was routed.

Body is forwarded verbatim except:
- `chat/completions` with `"stream": true`: set
  `stream_options.include_usage = true` (preserve other stream_options).
- Codex `/responses`: normalize the body per upstream attempt by removing
  `max_output_tokens`, `max_tokens`, `max_completion_tokens`, and `metadata`,
  and forcing `store: false` (required by the Codex backend).

Request headers forwarded: `Content-Type`, `Accept`, `OpenAI-Beta`,
`session_id`, `conversation_id`, `x-request-id`. Client `Authorization` is
DROPPED; upstream credential headers come from `core.Credential`.
Response streamed back unchanged (SSE flush per event). Hop-by-hop headers
stripped.

Optional attribution headers from clients (stored, never forwarded):
`X-LocalRouter-Session`, `X-LocalRouter-Task`, `X-LocalRouter-Agent`.
Each is truncated to 128 bytes on a UTF-8 boundary, with invalid UTF-8 and
control characters replaced by U+FFFD.
`X-LocalRouter-Class: background` lets an interactive-class key downgrade
itself; a background key can never upgrade.

## Workload classes

`interactive` and `background`. Each client key maps to one class.
Reserves protect capacity *from* `background`. `interactive` may consume an
account down to 0% (until upstream refuses).

## Quota model

`core.Window{Kind, UsedFrac 0..1, ResetAt, WindowSeconds}`; kinds `5h`, `weekly`.

Codex: `GET https://chatgpt.com/backend-api/wham/usage` with
the access token in `Authorization` using the `Bearer` scheme,
`ChatGPT-Account-Id: <id>`,
`User-Agent: codex-cli`. Map `rate_limit.primary_window` → `5h`,
`secondary_window` → `weekly`; `used_percent/100`; `reset_at` (unix
seconds) or `now + reset_after_seconds`; `limit_window_seconds`.
`rate_limit.allowed`/`limit_reached` → `Snapshot.Allowed`.

Codex passive observation: responses from `chatgpt.com` carry headers
`x-codex-primary-used-percent`, `x-codex-secondary-used-percent`,
`x-codex-primary-reset-after-seconds`, `x-codex-secondary-reset-after-seconds`,
`x-codex-primary-window-minutes`, `x-codex-secondary-window-minutes`
(any may be absent). The proxy hands headers to `QuotaSource.ObserveHeaders`.

Ollama Cloud: `GET https://ollama.com/api/usage` with
the API key in `Authorization` using the `Bearer` scheme.
`limits.session.usage` → `5h`,
`limits.weekly.usage` → `weekly`; values are ALREADY 0–1 fractions. No
reset time: `ResetAt` zero, `WindowSeconds` 18000 / 604800.

Poller: refresh each account every `quota.poll_interval` (default 5m),
plus debounced (≥ 20s apart) refresh after each completed request on that
account, plus immediately on a 429. Failures keep last-good snapshot and set
`Snapshot.Err`; never fabricate. If the very first fetch fails, `Latest`
returns ok=true with no windows, zero `FetchedAt`, and `Err` set (policy
treats zero `FetchedAt` as stale; control shows the error).

## Admission / policy (the core value)

For candidate account A and class C, for every window W of A's latest
snapshot:

- Effective used: if `W.ResetAt` non-zero and `now >= W.ResetAt`, treat
  `UsedFrac = 0` (window rolled; do not wait for a new sample).
- `floor(W) = reserve[A][W.Kind]` if C == background else 0.
- Admit iff `used + (inflight(A) + k) * policy.inflight_estimate + margin <= 1 - floor`
  where `k = 1` for background (the request being admitted counts as in
  flight) and `k = 0` for interactive; `margin = policy.safety_margin` for
  background, 0 for interactive.
- If `Snapshot.Allowed == false` and no window rolled past reset: deny all classes.

Staleness: snapshot older than `policy.stale_after` (default 10m) or absent:
- account has any reserve > 0 → deny `background`, allow `interactive`.
- account has no reserve → allow both (upstream 429 is the backstop).

Cooldown: after upstream 429 (or 401/403 after one refresh retry), the
account is excluded until `max(reset_at of exhausted window, now+60s)`;
cleared early if a fresh snapshot shows headroom. A 401/403 triggers a
credential refresh (`Invalidate` + retry on the same account) at most once
per account per 60s; inside that window it is handled as an account-level
failure without a refresh.

Request-scoped failures never cool an account down: the proxy releases the
lease with `Outcome.RequestScoped`, and policy then only decrements
in-flight and requests a non-urgent refresh. Otherwise one client could lock
every other class out of an account with a request that only fails for
itself. Only OpenRouter answers are classified as request-scoped (see
OpenRouter); codex/ollama/openai_compat 401/403/429 stay account-level.

Selection: routes give an ordered account list per class. First admissible
account wins. Admission is atomic with lease creation (mutex) to prevent the
check-then-act race: N concurrent background requests near the floor must
not all pass. `inflight(A)` counts open leases.

Lease release reports outcome {HTTP status, usage known?, bytes streamed}.
Failover: proxy may retry on the next admissible account ONLY if no response
bytes were sent to the client and status was 429/401/403/5xx-before-body
(OpenRouter: also an account-level 402; never a 403 or request-scoped 402).
Max 2 failovers. Each attempt is a separate ledger row.

## Concurrency limits and inbound timeouts

`limits.max_concurrent` (global) and `limits.max_concurrent_per_client`
(map of registered client name → cap) bound how many inference requests may be
active at once. 0 (the default) means unlimited; negative values are rejected
at load, as is a `max_concurrent_per_client` entry naming an unknown client. A
request's slot is acquired **after authentication and before the request body
is read**, and held for the entire request — including any streamed response
and every failover attempt — so it is released only when the request truly
ends. A refused request returns `429` with
`{"error":{"type":"concurrency_limit_exceeded"}}` and `Retry-After: 1` without
contacting an upstream, and the condition is distinct from a quota/admission
denial. Non-inference surfaces (`/v1/models`, `/control/v1/ingest`, control
endpoints) are not counted.

`timeouts.header|body|idle|shutdown` bound inbound handling: `header` is the
`ReadHeaderTimeout`, `body` is a read deadline lifted as soon as the body is
fully read (so it never truncates a long-lived SSE response), `idle` is the
keep-alive `IdleTimeout`, and `shutdown` is the graceful-drain budget. There is
deliberately no overall write or request timeout. A value of 0 selects the
default (10s / 30s / 120s / 30s); negative values are rejected.

## Ledger

SQLite (pure Go `modernc.org/sqlite`), WAL, file `<data_dir>/localrouter.db`.
One table `requests` (no prompt/response content, ever):

`id, started_at, finished_at, client, class, route, model, provider,
account_id, upstream_identity, status, failover_of, input_tokens,
cached_input_tokens, output_tokens, reasoning_tokens, usage_known,
cost_usd, cost_basis, latency_ms, bytes_out, session, task, agent, error`.

`cost_basis`: `api_equivalent` (subscription accounts: what it would cost at
API list price), `metered` (API-key billing), `provider_reported` (the cost the
upstream provider itself reported, e.g. OpenRouter `usage.cost`), or NULL if
unpriced. Unknown price ⇒ `cost_usd` NULL, never 0. `metered` and
`api_equivalent` amounts from local pricing are estimates; `provider_reported`
is a distinct request-level provenance.

A usable provider-reported cost (0 ≤ cost ≤ `core.MaxReportedCostUSD` = 1e6
USD, so NaN/±Inf are unusable; present only for OpenRouter) is
recorded verbatim as `cost_usd` with `cost_basis: provider_reported`. It takes
precedence over the local price table — including an explicit 0 — and is kept
even when token usage was not parseable (`usage_known = 0`), since the cost is
known independently. `usage.cost_details`, if present, is ignored: only
`usage.cost` counts, to avoid double counting. `localrouter pricing reprice`
never overwrites or clears a `provider_reported` row (NULL-safe on both the
select and the update); rows written before this feature are historical and are
not retroactively recovered. A row with known cost but unknown tokens still
contributes to cost totals, counts in `unknown_usage_requests` (token knowledge),
and is not an unpriced request.

Pricing file `pricing.yaml`: per model, USD per 1M `input`, `cached_input`,
`output` (reasoning billed as output). Shipped EMPTY of numbers; user or
`localrouter pricing import <litellm json>` populates it. Never invent prices.
Cost = (input−cached)·in + cached·cached_in + output·out, all /1e6.
Every price must be 0 ≤ price ≤ `core.MaxPricePerMTokUSD` (1e6 USD per 1M
tokens); loading `pricing.yaml` or `pricing.local.yaml` fails on any other
value (including YAML `.nan`/`.inf`), naming the model, and `pricing import`
fails the same way instead of writing such a table. A computed cost that is
not finite counts as unpriced (`cost_usd` NULL) in Record and Reprice. Cost
sums in Summary and Analytics saturate at the largest finite float64 so
responses stay JSON-encodable.

## Usage capture

- Responses SSE: event `response.completed` (also `response.incomplete`,
  `response.failed`) → `response.usage.{input_tokens,
  input_tokens_details.cached_tokens, output_tokens,
  output_tokens_details.reasoning_tokens}`. Non-stream: top-level `usage`.
- Chat completions: `usage.{prompt_tokens, completion_tokens,
  prompt_tokens_details.cached_tokens, completion_tokens_details.reasoning_tokens}`
  from the final chunk / body.
- Provider-reported cost: `usage.cost` (USD) from the same final record — the
  final chat chunk's top-level `usage`, the non-stream body's top-level `usage`,
  or a Responses terminal event's `response.usage`. The last meaningful final
  usage wins; costs are never summed per chunk. Captured only for OpenRouter.
  An explicit 0 is valid; missing, `null`, non-numeric, negative, non-finite,
  or above-1e6 (`core.MaxReportedCostUSD`) cost in the latest meaningful usage record clears any earlier intermediate
  cost, without discarding that record's valid token counts. Usage-less chunks
  do not clear the observation.
- A token count that is negative or above 1e12 (`core.MaxRecordTokens`) in
  any usage record makes the response's usage unknown (`usage_known = false`)
  rather than storing it.
- Client disconnect or missing usage ⇒ `usage_known = false`; lease still
  released. An already observed provider cost is retained alongside the
  transport/request error, independently of token knowledge; for an interrupted
  stream it is not a guarantee of the final billed amount.
- Parser must handle multi-line `data:` fields and `\r\n`, and must not
  buffer the entire stream (scan incrementally; keep ≤ 4 MiB per event).
  Non-stream JSON capture is bounded to 16 MiB; oversized bodies are still
  relayed, but their usage and cost are left unknown.

## Auth

Codex accounts: LocalRouter owns its OWN tokens (separate login; it never
reads or writes Hermes/Codex `auth.json`, avoiding rotating-refresh-token
contention). `localrouter login <account-id>` runs the device flow:
- `POST https://auth.openai.com/api/accounts/deviceauth/usercode` JSON
  `{"client_id": "app_EMoamEEZ73f0CkXaXp7hrann"}` → `device_auth_id`,
  `user_code`, `interval`. Show `https://auth.openai.com/codex/device`.
- Poll `POST .../api/accounts/deviceauth/token` JSON
  `{"device_auth_id","user_code"}`; 403/404 = pending. 200 →
  `authorization_code`, `code_verifier`.
- Exchange at `POST https://auth.openai.com/oauth/token` (form):
  `grant_type=authorization_code, code, redirect_uri=https://auth.openai.com/deviceauth/callback,
  client_id, code_verifier` → `access_token, refresh_token, id_token`.
- `chatgpt_account_id` from id_token/access_token JWT claim
  `https://api.openai.com/auth.chatgpt_account_id`.
- Refresh: `POST https://auth.openai.com/oauth/token` form
  `grant_type=refresh_token, refresh_token, client_id` when access expires
  within 5 min (JWT `exp`) or after a 401. Single-flight per account;
  persist rotated refresh token atomically (write temp + fsync + rename,
  mode 0600) BEFORE returning the new access token.
- Credential headers: `Authorization` (access token with the `Bearer` scheme),
  `ChatGPT-Account-Id: <id>`, `originator: codex_cli_rs`.
- Store: `<data_dir>/tokens/<account-id>.json`, dir 0700, file 0600.

Ollama / openai_compat: static key from `api_key_file` (preferred) or
`api_key_env`. Send the key in `Authorization` using the `Bearer` scheme.

Client keys: `clients[].key_file` containing the raw key; compared in
constant time. Keys never logged or returned.

## Control API (`internal/control`)

Read endpoints are unauthenticated on loopback (no secret material is ever
returned); config `control.require_auth: true` makes them require any client
key.

- `GET /control/v1/status` → `{schema_version:1, now, accounts:[{id,
  provider, healthy, cooldown_until, inflight, reserve:{"5h":0.1,...},
  windows:[{kind, used_frac, remaining_frac, reset_at, window_seconds}],
  background_admissible, interactive_admissible, snapshot_age_s, stale,
  error, reason, model_requests}], clients:[{name, class, host, ingest}]}` —
  `healthy` = no active cooldown and no fetch error; `reason` = policy
  explanation when a class is not admissible; each window also carries
  `rolled` (reset passed, used reported as 0). `clients` lists every
  registered client in config order (never key material).
- `GET /control/v1/usage?since=24h&group=account` → rows `{key, requests,
  input_tokens, cached_input_tokens, cache_creation_input_tokens,
  output_tokens, reasoning_tokens, cost_usd, unknown_usage_requests,
  unpriced_requests}`. `group` accepts any summary dimension:
  `account|model|class|client|host|route|task|agent` (default `account`).
- `GET /control/v1/analytics` and `GET /control/v1/analytics/dimensions` →
  time series + drill-down (see "Analytics" below); auth like `usage`.
- `POST /control/v1/admit` body `{class, model}` or `{class, account}` →
  dry-run `{decision: allow|deny, account_id, reason}` (does NOT create a
  lease).
- `POST /control/v1/ingest` — agent push endpoint (see "Multi-host"); always
  requires a client key whose client has `ingest: true`.
- `GET /` → single embedded HTML page (no external assets/CDNs), the
  analytics dashboard (see "Widget" below); polls every 30s while the tab is
  visible.
- `GET /healthz` → `ok` (liveness only: the process is answering).
- `GET /readyz` → `{"ready":true|false}` (unauthenticated, minimal; 503 when
  not ready). Readiness is local — the process is willing to serve and local
  storage answers a bounded ping. It deliberately says nothing about upstream
  provider health; an upstream outage is not a local readiness failure.
- `GET /control/v1/diagnostics` (auth like `status`) → `{schema_version, now,
  ready, storage:{configured, ok, error?}, inflight:{global_limit,
  global_active, global_peak, clients:[{name, limit, active}]},
  reload:{generation, ok, at, reason?, restart_only?},
  accounts:[{id, provider, healthy, stale, cooldown, reason,
  snapshot_age_s}]}`. Authenticated diagnostics; a limit of 0 means unlimited;
  no keys, secret paths, prompt content or per-account quota windows. `reload`
  is present only when a reload seam is wired; it is the sanitized last reload
  attempt (see "Configuration reload" below) and never carries key material.
- JSON responses are encoded before the status line is written; a body that
  cannot be encoded yields 500 `{"error":{"message":"encoding response
  failed"}}` instead of a 200 with an empty body.

## Logging

`log/slog` text to stderr. Never log tokens, keys, prompt/response bodies,
or full upstream URLs with query strings. Log: account chosen, class,
model, status, latency, usage numbers, policy denials with reason. A rejected
reload logs only its sanitized reason class and the restart-only config key
names; the raw error (which may embed a secret path) is never logged.

## Configuration reload

A running server re-reads its config on `SIGHUP` (Unix) or via the in-process
`App.Reload` seam (portable, and the only trigger Windows has). There is **no
HTTP endpoint** for reload. The new config is parsed and validated in full
before anything is published; only then is the reloadable subset swapped
atomically. In-flight requests finish against the generation they started on,
and persistent state — SQLite, the rotating-token cache, quota observations,
policy inflight/cooldown counters, collector progress and the listener — is
preserved by construction because it is shared, not rebuilt.

If validation fails, the change touches a restart-only field, or another reload
is already running, nothing is published: the previous (last-good) generation
keeps serving, the generation does not advance, and the attempt is recorded
with a sanitized `reason` and, for a restart-only rejection, the offending
`restart_only` key names. Rapid signals coalesce (a bounded number of reloads)
and reloads are serialized.

Reloadable in place: `clients[]` (`name`, `class`, `key_file`/`key_files`,
`host`, `ingest`), `routes[]`, `accounts[].reserve`, `pricing_file` contents,
`policy.stale_after`/`safety_margin`/`inflight_estimate`/`max_failovers`, and
`limits.max_concurrent`/`max_concurrent_per_client` (active counts preserved).

Pricing is **frozen per request by generation**, not resolved at ledger-write
time. One accepted generation publishes its handler, auth, routes, limits,
reserves, price table and generation number together under a single pointer, so
an inference or HTTP-ingest request is costed from the generation it was
admitted on even if a reload swaps the price table while it is in flight. The
in-process `claude_logs` collector selects the current generation once per
`Record`/`RecordBatch` call, so each batch is costed by the generation live when
that batch is written.

A reload replaces the live generation only: it never re-prices rows already
written and never rewrites the startup table. History is reconciled only by the
separate offline `localrouter pricing reprice` command, which loads the price
files from disk and opens the database itself — it is not run by a reload.
There is no global price set and no live re-price on reload, so a request never
mixes one generation's admission with another's price table.

Restart-only — a reload that changes any of these is **rejected whole**, never
partially applied: `listen`, `allow_non_loopback`, `allowed_hosts`,
`tls_cert_file`, `tls_key_file`, `data_dir`, the `pricing_file` path, the token
store location, `quota.poll_interval`, account topology (`accounts[].id`,
`provider`, `base_url`, `quota_source`, `api_key_file`, `api_key_env`,
`credentials_file`, `cost_basis`), `claude_logs.*`, `hermes_logs.*`,
`host_name`, `control.require_auth`, and the structural timeouts
(`timeouts.header`/`body`/`idle`/`shutdown`).

`GET /control/v1/diagnostics` reports the last attempt under `reload`:
`{generation, ok, at, reason?, restart_only?}`. `Generation` is 1 at startup
and increments by one per accepted reload; a failed attempt reports the
still-serving generation (it neither lags nor advances). `at` is UTC. The
document never contains keys, tokens or secret paths. Readiness (`/readyz`) is
unaffected by a reload: it stays a statement about local serving and storage.

Client keys rotate without downtime by deliberately overlapping two single-key
files: each file holds exactly one raw key, `key_file` and `key_files` are
mutually exclusive on a client, `key_files` entries must be distinct, and
duplicate keys across clients are rejected. List old and new, `SIGHUP`, roll the
client to the new key, remove the old file and `SIGHUP` again. A revoked key is
rejected for new requests at the next reload while in-flight requests holding
it finish. The config lists key file paths only; key material never appears in
the config, logs or diagnostics.

```bash
localrouter check -config ~/.config/localrouter/config.yaml    # validate first
kill -HUP "$(systemctl --user show -p MainPID --value localrouter)"
```

Rollback is implicit and automatic: a rejected reload never publishes, so the
last-good generation keeps serving and the generation number does not advance —
there is no separate rollback command. Diagnose a rejection from the sanitized
last attempt at `GET /control/v1/diagnostics` under
`reload:{generation, ok, at, reason?, restart_only?}` (`ok:false`, a `reason`,
and, for a restart-only change, the offending key names), and from the
`config reload rejected` log line, which carries only `reason`, `restart_only`
and `generation` — never the raw error or a secret path. Correct the config and
`SIGHUP` again.

## Acceptance tests (must exist across packages)

1. Reserve: background at used 0.91 with reserve 0.10 → deny, no upstream
   call; at 0.85 with no in-flight → allow.
2. In-flight: used 0.85, reserve 0.10, inflight_estimate 0.04, margin 0;
   3 concurrent background admits → exactly 1 allowed.
3. Per-account: primary 0.91 (reserve 0.1), secondary 0.54 (reserve 0) →
   background routed to secondary; ledger records secondary identity.
4. Failover: 429 from first account before body → retried on next; two
   ledger rows; first account in cooldown.
5. Streaming usage: Responses SSE `response.completed` usage captured;
   client abort mid-stream → `usage_known=false`, lease released.
6. Reset boundary: used 0.99 with `ResetAt` in the past → admissible.
7. Refresh single-flight: 10 concurrent Credential() with expiring token →
   exactly 1 refresh call, all 10 get the new token.
8. Ollama fractions: `usage: 0.484` → `UsedFrac 0.484`.
9. Secrets: no log line/response contains a token or client key.
10. No content columns in SQLite.
11. Reload: `SIGHUP` re-reads config; a valid change publishes a new generation
    while in-flight requests finish on the old one; an invalid change or a
    restart-only change is rejected whole (last-good keeps serving, generation
    unchanged) with a sanitized reason and, when restart-only, the offending key
    names. Rapid signals coalesce.
12. Diagnostics `reload` block: present only when the seam is wired; it carries
    `generation`/`ok`/`at` and, on failure, `reason` + `restart_only` — never
    keys, tokens or secret paths.

## Claude (quota-only account + transcript accounting)

Provider `claude` accounts are QUOTA-ONLY. LocalRouter never proxies inference
for them, never refreshes or writes Claude Code's credentials, and routes may
not reference them (config validation rejects it). Claude inference keeps
running through the official `claude` CLI.

### Claude quota (internal/quota)

- Token: read `accounts[].credentials_file` (default
  `~/.claude/.credentials.json`, JSON `{"claudeAiOauth":{"accessToken",
  "expiresAt" (unix ms), "subscriptionType", "rateLimitTier", ...}}`) at each
  fetch (cache by mtime). NEVER write it; NEVER call any token endpoint. If
  `expiresAt` has passed, skip the fetch and set `Err = "claude token expired;
  run claude to refresh"` (keep last-good windows).
- `GET https://api.anthropic.com/api/oauth/usage` with headers
  `Authorization` (access token with the `Bearer` scheme),
  `anthropic-beta: oauth-2025-04-20`,
  `Accept: application/json`, `User-Agent: claude-code/<ver>` (configurable,
  default `claude-code/2.1.0`).
- Mapping (utilization is a PERCENT 0–100): `five_hour` → `5h`,
  `seven_day` → `weekly`, `resets_at` RFC3339 → ResetAt, WindowSeconds
  18000 / 604800. Additional non-null `seven_day_<name>` objects (e.g.
  `seven_day_opus`, `seven_day_sonnet`) → window kind `weekly_<name>` (reserve
  for these is not configurable in MVP; they are display + they DO gate
  admission at 100% via the exhausted rule). Also accept the `limits[]` array
  when present: `kind:"weekly_scoped"` with `scope.model.display_name` X →
  `weekly_<lowercase X>` if not already set. Ignore unknown/null keys.
  `Plan` = credentials `subscriptionType`. Allowed: nil (no explicit gate).
- 401 from the usage API: do NOT invalidate/refresh; set Err `"claude token
  rejected; run claude to refresh"`.
- No header observation for claude (proxy never talks to Anthropic).

Window kinds beyond `5h`/`weekly` are allowed in `core.Window.Kind`; policy
applies reserves only for configured kinds, but the exhausted rule (used ≥
0.999 denies every class) applies to all windows.

### Claude transcript accounting (internal/claudelog)

Claude Code writes `<dir>/<project-slug>/<session-uuid>.jsonl` (also nested
`subagents/` files may exist — scan recursively for `*.jsonl`). Each line is a
JSON object; assistant entries carry `message.usage` and `message.model`,
`message.id`, top-level `requestId`, `sessionId`, `timestamp` (RFC3339),
`cwd`, `isSidechain`. The SAME API message is written multiple times
(streaming / one line per content block) with identical or growing usage.

- Dedupe key: `message.id` + ":" + `requestId` (fall back to whichever is
  present; skip entries with neither). For a key, keep the entry with the
  LARGEST output_tokens (final). Ledger row ID = "claude:" + sha256(key)[:32]
  so re-ingestion is idempotent (ledger Record is a no-op on existing ID).
- Usage mapping (Anthropic reports separately; core uses totals):
  `InputTokens = input_tokens + cache_creation_input_tokens +
  cache_read_input_tokens`, `CachedInputTokens = cache_read_input_tokens`,
  `CacheCreationInputTokens = cache_creation_input_tokens`,
  `OutputTokens = output_tokens`,
  `ReasoningTokens = output_tokens_details.thinking_tokens` (0 if absent).
  Skip `model == "<synthetic>"` and entries with all-zero usage.
- Record fields: Client `claude_logs.client` (default "claude-code"; the
  server overwrites it with the authenticated client name for ingested
  records — see "Per registered client"), Class "interactive", Route "claude",
  Provider "claude", AccountID = claude_logs.account, Model = message.model,
  StartedAt = FinishedAt = timestamp, Status 200, UsageKnown true,
  Session = sessionId, Task = project dir name, Agent = "subagent" if
  isSidechain else "main". NEVER store message content, cwd paths beyond the
  project dir slug, tool inputs, or anything else.
- Incremental scanning: remember per-file (size, offset of last complete
  line) in memory; on each scan read only new complete lines. Persist the
  offsets in a small JSON state file `<data_dir>/claudelog-state.json` (0600,
  atomic write) so restarts don't rescan everything (rescans are still safe
  thanks to idempotent IDs). Truncated/rotated file (size < offset) → rescan
  from 0. Lines > 8 MiB are skipped.
- Because a message's final usage line may arrive in a later scan than an
  earlier partial line, ingestion must not record a key until it is FINAL:
  treat a key as final when a later line in the same file has a different key,
  or the file has not been modified for ≥ 30s. (Keep pending keys in memory.)
- Runs every `claude_logs.scan_interval` (default 1m) plus once at startup.

### Ledger changes

- `Record` is idempotent on non-empty ID (`INSERT ... ON CONFLICT(id) DO
  NOTHING`), returns nil on duplicate.
- New column `cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0` (schema
  migration v2) and summary field `cache_creation_input_tokens`.
- Cost: `(input − cached − cache_creation)·input + cached·cached_input +
  cache_creation·cache_creation_input + output·output` per 1M; pricing YAML
  gains optional `cache_creation_input` (default = input). LiteLLM import maps
  `cache_creation_input_token_cost`.

### Admit CLI + model-less admit

- `POST /control/v1/admit` also accepts `{class, account}` (instead of
  `model`): dry-run that single account. Exactly one of model/account.
- `localrouter admit --class background --account claude-max [--url
  http://127.0.0.1:8787] [--json] [--key-file PATH]`: calls the control API;
  `--key-file` supplies the client key when `control.require_auth` is on.
  Exit 0 = allow, 1 = deny (prints reason), 2 = error/unreachable. Clients
  use it as a gate, e.g. before launching background Claude workers.

## Multi-host (central server + per-host agents)

One `localrouter serve` runs centrally, reachable by trusted clients over HTTPS.
Each host that runs Claude Code runs `localrouter agent`, which pushes Claude transcript usage and
Claude quota snapshots to the server. Hosts' Hermes/other clients use the
server's `/v1` with their own client keys.

### Server network mode (config)

- Server config default `os.UserConfigDir()/localrouter/config.yaml` (Linux/BSD
  `~/.config/localrouter/config.yaml`; macOS `~/Library/Application
  Support/localrouter/config.yaml`); `-config PATH` overrides it on every
  server subcommand that loads configuration (serve, check, login, pricing,
  and ledger); `admit` instead takes `--url` and `--key-file`.
- `allow_non_loopback: true` permits a non-loopback `listen`, and REQUIRES
  `control.require_auth: true` (validated).
- Host-header guard stays ON in network mode: accept loopback names plus
  `allowed_hosts` entries (case-insensitive exact match on the host part;
  IP literals compared as IPs). Unknown Host -> 403. (Blocks DNS rebinding.)
- Optional TLS: `tls_cert_file` + `tls_key_file` (both or neither; validated).
  The server binds its own listener (`net.Listen`) and serves with
  `http.Server.ServeTLS(ln, cert, key)` (with a 10s `ReadHeaderTimeout`), so
  TLS is explicit per listener rather than `ListenAndServeTLS`. Configure TLS
  for networked use. HTTP remains technically possible, but should only ever
  be considered over an encrypted network overlay protecting the entire
  client-to-router connection.
- The widget (`GET /`) stays unauthenticated but its data endpoints require a
  key (it already prompts for one and stores it in localStorage).

### Clients: host + ingest

- `clients[].host` attributes the client's proxied requests: proxy sets
  `RequestRecord.Host = client.Host`, defaulting to `host_name` when the
  client's `host` is empty (usage from un-attributed clients happens on this
  machine).
- `clients[].ingest: true` allows that key to call `POST /control/v1/ingest`.
  Other keys -> 403. (Ingest keys may also be normal inference keys.)

### Ledger: host column

- Migration v3: `host TEXT NOT NULL DEFAULT ''`; Record stores
  `RequestRecord.Host`; Summary supports `group=host` (key "" shown as
  "(server)" by the widget, NOT renamed in the API).
- `RecordBatch(ctx, []RequestRecord)` in one transaction, idempotent per ID
  (ON CONFLICT DO NOTHING); returns nil on duplicates. Max 1000 records.

### POST /control/v1/ingest

- Auth: bearer key of a client with `ingest: true`; body
  `core.IngestRequest` (max 4 MiB, `schema_version` must be 1, `host`
  non-empty, `[A-Za-z0-9._-]{1,64}`).
- The server OVERWRITES each record's `Host` with the request's `host`
  (the agent simply claims its own `host` label, validated as
  `[A-Za-z0-9._-]{1,64}`; trust model: ingest keys are trusted hosts), sets
  `Client = <authenticated client name>` (the key that pushed the batch, not
  whatever the agent claimed), and FORCES `Route="claude"`, `Provider="claude"`;
  records whose `AccountID` is not a configured claude account -> whole
  request 400 (no partial writes).
  Records must have an ID matching `[A-Za-z0-9:._-]{1,128}`,
  `UsageKnown=true`, usage token counts in [0, `core.MaxRecordTokens`] (1e12)
  and `started_at` within -400 days..+5 minutes of server time. `class` must
  be "", `interactive` or `background`; `model`, `session`, `task`, `agent`,
  `upstream_identity` and `failover_of` at most `core.MaxLabelBytes` (128)
  bytes and `error` at most `core.MaxErrorBytes` (256) bytes, each valid
  UTF-8 without control characters (the proxy truncates its own values to
  the same bounds); `status`, `latency_ms` and `bytes_out` non-negative.
  Otherwise 400 with `error.code` `invalid_record` (whole request rejected).
- Snapshots: each must name a configured claude account with
  `quota_source: agent`, else 400. Passed to `SnapshotIngester`; a snapshot
  with `FetchedAt` <= the stored one is ignored (counted in
  `snapshots_ignored`). Snapshots with `FetchedAt` more than 5 minutes in the
  future -> 400.
- Response 200 `core.IngestResponse`. Ingest is idempotent: re-sending the
  same batch yields 200 with records_accepted counting submitted records (the
  ledger dedupes silently).

### Quota: agent-sourced claude accounts

- Claude accounts with `quota_source: agent` are NEVER polled by the server
  (no credentials read, no HTTP). Their snapshot comes only from
  `IngestSnapshot`. Before any ingest, `Latest` returns ok=false (status shows
  stale/null, policy treats as missing -> background denied if reserved).
- `quota.Manager` implements `core.SnapshotIngester`: validates the account
  is claude+agent, stores a deep copy if newer, clears `Err` from the pushed
  snapshot unless the agent set it (agent-reported errors are preserved).

### `localrouter agent`

Runs on each host. Config file default `os.UserConfigDir()/localrouter/agent.yaml`
(Linux/BSD `~/.config/localrouter/agent.yaml`; macOS
`~/Library/Application Support/localrouter/agent.yaml`):

```yaml
server: https://router.example.com:8787     # recommended for networked clients
host: vm1                            # [A-Za-z0-9._-]{1,64}
key_file: ~/.config/localrouter/agent.key   # client key with ingest: true
account: claude-max                  # claude account id on the server
claude_projects_dir: ~/.claude/projects     # default
credentials:                         # how to read the Claude Code token (read-only)
  source: auto        # auto | file | keychain
  file: ~/.claude/.credentials.json  # default
  keychain_service: "Claude Code-credentials"  # macOS; default
  # keychain_account: defaults to $USER (Claude Code uses `-a $USER`; an
  # invalid username maps to "claude-code-user"). With CLAUDE_CONFIG_DIR set,
  # Claude Code appends "-<sha256(dir)[:8]>" to the service name.
push_interval: 1m                    # default
quota_interval: 5m                   # default; 0s disables quota push (duration)
state_dir: ~/.local/state/localrouter-agent   # default (darwin: ~/Library/Application Support/localrouter-agent)
```

- Usage: reuses `internal/claudelog.Collector` with a ledger implementation
  that buffers records and POSTs them via ingest (batches <= 500). claudelog
  must use `core.BatchLedger` when the ledger implements it and only persist
  file offsets after the batch succeeds (so a server outage never loses
  usage; records are re-sent later and deduped by ID).
- Quota: every `quota_interval`, read the token (read-only) and fetch
  `/api/oauth/usage` using the SAME parsing code as the server
  (export a function from internal/quota, e.g.
  `quota.FetchClaudeSnapshot(ctx, client, url, userAgent, token, expiresAt,
  subscriptionType, accountID, now) (core.Snapshot, error)`), then push it.
  Token expired -> push nothing (server keeps last snapshot, which goes stale).
- Credentials source `auto`: on darwin try keychain then file; elsewhere file.
  Keychain read = `security find-generic-password -a <account> -s <service> -w`
  (account default $USER, matching Claude Code; stdout is the same JSON as
  the file). Never write; never print the token; errors are
  sanitized.
- Exponential backoff on server errors: the delay never drops below the
  loop's interval and is capped at `max(5m, interval)`; logs never contain
  tokens, prompts, or file paths beyond the project dir name.
- `localrouter agent -config PATH [--once]`: `--once` = one scan + one quota
  push then exit (for testing/cron).

### Acceptance tests (multi-host)

- MH1 network mode without require_auth fails validation (config, DONE).
- MH2 Host guard: allowed_hosts entry accepted, unknown host 403, loopback ok.
- MH3 ingest with non-ingest key -> 403; no key -> 401.
- MH4 ingest overwrites Host/Client/Provider; unknown account -> 400, nothing written.
- MH5 ingest idempotent: same batch twice -> ledger rows unchanged.
- MH6 agent-sourced claude account never polled (fake usage server sees 0 hits);
  ingested snapshot appears in status; older snapshot ignored.
- MH7 agent end-to-end: real Collector over a fixture dir -> fake server via
  real HTTP handler -> rows in a real ledger with host set; server down ->
  offsets not advanced -> retried after recovery with no duplicates.
- MH8 proxy records Host from client config; usage?group=host aggregates.

## Hermes plugin (separate repo: ~/projects/hermes-localrouter)

Standalone Hermes user plugin (Python, stdlib only; installed into
`~/.hermes/plugins/localrouter/` or via `hermes plugins install`). It talks
to LocalRouter only through the documented control API.

- Config (plugin reads env/`config.yaml`-provided values via its own small
  JSON/YAML file `~/.config/localrouter/hermes-plugin.json`, never Hermes
  `.env`): `url` (default http://127.0.0.1:8787), `key_file`, `account_for_delegation`
  (optional, e.g. ollama-cloud), `model_for_delegation` (optional; used for
  admit by model), `fail_open` (default true), `timeout_s` (default 3).
- Tool `localrouter_status` (toolset `localrouter`): returns a compact
  JSON summary of every account: windows remaining %, reset times, reserve,
  background/interactive admissible, stale; plus 24h usage totals by model
  (tokens, cost, cost basis note). Read-only.
- Tool `localrouter_usage` (args: since=24h|7d, group=model|account|client|host).
- Hook `pre_tool_call` for `delegate_task`: POST /control/v1/admit
  `{class: background, model|account}`; deny -> `{"action":"block",
  "message": "LocalRouter: background quota reserve reached for <acct> (<reason>). Do this work yourself or wait until <reset>."}`;
  router unreachable -> allow if fail_open else block with a clear message.
  Never blocks other tools.
- Slash command `/quota`: prints the status summary for the human.
- No `pre_llm_call` injection (prompt-cache safety). No outbound calls except
  to the configured LocalRouter URL. Tests with `unittest` + a stdlib
  `http.server` fake; no network.

## Analytics (time series + drill-down)

### Host attribution

- Config `host_name` (default: short OS hostname, lowercased). Proxied
  requests from clients without `host` and the local `claude_logs` collector
  record `Host = host_name`. Agents record their own `host`.
- `localrouter ledger relabel-host --from "" --to <name> [-config PATH]`
  updates rows whose host equals `--from` (default "") to `--to` (one
  transaction, prints count). Used once to backfill pre-attribution history.

### Ledger: `Analytics(ctx, core.AnalyticsQuery) (core.AnalyticsResult, error)`

- Validation: Group in core.AnalyticsDimensions; filter keys in the same set;
  Bucket "hour"|"day"; From < To; span <= 400 days; hour buckets <= 31 days
  (else error). TopN 0 -> 8, max 50.
- Buckets: "hour" = UTC-aligned hours; "day" = LOCAL calendar days
  (time.Local, DST-correct: bucket boundaries are local midnights; a 23h or
  25h day is one bucket). BucketStarts covers [From, To) from the bucket
  containing From to the bucket containing To-1ns.
- SQL aggregates by (bucket key, group key) over
  `started_at >= From AND started_at < To` + filters; bucketing of local days
  happens in Go (do NOT trust SQLite localtime). Must handle 1M rows in < 1s
  on a laptop: aggregate in SQL by hour (`started_at/3600000`), then fold
  hours into local days in Go. Add index (started_at, host) if useful
  (migration v4).
- Sums use the existing overflow-safe approach (REAL/TOTAL); counts
  (requests, unknown_usage, unpriced) per point; cost_usd per point nil if
  the point has requests but none priced, else sum of priced; same rule as
  Summary.
- Rank keys by total tokens (input+output) desc, then key asc. Breakdown =
  the highest-ranked `core.AnalyticsMaxBreakdown` (200) keys in rank order
  (UsageRow.Key = group value, "" allowed); `breakdown_omitted` counts the
  remaining keys, which appear only in Totals and `__other__`. Series = top N
  in rank order, then `__other__` summing the rest (omitted if none). Every
  series has exactly len(BucketStarts) points (zeros where empty). Keys are
  ranked first and per-bucket points are kept only for the top N plus
  `__other__`, so memory does not grow with the number of distinct keys.
- Totals = sum over everything matched.

### HTTP

- `GET /control/v1/analytics?from=RFC3339&to=RFC3339|range=24h|7d|30d|90d
  &bucket=hour|day&group=host&filter.<dim>=<value>&top=8` (auth like
  usage). `range` is relative to now (to=now); `from`/`to` override. Default
  range=7d, bucket=day for ranges > 48h else hour. Filter values: use
  `filter.host=` (empty) to match empty. 400 on invalid input.
- `GET /control/v1/analytics/dimensions?range=30d` -> `{"dimensions":
  {"host":[{"key":..,"requests":..,"tokens":..}],...}}` distinct values per
  dimension in range (top 50 each, ranked by tokens) for filter dropdowns.
- `/control/v1/usage?group=` accepts any summary dimension (account, model,
  class, client, host, route, task, agent).

### Widget (internal/control/static/index.html, single file, no external
loads; CSP unchanged: inline script/style only, connect-src 'self')

A polished dashboard that respects `prefers-color-scheme` (light + dark),
system font stack, and also renders well narrow (Hermes side pane ~420px)
and wide (browser). Sections:

1. **Header**: title, last-updated, range picker (24h / 7d / 30d / 90d),
   metric picker (tokens | cost | requests), group-by picker (host, model,
   account, client, task, agent, class, route), key box (when auth needed).
2. **Quota cards** (existing data, restyled): per account a card with
   provider badge, plan, each window as a horizontal bar with remaining %,
   reserve marker, reset countdown, admissibility chips (interactive /
   background) and stale/error state. Provider-reported model counts (Ollama)
   as a compact list under the account.
3. **Stacked chart** (hand-written SVG, no libraries): stacked bars per
   bucket for the selected metric, one color per series (stable color per
   key across reloads via hash -> palette; `__other__` grey). Hover/focus a
   bar segment -> tooltip with bucket, key, value, and share. Legend chips
   toggle a series on/off. Y-axis with nice ticks and human units
   (1.2M tokens, $3.40). Bucket labels adapt (hours vs dates).
4. **Drill-down**: clicking a series (legend chip or bar segment) adds a
   filter `<group>=<key>` (breadcrumb chips above the chart, each removable,
   "All" resets) and advances group-by to the next sensible dimension
   (host -> model -> task -> agent; model -> host; account -> model;
   task -> model; otherwise model). Clicking a bar's bucket (x-axis label)
   zooms the time range to that bucket (day -> hourly view of that day).
   State is kept in the URL hash (`#range=7d&group=model&f.host=pf3llssv`) so
   reload/back works.
5. **Breakdown table** under the chart: the Breakdown with columns key,
   requests, input, cached (and % cache hit), cache write, output, cost,
   share bar (inline CSS bar). Sortable by clicking headers. Rows clickable
   = drill down. When `breakdown_omitted` > 0 a note under the table reads
   "N more not shown".
6. **Machines strip**: one compact card per host (from a host-grouped query
   for the current range, unfiltered): tokens, cost, requests, last seen
   (max started_at is NOT available from the API — show a 24h sparkline of
   tokens instead from an hourly host-grouped query).
- Accessibility: buttons are real <button>s, focus styles, chart has
  aria-label summary and the table is the accessible equivalent.
- Performance: at most 5 fetches per refresh (status, view, host totals, host 24h, client 24h); auto-refresh every 30s only
  when the tab is visible; never re-render the chart while hovering (defer).
- Security: all text via textContent; no innerHTML with data; key in
  localStorage as today.

### Per registered client

- `/control/v1/status` adds `clients: [{name, class, host, ingest}]` listing
  every configured client in config order (never key material).
- Ingested Claude transcript records are attributed to the REGISTERED client
  whose key pushed them: `Client = <ingesting client name>` (was the generic
  "claude-code"); `Route = "claude"` and `Provider = "claude"` still identify
  them as Claude Code usage. The local `claude_logs` collector uses
  `claude_logs.client` (default "claude-code") so a single-host setup can
  name it after a registered client too.
- Widget: a **Clients** panel beside Machines: one row/card per registered
  client (zero-usage clients shown greyed), plus any unregistered client keys
  seen in data (e.g. "claude-code"), each with class badge, host, tokens, cost,
  requests for the current range and a 24h sparkline; click = drill down
  `client=<name>`.

## OpenRouter (prepaid account)

`provider: openrouter` (`core.ProviderOpenRouter`). Config: `api_key_file` or
`api_key_env` required; `base_url` default `https://openrouter.ai/api/v1`;
`cost_basis` default `metered`; optional `management_key_file` /
`management_key_env` (openrouter only; paths expanded like other key files);
`reserve` must be empty (no 5h/weekly windows). Inference is forwarded like
`openai_compat` (`<base_url>/chat/completions`, `<base_url>/responses`).

Quota (`internal/quota`), both endpoints derived from the account `base_url`
so credentials only go to that host, polled with the normal interval plus
debounced post-request / urgent refreshes:

- `GET <base_url>/credits` → `{data:{total_credits, total_usage}}`, both
  required non-negative finite numbers. `core.Credits{TotalCreditsUSD,
  TotalUsageUSD, BalanceUSD = credits - usage (signed), FetchedAt, Err}`.
  Uses the management credential (a separate `auth.Manager`, supplied via
  `quota.Options.ManagementCredentials`) when configured, else the inference
  credential. 403 without a management key → `Err` "management key required";
  explicit management-key failures are reported as such (401 invalidates and
  retries once, management key only).
- `GET <base_url>/key` (inference credential only) → `core.KeyUsage`.
  `limit` and `limit_remaining` must be present (null = no cap; zero kept);
  a finite `limit` with a null `limit_remaining` (or a null `limit` with a
  finite `limit_remaining`) is a contradictory pair rejected as malformed, and
  an invalid response never marks the account healthy or available.
  `usage` required ≥ 0; `usage_daily|weekly|monthly`, `byok_usage*` optional
  (nil when absent/null, never 0), must be ≥ 0; `include_byok_in_limit`,
  `is_free_tier` optional bools; `limit_reset` optional string
  (`daily|weekly|monthly` → `LimitResetAt` = next 00:00 UTC / Monday / 1st;
  informational only, see policy below).
  Unknown fields (label, deprecated `rate_limit`) ignored; BYOK never summed
  into `usage`; remaining is never derived from lifetime usage.
- Each part keeps last-good values, its own `FetchedAt`, and `Err` on failure;
  a successful `/key` never refreshes or clears `/credits`. Before the first
  success a part has zero `FetchedAt` (unknown). `Snapshot.FetchedAt` = the
  oldest successful part; `Snapshot.Err` joins part errors. No windows, no
  `Allowed`. Error strings never include bodies or credentials.

Policy: a known `BalanceUSD <= 0`, or a known key cap with
`LimitRemainingUSD <= 0`, denies both classes regardless of staleness. The
predicted cap reset (`LimitResetAt`) is informational only: reaching it never
restores spending credit, so a stale exhausted cap keeps denying both classes
until a later successful `/key` response observes a positive
`LimitRemainingUSD`; only such a fresh observation reopens the gate (the
periodic response recomputes `LimitResetAt` for display only). A key reset
never overrides account exhaustion. An unknown or malformed key part never
denies on its own (no invented exhaustion), so unknown parts follow the normal
stale rule (no reserve → allow; upstream 402 is the backstop). Upstream 402 on
an openrouter account is request-scoped when the latest snapshot proves funds:
credits fetched within the last 10 minutes with a positive balance and no
exhausted key cap. That 402 (e.g. an unaffordable `max_tokens`) is relayed to
the client as the final answer, with no failover (every account would answer
the same, so a replay only amplifies) and no cooldown. Any other 402 is
account-level: proxy fails over (no bytes sent yet; streamed/completed
responses are never repeated), outcome carries the `Retry-After` hint, policy
cools down for `max(60s, hint)` and requests an urgent refresh. A 402 cooldown
clears early only when a balance fetched after it is positive and above the
balance known at the 402. Other providers' 402 handling is unchanged.
OpenRouter 403 (moderation-flagged input) is final and request-scoped: relayed
without credential refresh, failover or cooldown; 401 keeps the refresh-once,
then account-level handling. OpenRouter 429 (per-model or upstream-provider
rate limit) is request-scoped unless the snapshot shows a known non-positive
balance or an exhausted key cap; it may still fail over to the next account
(another key may not be limited), bounded by the failover limit.

Control status adds, for openrouter accounts only, `credits:{available,
balance_usd, total_credits_usd, total_usage_usd, exhausted, age_s, stale,
error}` (amounts null when unavailable) and `key:{available, unlimited,
limit_usd, limit_remaining_usd, limit_reset, limit_reset_at, exhausted,
usage_usd, usage_{daily,weekly,monthly}_usd, byok_usage{,_daily,_weekly,
_monthly}_usd, include_byok_in_limit, is_free_tier, age_s, stale, error}`.
`key.limit_reset_at` is informational (predicted reset, display only): status
`key.exhausted` follows the same authoritative rule as admission and stays
true for a zero-remaining cap even after that predicted reset passes, until a
fresh `/key` response observes a positive `limit_remaining`. An unknown key
part is never reported exhausted.
The widget shows the signed balance (e.g. `-$0.08`; a non-zero amount below one
cent keeps its sign as `<$0.01` / `-<$0.01` rather than rounding to `$0.00`),
"unavailable" rather than $0, and "unlimited" for a null cap.

## Engineering constraints

- Go 1.26.8 or newer (see `go.mod`), module `github.com/hpst3r/localrouter`.
- Allowed deps: `gopkg.in/yaml.v3`, `modernc.org/sqlite`,
  `golang.org/x/sync`. Nothing else without architect approval.
- `go vet ./...` and `go test -race ./...` must pass.
- Builds/tests: `GOFLAGS=-p=4`, run tests with `-p 4`. Never unbounded
  parallel builds.
- No network calls in tests (use `httptest`).
