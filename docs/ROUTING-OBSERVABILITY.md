# Routing Observability

Privacy-safe, structured per-attempt routing telemetry emitted by the proxy
(`internal/proxy`). This document describes the **current** event contract
(chunk 4, first vertical slice) and the fields intentionally reserved for the
chunk 2 integration. It is the authority for what may and may not appear in a
`routing_attempt_completed` record.

## Event

One structured completion record is emitted for **every ledger row** the proxy
writes for an attempt, exactly once, after that row is finalized. This includes
an attempt that fails locally before any upstream contact (credential
unavailable, see below). It is written through the proxy's existing injected
logger (`p.log`, a `*slog.Logger` supplied via `Deps.Logger`). No new globals,
channels, in-memory queues or persisted tables are introduced.

- logger message: `attempt completed`
- stable machine key: `event = routing_attempt_completed`

A denied request (policy rejection) performs no upstream attempt and therefore
emits **no** completion event; the pre-existing `policy denied` diagnostic is
unchanged. A policy that returns an unknown account also writes no ledger row
and so emits no event (pre-existing gap; only the `policy returned unknown
account` error line is logged).

### Delivery and ledger visibility

- The event is logged at **Info**. A logger configured above Info (Warn or
  Error) drops it silently; nothing else reports the loss. `localrouter serve`
  logs at Info through a text handler on stderr, so the event appears as
  `key=value` pairs there; consumers wanting JSON must supply a JSON handler.
- The event is emitted **after** the ledger write is attempted and regardless
  of its result. If the write fails, the proxy logs `ledger record failed` at
  Error with `id=<attempt_id>` and still emits the event, so that `attempt_id`
  then names no persisted row. `attempt_id` (and `request_id`/`failover_of`)
  are only guaranteed to join the ledger when no `ledger record failed` line
  was logged for those IDs. Without a configured ledger, the IDs never join a
  persisted row.

## Fields (current)

| attribute           | source                                   | notes |
|---------------------|------------------------------------------|-------|
| `event`             | constant `routing_attempt_completed`     | stable filter key |
| `request_id`        | ledger record ID of the **first** attempt for this client request | request correlation ID (`core.RequestRecord.ID`) |
| `attempt_id`        | this attempt's ledger record ID          | attempt correlation ID (`core.RequestRecord.ID`) |
| `failover_of`       | this attempt's `core.RequestRecord.FailoverOf` | predecessor attempt's record ID; omitted on the first attempt |
| `client`            | configured client name (`core.Client.Name`) | not the bearer token |
| `account`           | configured account ID (`core.Account.ID`) | |
| `provider`          | configured provider (`core.Account.Provider`) | |
| `class`             | request class (`core.Class`: `interactive`/`background`) | |
| `status`            | observed upstream HTTP status; `0` when none | |
| `outcome`           | one of the stable outcomes below         | |
| `auth_retry`        | bool: this attempt saw 401/403 and triggered an in-place credential refresh | |
| `failover_eligible` | bool: this attempt was **eligible** for failover — the failover budget still permitted another account (`canFailover`) and the failure was retryable | eligibility only, **not** occurrence (see below) |
| `latency_ms`        | this attempt's finalized ledger latency (`core.RequestRecord.LatencyMS`) | the same value written to the ledger row; computed once in `record()` from the record's Start/Finish timestamps, never a second clock read |

All identifiers are **existing** ledger identifiers. No new correlation IDs are
invented; `request_id`/`attempt_id`/`failover_of` are exactly the values already
present on `core.RequestRecord`.

### `failover_eligible` (eligibility) vs an actual failover (occurrence)

`failover_eligible` reports only that failover was **permitted** for this
attempt, i.e. `canFailover` (the failover budget had not been exhausted) held
and this attempt's failure was retryable. It does **not** assert that a
different account was actually leased and tried next: the alternate may not
exist (single-account route) or may be policy-denied.

An **actual** failover is evidenced only by the **next completed event**: that
event carries `failover_of` (the predecessor's record ID) together with a
**different** `account`. When no next attempt occurs — the last account failed,
the only alternate was denied, the credential was unavailable, or the transport
failed — there is no such event and no `failover_of` link, even though the
failing attempt may still report `failover_eligible=true`.

The obsolete key `failover` (which over-claimed occurrence) is **not** emitted;
the event attribute set is closed and the test suite fails if it reappears.

### Outcomes (stable)

Exactly one per completed attempt; these strings are part of the contract and
must not change without a schema-version bump.

- `success` — upstream answered with a non-error status.
- `upstream_error` — upstream answered 4xx/5xx (including a buffered retryable
  status that then fails over, the 401/403 auth-refresh attempt, and an error
  status whose body stalled for `StreamIdleTimeout` before it could be read).
- `transport_error` — no usable upstream response: dial/TLS failure, response
  header timeout, mid-stream read failure, idle stream timeout, or credential
  unavailable. The stream failures keep the status already relayed (e.g.
  `200`); the others report `status=0`.
- `client_cancelled` — the downstream client went away (before or during the
  response).

**Credential unavailable is a local attempt.** When the account's credential
cannot be obtained, the proxy never contacts upstream, but it still writes a
ledger row and emits an event with `outcome=transport_error`, `status=0`, and
`failover_eligible` set from the remaining failover budget. On the event alone
it is indistinguishable from a dial failure; the ledger row's error text
(`credential unavailable`) and the `credential unavailable` warning line tell
them apart. A dedicated outcome would be a contract change and is not part of
this slice.

### Auth-retry and failover linkage

- **Auth retry** (401/403, **same account**, credential refreshed in place): the
  first attempt emits `outcome=upstream_error`, `status=401`/`403`,
  `auth_retry=true`; the retry emits its own event. Both share the same
  `request_id`; the retry's `failover_of` is the first attempt's record ID. The
  linkage is distinguished from a failover because the account does not change
  and the predecessor event carries `auth_retry=true` (never
  `failover_eligible=true`).
- **Failover** (retryable status, **different account**): the failing attempt
  emits `outcome=upstream_error` with `failover_eligible=true`; the next
  account's attempt links back via `failover_of` and a different `account`.
  All attempts for one client request share the same `request_id`. Only the
  presence of that next event with a changed account is evidence that a failover
  actually occurred.
- **OpenRouter 402/403**: a 403 (moderation) is final for the request. It emits
  one `upstream_error` event with `auth_retry=false` and
  `failover_eligible=false`, and there is no retry or failover. A 402
  (affordability preflight or out of credit) is retryable: it reports
  `failover_eligible=true` while failover budget remains, including when its
  error body stalls.

## Explicitly excluded (privacy)

A completion event must never contain any of:

- client-supplied model strings (`req.model`), route aliases, or upstream model
  names;
- prompt/response bodies;
- request or response headers;
- credentials, bearer tokens, account secrets;
- URLs;
- raw upstream error text (already sanitized in the ledger via `sanitizeErr`;
  the event carries no `err` attribute at all).

The event's attribute set is closed: the test suite
(`internal/proxy/routing_observability_test.go`) fails if a record carries any
key outside the table above (slog's own `time`/`level`/`msg` envelope keys are
ignored), and fails if any value contains a synthetic secret placed in the
request body, a request header, or a provider error body.

The same suite checks the delivery contract on every terminal path (success,
auth refresh, second 401/403, retryable failover, exhausted failover budget,
non-retryable 4xx, credential unavailable, dial error, response-header timeout,
client cancel before and during the stream, idle timeout, upstream read error,
OpenRouter moderation 403, affordability and account 402, stalled 402 error
body).
It waits for the proxy handler to return, then requires exactly one event per
ledger row, in order, keyed by the row ID. It runs on a deterministic clock that
advances on every read, so `latency_ms` must be nonzero, must equal the row's
`LatencyMS`, and the proxy's total clock reads must match the expected count.

## Reserved for chunk 2 integration

The following will be added **only** when the chunk 2 model-alias integration
lands; they are deliberately absent now so that no alias/upstream model string
can be echoed through the new log path before that work is reviewed:

- route alias (the configured route name the request matched);
- upstream model name actually sent;
- `core.RequestRecord.Route` / `.Model` attribution.

Until then, `class`, `client`, `account` and `provider` carry the configured
routing identity, and the ledger (`core.RequestRecord`) remains the only place
where route/model attribution is persisted.

Latency is **no longer deferred**: `latency_ms` is emitted today on every record
path, sourced from the ledger row's own `LatencyMS` (one clock read in
`record()`, no extra read).

## Existing logs

All pre-existing `p.log.*` diagnostics are unchanged; the completion event is
purely additive. No existing log line was modified to fix a privacy bug in this
slice — none was newly exercised that required it.
