# Container deployment (rootless Podman + Quadlet, NGINX TLS edge)

This document covers running LocalRouter from the container image under
**rootless Podman**, managed by **Quadlet** user units, with **NGINX** as the
TLS-terminating edge. It is an operations guide for packaging that already
exists in this tree: `Containerfile`, `packaging/container/config.example.yaml`,
`packaging/quadlet/*.container` / `.network`, `packaging/nginx/localrouter.conf`,
and `scripts/release.sh`.

It does **not** change the runtime behavior contract in
[SPEC.md](SPEC.md) or the networked-operation guidance in
[NETWORK.md](NETWORK.md); where they overlap, those documents remain
authoritative for the application itself.

## Deployment shape

Three rootless Podman objects on one dedicated bridge:

| Object | Role | Host port |
|---|---|---|
| `localrouter` (router) | HTTP gateway, private netns, `0.0.0.0:8787` | **none** |
| `localrouter-nginx` | TLS terminator, the **only** host publisher | `127.0.0.1:8443` (set your interface IP) |
| `localrouter` network | normal (non-internal) bridge, separate netns | — |

The router image is **HTTP only**. It never terminates TLS: the shipped
`packaging/container/config.example.yaml` deliberately has **no**
`tls_cert_file` / `tls_key_file` entries and the image has no `EXPOSE` and
publishes no port. Terminate TLS in front (NGINX) and publish only there. In
the Quadlet layout, TLS is **NGINX-only**: the router container never receives
certificate or key mounts.

## Container filesystem layout

Every path below is a bind mount supplied by the operator. The image declares no
`VOLUME`, so a read-only root filesystem stays usable.

| Container path | Mode | Contents |
|---|---|---|
| `/etc/localrouter/config.yaml` | **ro** | config (the default `-config` path) |
| `/etc/localrouter/pricing.yaml` | **ro** | imported price table (may be absent) |
| `/run/secrets/*` | **ro** | client bearer keys, static provider API keys |
| `/var/lib/localrouter` | **rw** | **the only writable path**: SQLite ledger, rotated OAuth tokens, collector state |
| `/tmp` | tmpfs | run with `--tmpfs /tmp`; `ReadOnlyTmpfs=true` in Quadlet |

Two consequences an operator must internalize:

- **OAuth token state is writable state, not a read-only secret.** Codex OAuth
  tokens rotate at runtime, so they live under the writable `data_dir`
  (`/var/lib/localrouter/tokens`) and are **never** placed in a read-only
  `/run/secrets` mount. Provider API keys, by contrast, are static and belong in
  `/run/secrets`.
- **`data_dir` is explicit.** The example pins `data_dir: /var/lib/localrouter`.
  Do not rely on a `$HOME`-relative default — an unset `HOME` would fall back to
  the read-only working directory. The image sets `HOME=/var/lib/localrouter`
  precisely so any `$HOME`-relative default still resolves somewhere writable.

## Rootless identity (explicit non-root, both units)

Both Quadlet units run as an **explicit non-root** container UID/GID via:

```
UserNS=keep-id:uid=1000,gid=1000
User=1000
Group=1000
```

`keep-id:uid=1000,gid=1000` maps the **invoking (host) user** to container
UID/GID 1000. The host user does **not** have to be UID 1000: whatever UID you
invoke rootless Podman as becomes UID 1000 inside the container, so a single
operator account owns the read-only mounts (config, secrets, TLS) and the
writable state directory without root. `User=`/`Group=` then select that same
UID/GID for the image process rather than inheriting userns root. The image also
fixes an explicit `USER 1000:1000` (`adduser -u 1000`) so the identity is
explicit even outside Quadlet.

Hardening common to both units: `ReadOnly=true`, `DropCapability=ALL`,
`NoNewPrivileges=true`. NGINX additionally needs a writable pid dir and cache,
supplied as tmpfs (`Tmpfs=/tmp`, `Tmpfs=/var/cache/nginx`) only.

## Install: operator directories

Create the host-side layout under the data root (mode `700` on the directories
that hold key and token material):

```bash
mkdir -p ~/.local/share/localrouter/{etc,secrets,state,nginx,tls}
chmod 700 ~/.local/share/localrouter/secrets ~/.local/share/localrouter/state
chmod 700 ~/.local/share/localrouter/tls
```

The Quadlet units bind these as:

- `%h/.local/share/localrouter/etc:/etc/localrouter:ro,Z`
- `%h/.local/share/localrouter/secrets:/run/secrets:ro,Z`
- `%h/.local/share/localrouter/state:/var/lib/localrouter:rw,Z`
- `%h/.local/share/localrouter/nginx/localrouter.conf:/etc/nginx/conf.d/default.conf:ro,Z`
- `%h/.local/share/localrouter/tls:/etc/nginx/tls:ro,Z`

## Configuration and secrets

Copy the example **into `etc/`** and use **absolute paths** inside it:

```bash
cp packaging/container/config.example.yaml ~/.local/share/localrouter/etc/config.yaml
```

Then edit, at minimum: `allowed_hosts` (the public hostname NGINX forwards — this
checks the `Host` header and is **not a source-IP ACL**), the `accounts:`
(provider/account ids), `routes:` (models and account selection), `clients:`,
and keep `data_dir: /var/lib/localrouter`, `listen: 0.0.0.0:8787`,
`allow_non_loopback: true`, and `control.require_auth: true`. Supply the pricing
table at `/etc/localrouter/pricing.yaml` (imported separately — the repo ships no
prices; see the Pricing section of [README](../README.md)).

Client bearer keys — generate with the binary and place them under `secrets/`.
`keygen` writes exactly one raw key to a mode-**0600** file, refuses to overwrite
an existing file, and prints **no key material** (only `wrote new client key to
<path> (mode 0600)`), so it is safe to run from a script:

```bash
mkdir -p ~/.local/share/localrouter/secrets
bin/localrouter keygen ~/.local/share/localrouter/secrets/client-router.key
bin/localrouter keygen ~/.local/share/localrouter/secrets/client-worker.key
```

Provider API keys are static: create the files read-only under `secrets/` and
reference them by `api_key_file: /run/secrets/<name>.key`. Enter the key without
putting it in shell history, e.g.:

```bash
install -m 600 /dev/null ~/.local/share/localrouter/secrets/ollama.key
$EDITOR ~/.local/share/localrouter/secrets/ollama.key   # paste the provider key
```

**Never commit or paste real keys into examples.** The config example uses
placeholders (`/run/secrets/client-router.key`, `/run/secrets/ollama.key`) and
never embeds key material.

## TLS edge: operator-provided certs and renewal

NGINX reads `/etc/nginx/tls/tls.crt` and `/etc/nginx/tls/tls.key`, mounted
read-only from `~/.local/share/localrouter/tls`. **You provision both files.**
The router container never receives them.

Certificate renewal is operator-managed. After replacing the files, reload
NGINX so it re-reads them (the unit defines `ReloadCmd=/usr/sbin/nginx -s reload`):

```bash
systemctl --user reload localrouter-nginx
```

## Image and version tag

Podman uses `.containerignore` for build-context exclusions; Docker/Buildx uses
`.dockerignore`. Each file independently excludes credentials (keys, tokens,
certs, credential JSON, live `config.yaml`/`agent.yaml`/pricing files), local
databases, test-run dirs and build outputs. Podman gives `.containerignore`
precedence when both exist. Only the build stage sees the context, but build
caches and a `--target build` image keep it, so do not build from a tree that
holds live secrets anyway.

Base images (`golang`, `alpine`, `nginx-unprivileged`) are pinned as
`tag@sha256:<index digest>`; the tag is informational. Dependabot proposes
Containerfile digest bumps; the NGINX `Image=` in the Quadlet unit is updated
by hand (`skopeo inspect --raw docker://<image>:<tag> | sha256sum`). The
runtime `apk add ca-certificates` is not version-pinned, so the image is not
bit-for-bit reproducible across rebuilds.

The Quadlet unit references `ghcr.io/hpst3r/localrouter:VERSION`. **`VERSION` is a
literal placeholder** — replace it with a real published tag before installing,
or better, with the digest (`Image=ghcr.io/hpst3r/localrouter@sha256:<digest>`)
after verifying it as below. A tag can be re-pushed; a digest cannot.
There is currently **no published release**, so no tag exists yet to point at;
do not install the unit until a tag is available (see Verification status below).

### Verifying releases

The release workflow attaches a Sigstore-signed GitHub build provenance
attestation to every archive, to `SHA256SUMS`, and to the pushed image index
digest (stored in GHCR next to the image). The image digest is printed in the
release run's summary; it can also be read from the registry:

```bash
skopeo inspect --raw docker://ghcr.io/hpst3r/localrouter:<version> | sha256sum
```

Verify before installing (GitHub CLI 2.49 or newer):

```bash
gh attestation verify oci://ghcr.io/hpst3r/localrouter@sha256:<digest> --repo hpst3r/localrouter
gh attestation verify localrouter_<version>_linux_amd64.tar.gz --repo hpst3r/localrouter
sha256sum -c SHA256SUMS --ignore-missing
```

`SHA256SUMS` alone only detects corruption: it is published next to the
archives and can be replaced with them. Then pin the verified digest in
`localrouter.container`.

### Release workflow controls

- Every action in `.github/workflows/` is pinned to a full commit SHA (with a
  `# vX.Y.Z` comment), and the QEMU binfmt and buildkit helper images are pinned
  by digest. CI fails if a pin is missing (`.github/scripts/check-pins.sh`).
  Dependabot updates action SHAs weekly; bump the helper image digests in
  `release.yml` by hand with `skopeo inspect --raw ... | sha256sum`.
- The release build does not restore the Actions Go cache and runs
  `go mod verify` before testing and packaging.
- `publish-image` and `publish-release` run in the `release` GitHub
  environment. **The repository owner must configure it** (Settings →
  Environments → `release`): add required reviewers, and restrict deployment
  to protected tags matching `v*` (with a tag protection/ruleset rule on `v*`).
  Without that configuration the environment exists but enforces nothing; this
  repository's settings have not been verified.
- Only those two jobs hold write scopes (`packages: write` or `contents: write`,
  plus `id-token: write` and `attestations: write` for attestations).

## Quadlet units and the systemd unit collision

Copy the units and reload:

```bash
mkdir -p ~/.config/containers/systemd
cp packaging/quadlet/localrouter.network       ~/.config/containers/systemd/
cp packaging/quadlet/localrouter.container     ~/.config/containers/systemd/
cp packaging/quadlet/localrouter-nginx.container ~/.config/containers/systemd/
mkdir -p ~/.local/share/localrouter/nginx
cp packaging/nginx/localrouter.conf ~/.local/share/localrouter/nginx/localrouter.conf
systemctl --user daemon-reload
```

**Warning — name collision.** The generated Quadlet service is named
`localrouter.service`, the **same name** as the native `deploy/localrouter.service`
user unit. Running both would fight over the same unit name. **Choose ONE:**
either the native binary unit **or** the container/Quadlet unit. If the native
`localrouter.service` is installed and running, stop it **before** installing
the Quadlet unit:

```bash
systemctl --user stop localrouter.service     # only if the native unit is present
```

Stopping alone does not remove the collision: disable the native unit and move
its installed `~/.config/systemd/user/localrouter.service` out of the unit search
path before reloading systemd. Keep a backup for rollback. Confirm
`systemctl --user cat localrouter.service` shows the generated Quadlet unit before
starting it; an installed native unit can otherwise shadow the generator output.

There is **no automatic migration**: nothing moves your secrets or state between
the native layout (`~/.config/localrouter/...`) and the container layout
(`~/.local/share/localrouter/...`). Copying them across is a deliberate operator
step; do it while the service is stopped.

**Generated units cannot be `enable`d.** Quadlet-generated `.service` units have
no `[Install]` section and are started by systemd from the unit dependency graph,
not by `systemctl --user enable`. These units declare `WantedBy=default.target`,
so `systemctl --user start <unit>` (or `daemon-reload` + the target) brings them
up. Lingering (`loginctl enable-linger $USER`) so they run without a login
session is an **explicit administrator choice**, not required and not enabled by
this packaging.

## Ports and networking

- **Rootless HTTPS defaults to 8443.** Rootless Podman cannot bind privileged
  ports (unprivileged port start is 1024), so the edge publishes port 8443.
  The router publishes **no** host port. Nothing in this packaging changes
  `net.ipv4.ip_unprivileged_port_start` or any other sysctl to reach 443; 443 is
  simply not used.
- **Bind the edge to one interface.** The shipped unit has
  `PublishPort=127.0.0.1:8443:8443`, reachable from the host only. **Replace
  `127.0.0.1` with the LAN or overlay (Tailscale/WireGuard) interface IP**
  clients use, e.g. `PublishPort=192.0.2.10:8443:8443`. Never use a bare
  `8443:8443`: that binds every interface (`0.0.0.0` and `[::]`), including
  public or Wi-Fi ones. The router's own `listen` cannot help here; it binds
  `0.0.0.0` inside its private netns by design, so `PublishPort` is the only
  bind control.
- **A host firewall is still required.** Allow 8443 only from intended
  clients. Rootless port forwarding hides the client source IP (NGINX and the
  router see the forwarder's address), so neither can apply IP-based
  restrictions; filtering must happen on the host.
- **Egress.** The dedicated bridge is a **normal (non-internal)** bridge
  (`Internal=false`) precisely because the router must reach external inference
  providers over HTTPS. Do not set `Internal=true` here.
- **Upstream TLS trust** uses the runtime CA bundle (Alpine `ca-certificates`),
  so the router validates provider TLS against the standard cloud/provider CAs.
- The router keeps `Authorization` unchanged; NGINX preserves the client `Host`
  verbatim (`proxy_set_header Host $http_host`) so the router's host guard can
  match `allowed_hosts`.

## Health endpoints: public `/readyz` vs. the 8081 loopback probe

These are **not the same thing** and must not be conflated:

- **Public `/readyz` (through NGINX, port 8443)** now **proxies the router** and
  returns the router's JSON health document — i.e. real local readiness (serving
  + storage healthy), never a statement about upstream providers. The proxy is
  bounded: `proxy_connect_timeout 2s` and `proxy_read_timeout 5s`.
- **Loopback `127.0.0.1:8081/readyz`** is bound inside the NGINX container's own
  netns (never published, unreachable from the bridge or host) and returns a
  fixed `200 ok`. It is **NGINX-only liveness**, used solely as the NGINX
  container's `HealthCmd`. It is **not backend readiness** — it says nothing
  about the router.

The router's own `HealthCmd` probes `http://127.0.0.1:8787/readyz` directly, i.e.
the router's real readiness document (`{"ready":true}`, storage-probe bounded at
2s), never a proxied upstream call and never a liveness-only claim.

## Long-lived SSE

The edge is configured for streaming: `proxy_buffering off`,
`proxy_request_buffering off`, `proxy_read_timeout`/`proxy_send_timeout` at
3600s, `proxy_cache off`, `proxy_http_version 1.1` with an empty `Connection`
header for upstream keepalive. The router's upstream stream-silence protection
still applies underneath — the edge does not lift it. `timeouts.idle` instead
limits idle HTTP keep-alive connections between requests, not active SSE streams.

## NGINX DNS resolution and backend restarts

The `upstream` block resolves `localrouter:8787` **statically at NGINX start**. If
the router container is **recreated** (new container, and especially a new IP),
NGINX keeps pointing at the address it resolved at startup. Recreating the
backend may therefore require an NGINX reload or restart to pick up the new
address:

```bash
systemctl --user reload localrouter-nginx    # or restart
```

There is no auto-update, DNS watcher, or dynamic resolver in this config.

## Verification status

State this honestly — do not overclaim:

- **Local end-to-end acceptance passed after the readiness fix:** HTTPS auth,
  Host filtering, live SSE, key rotation, certificate reload, mount isolation,
  and public readiness returning non-2xx when the router is stopped.
- **Runtime-tested on `linux/amd64` only.** The `linux/arm64` native binary
  is cross-compiled, not run. The multi-arch container workflow is defined
  but the ARM64 image has not been built or run locally.
- **GitHub Actions publishes only future tags.** The release workflow triggers
  on `v*` tags with a **strict** `v<major>.<minor>.<patch>[-prerelease]` policy;
  SemVer **build metadata (`+...`) is rejected** because it is not a valid OCI tag
  grammar. Image publication is now **gated on the native vet/race job**
  (`build-native`) passing. **Do not claim a working image publish until that
  gate has passed a final check** — the workflow exists, but its end-to-end
  publish success is not yet established here.

## Native archives and licensing

`scripts/release.sh` cross-compiles static (`CGO_ENABLED=0`) binaries for **four
targets** — `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` — packages
each with `README.md`, and writes a `SHA256SUMS` manifest covering every archive.
It **never tags, pushes, or publishes**; the workflow does that separately, and
verifies `SHA256SUMS` before publishing.

Archives contain **no `LICENSE` file because the repository has no `LICENSE`** —
the script copies it only when it actually exists in the tree. Do not invent or
assume a license.