# LocalRouter

Loopback OpenAI-compatible LLM gateway. One binary that:

- authenticates local clients with per-client keys,
- selects an upstream subscription account per request,
- protects a configurable quota **reserve** (per account, per 5h/weekly window)
  from background workloads,
- records token usage and estimated cost in SQLite (never prompt/response content),
- serves a status API and a small embedded usage widget.

See `docs/SPEC.md` for the authoritative behavior.

## Build

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

One router serves every machine on the mesh (Tailscale/NetBird). Each machine
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
cp config.server.example.yaml ~/.config/localrouter/config.yaml   # set listen IP + allowed_hosts
for k in laptop mac vm1 vm2 laptop-bg mac-bg; do localrouter keygen ~/.config/localrouter/keys/$k.key; done
localrouter check && systemctl --user enable --now localrouter
```

Network mode requires `control.require_auth: true`; the widget asks for a key
once and remembers it in the browser. Requests with a Host header that is
not loopback or in `allowed_hosts` are refused (DNS-rebinding protection).

Each host (copy that host's key from the router to `~/.config/localrouter/agent.key`, mode 0600):

```bash
cp agent.example.yaml ~/.config/localrouter/agent.yaml           # set server + host
localrouter agent -config ~/.config/localrouter/agent.yaml --once   # test
# Linux:
install -Dm644 deploy/localrouter-agent.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now localrouter-agent
# macOS: see deploy/org.wporter.localrouter-agent.plist (reads the Keychain read-only)
```

Point each host's Hermes at `http://<router>:8787/v1` with its interactive key,
and delegation/cron at its `-bg` key. Agents buffer nothing in memory beyond a
scan: if the router is down, they do not advance their transcript offsets and
re-send later (records are deduplicated by ID).

## Pricing

Costs are only computed for models present in `pricing.yaml` (USD per 1M tokens).
LocalRouter ships no prices. To import LiteLLM's public table:

```bash
curl -fsSLo /tmp/prices.json \
  https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
bin/localrouter pricing import /tmp/prices.json
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
