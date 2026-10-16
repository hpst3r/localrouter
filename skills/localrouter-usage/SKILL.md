---
name: localrouter-usage
description: "Use when gating agent work on LocalRouter quota."
version: 1.0.0
author: William Porter (hpst3r), Hermes Agent
license: AGPL-3.0
platforms: [linux, macos, windows]
metadata:
  hermes:
    tags: [localrouter, quota, reserve, admission, usage, cost, mcp, delegation]
---

# LocalRouter usage & admission for agents

Project-owned, portable workflow for agents that spend capacity metered by
LocalRouter. Ships with the LocalRouter repository (`skills/localrouter-usage/`)
and assumes nothing about the host, account IDs, model aliases, key paths or
provider plans in play — discover those from the deployment under test.

Interface contract: [`docs/MCP.md`](../../docs/MCP.md). Server behavior:
[`docs/SPEC.md`](../../docs/SPEC.md).

## When to use

- Before a worker wave, a long autonomous run, or expensive repeated verification.
- "How much quota / tokens / cost is being used?", "why is this model or traffic missing from usage?".
- Deciding whether background work may start, and how much of it.
- Reconciling a reserve policy against observed usage, or reporting scope honestly.

Don't use for: changing routing, reserves, keys or configuration; reading
provider credentials; forcing work onto a path the operator did not authorize.

## 1. Discover the deployment, don't assume it

Find the control base URL, whether `control.require_auth` is on (then a client
key file is needed), and the actual account IDs, model/route names and groups in
play. The base URL, the auth setting and the model catalog come from the
deployment's own configuration or from whoever operates it — a `quota_status`
call does not answer them: it reports per-account quota status and the registered
client list. Never hard-code an account ID, model alias, key path or URL from
another deployment or from documentation examples.

**Completion:** you can name the control URL, whether a client key file is
required, and at least one account ID the router itself reports.

## 2. Read live state

Preferred — call the MCP tools through the host's MCP client integration. The
host launches this persistent stdio server (not a one-shot terminal query):

```text
localrouter mcp [--url URL] [--key-file FILE] [--timeout 10s] [--max-result-chars 24000]
```

| tool | arguments | reads |
|------|-----------|-------|
| `quota_status` | `account` (optional) | `GET /control/v1/status` — per-account windows, reserves, freshness, admissibility, cooldown, in-flight |
| `usage_summary` | `since` (default `24h`, ≤ `90d`), `group` (default `model`), `limit` (default `20`, ≤ `100`); no filters | `GET /control/v1/usage` — recorded ledger rows |
| `check_admission` | exactly one of `account`/`model`, `class` (`background` default, or `interactive`) | `POST /control/v1/admit` — dry-run decision |

Fallback — plain HTTP against a running server. Prefer the MCP tools or the
agent's native HTTP tool over shelling out; where only a shell is available, use
`terminal(command="curl --fail --silent --show-error --max-time 10 <url>")` on a
POSIX host, and on Windows the shell's own HTTP tool rather than an assumed GNU
`curl`:

```bash
curl --fail --silent --show-error --max-time 10 'http://127.0.0.1:8787/control/v1/status'
curl --fail --silent --show-error --max-time 10 'http://127.0.0.1:8787/control/v1/usage?since=24h&group=model'
localrouter admit --class background --account ACCOUNT_ID --json   # exit 0 allow, 1 deny, 2 error
```

For a remote router add `--url https://<router-host>` and
`--key-file <approved-existing-key-file>`. Plaintext HTTP to a non-loopback host
is refused.

**Completion:** know the account/route, the snapshot age and staleness, every
relevant window, the reserves, and any error — a health check is neither quota
telemetry nor an inference test. A result can also be bounded by the adapter's
content budget, so read `retrieval` (`truncated`, `dropped_items`) and `counts`
(`rows_truncated`, `*_returned` vs `*_total`) before treating it as complete, and
treat `source.upstream_schema_version: null` as "the endpoint reported no
version" rather than assuming one.

## 3. Interpret headroom correctly

- Fractions are 0–1. Compute (`headroom = max(0, remaining_frac − reserve[kind])`) with a tool, per window.
- A reserve is the operator's floor; a missing reserve entry means no configured reserve for that window, not a new threshold you may pick.
- The tightest applicable window constrains the run. Do not sum short and weekly allowances, and do not assume a short reset replenishes the week.
- `stale`, `snapshot_age_s`, `error`, empty `windows`, and `rolled` are read independently of `healthy` / admissibility booleans. A rolled window is the router's interpretation of a passed reset, not a fresh provider measurement.
- Shared pools: a model-specific allowance may still draw the same account allowance.
- Empty or missing telemetry means **unknown** headroom, not unlimited. Do not launch an optional large batch on that basis; diagnose, or use an already-authorized alternative with known headroom.

## 4. Gate the actual workload

Check admission for the class and the exact account or route the work will use —
not a different pool with spare quota. Exit 0 allows, 1 denies, 2 is an error:
never read an error as permission. A deny means pause, reduce scope, or move to
an independent already-authorized pool; it never means relabeling background work
as interactive to reach protected reserves. Record the decision and reason.

## 5. Make each wave economical

- Use local deterministic tools for arithmetic, parsing, deduplication, filtering, builds, transforms; spend model calls on judgment.
- Keep the primary on architecture, integration and verification; delegate bounded independent work to the authorized workers only.
- Give workers exact objectives, the needed evidence, scope boundaries, acceptance criteria and a compact result schema — no full-repo dumps, no duplicated discovery, no transcripts pasted back to the parent.
- Disjoint ownership per worker; independent review when it addresses a concrete risk, not as ritual.
- Finite worker count, execution timeout, output bound, retry budget; account for workers already running on every known host.
- Keep prompt prefixes and conversation/cache affinity stable; measure cache behavior rather than assuming it.
- Reuse verified artifacts and tests; rerun only checks invalidated by edits.
- Classify failures before retrying; respect cooldown/rate-limit; cap retries. On exhaustion, wait for the relevant reset or use an independent pool — another model on the same exhausted pool is not a fallback.
- Stop redundant workers at a safe boundary; checkpoint resumable progress.

## 6. Recheck between waves, not per tool call

Recheck status and admission before each new wave, after a long wave, after a
quota/rate-limit error, or when the provider switches. Reuse a fresh snapshot
inside a wave; the router already polls providers. If headroom approaches the
reserve or admission denies, stop launching optional work and wind down owned
workers. This cooperative behavior is not a hard cap.

## 7. Verify and report

State, for every figure: the account/provider, retrieval time, snapshot age and
staleness, the window(s) with used/remaining/reserve/reset, the admission
decision and reason, the ledger interval and dimensions, the unknown-usage and
unpriced counts, and the accounting semantics of any token or cache number.
Label each cost figure by what it is: a locally priced `metered` / `api_equivalent`
estimate (a subscription account's API-equivalent figure is a list-price
estimate, not spend), a provider-reported cost, or unknown — an unknown price is
unknown, never zero. Report truncation and partial coverage instead of implying a
census. Never add alternate ledger groupings together: `group=model` and
`group=account` are two views of the same recorded requests. Add
transcript-derived or other-host figures only after verifying they are not
already imported into the ledger rows you are reporting.

## Safeguards (non-negotiable)

- **Quota, ledger and cost are three different things.** Provider quota is
  account-wide and provider-reported; the ledger holds the rows this router
  recorded plus rows imported from other hosts/clients, and still misses traffic
  no client recorded; cost is a price resolved per row (a local
  `metered`/`api_equivalent` estimate, or a provider-reported figure).
- **Reserves belong to the operator.** Never change a route, reserve, key, class
  or the active model to turn a denial into an allowance without explicit
  authorization. Admission is a dry-run policy check, not a reservation and not
  a concurrency slot.
- **No data means unknown**, not zero and not unlimited.
- **Bounded waves.** A dry run cannot coordinate concurrent launchers or other
  hosts; external CLI workers need their own supervision and repeated checks.
- **Preserve explicit routing and privacy.** Do not silently downgrade
  quality-sensitive work, override a chosen provider/model, or move content to a
  new destination to save tokens.
- **Inspect, don't mutate.** This workflow reads; it does not edit config,
  reserves or keys, restart services, or create keys.
- **Respect provider terms for subscription inference.** Where a provider requires
  its subscription inference to run through its own client (Claude subscription
  plans), route it there. LocalRouter may check quota and import accounting, but
  the MCP adapter neither proxies that inference nor reads provider credentials —
  its only file is the client key it is launched with.
- Keep machine-specific keys, hosts and personal account details out of shared
  docs and skill text.

## Pitfalls

- Reading a stale snapshot as current; `healthy` does not imply fresh telemetry.
- Treating `unknown_usage_requests` (requests whose token usage the router could
  not meter) as the same as `unpriced_requests` (no usable price, so `cost_usd`
  is null). They are independent counters.
- Reading one ledger total as the account's usage: rows imported from other
  hosts/clients are included, and traffic no client recorded is absent.
- Summing alternate groupings (`group=model` vs `group=account`) that describe the same requests.
- Assuming the adapter has filters: `usage_summary` takes `since`/`group`/`limit` only, because the control API exposes no filters.
- Treating a capability refusal (`capability_requirements_required` on a capability-routed route) as an allow — it is a refusal.
- Expecting remote plaintext HTTP to work: non-loopback `http` is refused at startup.
- Confusing the adapter's tool allowlist with server-side scoped authorization; existing client keys may carry broader authority.
- Retrying a denied admission unchanged, or relabeling class to evade reserves.

## Verify

- Frontmatter parses and `description` stays a self-contained trigger.
- The documented commands and links resolve in a checkout of this repository
  (`docs/MCP.md`, `docs/SPEC.md`, `scripts/mcp-smoke.py`).
- Any usage or quota statement in a report cites its source, window, freshness
  and (for cost) its basis.
