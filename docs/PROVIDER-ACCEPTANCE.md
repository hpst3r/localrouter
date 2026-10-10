# Provider acceptance smoke test

`scripts/provider-smoke.py` is an **opt-in, live** acceptance check for a
LocalRouter instance you operate. It answers the one question the existing
tooling deliberately does not: *can this router actually finish a real
inference request through the provider its routes select, right now?*

`localrouter check` validates the configuration and the client key files, but
as `docs/NETWORK.md` states it "does **not** prove upstream connectivity, quota
availability, TLS certificate validity, or remote reachability." This tool
covers exactly that gap — nothing more.

> **Live requests spend real money.** Every run with `--live` sends synthetic
> inference requests through LocalRouter to a real upstream provider. That
> consumes provider quota, incurs cost on your provider account, and causes
> network egress from the router host. Run it only against an isolated
> LocalRouter you control, with a key and model you are willing to spend on.

## Contents

- [What it does](#what-it-does)
- [Protocols](#protocols)
- [Safety model](#safety-model)
- [Prerequisites](#prerequisites)
- [Usage](#usage)
- [Options](#options)
- [Exit codes](#exit-codes)
- [Report](#report)
- [Usage and cost provenance](#usage-and-cost-provenance)
- [Timeouts and stalled streams](#timeouts-and-stalled-streams)
- [Running the tests](#running-the-tests)
- [Non-goals](#non-goals)

## What it does

With `--live` and the operator-supplied target it performs, in order:

1. **`models`** *(only with `--check-models`)* — `GET <base>/v1/models` and
   confirms your `--model` appears in the advertised route list. LocalRouter
   synthesizes this list from `routes[].models` (`docs/SPEC.md`), so this is a
   cheap, zero-inference routing check.
2. **`chat_nonstream`** *(or `responses_nonstream`)* — a non-streaming request
   with the small fixed synthetic prompt and a 64-token cap. Parses the JSON
   response and reports the provider's own `usage`.
3. **`chat_stream`** *(or `responses_stream`)* — the same request streamed. Reads
   the response as a genuine SSE stream, incrementally, and verifies the framing:
   `data:` events, multi-line `data:` fields, comment lines, and the protocol's
   terminal event. Reports the usage the stream carried.

The two checks are chosen by `--protocol` (see [Protocols](#protocols)). The
prompt is a short synthetic string. No user data, no repository content and no
real conversation is ever sent.

**A 200 is not a pass unless it carries genuine, nonempty generated text.** An
empty `choices` list, a bare string, a usage-only object, whitespace-only
content, or a stream whose only content is `[DONE]`/a terminal event or
whitespace-only deltas is reported as `no_generation`, not a pass. Token counts
alone never count as a generation.

## Protocols

`--protocol` selects the API surface exercised. It defaults to `chat`, so an
existing invocation is unchanged.

| `--protocol` | Endpoint | Request shape | Stream terminal |
| --- | --- | --- | --- |
| `chat` (default) | `POST <base>/v1/chat/completions` | `messages` + `max_tokens: 64`; `stream_options.include_usage: true` when streaming | `data: [DONE]` |
| `responses` | `POST <base>/v1/responses` | the Codex Responses body: a `message` input with `input_text` parts, `max_output_tokens: 64`, `store: false` | `response.completed` |

The `responses` checks exist so you can accept the exact surface **Codex**
drives. A Responses stream is read for the `response.output_text.delta` event
family; completion is the `response.completed` event, not a chat `[DONE]` (which
Responses does not send, and which is never required there). A `response.failed`
or `response.incomplete` event is a failure even if it is well-formed, and a
stream that ends without a terminal event fails with `missing_terminal`. A
non-streaming response with `status: "failed"`/`"incomplete"` likewise fails.

## Safety model

* **Nothing is discovered.** The tool reads only the `--url`, `--key-file`,
  `--model` and timeout values you pass on the command line. It never reads
  LocalRouter configuration, never looks at `~/.config/localrouter`, never
  consults environment variables and has no default key path. If you do not
  supply a target, it does nothing.
* **`--live` is required.** Without it — including a bare invocation, `--help`,
  or any bad-argument path — the tool makes **zero network calls** and never
  opens the key file.
* **Loopback-only plain HTTP.** `http://` is accepted only for loopback hosts
  (`127.0.0.0/8`, `::1`, `localhost`). Any non-loopback plain-HTTP target is
  refused unless you explicitly pass `--allow-insecure-http`, which sends the
  client key in cleartext and should be a deliberate, temporary choice.
* **Credentials stay out of the URL.** URLs carrying embedded
  `user:pass@host` credentials are refused outright.
* **Redirects are never followed.** A `3xx` response is reported as a failure
  and the `Location` header is never read or printed, so the bearer key cannot
  be replayed to another origin.
* **No secret ever reaches the output, and no provider text is surfaced.** The
  client key, the synthetic prompt and every response body are never written to
  stdout, to the report, or into an error message. **No provider-controlled
  error `type`, `message` or other detail is ever surfaced** — `detail` is
  always `null`, and provider errors are reduced to a stable local code
  (`http_status` / `error_object` / `stream_error`). Nothing is matched back out
  of a provider error with a regex, because a plausible-looking error token can
  itself be (part of) a credential.
* **Bounded reads.** Every response body is read under a hard 1 MiB cap, and a
  single SSE line/event is capped at 64 KiB, so a broken or hostile endpoint
  cannot exhaust memory. The cap is enforced with a single extra byte of
  look-ahead, so a body of exactly the cap is not misreported as oversized.
* **Finite execution, except DNS.** Connect, per-read and overall deadlines
  bound the run, including a stalled SSE stream. Each connect/header/read socket
  timeout is clamped to the time remaining in the overall budget, so a per-read
  stall cannot extend the run past `--timeout`; on the main thread a wall-clock
  alarm also interrupts a stalled TLS handshake, a stalled header and a body
  that trickles bytes, and reports `overall_timeout`. DNS resolution is **not**
  bounded by the tool: a hung lookup lasts as long as the system resolver's own
  timeouts allow (see [Timeouts](#timeouts-and-stalled-streams)).
* **No production state changes.** The tool uses no database, applies no schema
  change and writes nothing anywhere. It only issues the three HTTP requests
  above and prints a report.

## Prerequisites

- Python 3.8+ (standard library only — no third-party packages).
- A running, isolated LocalRouter instance you control.
- A **client key file** containing exactly one raw key (the same kind of file
  you would list under a client's `key_files`; see `docs/NETWORK.md`). The key
  must be printable ASCII with no spaces; anything else is refused as
  `key_file_invalid` before any connection, without echoing it. Pass its
  **path**; never paste the key itself onto the command line, where it would
  land in your shell history and in `ps` output.
- The model id (`--model`) of a route on that instance that you are willing to
  spend provider quota on. For `--protocol responses` the route must front a
  provider that serves the Responses API (or LocalRouter must translate to it).

## Usage

Set placeholders for your environment — replace every `<...>` before running:

```sh
ROUTER_URL='https://<your-router-host>:8787'   # base URL, no /v1 suffix
KEY_FILE='/path/to/<your-client-key>.key'      # file containing one raw key
MODEL='<provider>/<model-id>'                  # a model id this router routes
```

Dry check first — this makes **no** network call and opens no key file:

```sh
python3 scripts/provider-smoke.py --help
```

Then a real run. **This spends money and causes egress:**

```sh
# LIVE: sends real inference requests that spend provider quota/money.
python3 scripts/provider-smoke.py \
  --live \
  --url "$ROUTER_URL" \
  --key-file "$KEY_FILE" \
  --model "$MODEL"
```

Add routing preflight, pretty output and tighter deadlines:

```sh
# LIVE: same real requests, plus a free GET /v1/models route preflight.
python3 scripts/provider-smoke.py \
  --live \
  --url "$ROUTER_URL" \
  --key-file "$KEY_FILE" \
  --model "$MODEL" \
  --check-models --pretty \
  --connect-timeout 5 --read-timeout 30 --timeout 60
```

Exercise the Codex Responses surface instead of chat-completions:

```sh
# LIVE: POST /v1/responses (non-stream + streamed), Codex request shape.
python3 scripts/provider-smoke.py \
  --live \
  --url "$ROUTER_URL" \
  --key-file "$KEY_FILE" \
  --model "$MODEL" \
  --protocol responses
```

Against a loopback instance over plain HTTP (allowed without an override), with
a private CA bundle for an internal HTTPS deployment:

```sh
# LIVE: a loopback router; still spends real provider quota.
python3 scripts/provider-smoke.py --live \
  --url 'http://127.0.0.1:8787' \
  --key-file "$KEY_FILE" \
  --model "$MODEL"

# LIVE: internal HTTPS with a private CA. Non-loopback plain HTTP would need
# --allow-insecure-http, which sends the key in cleartext — avoid it.
python3 scripts/provider-smoke.py --live \
  --url 'https://<your-router-host>:8787' \
  --key-file "$KEY_FILE" --model "$MODEL" \
  --cafile /path/to/<private-ca>.pem
```

Pipe the JSON report into `python3 -m json.tool` for readability, and check the
exit code:

```sh
python3 scripts/provider-smoke.py --live \
  --url "$ROUTER_URL" --key-file "$KEY_FILE" --model "$MODEL" \
  | python3 -m json.tool
echo "exit=$?"
```

## Options

| Option | Meaning |
| --- | --- |
| `--url URL` | LocalRouter base URL (e.g. `https://router.example.com:8787`). No `/v1` suffix; a path prefix is allowed (printable ASCII only — percent-encode anything else). Required with `--live`. |
| `--key-file PATH` | Path to a client key **file**. Never pass a key on the command line. Required with `--live`. |
| `--model MODEL` | A model id routed by this router. Required with `--live`. |
| `--protocol {chat,responses}` | Which API surface to exercise. `chat` (default) uses `/v1/chat/completions`; `responses` uses the Codex Responses `/v1/responses`. An unknown value is refused. |
| `--live` | **Acknowledgement required for any network call.** Requests spend provider quota, money and egress. |
| `--allow-insecure-http` | Permit non-loopback plain HTTP. Sends the key in cleartext; use only deliberately and temporarily. |
| `--cafile PATH` | PEM CA bundle for HTTPS (private CA). Ignored for `http://`. Loaded per check, so a missing or invalid bundle fails each check (`cafile_unreadable` / `tls_config_error`, exit `1`) rather than exiting `2`. |
| `--connect-timeout SEC` | TCP+TLS connect timeout (default `5`); exceeding it is `connect_timeout`. |
| `--read-timeout SEC` | Per-read stall timeout, i.e. the maximum gap between SSE bytes (default `30`). |
| `--timeout SEC` | Overall execution deadline (default `90`). |
| `--check-models` | Also `GET /v1/models` to confirm the model is routed. Free (no inference). |
| `--pretty` | Pretty-print the JSON report. |

## Exit codes

Matching the `localrouter admit` convention (`0` allow / `1` deny / `2` error):

| Code | Meaning |
| --- | --- |
| `0` | Every selected check passed. |
| `1` | A check failed — HTTP error status, malformed JSON/SSE, a missing `[DONE]`/terminal event, no generated text, a non-event-stream response, a timeout, an oversized response, an unfollowed redirect, a model that is not routed, or an unreadable `--cafile` / bad TLS configuration (reported once per check). The report is still printed to stdout with `"ok": false`. |
| `2` | A usage, consent, URL-policy, key-file or argument error raised **before** any request. A sanitized error object is printed to **stderr**. No network call was made. The key file is not opened for usage, consent, URL-policy or argument errors; the key-file errors themselves (`key_file_unreadable`, `key_file_empty`, `key_file_invalid`) are found by reading it. |

On exit `1` a stalled stream and a malformed stream are reported distinctly, so
you can tell "the provider never answered" from "the provider answered badly".

## Report

The tool prints exactly one JSON object to stdout. A passing run against a fake
loopback router looks like this (port and ids are placeholders):

```json
{
  "checks": [
    {
      "bytes_read": 167,
      "detail": null,
      "error": null,
      "latency_ms": 1,
      "model_listed": true,
      "models_count": 2,
      "name": "models",
      "ok": true,
      "status": 200,
      "truncated": false
    },
    {
      "bytes_read": 387,
      "cost": {"source": "usage.cost", "usd": 0.0},
      "detail": null,
      "error": null,
      "generated_chars": 2,
      "latency_ms": 0,
      "name": "chat_nonstream",
      "ok": true,
      "status": 200,
      "tokens": {"cached_input": 1, "input": 5, "output": 2, "reasoning": 0},
      "truncated": false,
      "usage": "present",
      "usage_shape": "prompt_tokens/completion_tokens"
    },
    {
      "bytes_read": 341,
      "cost": {"source": "usage.cost", "usd": 0.0002},
      "detail": null,
      "error": null,
      "generated_chars": 2,
      "latency_ms": 0,
      "name": "chat_stream",
      "ok": true,
      "sse": {"done": true, "events": 5},
      "status": 200,
      "tokens": {"cached_input": 1, "input": 5, "output": 2, "reasoning": 0},
      "truncated": false,
      "usage": "present",
      "usage_shape": "prompt_tokens/completion_tokens"
    }
  ],
  "errors": [],
  "live": true,
  "max_tokens": 64,
  "model": "<model-id>",
  "ok": true,
  "prompt_bytes": 30,
  "protocol": "chat",
  "schema": 1,
  "target": {"host": "127.0.0.1", "loopback": true, "port": 8787, "scheme": "http"},
  "tool": "provider-smoke",
  "warnings": []
}
```

Top-level fields: `tool`, `schema` (report version), `ok`, `live`, `protocol`
(the surface exercised), `model`, `max_tokens`, `prompt_bytes`, `target`
(scheme/host/port/loopback only — never credentials), `checks`, `warnings` and
`errors` (both are lists of `"<check>: <code>"` strings).

Per-check fields: `name`, `ok`, `status`, `latency_ms`, `bytes_read`,
`truncated`, `error` (a stable code or `null`), `detail` (always `null` — no
provider error text is surfaced), and `generated_chars` (the length of the
nonempty generated text a passing check produced), plus
`usage`/`usage_shape`/`tokens`/`cost` for the two generation checks,
`sse: {events, done}` for a chat stream (or `sse: {events, completed}` for a
Responses stream), and `models_count`/`model_listed` for the models check. When
`--protocol responses` is used the two generation checks are named
`responses_nonstream` and `responses_stream`.

`error` codes you may see: `http_status`, `malformed_json`, `malformed_sse`,
`missing_done`, `missing_terminal`, `no_generation`, `stream_error`,
`response_failed`, `response_incomplete`, `not_event_stream`,
`unexpected_redirect`, `response_too_large`, `oversize_event`,
`too_many_events`, `unexpected_shape`, `error_object`, `read_timeout`,
`overall_timeout`, `connect_timeout`, `connect_failed`, `tls_failed`,
`read_failed`, `model_not_routed`, `cafile_unreadable`, `tls_config_error`,
`bad_request_body`.

A non-200 response is always `http_status` with its real `status`, even when
the error body is served as `text/event-stream`: only a 200 body is parsed.

Every per-check `detail` is `null`: no provider-controlled error text is ever
surfaced (see [Safety model](#safety-model)).

For an argument/consent/policy failure the tool prints a small error object to
**stderr** and exits `2`:

```json
{"error": "live_required", "live": false, "ok": false, "schema": 1, "tool": "provider-smoke"}
```

Other `error` codes in that shape: `missing_url`, `missing_model`,
`missing_key_file`, `bad_model`, `bad_protocol`, `bad_timeout`,
`unsupported_scheme`, `invalid_url`, `missing_host`, `bad_port`,
`url_userinfo_not_allowed`, `url_query_not_allowed`,
`nonloopback_plain_http_refused`, `key_file_unreadable`, `key_file_empty`,
`key_file_invalid`.

## Usage and cost provenance

The tool reports **only** what the provider returned:

- `usage` is `"present"` when a usage object was received and `"missing"`
  otherwise.
- `tokens` is populated from either the `prompt_tokens`/`completion_tokens`
  shape or the `input_tokens`/`output_tokens` shape (with cached and reasoning
  detail counts when present), and is `null` when no recognized counts exist.
- `cost` is `{"source": "usage.cost", "usd": <number>}` only when the provider
  reported a finite, non-negative numeric `usage.cost` (an explicit `0` is a
  valid cost). Otherwise `cost` is `null`.

A missing usage block is reported as missing — never as zero tokens and never
as a fabricated cost. A present-but-unusable `usage.cost` (a string, a negative
number, a non-finite value) is ignored and noted in `warnings`. The tool does
**not** compute or estimate cost from a pricing table; it has no pricing model
and does not read the router's.

## Timeouts and stalled streams

Three deadlines apply, so once the host name has resolved a run terminates
inside the overall budget:

- `--connect-timeout` bounds TCP connect and the TLS handshake; exceeding it
  fails the check with `connect_timeout` (or `overall_timeout` once the overall
  budget is spent).
- `--read-timeout` bounds the gap between successive reads, so a stream that
  sends headers and then goes silent fails with `read_timeout` rather than
  hanging.
- `--timeout` is a hard wall-clock deadline for the whole run. Each socket
  timeout (connect, header wait, and every read) is clamped to the time
  remaining in this budget, so a per-connect or per-read stall can never extend
  the run past `--timeout`. On the CPython main thread a `SIGALRM` alarm
  additionally interrupts blocking socket operations — a stalled TLS handshake,
  a stalled header, or a body that trickles a byte within the per-read window —
  and the check fails with `overall_timeout`. When the alarm is unavailable (a
  non-main thread, or a platform without `SIGALRM`) the clamped socket timeouts
  and in-loop deadline checks still bound those operations.

**DNS is not bounded by the tool.** Host name resolution happens inside a
blocking `getaddrinfo` C call. A Python signal handler only runs after that call
returns, and the system resolver retries when interrupted, so neither the socket
timeouts nor the alarm can cut a hung lookup short: it lasts as long as the
resolver's own timeouts and retries (for glibc, `options timeout:`/`attempts:`
in `resolv.conf`). Pass an IP literal in `--url` (with `--cafile` and a matching
certificate for HTTPS) if you need a hard bound. A child-process supervisor or a
resolver thread would close this gap; neither is implemented.

The SSE parser reads in small chunks, so events split across TCP segments (or
across read boundaries) are reassembled correctly. A chunked or slow upstream
is therefore handled; a stalled one is bounded. The response-body cap is applied
with a single extra byte of look-ahead, so a body of exactly the cap is read
whole and not misreported as `response_too_large`.

## Running the tests

The suite is hermetic: it never contacts a real provider and never reads a real
credential. Every request goes to a throwaway loopback server started inside the
test process, and the zero-network / no-key-read guarantees are asserted by
monkeypatching the connection factory and key reader to raise if they are ever
called. The tests also assert that the sentinel key, the synthetic prompt and a
sentinel response body never appear in stdout, stderr or the report, and that no
provider-controlled error `type` (a credential-shaped sentinel) is ever echoed.

```sh
python3 -m py_compile scripts/provider-smoke.py scripts/test_provider_smoke.py
python3 -m unittest discover -s scripts -p 'test_provider_smoke.py' -v
# or:
python3 scripts/test_provider_smoke.py
```

The suite covers: `--help` and bare invocation making no network call; missing
`--live` consent, `--url`, `--model`, `--key-file` and an unknown `--protocol`;
bad timeouts; unreadable, empty, non-UTF-8 and non-printable-ASCII key files
(refused before any connection, without echoing the key); the non-loopback
plain-HTTP refusal and the explicit override; URL-userinfo refusal, a
non-ASCII URL path and an unparseable (malformed IPv6) URL; a full passing non-stream + fragmented
multi-line SSE run (chat and Responses); missing, unusable and negative costs;
missing usage; malformed JSON; malformed SSE; a missing `[DONE]` / missing
Responses terminal event; an HTTP 503 served as an event stream, and a 401
event stream whose malformed, oversized or overlong body still reports
`http_status` with its status; a non-event-stream response; a stalled stream
hitting the read timeout; a TCP connect timeout and a stalled TLS handshake
both reported as `connect_timeout`; a header stall that
must respect the overall budget across two requests; a redirect that is reported
and not followed; an oversized response being capped and a body of exactly the
cap not being capped; a connect failure; the generation-proof cases (empty
`choices`, a bare string, a usage-only object, a `[DONE]`-only chat stream,
whitespace-only chat and Responses streams and an output-less Responses
completion being rejected); provider error objects and
streamed error events being rejected with `detail` left `null`; and the
`--protocol` selection defaulting to `chat`.

## Non-goals

- It does not validate configuration or key files — use `localrouter check`.
- It does not exercise the control API, the ledger database, ingestion, or any
  schema; it touches no production state.
- It does not benchmark throughput, latency distributions or concurrency
  limits, and it does not test background/interactive workload classes beyond
  whatever key you supply.
- It does not manage retries, scheduling or alerting.
