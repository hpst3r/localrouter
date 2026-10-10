# Capability routing — operator examples

This is a **worked-examples companion** to the authoritative specification
([`SPEC.md`](SPEC.md), "Capability routing (`routes[].upstreams`)"). Read SPEC.md
for the contract; read this page for copy-pasteable config, the admission commands
you run to check a route before you trust it, and the upgrade/rollback story.

Nothing here is fetched at runtime and nothing here is a secret. Every host, id
and path below is a **placeholder** — substitute your own. LocalRouter never
reads a credential out of the config file; keys live in separate `0600` files (or
environment variables) and are never echoed in logs, responses or diagnostics.

---

## 1. The idea in one paragraph

A route maps one **stable, client-facing model name** (an *alias*, e.g. `fast` or
`vision`) to an ordered list of **configured accounts**. A route **without** an
`upstreams` block is *unconstrained*: the alias forwards to whichever healthy
account policy picks, exactly as before this feature existed. A route **with** an
`upstreams` block is *capability-constrained*: each candidate account carries a
**descriptor** that declares the protocols, input modalities and optional features
that backend actually supports, and the router narrows the candidates to the ones
that can serve the request. A constrained route also changes how a request is
**priced** (see §7) and it **fails closed** on anything it cannot classify (§5).

---

## 2. Full worked example

Two aliases — one legacy, one constrained — over two configured `openai_compat`
accounts. Replace `ACCOUNT_*`, the `.invalid` host and the key paths with your own.

```yaml
listen: 127.0.0.1:8787
data_dir: ~/.config/localrouter

clients:
  - {name: me, class: interactive, key_file: keys/me.key}

accounts:
  # Two upstream providers. Provider `openai_compat` sends /v1/responses to
  # <base_url>/responses and /v1/chat/completions to <base_url>/chat/completions.
  - id: ACCOUNT_FAST                      # PLACEHOLDER id — name it what you like
    provider: openai_compat
    base_url: https://api.example.invalid/v1   # PLACEHOLDER host, not a real one
    api_key_file: keys/ACCOUNT_FAST.key         # secret in a 0600 file, not here
  - id: ACCOUNT_VISION
    provider: openai_compat
    base_url: https://api.example.invalid/v1
    api_key_file: keys/ACCOUNT_VISION.key

routes:
  # ---- legacy / unconstrained: no `upstreams`, byte-identical old behaviour ----
  - name: fast
    models: [fast]                       # the client-facing alias
    upstream_model: gpt-fast-2026        # route-wide alias: sent for every candidate
    interactive: [ACCOUNT_FAST, ACCOUNT_VISION]
    background:  [ACCOUNT_VISION]

  # ---- capability-constrained: `upstreams` opts the route in -------------------
  - name: vision
    models: [vision]                     # a different stable alias
    interactive: [ACCOUNT_VISION, ACCOUNT_FAST]
    background:  [ACCOUNT_VISION]
    upstreams:                           # a descriptor for EVERY candidate above
      ACCOUNT_VISION:
        upstream_model: gpt-vision-2026  # this candidate's own backend model
        protocols: [responses, chat]     # REQUIRED: explicit, non-empty, no dups
        input_modalities: [text, image]  # REQUIRED: explicit, non-empty, no dups
        tools: true                      # booleans: absent == false == unsupported
        json_schema: true
        stream: true
      ACCOUNT_FAST:
        upstream_model: gpt-fast-2026
        protocols: [chat, responses]
        input_modalities: [text]         # text-only: this candidate is not vision
        tools: true
        json_schema: false
        stream: true
```

What this buys you:

- The client keeps asking for the stable alias `vision`; the *backend model* is
  chosen per candidate (`gpt-vision-2026` vs `gpt-fast-2026`) and the chosen
  backend's name is what gets rewritten into the request body's `model`.
- An image-bearing request never reaches `ACCOUNT_FAST`; a text-only request may
  reach either, in the operator's declared order.
- `fast` is untouched: same alias semantics and same pricing as before the feature.

Validate it before you serve it:

```bash
localrouter check -config ~/.config/localrouter/config.yaml
```

`check` runs the same strict validation the server does, so a missing descriptor, a
typo in a candidate id, a duplicate protocol or an `input_modalities` list that
isn't a subset of `{text, image}` is caught here rather than at reload time.

---

## 3. Descriptor reference

`routes[].upstreams` is a map from candidate **account id** → descriptor. It lives
on the route (not on the account) so a capability edit rides the normal
`routes[]` reload path (§8).

| Field | Type | Required | Meaning |
|---|---|---|---|
| `upstream_model` | string | no | The backend model to send for this candidate. Empty inherits the route-wide `upstream_model`; if that is also empty the client model is forwarded unchanged. |
| `protocols` | list | **yes** | Non-empty, duplicate-free subset of `chat`, `responses`. A `codex` candidate may **not** list `chat`. |
| `input_modalities` | list | **yes** | Non-empty, duplicate-free subset of `text`, `image`. `image` alone is a **vision-only** candidate and does **not** imply `text`. |
| `tools` | bool | no | Candidate can serve tool/function calling. Absent ⇒ `false` ⇒ unsupported. |
| `json_schema` | bool | no | Candidate can serve structured output (`json_schema` / `json_object`). Absent ⇒ `false`. |
| `stream` | bool | no | Candidate can serve a streaming response. Absent ⇒ `false`. |

Rules the loader enforces (all of them are `localrouter check` errors):

- A non-empty `upstreams` **must** contain a descriptor for **every** member of
  `interactive ∪ background`, and **no** key that is not a candidate. Missing
  descriptors and unknown ids are both rejected — typo protection, like reserves.
- `protocols` / `input_modalities` must be **explicitly supplied**. Inside a
  non-empty `upstreams` an absent field means *unsupported*; there is no implicit
  `text`, and no implicit protocol.
- Values are whitespace-trimmed.
- A route with no `upstreams` (absent, `null` or `{}`) is **unconstrained** and
  skips all of the above — byte-identical to a build without this feature.
- Deliberately *not* enforced: a text-only route is not required to have an
  image-capable candidate. Declarations only **narrow** a candidate's
  applicability; they never add a candidate or widen one to satisfy a request.

---

## 4. What a request *needs* — how the router reads the body

On a constrained route, classification inspects **typed structural fields only**.
It never scans free-form prompt text and **never dereferences a URL**.

Member names are read **exactly** (case-sensitive, as RFC 8259 and any map- or
dict-based upstream reads them). An object that holds a **duplicate** or a **case
variant** (e.g. `Type` beside `type`, `Content` beside `content`) of a member the
classifier reads fails closed. Otherwise readers that fold case, or that keep the
first or the last duplicate, would see a different request than the router
classified. Members the classifier does not read, and anything nested inside a
value it does not descend into (a tool's or a schema's JSON Schema), are not
checked.

- **Protocol** — the endpoint, not the body: `/v1/chat/completions` ⇒ `chat`,
  `/v1/responses` ⇒ `responses`. A `codex` account is additionally dropped from
  `/v1/chat/completions` before admission.
- **Modalities** — every modality the request *actually carries*, in order of
  first appearance: string instructions, message string content, `input_text`/`text`
  parts ⇒ `text`; `input_image`/`image_url` ⇒ `image`; `input_audio`/`input_audio`
  ⇒ `audio`; `input_file`/`file` ⇒ `file`. `text` is added only when the request
  really carries text, so an image-only request yields `[image]` and can reach a
  vision-only backend. A request with no modelled input at all defaults
  conservatively to `[text]`.
- **Tools** — a non-empty top-level `tools`/`functions` array, or a `tool_choice`/
  `function_call` that is not the string `"none"`. A lone `tool_choice: none` asks
  for nothing.
- **Structured output** — chat `response_format` or Responses `text.format` of
  type `json_schema` **or** `json_object` (v1 models both as one `json_schema`
  requirement); type `text` selects unstructured.
- **Stream** — a boolean `stream` field.

### `function_call_output` and other function payloads (verified)

Per the Responses schema a `function_call_output`'s `output` is either a plain
string (the JSON-encoded result) or an array of typed content parts:

- **string output ⇒ `text`**, and the string is **not scanned** — an image URL
  embedded in the result stays text and will *not* make the request image-bearing.
- **array output ⇒ classified part by part**: an `input_image` part makes it an
  image request; `input_audio`/`input_file` report `audio`/`file`; an **unknown
  content type**, a **typeless object**, or any part the classifier does not model
  **fails closed**.
- **bare object, missing or `null` output ⇒ fail closed.**
- `custom_tool_call_output` has the same `output` shape and is classified the
  same way.
- `function_call.arguments`, `custom_tool_call.input` and `local_shell_call.action`
  are opaque values the model emitted. These call items carry no modality and are
  **not** scanned. `reasoning` is likewise a non-content item.
- `local_shell_call_output.output` must be a string (⇒ `text`).
- `computer_call_output` is likewise classified from its nested payload
  (`computer_screenshot` ⇒ image; string ⇒ text; an unmodelled payload fails closed).
- An assistant `refusal` content part (chat and Responses) is `text`.
- A **typeless** Responses input object is accepted only in the easy-message
  shape, with a `role` and non-null `content`. Any other typeless object fails
  closed.
- These shapes are pinned against the openai-go v3.52.0 param types. Other agent
  items (`web_search_call`, `file_search_call`, `image_generation_call`,
  `code_interpreter_call`, `mcp_*`, `shell_call*`, `apply_patch_call*`, …) are
  still **unknown** and fail closed. Some of them carry generated images or
  server-side results.

"Fails closed" on a constrained route means a **400 `invalid_request_error`** with
no upstream call, no lease and no ledger row — never a silent downgrade to
text-only. A legacy (unconstrained) route never classifies at all, so its behaviour
is unchanged.

---

## 5. Known-but-unsupported inputs and opaque context

- **`audio` and `file`** are *known* input modalities the router can name but that
  **no descriptor can declare** (config only accepts `text` and `image`). A request
  carrying them is therefore unsatisfiable: the capability filter excludes every
  candidate and the request is **400 `capability_unsupported`**. This is explicit,
  not a malformed-body error — a `file` part is a real declared input, it just
  cannot be served in v1.
- **Opaque server-side context** names content this request cannot see, so it is
  **refused as unclassifiable** rather than guessed as text. This covers the
  Responses fields `previous_response_id`, `conversation` and `prompt` when
  non-null (a reusable prompt's template and `variables` may hold images or
  files), and the `item_reference` input item.
- An **unknown item type**, an unknown content type, or a malformed structured
  declaration (e.g. a missing `response_format.type`) also fails closed as
  `invalid_request_error`.

---

## 6. Checking a route before you trust it — the admit CLI

The `admit` command asks a **running** LocalRouter whether a request of a given
class *would* be admitted, **without** creating a lease. It exits **0 = allow**,
**1 = deny** (including a `capability_unsupported` refusal), **2 = usage error /
control API unreachable** (including `capability_requirements_required`).

```bash
# Legacy route: no capability flags needed, unchanged from before.
localrouter admit --class background --model fast

# Constrained route: the profile is REQUIRED. Omitting it (or leaving it
# incomplete) is a 400 capability_requirements_required, not a permissive pass.
localrouter admit --class interactive --model vision \
  --protocol responses --input-modalities image --requires-json-schema --json
```

Capability flags (only used for a `--model` request; combining them with
`--account` is a 400 — the server refuses the ambiguous request rather than guess):

| Flag | Meaning |
|---|---|
| `--protocol chat\|responses` | The wire protocol the request uses. Required for a constrained route. |
| `--input-modalities t1,t2,...` | Comma-separated. CLI vocabulary is `text`, `image`, `audio` (`file` is accepted by the control API but not by the CLI flag). |
| `--requires-tools` | The request sends a non-empty top-level `tools` array. |
| `--requires-json-schema` | The request asks for structured output. |
| `--stream` | The request asks for a streaming response. |

The CLI never fills in a default protocol or modality — it passes exactly what you
gave it, so a constrained route's refusal is surfaced rather than papered over.
On a legacy route, a profile does not narrow candidates by capability, but its
vocabulary is still validated (an unknown protocol or modality is
`capability_unsupported`). On any route, `--protocol chat` drops Codex accounts
exactly as `/v1/chat/completions` does, and a route whose candidates are all Codex
accounts is `capability_unsupported`.
Non-JSON output is a single line (`allow ACCOUNT` / `deny: reason`); `--json` prints
`{"decision","account_id","reason"}`.

Stable error `type` values the control API returns (and the CLI surfaces):

| `type` | When |
|---|---|
| `capability_requirements_required` | A constrained route was dry-run without a protocol, or with no modality named. |
| `capability_unsupported` | The declared profile excludes every candidate, or names something the router does not model (unknown protocol/modality) — includes `audio`/`file`, which are never satisfiable — or declares `chat` for a route whose candidates are all Codex accounts. |
| `invalid_request_error` | Inference-side: a body that cannot be classified on a constrained route. |

---

## 7. Cost attribution — resolved backend vs legacy alias

Pricing is **frozen per request by generation** (see §8), never resolved at
ledger-write time.

- **Legacy (unconstrained) route:** the ledger keys cost on the client-facing
  `model` — the alias pricing path, exactly as before. `pricing_model` stays empty.
- **Capability-constrained route:** the ledger keeps the client-facing `model` for
  analytics *and* sets `pricing_model` to the **resolved backend** for that attempt:
  the serving candidate's `upstream_model`, else the route-wide `upstream_model`,
  else the client model. Two candidates serving the same alias are therefore priced
  at their **own backends**, not at the alias.
- An **unpriced backend stays honestly unpriced**. There is no fallback from an
  unpriced backend to a possibly unrelated alias — the row is simply unpriced
  (`cost_usd` NULL).

The resolved backend name is recorded in `upstream_model` for every attempt; on
constrained routes `pricing_model` is what selects the price. History is only ever
re-priced by the separate offline `localrouter pricing reprice` command, never by a
reload.

---

## 8. Reload, the immutable generation, and restart-only topology

Reloads are triggered by `SIGHUP`, are **serialized** and **coalesced**, and are
**atomic**: one accepted generation publishes its handler, auth, routes, limits,
reserves, price table and generation number together under a single pointer. A
request admitted on generation *N* is costed on generation *N* even if a reload
swaps the price table mid-flight. `Generation` is `1` at startup and increments by
one per accepted reload.

**Hot-reloadable** (no restart): `routes[]` — *including* the optional capability
`upstreams` descriptors and their `upstream_model` values. Swapping a candidate's
backend or editing its modalities is therefore a hot reload; a request held across
the reload keeps the **old** descriptor (old backend, old price) while one admitted
after uses the new one.

**Restart-only** — a reload touching any of these is **rejected whole**, never
partially applied:

- `listen`, `allow_non_loopback`, `allowed_hosts`, TLS cert/key paths, `data_dir`,
  the `pricing_file` **path**, the token-store location, `quota.poll_interval`,
  `control.require_auth`, `host_name`;
- **account topology**: `accounts[].id`, `provider`, `base_url`, `quota_source`,
  `api_key_file`, `api_key_env`, `credentials_file`, `cost_basis`;
- `claude_logs.*`, `hermes_logs.*`;
- the structural timeouts (`timeouts.header`/`body`/`idle`/`shutdown`).

So: re-pointing a descriptor at a different model is a reload; re-pointing an
account's `base_url` or renaming its `id` needs a process restart.

**Rollback of a rejected reload is implicit.** A failed, restart-only or
concurrent attempt publishes nothing: the last-good generation keeps serving and
the generation number does not advance. There is no separate rollback command.
Diagnose from `GET /control/v1/diagnostics`, which reports the last attempt under
`reload:{generation, ok, at, reason?, restart_only?}` (`ok:false` plus a sanitized
`reason`, and for a restart-only change the offending key names). The
`config reload rejected` log line carries only `reason`, `restart_only` and
`generation` — never the raw error or a secret path. Correct the config and
`SIGHUP` again.

```bash
localrouter check -config ~/.config/localrouter/config.yaml   # validate first
kill -HUP "$(systemctl --user show -p MainPID --value localrouter)"
```

---

## 9. Upgrade and rollback (read before you deploy)

Two one-way doors, both worth planning around:

**Config is one-way.** Because parsing is strict (`KnownFields(true)`), a config
that contains `upstreams` does **not parse on a binary that predates this feature**
— the old binary rejects it as an unknown field. The upgrade is one-way;
**rolling the binary back requires deleting the `upstreams` blocks** (or restoring
the previous config). Keep the pre-upgrade config alongside the new one.

**Ledger schema `v3 → v4` is forward-only and one-way.** Capability routing adds two
nullable columns to the `requests` table: `upstream_model` (what the attempt
actually sent) and `pricing_model` (the cost-attribution key). Migration runs
automatically on first start of the new binary and leaves every pre-v4 row's columns
`NULL` — honest "unknown", never a backfilled guess. There is no down-migration: a
pre-v4 binary refuses to open a v4 database with
`ledger: database schema version 4 is newer than supported 3`, so **rolling the
binary back after the migration requires restoring a pre-upgrade backup.** Take the
backup **before** the first start of the new binary.

For a consistent offline backup of the default server directory, stop the service
first (a live copy of just `localrouter.db` is unsafe — SQLite WAL files can hold
committed data not yet in the main file) and follow the procedure in
[`NETWORK.md`](NETWORK.md), "Backup and restore". Note that the archive is not
encrypted by `tar`.

Rolling back the *binary* therefore has two requirements: revert the config (drop
`upstreams`), and restore the pre-migration database backup. Rolling back a refused
*reload* needs neither — the last-good generation is still serving.

---

## 10. Boundaries — what this is *not*

- **Declarative, not a security or privacy boundary.** Capability routing routes a
  request by what it *declares*; it narrows which backend may serve it and it fails
  closed on shapes it cannot classify, but it cannot stop a client from embedding
  pixels in a field the classifier does not model. Treat the descriptors as an
  operator-intent mechanism, not an enforcement or data-governance control.
- **No URL fetching.** The classifier is pure: it reads no configuration, performs
  no I/O and never dereferences a URL. An image *URL* in a string is text; only
  typed image parts make an image requirement.
- **No content retention.** Nothing here logs or returns prompt/response bodies;
  the ledger gains only the two attribution columns above — never content.

---

## 11. Verifying this behaviour yourself

```bash
go test ./...                                            # whole suite
go test ./internal/routing/  -run TestInferFunctionCallOutput -v
go test ./internal/proxy/    -run TestCapabilityFunctionCallOutput -v
go test ./internal/control/  -run TestAdmitConstrained -v
go test ./internal/config/   -run TestUpstreams -v
go test ./internal/app/      -run 'TestHotReload|TestCoreRoutesDescriptorProjection' -v
go test ./internal/ledger/   -run 'Pricing|Migrate' -v   # v3 -> v4
```

The routing package is pure and I/O-free, so these run without network, credentials
or a live service; the proxy and app tests use `httptest` loopback upstreams and
fake fixture key material.
