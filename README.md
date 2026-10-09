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
traffic that bypassed LocalRouter; ledger `cost_usd` (default
`cost_basis: metered`) remains a local price estimate per request.

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
the home directory is read-only except `~/.config/localrouter`. To upgrade,
rebuild, re-run `install`, then `systemctl --user restart localrouter`. With
lingering enabled (`loginctl enable-linger`), it runs without a login session.

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
Server config/client keys are loaded at startup; validate and restart after
changing them. Agents also need a restart after config/key changes.

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

## Client usage

```text
Base URL: http://127.0.0.1:8787/v1
API key:  contents of the client's key file
```

Codex accounts serve `/v1/responses` only. Ollama/OpenAI-compatible accounts serve
both `/v1/responses` and `/v1/chat/completions`.

Optional request headers: `X-LocalRouter-Class: background` (downgrade only),
`X-LocalRouter-Session`, `X-LocalRouter-Task`, `X-LocalRouter-Agent`.
