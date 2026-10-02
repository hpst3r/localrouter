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
