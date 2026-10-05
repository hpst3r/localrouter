# LocalRouter MVP — Specification (contract for all packages)

LocalRouter is a single Go binary: a loopback OpenAI-compatible proxy that
selects an upstream subscription account per request, enforces per-account
quota reserves by workload class, records token usage + estimated cost in
SQLite, and exposes a small read API + status widget.

Non-goals for MVP: context composition, local tokenization/estimation,
forecasting engine, Prometheus, trace explorer, multi-user auth, TLS,
chat-completions→Responses translation for Codex.

## Packages and ownership

| Package | Owns | Depends on |
|---|---|---|
| `internal/core` | Shared types + interfaces (frozen; architect-owned) | stdlib |
| `internal/config` | YAML config load/validate (architect-owned) | core |
| `internal/quota` | Codex + Ollama quota fetchers, poller, passive header observation | core |
| `internal/policy` | Admission gate, reserves, in-flight leases, cooldowns, account selection | core |
| `internal/ledger` | SQLite request ledger, summaries, pricing table + cost | core |
| `internal/auth` | Codex OAuth device login, token store, single-flight refresh; static API keys | core |
| `internal/proxy` | HTTP inference surface, client auth, forwarding, SSE usage capture, failover | core |
| `internal/control` | `/control/v1/*` JSON API + embedded HTML widget | core |
| `cmd/localrouter` | Wiring + CLI (architect-owned) | all |

Packages MUST import only `core` (and `config` where noted by architect) —
never each other. Use fakes of `core` interfaces in tests.

## Wire surfaces

Listen default `127.0.0.1:8787`. Refuse non-loopback bind unless
`allow_non_loopback: true`.

Inference (client bearer required, `Authorization: Bearer <client key>`):

- `POST /v1/responses` — Responses API. For `provider: codex` upstream is
  `https://chatgpt.com/backend-api/codex/responses`. For `provider: ollama`
  / `openai_compat` upstream is `<base_url>/responses`.
- `POST /v1/chat/completions` — forwarded to `openai_compat`/`ollama`
  accounts only. A route resolving to a codex account on this path returns
  400 `{"error":{"message":"codex accounts only serve /v1/responses"}}`.
- `GET /v1/models` — synthesized from config `routes[].models` (exact names).

Body is forwarded verbatim except:
- `chat/completions` with `"stream": true`: set
  `stream_options.include_usage = true` (preserve other stream_options).
- Codex `/responses`: no body changes.

Request headers forwarded: `Content-Type`, `Accept`, `OpenAI-Beta`,
`session_id`, `conversation_id`, `x-request-id`. Client `Authorization` is
DROPPED; upstream credential headers come from `core.Credential`.
Response streamed back unchanged (SSE flush per event). Hop-by-hop headers
stripped.

Optional attribution headers from clients (stored, never forwarded):
`X-LocalRouter-Session`, `X-LocalRouter-Task`, `X-LocalRouter-Agent`.
`X-LocalRouter-Class: background` lets an interactive-class key downgrade
itself; a background key can never upgrade.

## Workload classes

`interactive` and `background`. Each client key maps to one class.
Reserves protect capacity *from* `background`. `interactive` may consume an
account down to 0% (until upstream refuses).

## Quota model

`core.Window{Kind, UsedFrac 0..1, ResetAt, WindowSeconds}`; kinds `5h`, `weekly`.

Codex: `GET https://chatgpt.com/backend-api/wham/usage` with
`Authorization: Bearer <access>`, `ChatGPT-Account-Id: <id>`,
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
`Authorization: Bearer <api key>`. `limits.session.usage` → `5h`,
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
cleared early if a fresh snapshot shows headroom.

Selection: routes give an ordered account list per class. First admissible
account wins. Admission is atomic with lease creation (mutex) to prevent the
check-then-act race: N concurrent background requests near the floor must
not all pass. `inflight(A)` counts open leases.

Lease release reports outcome {HTTP status, usage known?, bytes streamed}.
Failover: proxy may retry on the next admissible account ONLY if no response
bytes were sent to the client and status was 429/401/403/5xx-before-body.
Max 2 failovers. Each attempt is a separate ledger row.

## Ledger

SQLite (pure Go `modernc.org/sqlite`), WAL, file `<data_dir>/localrouter.db`.
One table `requests` (no prompt/response content, ever):

`id, started_at, finished_at, client, class, route, model, provider,
account_id, upstream_identity, status, failover_of, input_tokens,
cached_input_tokens, output_tokens, reasoning_tokens, usage_known,
cost_usd, cost_basis, latency_ms, bytes_out, session, task, agent, error`.

`cost_basis`: `api_equivalent` (subscription accounts: what it would cost at
API list price), `metered` (API-key billing), or NULL if unpriced. Unknown
price ⇒ `cost_usd` NULL, never 0.

Pricing file `pricing.yaml`: per model, USD per 1M `input`, `cached_input`,
`output` (reasoning billed as output). Shipped EMPTY of numbers; user or
`localrouter pricing import <litellm json>` populates it. Never invent prices.
Cost = (input−cached)·in + cached·cached_in + output·out, all /1e6.

## Usage capture

- Responses SSE: event `response.completed` (also `response.incomplete`,
  `response.failed`) → `response.usage.{input_tokens,
  input_tokens_details.cached_tokens, output_tokens,
  output_tokens_details.reasoning_tokens}`. Non-stream: top-level `usage`.
- Chat completions: `usage.{prompt_tokens, completion_tokens,
  prompt_tokens_details.cached_tokens, completion_tokens_details.reasoning_tokens}`
  from the final chunk / body.
- Client disconnect or missing usage ⇒ `usage_known = false`; lease still
  released.
- Parser must handle multi-line `data:` fields and `\r\n`, and must not
  buffer the entire stream (scan incrementally; keep ≤ 4 MiB per event).

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
- Credential headers: `Authorization: Bearer <access>`,
  `ChatGPT-Account-Id: <id>`, `originator: codex_cli_rs`.
- Store: `<data_dir>/tokens/<account-id>.json`, dir 0700, file 0600.

Ollama / openai_compat: static key from `api_key_file` (preferred) or
`api_key_env`. Header `Authorization: Bearer <key>`.

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
  error, reason}]}` — `healthy` = no active cooldown and no fetch error;
  `reason` = policy explanation when a class is not admissible; each window
  also carries `rolled` (reset passed, used reported as 0).
- `GET /control/v1/usage?since=24h&group=account|model|class|client` →
  rows `{key, requests, input_tokens, cached_input_tokens, output_tokens,
  reasoning_tokens, cost_usd, unknown_usage_requests}`.
- `POST /control/v1/admit` body `{class, model}` → dry-run
  `{decision: allow|deny, account_id, reason}` (does NOT create a lease).
- `GET /` → single embedded HTML page (no external assets/CDNs), polls
  `/control/v1/status` and `/control/v1/usage?since=24h&group=account`
  every 15s; per account: bars for 5h/weekly remaining with a reserve marker,
  reset countdown, health; summary tokens + cost for 24h/7d.
- `GET /healthz` → `ok`.

## Logging

`log/slog` text to stderr. Never log tokens, keys, prompt/response bodies,
or full upstream URLs with query strings. Log: account chosen, class,
model, status, latency, usage numbers, policy denials with reason.

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
  `Authorization: Bearer <token>`, `anthropic-beta: oauth-2025-04-20`,
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
- Record fields: Client "claude-code", Class "interactive", Route "claude",
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
  http://127.0.0.1:8787] [--json]`: calls the control API; exit 0 = allow,
  1 = deny (prints reason), 2 = error/unreachable. Clients use it as a gate,
  e.g. before launching background Claude workers.

## Multi-host (central server + per-host agents)

One `localrouter serve` runs centrally (mesh-reachable). Each host that runs
Claude Code runs `localrouter agent`, which pushes Claude transcript usage and
Claude quota snapshots to the server. Hosts' Hermes/other clients use the
server's `/v1` with their own client keys.

### Server network mode (config)

- `allow_non_loopback: true` permits a non-loopback `listen`, and REQUIRES
  `control.require_auth: true` (validated).
- Host-header guard stays ON in network mode: accept loopback names plus
  `allowed_hosts` entries (case-insensitive exact match on the host part;
  IP literals compared as IPs). Unknown Host -> 403. (Blocks DNS rebinding.)
- Optional TLS: `tls_cert_file` + `tls_key_file` (both or neither) ->
  `ListenAndServeTLS`. Mesh (WireGuard) traffic is already encrypted, so TLS
  is optional.
- The widget (`GET /`) stays unauthenticated but its data endpoints require a
  key (it already prompts for one and stores it in localStorage).

### Clients: host + ingest

- `clients[].host` attributes the client's proxied requests: proxy sets
  `RequestRecord.Host = client.Host` (empty -> "").
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
  (agents cannot attribute to another host... except via their own `host`
  string; trust model: ingest keys are trusted hosts) and FORCES
  `Client="claude-code"`, `Provider="claude"`; records whose `AccountID` is
  not a configured claude account -> whole request 400 (no partial writes).
  Records must have non-empty ID, `UsageKnown=true`, non-negative usage;
  otherwise 400.
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

Runs on each host. Config file (default `~/.config/localrouter/agent.yaml`):

```yaml
server: https://router.tail:8787     # or http:// on the mesh
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
quota_interval: 5m                   # default; 0 disables quota push
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
- Exponential backoff (cap 5m) on server errors; logs never contain tokens,
  prompts, or file paths beyond the project dir name.
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

## Engineering constraints

- Go 1.26, module `github.com/hpst3r/localrouter`.
- Allowed deps: `gopkg.in/yaml.v3`, `modernc.org/sqlite`,
  `golang.org/x/sync`. Nothing else without architect approval.
- `go vet ./...` and `go test -race ./...` must pass.
- Builds/tests: `GOFLAGS=-p=4`, run tests with `-p 4`. Never unbounded
  parallel builds.
- No network calls in tests (use `httptest`).
