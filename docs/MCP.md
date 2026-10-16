# MCP adapter (`localrouter mcp`)

`localrouter mcp` serves a read-only Model Context Protocol server on stdio in
front of a running LocalRouter. It exposes exactly three tools — `quota_status`,
`usage_summary` and `check_admission` — implemented in `internal/mcpserver` and
wired into the existing binary by `cmd/localrouter/mcp.go`. Same binary: no
runtime added, no daemon, no listening port.

The adapter is a thin HTTP client of the control API described in
[SPEC.md](SPEC.md) ("Control API"). It forwards to three fixed paths, validates
the responses strictly, passes the upstream documents through unchanged, labels
what it cannot interpret, and never invents a field, a default or a decision. It
is not a ledger, a policy engine or an inference path; it never reads a provider
credential, and the only secret it touches is the client bearer key file named
by `--key-file`.

## Goal

An agent (Hermes, Claude Code, any MCP client) can ask three questions without
new credentials, database access or server changes:

- how much quota headroom exists right now, and how fresh that reading is;
- what usage the router actually recorded over a window;
- whether a workload class would be admitted on an account or a model's route.

## Fixed rules

1. **HTTP adapter only.** It forwards to `GET /control/v1/status`,
   `GET /control/v1/usage` and `POST /control/v1/admit`. Those three paths are
   compile-time constants: no tool argument can choose a URL, host or path.
2. **Read-only.** It never opens the router database, ledger, configuration or
   provider credentials, and never touches `/control/v1/ingest` or the
   inference API. `check_admission` is a dry run that takes no lease.
3. **No policy of its own.** Every decision, reserve, window and freshness
   figure comes from the server. The adapter validates, labels and bounds the
   upstream documents; it never invents a field or a default.
4. **Same binary, no new service.** `localrouter mcp` is a subcommand of the
   existing Go binary (official MCP Go SDK, stdio transport). No Python
   runtime, no daemon, no listening port.
5. **Loopback by default, HTTPS off-host.** Plaintext `http` is accepted only
   for a loopback host; any other host must use `https`. A URL carrying
   userinfo, a query string, a fragment or a path is refused at startup, so a
   misconfigured `--url` can never send the client key in the clear.
6. **Never fabricate.** Stale, empty, unpriced and unknown-usage inputs are
   preserved as such. A transport or malformed-response problem is an error
   result, never an `allow` and never a zero.

## Launch

```text
localrouter mcp [--url URL] [--key-file FILE] [--timeout 10s] [--max-result-chars 24000]
```

| flag | default | meaning |
|------|---------|---------|
| `--url` | `http://127.0.0.1:8787` | control API base URL; normalized to `scheme://authority`. Loopback may be `http`; every other host must be `https`. No userinfo, query, fragment or path. |
| `--key-file` | none | file holding a client bearer key; needed ONLY when the server sets `control.require_auth`. Read once at startup. |
| `--timeout` | `10s` | per-request timeout; must be positive and at most `5m`. |
| `--max-result-chars` | `24000` | Compact serialized CallToolResult budget in UTF-8 bytes; at least `1024` (the flag name is retained for compatibility). |

MCP client configuration (host `command` + `args`; use absolute paths when the
client does not inherit your `PATH`):

```json
{
  "mcpServers": {
    "localrouter": {
      "command": "localrouter",
      "args": [
        "mcp",
        "--url", "http://127.0.0.1:8787",
        "--key-file", "/absolute/path/to/client.key",
        "--timeout", "10s",
        "--max-result-chars", "24000"
      ]
    }
  }
}
```

Omit `--key-file` for a stock loopback server (the read endpoints are
unauthenticated unless `control.require_auth: true`). For a remote central
router use `--url https://<router-host>` with the same key file an approved
client already uses.

Notes:

- **stdout carries JSON-RPC only.** Nothing else may be written to stdout by the
  process. Flag/usage diagnostics and start-up errors go to stderr through the
  ordinary CLI error path.
- The advertised server identity is `localrouter-mcp` (the adapter's own version,
  not the router's). Protocol version negotiation is the official Go SDK's and the
  adapter pins nothing: `github.com/modelcontextprotocol/go-sdk` (currently
  v1.8.0) supports `2026-07-28` down to `2024-11-05` and offers its newest first,
  so the version a session negotiates depends on the client.
- **stdio is a persistent request/response session.** Keep stdin open until all
  replies have been read. Do not pipe a batch of requests into the process and
  close stdin immediately: the SDK treats EOF as session termination and may
  discard pending responses. Use a conforming MCP host or the smoke harness;
  the adapter is not a one-shot JSON command.
- Registering the adapter with a client is an operator step outside this
  repository's changes. The repo ships the command; it does not install,
  register or reconfigure anything.

## Tools

All three tools carry MCP annotations `readOnlyHint: true`, `idempotentHint:
true`, `destructiveHint: false`, `openWorldHint: false`, and each takes a closed
argument object (`additionalProperties: false`) that is decoded strictly:

- the argument value must be a single JSON object (omit it or pass `{}` for no
  arguments);
- every key must be one the tool declares — an unknown field is a rejected call,
  not an ignored typo;
- a value must have the declared JSON type, with no coercion (`"7"` is not the
  integer `7`, `true` is not a string);
- duplicate keys, explicit null values, blank strings, and surrounding whitespace
  are rejected. Only omitted optional fields receive defaults.

A violation is `invalid_arguments` before any request leaves the process. Every
successful result is returned as **structured content and identical JSON text**,
so text-only and schema-only hosts read the same document.

Each tool also advertises a JSON **output schema** (`outputSchema`), so a host can
validate a result without guessing. Every output schema is an object that accepts
either the success document or the failure envelope (`anyOf` requires
`retrieval`/`source`/`counts` — or `decision`/`admitted` for an admission result —
on one branch, and `error` on the other), and it pins the adapter's own fields
(`retrieval`, `source`, `counts`, `cost_note`, `decision`, …) while declaring the
opaque upstream objects inside `accounts`, `clients`, `rows`, and `budget`
as objects that permit additional properties. Adapter-owned `retrieval` and
`source` blocks remain closed. That is deliberate: the adapter passes upstream objects through
verbatim, so their fields are whatever the control API reported and the schema
must not pretend otherwise. The `check_admission` **input** schema likewise
encodes `oneOf` exactly one of `account`/`model`; the handler re-checks it at
runtime with the same `invalid_arguments` result.

### `quota_status`

| argument | type | meaning |
|----------|------|---------|
| `account` | string, optional | exact account ID; when given, only that account is returned, and an unknown ID is `not_found` (not an empty list). Omit to report every account. |

Forwards to `GET /control/v1/status`. The response is validated structurally and
strictly: it must be a JSON object with an `accounts` array of objects; `clients`,
when present, must also be an array of objects; `now` must be a string when
present; `schema_version` must be a positive integer when present. A null/missing
array, a null or non-object element, or a wrong-typed field is `bad_response`,
never an empty or stringified success. Each account element is passed through
verbatim, so freshness and policy fields survive intact: `windows` (`kind`,
`used_frac`, `remaining_frac`, `reset_at`, `window_seconds`, `rolled`),
`reserve`, `background_admissible`,
`interactive_admissible`, `reason`, `snapshot_age_s`, `stale`, `error`,
`cooldown_until`, `inflight`, `model_requests` (provider-reported counts,
including traffic that bypassed the router), and `credits`/`key` for OpenRouter
accounts. The registered client list (`clients`) is passed through too, and never
contains key material.

```json
{
  "retrieval": {"retrieved_at": "…", "truncated": false, "dropped_items": 0, "max_result_chars": 24000},
  "source": {"endpoint": "/control/v1/status", "upstream_schema_version": 1},
  "now": "…",
  "accounts": [ { "id": "…", "provider": "…", "healthy": true, "windows": [ … ], "reserve": { … }, "…": "…" } ],
  "clients": [ { "name": "…", "class": "…", "ingest": false } ],
  "counts": {"accounts_returned": 1, "accounts_total": 1, "clients_returned": 1, "clients_total": 1}
}
```

### `usage_summary`

| argument | type | default | bounds |
|----------|------|---------|--------|
| `since` | string | `24h` | Go duration (`24h`, `168h`) or whole days (`7d`); positive, at most `90d` |
| `group` | enum | `model` | `account`, `model`, `class`, `client`, `host`, `route`, `task`, `agent` |
| `limit` | integer | `20` | `1`–`100` rows |

There are **no filters**: the control API usage endpoint exposes none, so the
tool exposes none. The tool is deliberately tighter than the endpoint on the
window: the server accepts `since` up to 400 days, the tool caps at 90. The
response is validated the same strict way: a `rows` array of objects is required,
and `since` must be a string and `schema_version` a positive integer when
present. A null/missing `rows`, a null or non-object element, or a wrong-typed
field is `bad_response`.

Forwards to `GET /control/v1/usage?since=&group=`. Rows are passed through
verbatim with the ledger's own fields — `key`, `requests`, `input_tokens`,
`cached_input_tokens`, `cache_creation_input_tokens`, `output_tokens`,
`reasoning_tokens`, `cost_usd`, `unknown_usage_requests`, `unpriced_requests` —
so token accounting and the two "missing data" counters are neither re-derived
nor dropped. No `cost_basis` is added: the endpoint does not return one.

```json
{
  "retrieval": {"retrieved_at": "…", "truncated": false, "dropped_items": 0, "max_result_chars": 24000},
  "source": {"endpoint": "/control/v1/usage", "upstream_schema_version": 1},
  "since": "…",
  "group": "model",
  "rows": [ { "key": "…", "requests": 12, "input_tokens": 0, "cost_usd": 0.0, "unpriced_requests": 0, "…": "…" } ],
  "counts": {"rows_returned": 1, "rows_total": 1, "limit_applied": 20, "rows_truncated": false},
  "cost_note": "…"
}
```

`cost_note` labels every `cost_usd` value, because the control API publishes one
resolved figure per row and does not expose the per-row basis. It states plainly
that these are the ledger's recorded figures — not an invoice and not actual
spend — that a single aggregate can mix bases, and that requests with no usable
price are counted in `unpriced_requests` (and requests whose tokens could not be
metered in `unknown_usage_requests`) instead of being shown as zero. The bases a
recorded figure can have are a local **metered** estimate (API-key billing), a
local **api_equivalent** estimate (subscription accounts: what the request would
cost at API list price — a list-price comparison, not money spent), or the figure
a provider such as OpenRouter itself reported for that request. Which one applies
to a given row cannot be read off the result, so cite a `cost_usd` as the ledger's
recorded cost unless you can establish the basis from the router's own ledger or
pricing configuration.

The two missing-data counters mean different things and are independent:

- `unknown_usage_requests` counts requests whose **token usage** the router could
  not meter;
- `unpriced_requests` counts requests whose **price** is unknown, so `cost_usd`
  is null (never 0) for them.

A request can be metered but unpriced, priced but unmetered (a provider-reported
cost with no parseable tokens), or, when neither is known, counted only in
`unknown_usage_requests`. Read them separately; neither is a synonym for the
other or for "missing money".

### `check_admission`

Exactly one of `account` or `model`, plus an optional `class`.

| argument | type | default | meaning |
|----------|------|---------|---------|
| `account` | string | — | account ID to dry-run |
| `model` | string | — | model whose configured route to dry-run |
| `class` | enum | `background` | `background` or `interactive` |

Exactly one of `account`/`model` is required; both or neither, explicit null,
or blank values are `invalid_arguments`. Forwards to `POST
/control/v1/admit` with `{class, model}` or `{class, account}`.

```json
{
  "retrieval": { "retrieved_at": "…", "truncated": false, "dropped_items": 0, "max_result_chars": 24000 },
  "source": {"endpoint": "/control/v1/admit", "upstream_schema_version": null},
  "decision": "allow",
  "admitted": true,
  "account_id": "…",
  "reason": "…",
  "budget": { "…": "advisory estimate, present only when spend controls are configured" }
}
```

A **deny is a normal, successful answer** (`isError` false) — it is a policy
result, not a tool failure. The adapter requires a definite `allow`/`deny`
string: an admission body with neither is `bad_response`, and a `schema_version`
that is present but not a positive integer is `bad_response` too. `budget` is
passed through only when the server sent a JSON object; a non-object `budget`
field is ignored. When present it is the server's advisory spend-control estimate
— it creates no reservation and never changes the decision. An admission result
that cannot fit the result budget is reported as a bounded `bad_response` rather
than a silently truncated decision. The control API's admit response normally
carries no `schema_version`, so `source.upstream_schema_version` is typically
`null` there — the example above shows that case.

The tool exposes no capability profile (`protocol`, `modalities`, `tools`,
`json_schema`, `stream`). On a route that has opted into capability routing the
server therefore answers `capability_requirements_required` (HTTP 400), which
the adapter surfaces as an error code of the same name — a refusal, never an
allow. Use the `localrouter admit` CLI with its requirement flags when a
capability-profile dry run is needed.

## Mapping to the control API

| tool | method and path | forwarded arguments |
|------|-----------------|---------------------|
| `quota_status` | `GET /control/v1/status` | none (account selection happens locally on the returned document) |
| `usage_summary` | `GET /control/v1/usage` | `since`, `group` (fixed keys with adapter-validated values) |
| `check_admission` | `POST /control/v1/admit` | `{class}` plus exactly one of `model`/`account` |

`limit` and `max_result_chars` are adapter-side bounds: the control API has no
row limit and no content budget, so the adapter truncates and says so.

## Timeouts, budgets and result size

- `--timeout` (default 10s, maximum 5m) bounds each HTTP round trip and is the
  per-tool call deadline; the same context is passed to the HTTP request, so an
  MCP cancellation ends the call as `server_timeout`.
- `--max-result-chars` (default 24000, minimum 1024) bounds the **serialized tool
  result the host receives**, not just the document: both copies of it (the JSON
  text content, which is byte-identical to the structured content), the
  `retrieval`/`source`/`counts` envelope and the result metadata. The budget is
  measured with final metadata and both escaped content copies included. The
  outer JSON-RPC transport envelope is excluded.
- Arrays are bounded element by element — never by slicing encoded JSON — so every
  kept element stays valid on its own. Accounts and clients are fitted together
  for `quota_status`; rows are fitted for `usage_summary`.
- Truncation is always reported, never silent. `retrieval.truncated` /
  `retrieval.dropped_items` count every element dropped, whether by the `limit`
  or by the content budget; `counts.rows_truncated` is true when either dropped a
  row (with `rows_returned`/`rows_total` for the real total), and
  `accounts_returned`/`accounts_total` and `clients_returned`/`clients_total`
  distinguish "returned" from "exists".
- A document that cannot fit the budget even with zero elements is `bad_response`,
  and so is an admission result that cannot fit: a decision and its reason are
  never silently truncated.
- Error results are bounded too: a message can quote a caller-supplied string (an
  unknown argument name, an unknown account ID) or a long upstream field, so an
  over-long message is shortened with an explicit marker and, if even that cannot
  fit, reduced to the bare error code — the failure envelope stays valid either way.
- Upstream bodies are read under a 1 MiB cap; a larger, empty or non-object body
  is `bad_response`.
- There is no caching, polling or background refresh: one tool call is one
  request. The server already polls providers, so a snapshot read twice inside a
  wave is the same snapshot unless its age moved.
- No automatic retries: a failed call is reported, and retrying is the caller's
  decision.

## Errors

A failed tool call sets `isError: true` and returns
`{"error": {"code": "…", "message": "…", "detail": "…"}}` as both text and
structured content. Branch on `code`; the message may change.

| code | meaning |
|------|---------|
| `invalid_arguments` | rejected before any request left the process: unknown field, wrong type, out-of-range value, or a missing/duplicated/`null` account-model choice |
| `auth_required` | control API answered 401 — pass `--key-file` |
| `forbidden` | control API answered 403 — key refused |
| `not_found` | control API answered 404 — unknown account or model (including an unknown `account` in `quota_status`) |
| `endpoint_unavailable` | control API answered 405/501/503/502/504 (e.g. ledger unavailable) |
| `bad_response` | redirect (not followed), unreadable/oversized/non-object body, a null or missing `accounts`/`rows` array, a null or non-object array element, a wrong-typed `now`/`since`, a `schema_version` that is present but not a positive integer, an admission body without an `allow`/`deny` decision, a document or admission result that cannot fit the content budget, or any other 200 the adapter cannot interpret |
| `server_unreachable` | connection failed |
| `server_timeout` | no answer before the deadline, or the call was cancelled |
| `capability_unsupported`, `capability_requirements_required` | the control API's own stable capability refusal types, passed through verbatim |

An error is never permission. Unknown or stale inputs are not errors: they
arrive as data (`stale`, `error`, `snapshot_age_s`, empty `windows`, non-zero
`unknown_usage_requests`, non-zero `unpriced_requests`) and must be read as
unknown.

## Provenance

Every result carries its own source metadata so a report can be attributed:

- `retrieval.retrieved_at` — when this result was produced (RFC 3339, UTC);
- `retrieval.truncated` / `dropped_items` / `max_result_chars` — the bound that
  applied and whether it bit;
- `source.endpoint` — which control document answered, and
  `source.upstream_schema_version` — the version that document reported
  (`schema_version`, currently 1), or `null` when absent or explicitly null.
  A non-null invalid version instead causes `bad_response`; the adapter never
  invents a version for a response that did not report one;
- `counts` — returned vs total;
- `cost_note` on usage results — the cost-basis label.

What the numbers mean, and where they come from:

- **Quota** is provider-reported and account-wide; it covers every client and
  host on that account. `snapshot_age_s`/`stale`/`error` describe that reading's
  freshness, and `rolled: true` is the router's interpretation of a passed
  reset time, not a fresh provider measurement. A `healthy: true` response does
  not by itself prove usable telemetry.
- **The ledger is recorded traffic, not the account.** It holds the rows this
  router recorded itself plus rows imported from other hosts/clients through the
  ingest endpoint, so a row's dimensions tell you which host observed it. It is
  still not the account's complete usage: traffic that bypassed every recording
  client, or that a client could not meter, is missing from it — a metering gap
  counted in `unknown_usage_requests` among the rows that were recorded.
  `model_requests` in status is the provider's own per-model view and covers all
  traffic on the account, including clients that bypass the router.
- **Cost** is the ledger's recorded cost per row (see `cost_note` above):
  `api_equivalent` (subscription accounts: API list price, not spend),
  `metered` (API-key billing priced locally) or `provider_reported` (the
  provider's own cost for that request). Rows whose price is unknown carry a null
  `cost_usd` and are counted in `unpriced_requests`, never shown as zero cost.
- **Do not sum across groupings.** `group=model`, `group=account` and the other
  dimensions are alternate views of the same recorded requests; adding them
  together double-counts. Adding transcript-derived or other-host figures on top
  of ledger rows is only valid when you can verify they are not already imported
  into those rows.

## Security

- **Transport.** Loopback plaintext HTTP by default; remote plaintext HTTP is
  refused at startup (only `https` to a non-loopback host starts). Redirects
  are never followed, so the bearer key is never re-sent to a redirect target.
  Environment proxy settings are ignored by the adapter.
- **Credential.** Only the operator-selected client key file is read, once at
  startup. Its value is sent in the HTTP Authorization bearer header, never in
  tool arguments or command-line values, and is not logged or reflected into
  error messages. Provider credentials are never read.
- **No broader surface.** No configuration read or write, no database, no
  provider credentials, no ingest, no inference, no reserve override, no admin
  action. A failed call reflects no upstream body: error messages carry the
  operator-configured base URL and, for a transport failure, the wrapped cause,
  plus HTTP status codes and the control API's two capability type names.
- **Client keys may outrank the adapter.** Existing LocalRouter client keys can
  carry authority the adapter never uses — proxying/inference, and `ingest` for
  a client so configured. The three-tool allowlist is a **client-side
  restriction, not server-side scoped authorization**: the server sees the same
  key an approved client would use and enforces only what the key already
  allows. This adapter defines no new auth scope and claims none. Pick a key
  whose server-side grants you are willing to use; a dedicated read-only scope
  would be a separate server change.
- **Data.** Results contain account IDs, client names, model names, token
  counts, recorded costs and policy reasons — operational metadata. Never
  prompt or response content, never key material.
- **Scope of registration.** Pointing a client at this server is the operator's
  action; it should be a deliberate choice of URL, key and client, not a default.

## Limitations

- **A dry run is not a reservation.** `check_admission` is a snapshot policy
  check: it takes no lease, reserves no slot, and cannot coordinate concurrent
  launchers or workers on other hosts. External CLI workers still need their own
  concurrency and budget supervision, and a worker may still be cut off
  mid-flight. Recheck between waves.
- **The ledger is not the account.** Per-worker attribution cannot be derived
  from a quota delta: quota moves account-wide, may be rounded or delayed, and
  covers other clients. Never convert a quota percentage into remaining
  requests without a measured, workload-specific rate and explicit uncertainty.
- **Unknown is not unlimited.** Missing, stale or empty telemetry means unknown
  headroom. Do not launch optional large batches on that basis.
- **Shared pools.** A model-specific allowance may still draw on a common
  account allowance; short windows do not replenish the week. Do not sum
  windows.
- **Tighter than the endpoint.** `since` is capped at 90 days and rows at
  `limit`/the content budget, so a result can be a truncated view, not a
  census; read the `counts`/`retrieval` fields.
- **No capability profile** is exposed, so model-based checks on
  capability-routed routes return a refusal rather than a decision (see above).
- **No enforcement.** MCP improves discovery and routine checks; it does not
  enforce agent behavior, and it adds no policy of its own.
- **Advisory budget.** The spend estimate in an admission result never changes
  the decision.
- **Not covered here.** Installing or registering the server with a client,
  changing router configuration, reserves or keys, and any inference or
  accounting path are out of scope for this document.

## Verifying

- Live read checks against a running server with any HTTP client (on a POSIX
  shell `curl` works; on Windows prefer the agent's native HTTP/MCP tool over an
  assumed GNU `curl`):
  `curl --fail --silent --show-error --max-time 10 'http://127.0.0.1:8787/control/v1/status'`
  and `… '/control/v1/usage?since=24h&group=model'`.
- Adapter CLI sanity: `localrouter mcp --help` prints the flag contract
  without starting a session.
- Stdio acceptance: `scripts/mcp-smoke.py --binary <built localrouter>` drives
  `initialize`, `tools/list`, one call of each tool, a `limit` bound check and
  adversarial argument checks over a real stdio session. It fails with
  `server_unavailable` if the binary does not implement `mcp` — there is no
  fabricated success path.
- Agent-side usage workflow: [`skills/localrouter-usage/`](../skills/localrouter-usage/SKILL.md).
