# LocalRouter

OpenAI-compatible LLM gateway, loopback by default and optionally shared between
"trusted" machines over HTTPS. Supports per-client bearer keys, but not
multi-user authentication or tenant isolation. One binary that:

- authenticates local clients with per-client keys,
- selects an upstream subscription account per request,
- protects a configurable quota **reserve** (per account, per 5h/weekly window)
  from background workloads,
- records token usage and estimated cost in SQLite (never prompt/response content),
- serves a status API and a small embedded usage widget.

See [the specification](docs/SPEC.md) for the behavior contract and
[Network deployment and operations](docs/NETWORK.md) for secure central-server
setup, remote verification, key rotation, troubleshooting, and backup/restore.
For the container image under rootless Podman/Quadlet with an NGINX TLS edge,
see [Container deployment](docs/CONTAINERS.md).

## Build

Requires Go 1.26.8 or newer (see `go.mod`). Build/install snippets below target
Linux unless marked otherwise; macOS agent installation and explicit config
paths are covered in [the network guide](docs/NETWORK.md#2-prepare-each-agent-host).

```bash
GOFLAGS=-p=4 go build -o bin/localrouter ./cmd/localrouter
```

## Setup

```bash
mkdir -p ~/.config/localrouter/keys
cp config.example.yaml ~/.config/localrouter/config.yaml   # then edit
bin/localrouter keygen ~/.config/localrouter/keys/me.key
bin/localrouter keygen ~/.config/localrouter/keys/hermes.key
bin/localrouter keygen ~/.config/localrouter/keys/hermes-bg.key
install -m 600 /dev/null ~/.config/localrouter/keys/ollama.key   # paste Ollama API key
bin/localrouter login codex-primary      # device-code login; LocalRouter keeps its OWN tokens
bin/localrouter login codex-secondary
bin/localrouter check
bin/localrouter serve
```

Widget: <http://127.0.0.1:8787/>. JSON: `/control/v1/status`, `/control/v1/usage?since=7d&group=model`.
Liveness: `/healthz`. Local readiness (willing to serve + storage healthy, never
a statement about upstream providers): `/readyz`. Authenticated diagnostics,
including inference concurrency: `/control/v1/diagnostics`.

### OpenRouter

`provider: openrouter` forwards `/v1/chat/completions` and `/v1/responses` to
`<base_url>` (default `https://openrouter.ai/api/v1`) with the account's API key
(`api_key_file` / `api_key_env`); model ids with slashes
(`anthropic/claude-sonnet-4.5`) pass through unchanged. See
[`config.openrouter.example.yaml`](config.openrouter.example.yaml).

Instead of 5h/weekly windows, LocalRouter polls two provider figures:

- **Account balance** — `GET <base_url>/credits`: `total_credits - total_usage`
  in USD, kept **signed** (e.g. `770.8176 - 770.893717902 = -$0.08`). OpenRouter
  documents this endpoint as management-key only; set the optional
  `management_key_file` / `management_key_env`, which is used **only** for
  `/credits` (never for inference or `/key`). Without it the inference key is
  tried; on 403 the balance is shown as *unavailable* (never as $0) and the
  account is reported unhealthy, while key data below still works.
- **Key usage and cap** — `GET <base_url>/key` with the inference key: lifetime,
  daily, weekly and monthly spend, BYOK spend (shown separately), and the key's
  spending cap. `limit: null` is shown as unlimited; a cap of `0` is a real cap.

Both are the provider's authoritative lifetime/period totals and include
traffic that bypassed LocalRouter; ledger `cost_usd` remains a per-request
figure — a local price estimate (`cost_basis: metered`, the default, or
`api_equivalent`), or the exact amount OpenRouter reported in `usage.cost`
(`cost_basis: provider_reported`).

A known balance at or below zero, or a key cap with nothing left, denies every
class even if the data is stale. The cap's predicted reset date is informational
only: it never reopens a spent key on its own — the gate reopens solely when a
fresh `/key` response reports positive `limit_remaining`. An
unknown balance or key part does not deny on its own (upstream `402` is the
backstop). An
OpenRouter `402` fails over to the next account before any byte is sent, cools
the account down (60s or `Retry-After`), and refreshes its balance urgently; a
top-up clears the cooldown early. `reserve` is rejected for these accounts.

Optional `limits.max_concurrent` / `limits.max_concurrent_per_client` cap
active inference requests (0 = unlimited, negatives rejected); a saturated
request gets `429` `concurrency_limit_exceeded` with `Retry-After: 1`. Optional
`timeouts.header|body|idle|shutdown` bound inbound reads, idle keep-alive, and
the graceful drain; 0 selects the default. See [the specification](docs/SPEC.md).

## Run as a service (systemd --user)

```bash
GOFLAGS=-p=4 go build -trimpath -o bin/localrouter ./cmd/localrouter
install -Dm755 bin/localrouter ~/.local/bin/localrouter
install -Dm644 deploy/localrouter.service ~/.config/systemd/user/localrouter.service
systemctl --user daemon-reload && systemctl --user enable --now localrouter
journalctl --user -u localrouter -f
```

The unit validates config before start, restarts on failure, and is sandboxed:
the home directory is replaced by an empty tmpfs that exposes only
`~/.config/localrouter` (writable; `keys/` and `tls/` read-only), `~/.claude` and
`~/.hermes` (read-only) and the binary. Paths configured elsewhere, and hosts
without unprivileged user namespaces, need a drop-in; see
[systemd sandbox](docs/NETWORK.md#systemd-sandbox). To upgrade,
rebuild, re-run `install`, then `systemctl --user restart localrouter`. With
lingering enabled (`loginctl enable-linger`), it runs without a login session.

### Verifying release downloads

Tagged releases publish four native archives, `SHA256SUMS` and a multi-arch
image at `ghcr.io/hpst3r/localrouter`, each with a Sigstore-signed GitHub build
provenance attestation. `SHA256SUMS` lives in the same release as the archives,
so it detects corruption, not tampering; verify the attestation (GitHub CLI 2.49
or newer):

```bash
gh attestation verify localrouter_<version>_linux_amd64.tar.gz --repo hpst3r/localrouter
gh attestation verify oci://ghcr.io/hpst3r/localrouter@sha256:<digest> --repo hpst3r/localrouter
```

Deploy the image by digest, not tag; see [Container deployment](docs/CONTAINERS.md#verifying-releases).

## Reload configuration (SIGHUP)

Send `SIGHUP` to a running server to re-validate the config file and apply the
reloadable subset in place — without dropping in-flight requests, losing
quota/cooldown state, re-opening the database or refreshing credentials:

```bash
kill -HUP "$(systemctl --user show -p MainPID --value localrouter)"
# or, for a foreground process:  pkill -HUP -x localrouter
```

The new configuration is validated in full first and published atomically. On
any error the previous configuration keeps serving (last-good), the generation
does not advance, and the failure is logged with a sanitized reason. Rapid
signals coalesce (a burst causes a bounded number of reloads) and reloads are
serialized. Windows has no SIGHUP; the in-process `App.Reload` seam is the
portable trigger.

**Reloaded without restart:**

| Config key | Effect |
|---|---|
| `clients[]` (`name`, `class`, `key_file`/`key_files`, `host`, `ingest`) | new keys and client attributes |
| `routes[]` (`models`, `upstream_model`, `interactive`, `background`) | new model/route table |
| `accounts[].reserve` | per-account reserves |
| `pricing_file` contents (`pricing.yaml`, `pricing.local.yaml`) | price table |
| `policy.stale_after`, `policy.safety_margin`, `policy.inflight_estimate` | admission knobs |
| `policy.max_failovers` | failover budget |
| `limits.max_concurrent`, `limits.max_concurrent_per_client` | concurrency limits (active counts preserved) |

**Restart-only — a reload that changes any of these is rejected whole** (never
partially applied), with the offending keys named in the log and diagnostics:

- `listen`, `allow_non_loopback`, `allowed_hosts`, `tls_cert_file`, `tls_key_file`
- `data_dir`, the `pricing_file` *path*, token store location
- `quota.poll_interval`
- account topology: `accounts[].id` / `provider` / `base_url` / `quota_source` /
  `api_key_file` / `api_key_env` / `credentials_file` / `cost_basis`
- `claude_logs.*`, `hermes_logs.*`, `host_name`
- `control.require_auth`
- structural timeouts: `timeouts.header`, `timeouts.body`, `timeouts.idle`,
  `timeouts.shutdown`

Reload never changes the listener, TLS material, storage paths or inbound
timeouts, and it adds no HTTP endpoint: the only triggers are `SIGHUP` (Unix)
and the in-process `App.Reload` method. The last reload attempt is exposed at
`GET /control/v1/diagnostics` under `reload` as a sanitized document (a
generation, timestamp, and, on failure, the reason and the restart-only key
names) — never keys, tokens or secret paths. A failed attempt reports the
still-serving generation, so it never lags or advances falsely.

### Rotate a client key without downtime

Each key file holds exactly one raw key, so rotation uses an overlap window
across two files:

```yaml
clients:
  - name: laptop
    class: interactive
    key_files: [keys/laptop-old.key, keys/laptop-new.key]   # both accepted
```

1. Write the new key file and list both under `key_files`. Do not set `key_file`
   and `key_files` on the same client: exactly one is required.
2. `SIGHUP`; both keys now authenticate for **new** requests.
3. Switch the client to the new key; existing requests finish on the old key.
4. Remove the old key from `key_files` and `SIGHUP` again.

A revoked key is rejected for new requests immediately after the reload. The
config file lists only file paths, never key material.

Practical commands:

```bash
# 1. Write keys/laptop-new.key (0600), list both files, validate, then reload:
$EDITOR ~/.config/localrouter/config.yaml           # key_files: [old, new]
bin/localrouter check -config ~/.config/localrouter/config.yaml
kill -HUP "$(systemctl --user show -p MainPID --value localrouter)"
# 2. Point the client at keys/laptop-new.key and confirm it works, then:
$EDITOR ~/.config/localrouter/config.yaml           # drop the old key file
kill -HUP "$(systemctl --user show -p MainPID --value localrouter)"
```

### Rollback and reload diagnostics

A rejected reload rolls back automatically — nothing is published, so the
last-good generation keeps serving and the generation number does not advance.
There is no separate rollback command; correct the config file and `SIGHUP`
again. Inspect the sanitized last attempt:

```bash
# reload:{generation, ok, at, reason?, restart_only?}
# The header is read from a process substitution, so the key never appears
# in curl's argv (printf is a shell builtin).
curl --silent --show-error \
  -H @<(printf 'Authorization: Bearer %s\n' "$(cat ~/.config/localrouter/keys/me.key)") \
  http://127.0.0.1:8787/control/v1/diagnostics | python3 -m json.tool | sed -n '/"reload"/,/}/p'
journalctl --user -u localrouter -n 20 --no-pager | grep -i 'config reload'
```

On failure `ok` is `false` with a sanitized `reason`, plus the offending
`restart_only` key names when the change was restart-only; a failure reports the
still-serving generation (it neither lags nor advances). The diagnostic and the
`config reload rejected` log line never carry keys, tokens, secret paths or the
raw error.

Packaging wires the same contract: the container/systemd unit reloads with a
HUP (`ExecReload`/`podman kill --signal=HUP localrouter`). That belongs to the
packaging change, not to this repository's config; no Compose file is added.

### Pricing across a reload

Pricing is **frozen per request by generation**, not resolved at ledger-write
time. One accepted generation publishes its handler, auth, routes, limits,
reserves, price table and generation number together under a single pointer, so
an inference or HTTP-ingest request is costed from the generation it was
admitted on even if a reload swaps the price table while it is in flight. The
in-process `claude_logs` collector selects the current generation once per
`Record`/`RecordBatch` call, so each batch is costed by the generation live when
that batch is written.

A reload swaps only the live generation: it never re-prices rows already written
and never rewrites the startup table. History is reconciled only by the separate
offline `localrouter pricing reprice` command, which loads the price files from
disk and opens the database itself — it is not run by a reload. There is no
global price set and no live re-price on reload, so a request never mixes one
generation's admission with another's price table.

## Hermes usage import

Hermes Agent sends some traffic directly to providers (e.g. its Anthropic
provider, direct Codex/Ollama). `hermes_logs` imports Hermes's own per-session
token accounting read-only from `~/.hermes/state.db` and
`~/.hermes/profiles/*/state.db` (`session_model_usage`), every minute, as
usage increases only. Rows Hermes already sent through this router are
skipped. Records use route `hermes`, client `hermes` (or `hermes/<profile>`),
and agent `main`, `subagent` or `aux:<task>`.

```yaml
hermes_logs:
  enabled: true
  accounts:            # Hermes billing_provider -> LocalRouter account
    anthropic: claude-max
    openai-codex: codex-primary
    ollama-cloud: ollama-cloud
```

## Multi-host (central router + agents)

One router serves the trusted client machines over HTTPS. Each machine
that runs Claude Code also runs `localrouter agent`, which pushes its Claude
transcript usage and Claude quota to the router. Codex/Ollama logins live only
on the router.

```text
laptop / Mac / vm1 / vm2                         router box (always on)
  Hermes, scripts ── /v1 (own client key) ─────► localrouter serve
  localrouter agent ─ /control/v1/ingest ──────►  ├ Codex + Ollama credentials
    (reads ~/.claude read-only)                    ├ ledger (per-host), widget
                                                   └ reserve gate for every host
```

Router box:

```bash
cp config.server.example.yaml ~/.config/localrouter/config.yaml   # set listen IP, allowed_hosts, and TLS
for k in laptop mac vm1 vm2 laptop-bg mac-bg; do localrouter keygen ~/.config/localrouter/keys/$k.key; done
~/.local/bin/localrouter check -config ~/.config/localrouter/config.yaml
# Install the binary/unit and upstream credentials as described above first.
systemctl --user enable --now localrouter
# If already running, restart it to load the changed config:
systemctl --user restart localrouter
```

Configure `tls_cert_file` and `tls_key_file`, provision the certificate/key,
and establish client certificate trust before starting the network service
([HTTPS setup](docs/NETWORK.md#https-setup)). The server example enables TLS;
replace its certificate paths and example address/hostname before use.

Network mode requires `control.require_auth: true`; the widget asks for a key
once and remembers it in the browser. Requests with a Host header that is
not loopback or in `allowed_hosts` are refused (DNS-rebinding protection).
`allowed_hosts` is **not a network ACL**: restrict reachability using network
access controls and the host firewall, and bind to the intended interface.
**Use HTTPS for networked clients.** HTTP is possible, but should only ever be
considered when the entire connection is protected by an encrypted network
overlay (such as Tailscale or NetBird). This is a
trusted-fleet/"homelab" service, and is absolutely not a public/multi-tenant gateway:
valid keys can read shared control data, and ingest-enabled clients are trusted
reporters. The widget stores its key in browser localStorage.

Each host: first install a binary for that OS/architecture at
`~/.local/bin/localrouter`; it is not distributed by the router. Create
`~/.config/localrouter` and securely copy that host's key from the router to
`~/.config/localrouter/agent.key` (mode 0600). Then:

```bash
cp agent.example.yaml ~/.config/localrouter/agent.yaml           # set server + host
~/.local/bin/localrouter agent -config ~/.config/localrouter/agent.yaml --once   # test
# Linux:
install -Dm644 deploy/localrouter-agent.service ~/.config/systemd/user/localrouter-agent.service
systemctl --user daemon-reload && systemctl --user enable --now localrouter-agent
# macOS: create ~/Library/LaunchAgents and ~/Library/Logs, then follow
# deploy/org.wporter.localrouter-agent.plist (reads the Keychain read-only)
```

Point each host's Hermes at `https://router.example.com:8787/v1` with its interactive key,
and delegation/cron at its `-bg` key. Agents buffer nothing in memory beyond a
scan: if the router is down, they do not advance their transcript offsets and
re-send later (records are deduplicated by ID).

Before relying on the deployment, run the [remote acceptance checks](docs/NETWORK.md#3-verify-from-a-remote-client).
A successful `check`, `/healthz` (liveness) or `/readyz` (local readiness) does
not verify upstream inference; only a real model request does.
Server config/client keys are loaded at startup and the reloadable subset can be
re-read in place with `SIGHUP` (see [Reload configuration](#reload-configuration-sighup));
validate with `localrouter check` first. Agents still need a restart after
config/key changes.

For authenticated quota gating from a client:

```bash
~/.local/bin/localrouter admit --url https://router.example.com:8787 \
  --key-file ~/.config/localrouter/agent.key \
  --class background --account claude-max --json
```

Exit codes: 0 allow, 1 deny, 2 error. This is a dry run, not an inference call
or a reservation of quota.

## Pricing

Costs are only computed for models present in `pricing.yaml` (USD per 1M tokens).
LocalRouter ships no prices. To import LiteLLM's public table:

```bash
mkdir -p ~/.config/localrouter
curl -fsSLo ~/.config/localrouter/litellm-prices.json \
  https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
bin/localrouter pricing import ~/.config/localrouter/litellm-prices.json
```

Hand-maintained overrides and aliases live in `pricing.local.yaml` next to
`pricing.yaml`; import never overwrites it. Use `aliases:` to map the model
name clients send (e.g. Ollama's `glm-5.3`) to a priced LiteLLM key
(`zai/glm-5.3`). After importing or editing prices, apply them to history:

```bash
bin/localrouter pricing reprice
```

Subscription accounts record `api_equivalent` cost (what it would cost at API list
price), not actual spend. Unpriced models show as unpriced, never $0.

When an OpenRouter response carries a usable request cost in `usage.cost`,
that provider-reported amount is recorded as `cost_usd` with
`cost_basis: provider_reported`, taking precedence over the local price table
(including a genuine $0, e.g. `:free` models) and independent of whether token
usage was parseable. Missing or unusable costs fall back to the existing local
price estimate; if no price is available, they remain unpriced. `pricing reprice`
never recomputes or clears provider-reported costs; rows recorded before this
feature are historical and are not retroactively recovered. `cost_details` from
the provider is ignored — only `usage.cost` is used, to avoid double counting.
For an interrupted stream, an already observed cost is retained alongside the
request error; it is a provider-reported observation, not a guarantee of the
final billed amount. Account-wide spend from `/credits` remains separate.

## Client usage

```text
Base URL: http://127.0.0.1:8787/v1
API key:  contents of the client's key file
```

Codex accounts serve `/v1/responses` only. Ollama/OpenAI-compatible accounts serve
both `/v1/responses` and `/v1/chat/completions`.

Optional request headers: `X-LocalRouter-Class: background` (downgrade only),
`X-LocalRouter-Session`, `X-LocalRouter-Task`, `X-LocalRouter-Agent`.
