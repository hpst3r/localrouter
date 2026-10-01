# Critical Review + Reduced MVP — llm-usage-quota-service-implementation-plan.md

Reviewed: `/home/wporter/projects/localrouter/llm-usage-quota-service-implementation-plan.md` (1436 lines).
All line refs are to that file. Verified live 2026-09-21 against the real accounts on this machine.
Opinions are labelled **[OPINION]**; everything else is a verified measurement or direct quote.

---

## 1. The plan's central premise is factually wrong

The plan is built on the assumption that provider quota is fundamentally unobservable and must be
*estimated*: line 11 ("where observable"), line 12 ("Estimate usage where authoritative provider data
is unavailable"), §9 lines 554-595 (acquisition mechanism left open per provider), line 592
("Whether UI-derived quota collection is permitted"), §16 886-895 (collector failure → stale + lower
confidence). It never names a single authoritative API.

**Verified: authoritative, per-account, both-window quota endpoints exist for two of the three
providers the user cares about.**

### Codex / ChatGPT — `GET https://chatgpt.com/backend-api/wham/usage`
Headers: `Authorization: Bearer <oauth access_token>`, `ChatGPT-Account-Id: <chatgpt_account_id>`.

Live response for this machine's two pooled accounts:

| account | plan | primary (5h) | secondary (weekly) | allowed | limit_reached |
|---|---|---|---|---|---|
| `openai-codex-oauth-1` (`priority 0`) | plus | **used_percent 100**, `limit_window_seconds 18000`, reset in 3641s | used_percent 16, `604800`, reset in 590441s | **false** | **true** |
| `openai-codex-oauth-2` (`priority 1`) | team | used_percent 54, `18000`, reset in 15451s | used_percent 8, `604800`, reset in 602251s | true | false |

Also returns `reset_after_seconds`, `rate_limit_reached_type`, `credits`, `rate_limit_reset_credits`,
`additional_rate_limits`. This is *exactly* the rolling 5-hour + weekly model the user described,
per account, with reset timestamps and an explicit boolean gate.

### Ollama Cloud — `GET https://ollama.com/api/usage`
Header: `Authorization: Bearer $OLLAMA_API_KEY`. Live response:

```json
{"activity":{"cost":"0.00000","period":{"type":"last_4_weeks","starting_at":"2026-08-31T00:00:00Z", ...},"models":[]},
 "limits":{"session":{"usage":0.484,"models":[{"name":"deepseek-v4.1-flash","request_count":1602}]},
           "weekly":{"usage":0.086,"models":[...]}}}
```

`session` ≈ the 5-hour window; `usage` is a **0–1 consumed fraction, not tokens**; `weekly` is the
weekly window. **No `reset_at` is returned** — only `activity.period` bounds. Token-level quota and
reset timestamps are not exposed.

**Consequence:** "percent remaining" is not an estimate for either provider. §8.3 (estimation),
§8.4 (reconciliation) and most of §10 (forecasting) are solving a problem the user does not have.

---

## 2. Overengineering, quantified

- 1436 lines, 25 sections, 9 phases (1001-1215), 13 explicit "engineering decision" deferrals,
  12 "optional" items.
- Phase 0 (1003-1027) mandates a versioned schema document + example payloads + validation rules
  as a *gate before any code*, for a system the user wants as a lightweight binary.
- §23 lists nine categories of decisions to defer (1301-1376); §24 (1383-1405) prescribes a
  9-stage build order whose first five stages are all pure accounting.

**Verified: a large fraction of the plan is already implemented inside Hermes on this machine**
(`/home/wporter/.hermes/hermes-agent`, v0.20.5 + local patches):

| plan section | already exists as |
|---|---|
| §3.2 collectors / §9 quota collection | `agent/account_usage.py` (902 lines) — Codex `/wham/usage`, `/wham/rate-limit-reset-credits`, consume; plus Anthropic + OpenRouter. `_codex_backend_urls()` 428-445, `_fetch_codex_account_usage()` 510, `AccountUsageWindow/Snapshot` 26-47 |
| §3.4 / §4.6 / §7.5 context composition | `agent/context_breakdown.py` (360 lines) — categories `system_prompt, tool_definitions, rules, skills, mcp, subagent_definitions, memory, conversation` |
| §4.5 usage events / §3.7 analytics | `agent/aux_accounting.py` (138 lines) — ContextVar session accounting for aux calls, with `_EXCLUDED_TASKS` to avoid MoA double-count |
| §8.1 provider-reported usage | `agent/codex_runtime.py:105-180` `_record_codex_app_server_usage()` → `CanonicalUsage` + `estimate_usage_cost` (`agent/usage_pricing.py`), `_session_db.queue_token_counts()` |
| streaming usage capture (line 1324) | `auxiliary_client.py:9084,9290`, `chat_completion_helpers.py:3933` set `stream_options={"include_usage": True}`; extracted at `chat_completion_helpers.py:4000` |
| §4.2 Account identity | `agent/credential_pool.py:718-740` `_codex_chatgpt_account_id()` (JWT `chatgpt_account_id`) |
| header-based quota capture | `run_agent.py:4177 _capture_rate_limits()`, `agent_init.py:1027-1029 _rate_limit_state`, `agent_runtime_helpers.py:4412-4418` (`retry-after`, `x-ratelimit-reset`) |

**[OPINION]** The plan re-specifies ~60–70% of working code, in a second process, from scratch.
The MVP should package and extend the existing in-process modules rather than build a parallel service.

---

## 3. Missing requirements (the actual stated need)

### 3.1 OpenAI-compatible auth hub / proxy — absent
Line 28 makes "Proxy all LLM traffic" an explicit **non-goal**; the only other mention is line 1328
as an open decision. Nothing covers request-side semantics. The load-bearing omission:

- **Wire surface.** This machine runs `provider: openai-codex`, `base_url: https://chatgpt.com/backend-api/codex`
  (`~/.hermes/config.yaml:3-4`), and Hermes' Codex transport is `agent/transports/codex.py` +
  `codex_app_server.py`/`codex_app_server_session.py` — i.e. **Responses API over a JSON-RPC
  app-server session with its own `session_id`**, not `/v1/chat/completions`. A chat-completions-only
  hub cannot serve the primary ChatGPT/Codex account at all. This is the single biggest gap.
- **Credential injection / refresh ownership.** The hub must inject `ChatGPT-Account-Id` and own
  OAuth refresh. Plan line 865 lists "Credential rotation" only as an open decision.
  Verified hazard: `~/.hermes/auth.json` `credential_pool.openai-codex[*]` each carry **both**
  `access_token` and `refresh_token`, refreshed today by Hermes (`last_refresh`). Two writers
  refreshing the same rotating refresh token is a real corruption path — the same failure class the
  `hermes-codex-oauth-account-isolation` skill documents.
- **Streaming relay** (line 1324) is load-bearing, not optional: Codex emits a terminal `token_count`
  event carrying `rate_limits` `{primary:{used_percent, window_minutes}}` per turn, plus
  `response.usage`. Hermes already parses the usage half (`codex_runtime.py:1060`).

### 3.2 Reserve policy — specified generically, not as the user's rule
§12.2 (718-730) offers five reservation kinds and defers to "simple percentage or absolute
reservations". The user's requirement is a specific three-entry rule. §4.2's `Account` entity
(198-211: `account_id, provider_id, display_name, account_type, metadata, enabled`) has **no
priority, no reserve, no health/exhaustion field** — there is nowhere to express
"primary is reserve-protected, secondary is overflow".

**[OPINION]** Reuse what already exists instead of inventing an account registry: `auth.json`
already encodes rotation as `priority` (0,1) plus `last_status` (`exhausted`/`ok`),
`last_error_code` (429), `last_error_reason` (`usage_limit_reached`), `last_error_reset_at`.

**Note:** "reserve 10%" is well-defined and directly computable — admit only while
`used_percent < 90`. But §4.7 (281-301) keeps `used`, `remaining`, `used_pct`, `remaining_pct`, `unit`,
`window_start`, `window_end`; for these providers only `used_percent` (Codex) or a 0–1 `usage` fraction
(Ollama) exists. Those other columns will be null forever.

**[OPINION]** Note the reserve is currently moot on the primary: acct-1 is at `limit_reached=true`
with `reset_at` 3641s out. It only becomes meaningful after that reset.

### 3.3 Acceptance criteria don't include enforcement
§22 (1276-1294) is 14 criteria, all read/observability. **None** requires enforcing a reserve,
refusing admission, or attribution correctness. A system meeting the plan's own definition of
"useful" cannot do the thing the user asked for.

---

## 4. Internal inconsistencies

1. **line 11 vs line 592** — "where observable" vs leaving acquisition mechanism entirely open;
   §16 nonetheless assumes a "collector" abstraction for what is two HTTP GETs. **[OPINION]** delete it.
2. **line 31 vs §8.2/§8.3** — "Guarantee exact token counts" then tokenizer mapping/versioning and
   estimation heuristics, while **both** providers report counts authoritatively (Ollama
   `prompt_eval_count`/`eval_count`/`prompt_eval_cached_count`; Codex `inputTokens`/`outputTokens`/
   `cachedInputTokens`/`reasoningOutputTokens` — `codex_runtime.py:156-160`). §8.4 reconciliation
   only pays off if estimates disagree with authoritative data. **[OPINION]** delete §8.2/8.3/8.4.
3. **line 264 vs §12.1/§11** — "Multiple usage events may exist for the same request" (append-only
   log) but hysteresis and admission need a *current* value, and there is **no tie-break rule** for
   which of N samples for one window is authoritative. Verified gap.
4. **§7.1/§10** — burn rate + projected exhaustion over "configurable windows", when both endpoints
   already hand you `reset_at`/`reset_after_seconds`. **[OPINION]** keep "seconds until reset", delete
   the forecasting engine.
5. **§14 (802-826)** — exposes `llm_quota_remaining_ratio`/`llm_quota_burn_rate` as scrape-time
   metrics while the only source is an on-demand authenticated GET; no mechanism specified. Verified gap.
6. **§15.1/§15.3 vs hub reality** — the plan's threat model (§15.4 869-879) assumes local read-mostly
   telemetry. A hub that injects the user's ChatGPT credentials into outbound requests **is a
   credential broker** — a strictly larger responsibility. **[OPINION]** this is the one area the MVP
   must be *more* careful than the plan, not less.

---

## 5. Privacy / security

- §15.1 (no content by default) becomes **mandatory** on the proxy path (the hub sees full prompts),
  not a preference. Verified: nothing in the plan mandates this because the proxy path doesn't exist.
- **Retry/fallback data-flow leak:** §11 line 664 lets the *caller* execute "Prefer alternate provider".
  Any failover re-sends the request body to a different provider account, and changes which account is
  charged — precisely the attribution the user wants metered. **[OPINION]** the hub must own failover
  and record the account actually used.
- **Token duplication:** `~/.hermes/auth.json` holds live `access_token` + `refresh_token` for both
  ChatGPT accounts. The plan never says where the hub reads credentials from. **[OPINION]** read the
  existing pool (or be handed tokens by Hermes) — do not create a second copy.
- §15.4 (873-879) requires TLS only "if exposed beyond localhost". **[OPINION]** loopback-only bind +
  one static bearer token; no read endpoint echoes token material.

---

## 6. Concurrency / race conditions — plan is silent and the risks are real

**Verified absent:** zero occurrences of `atomic`, `transaction`, `lock`, or `concurren*` in 1436 lines.
§16 (882-925) covers collector/tokenizer/persistence/policy failure but has **no request-path failure
analysis**, because the plan has no request path.

Races the hub must handle:

- **R1 — reserve breach under concurrency.** Two concurrent admits both read `used_percent=88`, both
  pass a 90% gate, both consume >10%. Needs an in-process *pessimistic reservation*
  (`used_frac + in_flight_frac <= 1 - reserve`), not read-then-act.
- **R2 — blind window.** `used_percent` only refreshes after completion or a throttled poll. Hermes'
  existing 429 handling (`agent_runtime_helpers.py:1092,1528,4412`) is *reactive* — it discovers
  exhaustion by receiving a 429. Local reservation is the only way to do better.
- **R3 — refresh stampede.** Rotating refresh tokens; concurrent refreshes invalidate each other
  (documented by the `hermes-codex-oauth-account-isolation` skill). Needs single-flight per account.
- **R4 — streaming usage at the tail.** Client disconnect mid-stream may mean `usage` is never seen.
  Must be recorded as unknown, and unknown must not silently pass the reserve gate.
- **R5 — reset boundary.** Sample says 99% used with `reset_at` 2s away; the request completes after
  reset. Naive last-sample logic keeps the reserve engaged for up to 5h. Reserve state must be keyed
  on **window identity (`reset_at`)**, not timestamp.

---

## 7. Provider quota uncertainty — framing is inverted

Uncertainty is **low** for the user's accounts (two undocumented-but-stable endpoints, verified live)
and **high** only in the sense that both routes are unpublished: OpenAI's `wham/usage` lives under
`/backend-api` outside the public API; Ollama's `/api/usage` is undocumented (upstream issues
#15663 / #16448). **[OPINION]** §9's confidence/quality machinery collapses into one honest field:
which endpoint answered, and when.

Two real caveats to keep:
- `used_percent` is not perfectly fresh or monotonic — community reports describe values jumping and
  reset timers lagging. **[OPINION]** §12.1 hysteresis (705-716) is genuinely useful, but for
  *upstream jitter suppression*, not threshold oscillation. Reclassify it.
- **Ollama has no `reset_at`.** The hub must infer the 5h session boundary locally (from when `usage`
  last dropped). Verified: live response carries only `activity.period` bounds.

---

## 8. Account attribution

§4.2's `Account` is a registry; the user needs a **selector with provenance**. §4.9 Policy State
(318-330) is per-account but poll-driven (`last_evaluated_at`), not request-driven.

**[OPINION]** Attribution must record the identity derived from the credentials used
(`_codex_chatgpt_account_id()`, `credential_pool.py:718-727`), never the client-supplied `account_id`
— that value is untrusted. Verified the pool already carries `id`, `label`, `priority`,
`last_status`, `last_error_*`, `request_count` and JWT-derived `chatgpt_account_id`: a working
attribution + failover substrate already on disk.

---

## 9. Proxy vs. direct instrumentation — decision

**[OPINION] Hybrid, because the Codex wire protocol forces the split.**

- **Do not** try to serve ChatGPT/Codex from a `/v1/chat/completions`-only hub. The Codex path is
  Responses-API/JSON-RPC app-server (`agent/transports/codex.py`, `codex_app_server_session.py`);
  a shim would have to reimplement that protocol. High cost, low payoff.
- **Do** proxy **Ollama Cloud** (`base_url: https://ollama.com/v1`, plain OpenAI-compatible) and any
  other plain OpenAI-compatible provider. The hub gets exact per-request usage from the response
  (`prompt_eval_count`/`eval_count`/`prompt_eval_cached_count`, including the final streaming chunk)
  and can enforce reserves *before* dispatch. Cheap, and satisfies "one auth hub" literally.
- **Do** instrument **ChatGPT/Codex in-process**, extending what exists: authoritative per-account
  quota (`account_usage.py`), per-turn usage (`codex_runtime.py:105+`), account identity
  (`credential_pool.py:718`), rate-limit capture (`run_agent.py:4177`). A proxy buys little and
  invites the refresh contention in R3.

Rationale: metering fidelity and enforcement both come from reading *authoritative usage*, and that
read is identical whether or not traffic is proxied. A proxy earns its cost only where it removes a
provider-specific auth burden or is the only way to observe usage — for Codex it is neither.

**Honest counter-argument:** without a proxy, non-Hermes clients (raw `codex` CLI, VS Code extension)
stay unmetered. If "everything I run is metered" is the real requirement, the hub must proxy those too
and therefore must speak the Codex surface. **[OPINION]** Scope v1 to OpenAI-compatible
chat-completions clients and explicitly accept that native Codex clients remain outside the boundary.
This is a scope decision the user should make.

---

## 10. Recommended MVP

### Delete
- §8.2 local tokenization, §8.3 estimation, §8.4 reconciliation — both live providers report tokens.
- §9 collector framework — two HTTP GETs plus a throttle.
- §10 forecasting engine — pass through `reset_at`/`reset_after_seconds`.
- §3.4 / §4.6 / §7.5 context composition *as new work* — already exists (`context_breakdown.py`).
- §6's four write endpoints — the hub observes the request itself; there is no separate accounting API.
- §13 dashboard, §14 Prometheus, §18 logging spec, §19 retention/rollups, §21 load + security test plans.
- §23's nine decision categories and §20 Phase 0's schema-document gate.

### Defer (not v1)
§12.2's five reservation kinds; §4.9 Policy State as an entity; §3.8's widget/CLI/Prometheus consumers;
§11's ten constraint types.

### Add
1. **Quota adapter interface, exactly two implementations:** `codex_wham`, `ollama_usage`.
   Normalized: `{account, window_kind: session_5h|weekly|four_week, used_frac, window_seconds,
   reset_at|null, allowed|null, fetched_at, source}`.
2. **Per-account reserve policy**, flat config: acct-1 `reserve_frac 0.10`, acct-2 `0.00`, ollama `0.10`.
3. **Reservation-based admission gate** (the single most important new component):
   `admit iff used_frac + in_flight_frac <= 1 - reserve_frac`, in-flight tracked per
   (account, window) and released on completion/abort.
4. **Account selector with provenance** — pick by `priority` among gate-passing accounts; record the
   JWT-derived `chatgpt_account_id` actually used on every request record.
5. **Single-flight token refresh** per account; reuse `last_status`/`last_error_*` as the health record.
6. **Streaming-safe usage capture** — parse final chunk / Codex `token_count`; on absence record
   `usage_unknown=true` and count it against an unknown-usage budget instead of passing silently.
7. **One local JSON API, three routes:** `GET /accounts` (normalized quota + reserve headroom),
   `POST /admit`, `POST /usage`.
8. **Loopback-only bind, one static bearer token, no token material in responses, no bodies persisted.**

**Boundary:** single static binary, one SQLite file, three tables (`request`, `usage`,
`quota_sample`), two outbound GET adapters, one admit gate, one local API, two provider surfaces
(Ollama proxied, Codex instrumented in-process).

### Acceptance tests (falsifiable)
1. **Reserve enforcement** — stub `used_percent=91`, assert `POST /admit` denies and no upstream call
   is made; stub 89 with no in-flight, assert allow.
2. **In-flight reservation** — stub `used_percent=85`; 3 concurrent admits at 4% each; exactly 1
   admits, 2 deny.
3. **Per-account reserve** — acct-1 at 91% (deny), acct-2 at 54% (allow): selector routes to acct-2 and
   the recorded attribution is acct-2's `chatgpt_account_id`, not the requested label.
4. **Attribution on failover** — force a 429 on acct-1 mid-request; the request record names the
   account that actually served it, and acct-1's health flips to `exhausted` with
   `last_error_reset_at` = the endpoint's `reset_at`.
5. **Streaming usage** — relay a Codex SSE stream, assert usage captured from the terminal
   `token_count`/`response.usage` event; abort at 50% and assert `usage_unknown=true` and the
   in-flight reservation is released.
6. **Reset boundary** — sample `used_percent=99` with `reset_at` 2s away; after the reset the reserve
   releases without waiting for a new sample (window-identity keying).
7. **Refresh single-flight** — 10 concurrent calls with an expired access token: exactly one refresh,
   10 successes, no token collapse.
8. **Ollama fraction semantics** — stub `limits.session.usage=0.484`; assert normalized `used_frac`
   is `0.484` (not 48.4), window kind is 5h, and a 10% reserve denies at `usage >= 0.90`.
9. **Secrets** — no response body or log line contains a token substring; hub refuses to bind
   non-loopback without an explicit flag.
10. **No content** — the SQLite file contains no column populated from request bodies.

### One question the user must answer
Does "primary vs secondary Codex account" mean the `priority` 0/1 the credential pool already has?
**[OPINION]** Yes: verified `openai-codex-oauth-1` is `priority 0`, currently `exhausted`
(`limit_reached=true`), and `openai-codex-oauth-2` is `priority 1`, `plan=team`, `allowed=true`.
The recommended defaults map cleanly onto those two entries; the 10% reserve on the primary only
becomes meaningful after its current window resets.
