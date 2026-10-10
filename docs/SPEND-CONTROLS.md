# Operator spend controls (`budgets`)

This document describes the **spend-control** subsystem of LocalRouter — the
`budgets:` block, the durable per-attempt reservation it takes, and how that
reservation is settled — as it is actually implemented in this tree. Every
number and rule below is taken from the source, not from the design notes:

| Concern | Source |
| --- | --- |
| Config schema, parsing, validation | `internal/config/config.go` (`BudgetConfig`, `BudgetLimits`, `ReserveUSD`, `ReservationMicros`, `Limits`) |
| Money type (exact decimal → micro-USD) | `internal/budget/money.go` (`ParseUSD`, `ReportedUSDToMicros`) |
| Durable store (SQLite) | `internal/budget/store.go` (`Open`, `Reserve`, `Settle`, `ReconcileOrphans`, `Snapshot`) |
| Runtime adapter (proxy ↔ store) | `internal/budget/gate.go` (`Gate`, `NewGate`, `Reserve`, `Settle`) |
| Process ownership lock | `internal/budget/owner_unix.go`, `internal/budget/owner_windows.go` |
| Proxy reserve/settle call sites | `internal/proxy/proxy.go` (`Deps.Budget`), `internal/proxy/forward.go` (`attempt`, `recordBudget`, `stream`) |
| Cost resolution (reported / estimated / unknown) | `internal/ledger/ledger.go` (`ResolveRecordCost`, `CostBasisReported|Estimated|Unknown`) |
| Price table + local aliases | `internal/ledger/pricing.go` (`LoadPricing`, `LocalPricingPath`) |
| Wiring, startup order, reload | `internal/app/app.go` (`openBudgetStore`, `closeBudget`), `internal/app/reload.go` (`budgetPresence`, `budgetGate`, `restartFields`) |

## 1. What this is — and what it is not

Spend controls place a **fixed, durable hold** on a per-attempt basis before an
upstream request is sent, and book the attempt's **resolved** cost when it
finishes. They are an admission guard backed by a local database.

They are **not** a guaranteed cap on any external provider bill. Read this
before enabling them:

- **`reserve_usd` is a fixed hold, not a bill cap.** The code and its comments
  say so explicitly: "This is a fixed reservation only — it is explicitly NOT a
  cap on the external provider bill, which can exceed it"
  (`config.go`, `BudgetConfig.ReserveUSD`).
- **The fixed hold may under-reserve.** One hold is taken per attempt up front;
  a single attempt's real cost can exceed it. A settlement books the **full**
  observed charge even when it is larger than the hold (overrun is recorded, not
  clamped — `store.go` `normalizeCharge`/`Settle`).
- **Only LocalRouter-mediated spend is counted.** Cost is resolved from the
  ledger's own records: a provider-reported cost (OpenRouter only) or a local
  price-table estimate. Usage a provider bills outside LocalRouter — direct
  client traffic, a second machine, a provider console, a model the local table
  does not price — is invisible here and does not count against a ceiling.
- **Denials are generic.** The client sees `budget_exceeded` (429) or
  `budget_store_error` (503); the scopes, keys, periods and remaining amounts
  stay in LocalRouter's logs and error chain, never in the HTTP body.

Treat `budgets` as a local guardrail, not as an authoritative spend meter.

## 2. Enabling and reloading

The block is optional and off by default:

- **Omitted ⇒ disabled.** `Budgets *BudgetConfig` is nil, no store is opened,
  no ownership lock is taken, and the proxy's budget hook is nil, so behaviour
  is exactly as before this subsystem existed (`config.go`; `app.go` Build
  guards on `cfg.Budgets != nil`; `proxy.go` requires `Deps.Budget != nil`).
- **Present ⇒ enabled**, and then `reserve_usd` is **required** and at least one
  client or account ceiling is **required** (`BudgetConfig.validate`).

Reload behaviour (`internal/app/reload.go`):

- **Enabling or disabling spend controls requires a restart.** The process-wide
  store and its ownership lock exist only when `Build` saw the block.
  `restartFields` reduces the whole block to a single presence bit
  (`budgetPresence`), so a nil↔present change is a restart-only field reported
  as `budgets`.
- **Everything inside the block is live-reloadable.** `reserve_usd` and every
  client/account ceiling are erased from the restart-only comparison and are
  re-read on `SIGHUP`. Each generation builds a **fresh, immutable** `Gate`
  (`budgetGate` → `NewGate`) that pins that generation's limits and reserve, so
  an in-flight request keeps the generation's gate it started under. Limits are
  copied into the gate (`NewGate` uses `append([]Limit(nil), limits...)`), so a
  reload can neither loosen nor tighten a decision already in progress.
- Note: the `budgets` field name in a restart-required report is the YAML tag
  `budgets`, not a per-key list.

## 3. The fixed reservation: `reserve_usd`

```yaml
budgets:
  reserve_usd: "0.05"      # required, exact decimal USD string, must be > 0
```

- **Decimal text, never a float.** `reserve_usd` is a YAML **string** and is
  parsed with `budget.ParseUSD`, which produces exact `int64` **micro-USD**
  (1 USD = 1,000,000 micros). No configured amount anywhere in the budget path
  is ever converted through `float64` (`config.go`, `money.go`).
- **Grammar** (`ParseUSD`, after trimming surrounding whitespace):
  `[+]? DIGIT+ ( "." DIGIT+ )?`
  - Nonnegative; a leading `-` is always rejected, a single leading `+` is
    allowed and has no effect.
  - At least one digit is required on each side of the decimal point, so `.5`
    and `5.` are invalid.
  - At most six fractional digits are exact; a 7th-or-later fractional digit is
    accepted **only** if every extra digit is `0`. A nonzero digit below one
    micro is rejected.
  - Exponent notation, `NaN`/`Inf`, digit grouping, underscores, hex and
    internal whitespace are all rejected. Magnitudes above `math.MaxInt64`
    micros are rejected.
  - Valid examples: `"0"`, `"0.0"`, `"5"`, `"5.25"`, `"1.234567"`, `" 5.25 "`.
- **Required and strictly positive when enabled.** There is no default and no
  estimation formula. `ReservationMicros()` returns an error if the block is
  absent, and `validate` rejects a non-positive `reserve_usd`
  (`"… must be positive when budgets are enabled"`). The gate also refuses a
  non-positive hold at reserve time (`ErrInvalid`).
- The hold is the same number for every attempt, regardless of model, tokens or
  route.

## 4. Client and account ceilings: `daily_usd` / `monthly_usd`

Ceilings are keyed by the configured **client name** and configured **account
id**:

```yaml
budgets:
  reserve_usd: "0.05"
  clients:
    hermes:                 # must name a configured client
      daily_usd: "1.00"
      monthly_usd: "20.00"
  accounts:
    codex-primary:          # must name a configured account
      monthly_usd: "50.00"
```

- **Both amounts are optional.** `DailyUSD`/`MonthlyUSD` are `*string`; a
  **nil pointer (omitted) means unlimited** for that scope and period.
- **An explicit `"0"` is a real zero budget** and denies **every** reservation
  for that scope/period — it is a legitimate way to freeze a client or account.
- Same **decimal-text** rule as `reserve_usd`, parsed with `budget.ParseUSD`;
  omitted periods produce no `Limit` at all (`BudgetLimits.Limits`).
- **Keys must be known.** `budgets.clients` must name a configured client and
  `budgets.accounts` must name a configured account, or `Validate` fails. A
  quota-only account (e.g. a `claude` account that cannot serve inference) is
  accepted and may carry a ceiling; it simply never spends. At least one client
  or account ceiling is required when the block is present. What counts is a
  present `daily_usd`/`monthly_usd` amount, not a map entry: an empty entry
  (`clients: {a: {}}`) or an explicit null/blank amount (`daily_usd:`) is
  unlimited, so a block made only of those is rejected.
- **Scopes are independent.** Each ceiling is its own `(scope, key, period)`
  budget; a settlement books the full charge against every tracked instance.

## 5. Complete example (placeholders only)

This is a complete, runnable-shape `budgets` block for an **isolated** config.
It references a configured client and account and uses placeholder key paths —
there are **no credentials here**, and budgets never contains credentials.

```yaml
# ~/.config/localrouter/config.yaml  (illustrative paths only)
listen: 127.0.0.1:8787
data_dir: /home/operator/.config/localrouter

clients:
  - name: me
    class: interactive
    key_file: keys/me.key          # placeholder path; a real single-key file
  - name: hermes
    class: interactive
    key_file: keys/hermes.key      # placeholder path

accounts:
  - id: codex-primary
    provider: codex
    reserve: { 5h: 0.10, weekly: 0.10 }

budgets:
  reserve_usd: "0.05"              # fixed per-attempt hold; required; > 0
  clients:
    me:
      daily_usd: "1.00"            # omitted => unlimited
      monthly_usd: "20.00"
    hermes:
      daily_usd: "0"               # explicit "0" => deny every reservation
  accounts:
    codex-primary:
      daily_usd: "5.00"
      # monthly_usd omitted => unlimited for this account/month
```

An empty `accounts:` / `clients:` sub-block is valid; the requirement is at
least one present `daily_usd` or `monthly_usd` amount somewhere in the block.

## 6. Runtime semantics of a hold

The reservation is taken **per upstream attempt**, never per client request.

- **After the credential, before the send.** `proxy.attempt` resolves the
  account credential first and only then calls `Budget.Reserve`, immediately
  before the HTTP send. A credential failure therefore never holds budget
  (`forward.go`).
- **On every attempt, including the 401/403 retry and every failover.** The
  reserve call sits inside the per-attempt loop, so it runs again for the
  in-place credential-refresh retry after a 401/403 and again for each failover
  account (`forward.go` `attempt`; the loop in `forward`).
- **A Reserve error is terminal.** When `Reserve` fails, the attempt is **not**
  sent, does **not** fail over onto another account's budget, and is **not**
  settled — nothing was admitted (`proxy.go` `Budget` interface doc;
  `forward.go`). A client that disconnects while `Reserve` runs is recorded as
  `client disconnected`, not as a store error, and gets no 503.
- **Settle on every path after a successful Reserve.** `recordBudget` pairs
  `Reserve` with exactly one `Settle` for the credential-refresh retry, each
  failover account, a transport failure, a client cancellation, and the stream
  relay (a relay that ends early settles through `SettleIncomplete` instead;
  see §7). Settle runs on a context detached from the request with a 5-second
  bound (`context.WithoutCancel`, `ledgerTimeout`), so **a cancelled client
  never loses its hold** (`forward.go` `recordBudget`).
- **UTC periods, fixed at admission.** A period instance is a concrete
  `[start, end)` **UTC** window: day = 00:00 UTC, month = the 1st at 00:00 UTC
  (`store.go` `periodStart`). The instance is chosen once, at admission, from
  the attempt timestamp and is **never re-derived at read time**, so a late
  settlement books into the period the hold was admitted under — no
  cross-period drift.
- **All four instances are tracked.** Every admitted attempt holds the client
  budget and the account budget, each for the UTC day and the UTC month —
  **whether or not** a limit is configured for them — so a ceiling added later
  is evaluated against the true history of the period, not just the subset it
  used to gate (`store.go` `trackedPeriods`).
- **Atomic, fail-closed admission.** `Reserve` checks every applicable limit
  inside one `IMMEDIATE` write transaction: for each, `spent + reserved + hold`
  must not exceed the limit. If any limit has no room, or any integer sum would
  overflow `int64`, **nothing is written** and the call returns `ErrExceeded`
  (or the arithmetic error). Idempotent on the attempt id: an identical replay
  is a no-op; the same id with a different payload is `ErrConflict`.

## 7. Settlement: reported > estimated > unknown (held floor)

When an attempt ends, its cost is resolved by `ResolveRecordCost` and booked
under one of three bases. The **resolution order is fixed**:

1. **Reported** — a trusted provider-reported cost. Only **OpenRouter** is
   trusted; cost fields from any other provider are ignored (`forward.go` sets
   `ReportedCostUSD` only when `rec.Provider == core.ProviderOpenRouter`). An
   explicit, finite, non-negative reported value of at most
   `core.MaxReportedCostUSD` — **including zero** — resolves as
   `CostBasisReported` and wins over the price table. A larger value is
   unusable and falls through to the next basis.
2. **Estimated** — otherwise, if the attempt has **known usage** and the
   generation's price table prices the model, the table's computed cost resolves
   as `CostBasisEstimated`. Reasoning tokens are part of output; cached and
   cache-creation tokens are subsets of input and clamped so the uncached
   remainder is never negative (`ledger.go` `resolveCost`, `pricing.go` `Cost`).
3. **Unknown** — otherwise the cost is nil and the basis is `CostBasisUnknown`,
   and the caller settles at its own floor.

How each basis is charged (`store.go` `normalizeCharge`, `Settle`;
`gate.go` `Settle`):

- **Full overrun, never clamped.** For `reported` and `estimated`, the **full**
  observed charge is booked, even when it exceeds the reservation; the period
  total may therefore exceed its limit.
- **Unknown is never free.** An `unknown` settlement is floored at
  `max(observed, reserved)`, so an unknown outcome can never release the
  store-held reservation for a discount or for nothing. In the gate's normal
  unknown path the requested charge is `0`, so the hold is charged in full at
  the reserved amount.
- **An incomplete response is a lower bound, never a final cost.** When the
  relay ends early — client disconnect, stream idle timeout, or upstream read
  error — any OpenRouter `usage.cost` already seen may come from an
  intermediate usage record (a stale `0` or a partial amount). The ledger row
  still keeps that observed cost, but the proxy settles the budget with
  `Gate.SettleIncomplete`, which books it as `unknown` with the observed cost as
  the requested charge. The charge is therefore `max(hold, observed)`: a stale
  zero or partial cost never releases the hold, and an observed cost above the
  hold is booked in full. A response that completes normally with a reported
  `0` is still a real, free outcome and releases the hold.
- **Failed attempts are charged the full hold (deliberate policy).** Every
  attempt that settles with no usable cost — the 401/403 attempt before a
  credential-refresh retry, each 429/5xx (or OpenRouter 402) failover attempt,
  a transport error (including one where nothing reached the upstream), and an
  upstream error response relayed to the client (including an OpenRouter 403
  moderation rejection, and a 402 whose body stalled while it was being
  classified) — is booked as `unknown` at the
  full hold, permanently. The proxy cannot prove that an attempt was not
  billed, so it errs on the side of charging. The consequence is that failing
  requests consume ceilings like real spend, and because account ceilings are
  shared, one client's failing requests can use up an account ceiling and get
  other clients on that account denied with `budget_exceeded`. The operator
  report shows this phantom spend under `unknown`. Size `reserve_usd` and the
  account ceilings with that in mind.
- **Settle never silently discounts.** If the resolved cost cannot be
  represented as micro-USD (`ReportedUSDToMicros` rejects `NaN`, `±Inf`,
  negative, or `int64` overflow), `Gate.Settle` returns an error and leaves the
  **whole** reservation held, so the startup reconciler — not a silent
  discount — disposes of it.
- **Strict and idempotent.** Settling an attempt that was never reserved returns
  the store's `ErrUnknownReservation` (a wiring fault, logged loudly). A
  repeated settle with the same normalized payload is a no-op; a conflicting
  second settle returns `ErrConflict`. A lost claim books nothing.
- **Reported costs are the one exception to the ledger's float.** Integer
  micro-USD is the unit everywhere; the only `float64` entry point is
  `ReportedUSDToMicros`, which folds an already-received provider cost into
  micros **rounding up** so the result never understates the reported cost.

## 8. Denials and store errors (client-visible)

| Condition | HTTP status | JSON `error.type` | HTTP body message |
| --- | --- | --- | --- |
| A ceiling has no room (`ErrExceeded`) | `429 Too Many Requests` | `budget_exceeded` | `localrouter: budget exceeded` |
| Budget store unavailable (any other error) | `503 Service Unavailable` | `budget_store_error` | `localrouter: budget store unavailable` |

Source: `internal/proxy/forward.go`.

- **Terminal, no failover.** Both are returned as `done: true`: the attempt is
  not retried, does not fail over to another account, and is not settled.
- **No `Retry-After` promise.** Neither budget response sets a `Retry-After`
  header, and none is specified. Do not build retry logic on `Retry-After` from
  these responses. (For contrast, LocalRouter's *concurrency* rejection sets
  `Retry-After: 1`, and the separate *policy/quota* denial sets `Retry-After:
  60`; those headers belong to those mechanisms, not to `budgets`.)
- **Generic bodies (privacy).** The scope, key, period and remaining amounts
  wrapped by `ErrExceeded` are **not** relayed to the client; the HTTP body is
  the generic message above. LocalRouter's logs carry the events
  (`budget exceeded`, `budget reserve failed`, `budget settlement failed;
  reservation left held for reconciliation`). The two failure events carry only
  a sanitized `class` (`closed`, `timeout`, `canceled`, `invalid`, `conflict`,
  `no_store`, or `store` for anything else), never the store's own error text,
  which can embed filesystem paths or DSNs.
- **Distinct from concurrency and quota.** `budget_exceeded` (429) and
  `concurrency_limit_exceeded` (429) and the policy `quota_reserve` denial are
  three separate rejections with different types; the budget one is neither a
  concurrency slot nor a quota snapshot.

## 9. Persistence, ownership and crash recovery

Everything durable lives in **two files under `data_dir`**:

- `budgets.db` — the SQLite store (WAL). Created mode `0600`; `Open` sets
  `journal_mode(WAL)`, `busy_timeout(5000)`, `_txlock=immediate`,
  `foreign_keys(1)`, and bounds the connection pool at two. Writes are
  serialized by a mutex (one write transaction at a time), so a concurrent read
  *can* use the second connection while a write is open — this makes a read
  **possible, not guaranteed**: it does not promise that a read is never
  blocked. A read is bounded by its own deadline (see §11), not by any
  non-blocking guarantee.
- `budgets.lock` — the process-ownership lock file (see below). It is created
  `0600` and is **deliberately never unlinked**.

**Exclusive ownership before any reconciliation.** At startup, `Build` calls
`openBudgetStore`, which runs, in this order (`app.go`):

1. `budget.AcquireOwnership(data_dir/budgets.lock)` — takes the whole-file lock.
2. `budget.Open(data_dir/budgets.db)` — opens (creating if needed) the store.
3. `store.ReconcileOrphans(ctx)` — with a 5-second bound
   (`budgetStartupTimeout`).

Ownership is acquired **before** the store is opened and reconciliation runs
**before** the store is shared with any generation, so no other process can be
reconciling the file concurrently. Any failure releases exactly what was
acquired, leaving ownership free for the next attempt.

**The lock (`owner_unix.go`).** Advisory whole-file `flock(2)` with
`LOCK_EX|LOCK_NB` on `budgets.lock`. It is non-blocking: a second owner gets
`ErrOwnershipHeld` immediately rather than stalling. `flock` is chosen over
`fcntl`/POSIX record locks because it belongs to the open file description, so
two opens of the same path **within one process** also contend. The lock file is
never unlinked on release — unlinking would race two owners onto two inodes;
leaving it keeps every acquirer contending on one stable path. A symlink at the
lock path is refused (and re-checked at open with `O_NOFOLLOW`). Existing files
and parent directories are used as found and are never `chmod`-ed.

- **Platforms:** built for **Linux and macOS** only (`//go:build linux ||
  darwin`). **Windows is unsupported:** `owner_windows.go` makes
  `AcquireOwnership` always fail with `ErrOwnershipUnsupported` rather than
  returning a no-op guard that would falsely imply single-process ownership.
  Windows is not a LocalRouter release platform.
- **Filesystem:** `flock` is only meaningful on a **local filesystem**. Do not
  put `data_dir` (hence `budgets.db`/`budgets.lock`) on network storage such as
  NFS or SMB; the advisory lock cannot be relied on there.

**Orphan reconciliation.** `ReconcileOrphans` settles every still-open
reservation **at its reserved ceiling**, with basis `unknown` and settle reason
`orphan_startup`. It is idempotent, never releases reserved money (the ceiling
is charged, not refunded), and each orphan is claimed transactionally first, so
a stale list entry is a harmless no-op rather than a second charge. `Open`
never reconciles on its own — the caller must invoke it explicitly at startup,
before serving, as the exclusive owner.

**Crash / forced shutdown.** A crash or forced shutdown can leave reservations
**open** if their settlement does not finish. Successfully settled reservations
remain settled. There is no TTL sweep; the next startup's `ReconcileOrphans`
charges any remaining open reservation at its held ceiling as `unknown`. If a
settlement *failed* at runtime (e.g. a store error), the hold is likewise left
in place for that startup reconciler — never a silent discount.

**Never delete or reset the database.** Do **not** remove `budgets.db` to
"clear" spend. It is the durable record of holds and settled spend, its deletion
discards that history, and there is no reset command and no TTL. Recovery from
stuck holds is by restarting the process so startup reconciliation charges them
at the ceiling as `unknown`.

**Teardown ordering.** `App.Close`/`closeBudget` closes the store **first**, then
the ownership lock, so none of this process's writes are still on the file at
the instant the lock drops and another process could become owner and
reconcile. It is valid only after `Serve` has returned / the drain has
completed; a forced shutdown that skips this leaves the open holds described
above.

## 10. Pricing and provenance (aliases preserved)

Settlements priced as `estimated` use the generation's frozen price table,
loaded by `ledger.LoadPricing(cfg.PricingFile)`:

- The hand-maintained **overrides/aliases** file lives beside the imported one
  as `<pricing_file base>.local.<ext>` (`LocalPricingPath`). Its `models:` entries
  **replace** imported entries, and its `aliases:` map a model name as clients
  send it to another priced key (`"glm-5.3" -> "zai/glm-5.3"`). `pricing import`
  **never writes** this file, so local aliases survive an import. Aliases in the
  *imported* file are rejected (they must live in the `.local` file), and an
  alias to an unpriced key is a startup error.
- Each reload pins a **generation-scoped** price view
  (`Ledger.WithPricing`), so records admitted in one generation are costed at
  that generation's prices even if the table changes while they are in flight.

**Caveat — not route/backend aware.** The budget gate is a fixed per-attempt
hold against the attempt's client and account keys. It is **not integrated with
routing**: it does not select, rank, or influence which route/account/backend
serves a request, and it does not consult backend identity or model routing.
Do not describe spend controls here as route-aware or backend-aware. Routing
admission (quota reserves, policy) is a separate mechanism with its own
rejection type and its own `Retry-After` behaviour.

## 11. Diagnostics (operator read endpoint)

Spend controls are **read back over the control API**. The one *read-only*
operator endpoint is:

```
GET /control/v1/budgets?scope=client|account&key=<name>[&period=day|month]
```

Source: `internal/control/budget.go` (`budgets`, `budgetDoc`, `budgetRow`,
`knownIdentity`), routed in `internal/control/control.go` (`Handler`), wired
from `internal/app/reload.go` (`control.Deps{Budgets: a.budgetSource(c.Budgets)}`)
and adapted in `internal/app/budgetreport.go` (`budgetReport`).

- **Query.** `scope` is **required** and exactly `client` or `account`; `key`
  is **required** and must name a **configured** identity for that scope in
  this generation. `period` is **optional** (`day` or `month`); omitted means
  both, so a response carries **at most two rows**. There is deliberately **no
  all-identities/list query**: the readable set is bounded by the generation's
  own configured clients and accounts, and an unnameable key never reaches the
  store. (`q.Get` takes the *first* value of any repeated parameter; duplicate
  `scope`/`key`/`period` are silently ignored, and unknown extra parameters are
  ignored rather than rejected, so a caller cannot smuggle in a second scope.)
- **Status codes.** `400` for a bad `scope`, a missing `key`, or a `period`
  other than `day`/`month`; `404` when `key` is not a configured identity for
  the scope; `503` with the generic bodies of §8 (`budget store unavailable` /
  `budget read timed out`) when the store read fails or the read deadline
  expires; `200` otherwise. Every response is `application/json` with
  `Cache-Control: no-store`.
- **Disabled shape.** When spend controls are not configured for the
  generation (a nil `BudgetSource`), the body is exactly
  `{"schema_version":1,"enabled":false}` — no money and no store access.
- **DTO (exact keys).** Enabled, the top level is `schema_version`, `enabled`,
  `reserve_micros` (the generation's fixed per-attempt hold) and `rows`. Each
  row is `scope, key, period, start, reset` (the UTC boundaries), the settled
  and held counters `reported_micros, reported_usd, estimated_micros,
  estimated_usd, unknown_micros, unknown_usd, reserved_micros, reserved_usd`,
  plus `limit_micros, limit_usd, available_micros, available_usd` **only when a
  ceiling is configured**. An omitted limit/available pair means **unlimited**;
  an explicit `"0"` ceiling is reported as a real zero. Every `*_usd` string is
  the exact six-fractional-digit rendering of its paired integer micros (never a
  float), and `available` is **signed and never clamped**, so a settled overrun
  reads as a negative available. An amount that cannot be represented as
  `int64` micro-USD fails the whole read closed (`503`) rather than wrapping.
- **Authority.** The limits come from the generation's **captured config**
  (the same copied slice the generation's `Gate` enforces), never from the
  store's informational limit column, so the report cannot disagree with
  enforcement. A single UTC clock read governs every boundary in one response,
  and the whole document is read under **one 2-second deadline**.
- **Auth.** Registered on the **same gate** as `status`/`usage`/`diagnostics`
  (`s.auth`). With `control.require_auth: true` a valid client bearer key is
  required; with **`require_auth` false — the default — the endpoint is
  unauthenticated (public on the listen address)**, exactly like the other
  `/control/v1/*` reads. Do **not** describe it as always authenticated. Under
  `RequireAuth` any valid client key may read any configured identity (client
  or account); that is the existing access model, not a new policy or approval.

### Advisory estimate on admission (`POST /control/v1/admit`)

Spend controls add one **advisory** field to the existing dry-run admission
endpoint — the same `POST /control/v1/admit` the `admit` CLI and the delegation
hook call (see `docs/SPEC.md`). It is the only other place a budget number is
surfaced, and it is informational: it takes **no** reservation and changes
**nothing** about the decision beside it.

Source: `internal/control/budget_admit.go` (`admitBudget`, `admitBudgetClient`),
attached in `internal/control/control.go` (`admit`, `admitResponse.Budget`).

- **Presence.** The block is present only when spend controls are configured
  for the generation **and** the policy decision was `allow`. With no `budgets`
  block, or on a policy denial, the field is absent, so the response is
  **byte-for-byte the legacy one** — a deployment without spend controls sees no
  new bytes. The block never overrides `decision`, `account_id` or `reason`.
- **Exact DTO.** The `budget` object is exactly
  `{"advisory":true,"reservation_created":false,"client":…,"account_id":…,
  "hold_micros":…,"hold_usd":…,"allow":…,"reason":…}`. Always present:
  `advisory` (always `true`), `reservation_created` (always **`false`** — the
  estimate books nothing), `hold_micros`/`hold_usd` (the generation's fixed
  per-attempt hold — exact integer micros and its six-digit USD rendering) and
  `allow`. `client` and `account_id` appear only when a real identity was named;
  `reason` appears only when no verdict could be computed.
- **What `allow` answers.** Whether the fixed hold would fit under every ceiling
  that names the authenticated client or the policy-chosen account, for the UTC
  day and month containing this instant. An empty `reason` means a verdict was
  computed; otherwise `reason` names why none was and `allow` is `false`:
  `client_identity_unavailable` (no authenticating client),
  `account_identity_unavailable` (the policy named an account this generation
  does not configure), `budget_store_error` (the store could not be read, an
  amount did not fit micro-USD, or the hold is non-positive — it fails closed
  rather than guessing) or `budget_exceeded` (every input was read and the hold
  would not fit — this mirrors the proxy's own denial but **denies nothing**).
- **Fixed-hold advice, not a bill cap.** Read §1 and §3 with this: the number is
  the same fixed hold for every attempt, regardless of model or route, and one
  attempt's real cost can exceed it. `allow:true` means the hold fits — not that
  the attempt is free or capped; a settlement still books the full observed
  charge (overrun, never clamped). It is **not** route- or backend-aware, and
  (unlike the proxy's `Reserve`) it takes no reservation, so it can never cause,
  prevent or duplicate one.
- **Only the chosen identity; no failover.** The estimate reads exactly the four
  period instances an admitted attempt pins — the authenticated **client** and
  the **policy-selected account**, each for the UTC day and the UTC month — and
  nothing else. It never reads a candidate the policy declined, never calls the
  policy's `Acquire`, and never invents a failover attempt. A foreign or empty
  account id yields no estimate rather than some other account's numbers.
- **Caller bearer is revalidated even when the endpoint is not gated.** Whenever
  spend controls are configured the estimate authenticates the request's
  `Authorization` bearer through the *same* authenticator the auth gate uses —
  **independently of `control.require_auth`**. The client-scope hold must name a
  real client, so with no bearer (or a malformed or unknown one) the reason is
  `client_identity_unavailable` and the **store is never read**. This is not a
  second authorization decision and it never fails the request: an
  unauthenticated caller still receives the policy's own `allow`/`deny` and is
  simply given no budget estimate.
- **Bounded, one clock, but not atomic.** The whole estimate is one bounded store
  interaction: a single 2-second deadline (`admitBudgetTimeout`) shared by the
  four instance reads, and a single UTC clock read governing every period
  boundary — the same shape as the read endpoint above. The four reads are,
  however, **separate `Snapshot` calls, not one transaction**: the block is not
  an atomic snapshot of the four instances, and a **concurrent** attempt or
  settlement can change the money between the estimate and the proxy's
  authoritative `Gate.Reserve`. `allow:true` is therefore advisory only and
  **not** a guarantee that the real reservation will succeed. The UTC period
  boundary can likewise roll between the estimate's instant and the attempt's,
  so a boundary-crossing caller can be estimated against one day/month instance
  and admitted under the next.

There is still **no CLI subcommand** and **no budget field** in
`status`/`usage`/`diagnostics`: the store's `Snapshot(ctx, scope, key, period,
at)` Go read API is surfaced only through the read endpoint above and the
advisory estimate here — two consumers of the same read. Do not invent a
`localrouter budget status`-style command or an extra status field. The
client-visible 429/503 responses (§8), the read endpoint and the advisory
estimate above, and LocalRouter's logs are the budget signals available.

## 12. Acceptance (offline, isolated, no service discovery)

Spend controls are verified with the **native binary** and an **isolated**
config — no network calls, no live inference, no service discovery.

Build the native binary (the release script cross-compiles static
`CGO_ENABLED=0` binaries and never tags, pushes or publishes):

```bash
# from the repo root
go build -o /tmp/localrouter ./cmd/localrouter
# or, for release packaging:
#   scripts/release.sh /abs/durable/out-dir [version]
```

Then validate an isolated config with the built-in checker. `localrouter check`
parses the whole config **including the `budgets` block and its decimal
amounts**, and validates the client key files:

```bash
# Isolated config + its own data_dir and placeholder key files.
export CFG=/tmp/lr-spend-accept/config.yaml
mkdir -p /tmp/lr-spend-accept/keys
# ... write config.yaml (see §5) and placeholder key files:
#     keys/me.key, keys/hermes.key  (one raw key per file, mode 0600)
/tmp/localrouter keygen /tmp/lr-spend-accept/keys/me.key
/tmp/localrouter keygen /tmp/lr-spend-accept/keys/hermes.key
/tmp/localrouter check -config "$CFG"
# -> config OK: 2 clients, 1 accounts, N routes; data_dir=...
```

Notes for this step:

- **`check` is config-only.** It does **not** open the budget store, does not
  create `budgets.db` or take `budgets.lock`, and does **not** prove upstream
  connectivity, quota availability, TLS validity or remote reachability. A bad
  `reserve_usd` or ceiling fails `check`; a *live* budget decision does not (and
  must not) get exercised here.
- **No service discovery.** Do not probe `/v1/models`, mDNS, or any network
  endpoint as part of validating spend controls. Keep it to `check` (and, if you
  wish, unit tests) against an isolated directory.
- **No live calls and no secrets.** Do not run a live spend test as part of
  enabling budgets; acceptance here creates no `budgets.db` and spends nothing.

## 13. Privacy and redaction

- Budget **denials** are generic (see §8); a denial body never carries the
  scope, key, period or remaining amounts.
- The **read endpoint (§11) is deliberately different**: it returns configured
  identity names, ceilings and spend to any caller that can reach the control
  API — under `require_auth: true` only with a valid client key, and **when
  `require_auth` is false (the default) with no credential at all**. Treat the
  control API as operator-only surface; set `require_auth: true`, and note that
  anything non-loopback already forces it (`allow_non_loopback` requires it).
- Store errors are sanitized **both** before they reach an HTTP body and before
  they are logged: the snapshot-failure `slog.Warn` and the generic `503` both
  pass the error through `budgetReadError`, which collapses it to
  `budget store unavailable` / `budget read timed out`. The raw chain — which
  can embed filesystem paths, DSNs or credentials — appears in neither the
  response nor the log record (pinned by `TestBudgetsSourceErrorLogIsRedacted`).
  The advisory estimate logs nothing at all: a store failure there only sets the
  generic `budget_store_error` reason in the response. Treat LocalRouter's log
  files as sensitive anyway.
- The advisory estimate on `admit` exposes the authenticated client name and the
  policy-chosen account id (when present) to whoever can call that endpoint,
  exactly as the read endpoint exposes configured identities; the same
  "operator-only surface" guidance above applies to it.
- This document contains **no credentials, API keys, tokens or connection
  strings**. The example in §5 uses placeholder key **paths** only; `budgets`
  never holds credentials (secrets live in separate `0600` files, never in the
  config). If any real secret is ever present in a config or source file, treat
  it as `[REDACTED]` and do not reproduce the value.
