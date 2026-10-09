# Network deployment and operations

For networked operation, LocalRouter currently supports ONLY a central server shared by **trusted machines**. The recommended deployment binds to a specific Tailscale/NetBird mesh IP, with mesh ACLs restricting who can connect. This is not a public or multi-tenant gateway. High-availability has NOT been tested and was not a design goal for this MVP.

See [README](../README.md) for the build and basic setup, [server example](../config.server.example.yaml), [agent example](../agent.example.yaml), and [SPEC](SPEC.md) for the implementation contract.

## Security boundary

- Inference always requires a client bearer key. Network mode also requires `control.require_auth: true`.
- Any valid client key can read shared status and usage data. There are no per-client visibility or per-account inference permissions beyond workload class and configured routes.
- `ingest: true` is a trusted reporting permission, not host/account isolation. That client can report usage for any configured Claude account and snapshots for any Claude account with `quota_source: agent`. The request's `host` is a client-supplied label; the server attributes records to the authenticated client name.
- `allowed_hosts` checks the HTTP Host header to protect browsers against DNS rebinding. **It is not a source-IP ACL.** Enforce access with the mesh ACL/firewall.
- Plain HTTP is appropriate only when the entire path is inside the encrypted mesh or another protected tunnel (though you should use HTTPS regardless). On an ordinary LAN, use HTTPS. Do not publish this listener to the Internet.
- The widget stores its bearer key in browser localStorage. Use a trusted browser profile; clear the saved key/site storage on shared machines. The key retains its normal inference/ingestion permissions.
- Body-size limits and upstream timeouts exist, but there is no hard per-client rate/concurrency cap or explicit inbound body-read/idle timeout. Trusted clients and restricted reachability remain important.
- Claude inference remains on the official Claude CLI. Agents read Claude credentials locally for quota polling; they send usage metadata and snapshots, not Claude tokens or transcript content, to the router.

## 1. Prepare the router

Run commands from the repository root. Install a binary built for the target machine:

```bash
GOMAXPROCS=4 GOFLAGS=-p=4 go build -trimpath -o bin/localrouter ./cmd/localrouter
install -Dm755 bin/localrouter ~/.local/bin/localrouter
mkdir -p ~/.config/localrouter/keys
chmod 700 ~/.config/localrouter ~/.config/localrouter/keys
cp config.server.example.yaml ~/.config/localrouter/config.yaml
chmod 600 ~/.config/localrouter/config.yaml
```

Do not overwrite an existing working config without saving it first. Edit the copied config:

```yaml
listen: 100.64.0.10:8787
allow_non_loopback: true
allowed_hosts: [router, router.tail.example.ts.net, 100.64.0.10]
control:
  require_auth: true
```

Replace the example IP/names with the router's actual mesh address and names clients will use. Bind to the mesh IP rather than `0.0.0.0`. Permit TCP 8787 only from the intended devices through your mesh ACLs and any applicable host firewall; do not add a broad public-interface firewall exception or port-forward.

Generate a distinct key for every configured client:

```bash
for k in laptop mac vm1 vm2 laptop-bg mac-bg; do
  ~/.local/bin/localrouter keygen ~/.config/localrouter/keys/$k.key
done
```

`keygen` creates mode-0600 files and refuses to overwrite existing files. Adjust the loop and `clients` list together. Give worker/cron clients `class: background`; enable `ingest` only for keys used by agents.

Set up the upstream credentials and routes you actually use. The example includes Codex and Ollama; remove unused accounts/routes rather than leaving placeholder credentials. Create the Ollama key file securely with mode 0600 and enter its API key without putting it in shell history. For Codex, perform separate LocalRouter logins:

```bash
~/.local/bin/localrouter login -config ~/.config/localrouter/config.yaml codex-primary
~/.local/bin/localrouter login -config ~/.config/localrouter/config.yaml codex-secondary
~/.local/bin/localrouter check -config ~/.config/localrouter/config.yaml
```

`check` validates the configuration and client key files. It does **not** prove upstream connectivity, quota availability, TLS certificate validity, or remote reachability.

Install the Linux user service:

```bash
install -Dm644 deploy/localrouter.service ~/.config/systemd/user/localrouter.service
systemctl --user daemon-reload
systemctl --user enable --now localrouter
journalctl --user -u localrouter -n 50 --no-pager
ss -ltn 'sport = :8787'
```

If already running, use `systemctl --user restart localrouter` after changing the config. `enable --now` alone does not reload a running process. For unattended startup without a login session, arrange user lingering (`loginctl enable-linger`; authorization may be required). Make sure the mesh interface is available; startup failures are visible in the journal.

### HTTPS outside the mesh

Provision a certificate whose subject alternative names cover the hostname/IP clients use. Put it and its private key somewhere the service user can read, preferably under the configuration directory; protect the private key with mode 0600. Configure both:

```yaml
tls_cert_file: tls/cert.pem
tls_key_file: tls/key.pem
```

Relative paths resolve against the config directory. Restart the server, use `https://` in every client/agent URL, and trust the issuing CA on each machine. Browser, curl, Go agent, and LLM-client trust must all work. For a private CA, install it in the appropriate trust stores (curl can use `--cacert /path/to/ca.pem`). There is no agent-specific CA-file setting. Do not disable certificate verification or use `curl -k` as the production solution.

## 2. Prepare each agent host

Install `localrouter` into `~/.local/bin/localrouter` on **each** host; the server binary is not automatically distributed. Build locally using the bounded command above, or securely transfer a binary built for that OS/architecture. On macOS, create `~/.local/bin` and use `install -m 755 bin/localrouter ~/.local/bin/localrouter` (the Linux `install -D` flag is not portable).

Create `~/.config/localrouter`, copy `agent.example.yaml` there as `agent.yaml`, and securely transfer that host's router-generated key to `~/.config/localrouter/agent.key`. Apply restrictive permissions:

```bash
chmod 700 ~/.config/localrouter
chmod 600 ~/.config/localrouter/agent.yaml ~/.config/localrouter/agent.key
```

Edit the server URL, host label, and Claude account ID. Use the same configured Claude account ID for machines sharing that subscription; do not map a different subscription to it. The default credential source reads a file on Linux and tries the login Keychain then file on macOS.

Test before installing the service:

```bash
~/.local/bin/localrouter agent -config ~/.config/localrouter/agent.yaml --once
```

This performs a transcript scan and, if enabled, a quota push. A successful exit alone does not prove a fresh snapshot: expired Claude credentials are skipped without making `--once` fail, and an idle host may have no usage records. Check the server status/usage below. Set `quota_interval: 0s` on a host that should push transcripts but not quota; duration values use Go duration syntax.

Linux:

```bash
install -Dm644 deploy/localrouter-agent.service ~/.config/systemd/user/localrouter-agent.service
systemctl --user daemon-reload
systemctl --user enable --now localrouter-agent
journalctl --user -u localrouter-agent -n 50 --no-pager
```

For macOS, follow the install instructions in [the LaunchAgent plist](../deploy/org.wporter.localrouter-agent.plist): create `~/Library/LaunchAgents` and `~/Library/Logs` first, substitute `__HOME__`, and bootstrap it in the GUI session so the login Keychain is available. Logs go to `~/Library/Logs/localrouter-agent.log`. Keep the config path explicit as in the supplied plist; platform-default config-directory resolution differs from Linux.

Agents retry when the server is unavailable. Successful batches are deduplicated on retry; failed batches do not advance transcript offsets past unsent data. Keep transcript files and agent state until recovery—there is no independent durable queue containing the original transcripts.

## 3. Verify from a remote client

The following shell examples assume curl and Python 3, a mesh URL, and the client's key at the default agent path. Change the URL/path as appropriate. The authenticated curl commands read the key through stdin config rather than exposing it in curl's command-line arguments. Do not use shell tracing (`set -x`) around credential handling.

```bash
ROUTER_URL=http://100.64.0.10:8787
KEY_FILE="$HOME/.config/localrouter/agent.key"

# 200 + "ok": HTTP reachability only, no authentication/provider test.
curl --fail-with-body --silent --show-error "$ROUTER_URL/healthz"

# 401: shared control data is not accessible without a key.
curl --silent --show-error --output /dev/null --write-out '%{http_code}\n' \
  "$ROUTER_URL/control/v1/status"

# 403: the Host-header guard is active.
curl --silent --show-error --output /dev/null --write-out '%{http_code}\n' \
  -H 'Host: unlisted.invalid' "$ROUTER_URL/healthz"

# Authenticated status: accounts, freshness, registered clients.
python3 -c 'import pathlib,sys; k=pathlib.Path(sys.argv[1]).read_text().strip(); print("header = \"Authorization: Bearer "+k+"\"")' "$KEY_FILE" |
  curl --config - --fail-with-body --silent --show-error \
  "$ROUTER_URL/control/v1/status"

# Usage attributed to agent hosts.
python3 -c 'import pathlib,sys; k=pathlib.Path(sys.argv[1]).read_text().strip(); print("header = \"Authorization: Bearer "+k+"\"")' "$KEY_FILE" |
  curl --config - --fail-with-body --silent --show-error \
  "$ROUTER_URL/control/v1/usage?since=7d&group=host"

# Dry-run reserve gate; does not call inference or reserve a lease.
~/.local/bin/localrouter admit --url "$ROUTER_URL" --key-file "$KEY_FILE" \
  --class background --account claude-max --json
```

The key-to-curl helper assumes an unmodified `localrouter keygen` key. `admit` exits 0 for allow, 1 for deny, and 2 for an error. Open the widget at the same base URL and enter a valid key. For inference, use `<base URL>/v1` and the appropriate interactive/background client key. A real model request is a separate, quota-consuming acceptance test; `/healthz`, `/v1/models`, and `admit` do not prove upstream inference works.

## Troubleshooting

| Symptom | Check |
|---|---|
| Connection refused or timeout | Service journal, mesh connection/ACL, bind address, port, host firewall. |
| TLS validation error | URL name versus certificate SAN, chain/expiry, client CA trust. |
| `401` | Missing/invalid bearer key; correct client key file; restart the server after replacing keys. |
| `403` on `/healthz` or status | Host header not accepted. Add the actual URL's hostname/IP to `allowed_hosts`, validate, restart. |
| `403` on ingest with an accepted Host | That key lacks `ingest: true`. Enable only for the intended agent and restart. |
| `400` on ingest | Agent host/schema/account mismatch, invalid records, or clock skew. Inspect the sanitized agent/server errors; configured account IDs must agree. |
| Stale Claude quota | Agent running? `quota_interval` enabled? Correct account with `quota_source: agent`? Claude login/token current? Clocks synchronized? Run the official Claude CLI to refresh its own credentials. |
| Missing usage | Correct projects directory, agent key/account, journal errors, successful batches and intact source transcripts. Allow a scan interval/finalization delay for recent messages. |
| Background denied | Reserve/cooldown/exhausted window or stale reserved account; inspect status reason. Interactive may still be admissible. |
| Writes fail only under systemd | Custom data/state paths are outside the supplied sandbox's `ReadWritePaths`. |

A fresh quota snapshot and recent host usage are separate signals: usage can push while quota polling fails, and quota can refresh on a host with no new transcript usage.

## Changes, keys, and upgrades

- Server configuration, client keys, and in-memory pricing are loaded at startup. Validate and restart after changes. Restart agents after their key/config changes. `daemon-reload` is needed only when service units change.
- To rotate a client key, generate a new file at a new path, securely distribute it, update the server client's `key_file` and client/agent configuration, then restart server and affected agents. A coordinated switch has a brief interruption; the server does not accept two keys for one client entry. Use a temporary separate client entry only if that extra identity/permissions are intentional.
- To revoke a key, remove its client entry (or replace its key), validate, and restart the server. Removing the file alone does not evict the already loaded key. Existing in-flight requests are not continuously re-authenticated.
- Rebuild/install the target binary and restart its service for upgrades. The server drains requests for up to 30 seconds on shutdown, then cuts remaining streams; schedule restarts accordingly.
- The supplied Linux server unit permits writes only under `~/.config/localrouter`; the agent unit permits its default state directory. If moving `data_dir`, pricing files, or `state_dir`, create the destination with restrictive permissions and update the corresponding unit's `ReadWritePaths`. Read access to credentials/transcripts must also remain possible.

## Backup and restore

Backups include credentials: encrypt them, restrict access, and keep them out of Git. The default server directory contains configuration, client/upstream keys, Codex token files, pricing, the SQLite ledger, and collector state. Include any configured files outside it as well. Agent state is separate; source Claude transcripts remain necessary for replay.

For a simple consistent offline backup of the default server directory:

```bash
(
  set -eu
  # Choose a durable, access-controlled location; never use /tmp for the archive.
  umask 077
  mkdir -p "$HOME/.local/state/localrouter-backups"
  systemctl --user stop localrouter
  # Restart on either successful backup or an archiving failure.
  trap 'systemctl --user start localrouter' EXIT
  if systemctl --user is-active --quiet localrouter; then
    printf '%s\n' 'Refusing to copy a running ledger' >&2
    exit 1
  fi
  # Do not run pricing/relabel commands while this backup is in progress.
  tar -C "$HOME/.config" -czf \
    "$HOME/.local/state/localrouter-backups/localrouter-$(date +%Y%m%d-%H%M%S).tar.gz" localrouter
)
```

The archive is **not encrypted by tar**; encrypt it before off-host storage. Check tar's exit status before considering the backup complete, and restart the service even if archiving fails. Do not copy just `localrouter.db` while the service is running: SQLite WAL files can contain committed data not yet in the main file.

Restore with the service stopped and after saving the existing directory separately. Restore the whole snapshot (including any SQLite sidecars present), ownership, and restrictive permissions into the intended paths; run `localrouter check`, start the service, and verify status/usage. Restored credentials may be expired or superseded by later OAuth refreshes: stop the old router instance and perform a fresh LocalRouter Codex login if needed. Do not run two servers from copied rotating credentials.

For an agent, stop it before backing up/restoring its `state_dir`; include its config/key securely and retain the original Claude transcripts. Validate recovery by observing host usage after a restart without unexpected duplicate counts.
