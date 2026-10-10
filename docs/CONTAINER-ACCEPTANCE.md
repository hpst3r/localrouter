# Container Acceptance (chunk 5): rootless Podman + NGINX on an internal bridge

`scripts/container-acceptance.py` is an **isolated, rootless-Podman** acceptance
harness for the shipped LocalRouter container surface. It covers the
`Containerfile` image, the shipped NGINX edge (`packaging/nginx/localrouter.conf`,
mounted verbatim from the pinned HEAD export) and the Quadlet hardening model. It runs them end to end on a
throwaway `--internal` bridge.

It is deliberately narrow. It owns three files and edits nothing else. The
shipped `Containerfile`, `.containerignore` and everything under `packaging/`
are **read-only references** that it never modifies (as merged by the security
review, PR #9: digest-pinned base and NGINX images, loopback `PublishPort`,
fail-closed secret-file modes). Nothing is installed: no Quadlet units, no
systemd services, no production config, keys or DB.

| File | Role |
| --- | --- |
| `scripts/container-acceptance.py` | the harness (stdlib only, no third‑party deps) |
| `scripts/tests/test_container_acceptance.py` | unit tests: command construction, orchestration against a fake podman, cleanup, budgets, report schema (stdlib `unittest`; no podman, no network) |
| `docs/CONTAINER-ACCEPTANCE.md` | this document |

---

## Fail-closed by construction (the core invariant)

1. **Preflight comes first.** Before any build, podman must report
   `Rootless=true`. The base images and the NGINX image, by the exact digest
   pins of the shipped `Containerfile` `FROM` lines and NGINX Quadlet `Image=`
   (unit tests fail when the two drift), plus the fixture image, must already
   be in local storage, and `openssl`
   must be present. Any miss aborts the run with `executed_stages =
   ["host-preflight"]`. Nothing is built and nothing is pulled.
2. **A failed build is a hard failure.** If the build returns non‑zero (or
   times out), the harness:
   - records the build check (`build.offline_nonetwork` or
     `build.dependency_network_opt_in`) as `FAIL` with `severity="gate"`;
   - sets `outcome = fail` and exits **1**;
   - starts **no network and no container**;
   - **never** falls back to a cached image;
   - still adopts and removes any image the failed or killed build left
     under its per-run tag or `--iidfile` (see "Ownership" below).
3. **The router runs the image that was built.** The image Id is taken from
   `podman build --iidfile` and cross-checked against the per-run tag. The
   router is started from that **immutable Id**, not a tag. Once it is
   running, `router.image_matches_build` (gate) asserts that the container's
   image Id equals it.
4. **Cached-smoke mode can never PASS.** `--no-build` reports
   `cached_smoke_not_release_acceptance` and exits 1.

A run reaches `pass` only when all of these hold:
- a build of the exported HEAD succeeded;
- the live router was verified to be that exact image;
- every gate check below passed;
- cleanup confirmed that no owned resource remains.

---

## Build modes and what they guarantee

| Mode | Build argv (abridged) | Today on this host |
| --- | --- | --- |
| default (offline) | `podman build --pull=never --network=none …` | **fails closed**: the runtime stage's `apk add --no-cache ca-certificates` cannot reach the Alpine repo (`ca-certificates (no such package)`) |
| `--allow-build-network` | `podman build --pull=never …` (only `--network=none` is dropped) | builds |
| `--no-cache` (with either) | adds `--no-cache --layers=false` | every step re-runs; no intermediate layer images are committed |

`--allow-build-network` is an **explicit, user-approved opt-in** for
**build-time dependency downloads only**:

- The image build's `RUN` steps can use the default rootless build network. In
  the shipped `Containerfile` that is `go mod download` (Go modules, verified
  against `go.sum`) and `apk add ca-certificates` (signed Alpine index).
- Base images are **still never pulled**: `--pull=never` is unconditional in
  every mode, and the preflight requires them cached.
- The **runtime is unaffected**: router, fixture and edge stay on the
  `--internal` fixture-only bridge (below).
- The harness cannot restrict *which* hosts a `RUN` step contacts. The scope
  "apk/Go dependencies only" is enforced by the contents of the shipped
  `Containerfile`, not by a firewall.
- The report records it as `build.network = "enabled"` plus a limitation line.
  The default stays `"none"`, and the offline default is unchanged and still
  fails closed.

**Layer cache (precise claim).**
- Without `--no-cache`, podman reuses a cached layer whenever the instruction
  and its inputs match. For `COPY` steps the inputs are file content checksums,
  so the `go build` layer always derives from this run's context.
- `RUN` steps with unchanged instructions (`apk add …`, `go mod download`)
  **may come from an earlier build**, with whatever package versions were
  fetched then. The report says so (`build.layer_cache = "enabled"` plus a
  limitation).
- `--no-cache` re-runs every step, which needs `--allow-build-network` today.
  It sets `build.layer_cache = "disabled"`.
- A no-cache build still uses the cached **base images**, by design.

### Source provenance

The build context is **not** the live worktree. `HEAD` is resolved **once**
(`git rev-parse HEAD`). Then the tree, `describe` and the export all use that
recorded full commit id: `git archive <sha>` into `<run>/context/`.
`export_head_context` refuses anything that is not a full 40/64-hex id, so a
`HEAD` or branch move during the run cannot change what is built. The report's
`source` block records:

| Field | Meaning |
| --- | --- |
| `head`, `tree`, `describe` | the recorded commit, `<head>^{tree}`, `git describe --tags --always <head>` (also passed as `--build-arg VERSION=…`, which only sets the OCI version label) |
| `worktree_dirty`, `dirty_entries`, `status_sha256` | `git status --porcelain=v1 --untracked-files=all` state of the worktree (names only, no file content is read) |
| `context_sha256`, `context_files` | sha256 over (path, exec bit, content sha256) of every exported file, before `.containerignore` filtering by podman |
| `containerfile_sha256` | sha256 of the exported `Containerfile` |

**Edge config.** NGINX mounts `packaging/nginx/localrouter.conf` **from that
export** (`<run>/context/…`), not from the live worktree. The config must be a
regular file inside the export, or the run fails closed. The top-level
`edge_config` report field records the mounted `path`, its `sha256` and its
`source` (`git archive <sha>`). Only `--no-build` cached smoke, which is never
acceptance, has no export. It mounts the worktree file and records it as
`live worktree (… not pinned)`.

**Guarantee:** the accepted image and the mounted edge config come from exactly
the recorded commit's tree. **Not guaranteed:** that uncommitted or untracked
changes work. They are excluded from the image and the edge, and a dirty
worktree adds a limitation saying so. Commit before accepting.

---

## What `pass` proves (and what it does not)

Every check is a `gate` unless noted. Each was **observed on live containers
running the built image**:

| Stage | Checks |
| --- | --- |
| `host-preflight` | `preflight.podman_rootless`; `preflight.build_base_image` ×2 (or `preflight.router_image` in `--no-build`); `preflight.nginx_image`; `preflight.provider_image`; `preflight.openssl` |
| `source` | `source.head_exported` (HEAD, tree, context and Containerfile digests) |
| `build` | `build.offline_nonetwork` **or** `build.dependency_network_opt_in`; `build.image_id_recorded` (iidfile Id == tag Id) |
| `network` | `network.internal`: the per-run bridge was created `--internal` and inspects as such |
| `router` | `router.image_user_nonroot`: the **image's own `USER`** (`Config.User` of the image, before any `--user` override) is non-root; `router.image_matches_build`; `router.no_host_port`; `router.mounts_allowlisted`; `router.nonroot_uid` (container `Config.User`, i.e. the `--user 1000:1000` override); `router.process_uid` (`id -u` inside = 1000); `router.readonly_rootfs`; `router.no_tls_mount`; `router.readyz_ready`; `router.data_dir_writable`; `router.root_immutable` |
| isolation (router, fixture, edge) | `<role>.hardened`: `ReadonlyRootfs`, `EffectiveCaps == []`, no `CapAdd`, `no-new-privileges`; `<role>.internal_network_only`: attached to the run's internal bridge only; `<role>.no_default_route`: `/proc/net/route` inside has no `0.0.0.0/0` route |
| `edge` | `edge.loopback_only_publish` (`127.0.0.1:<freeport>` only); `edge.readyz_over_tls`; `edge.known_host_accepted` (200); `edge.unknown_host_rejected` (403); `edge.auth_required_401`; `edge.auth_bearer_accepted` (200); `edge.bad_key_rejected` (401); `edge.sse_status_200`; `edge.sse_chunks_streamed` (3); `edge.sse_not_buffered` (arrival spread ≥ 1s against a 3s fixture gap); `edge.readyz_reflects_backend`: after `podman stop` of the router, edge `/readyz` returns **any status ≥ 400** (observed: 504) |
| `host` | `host.no_forbidden_port_published`: no container with this run's label publishes 8787/8443/8081 |
| cleanup | `cleanup.no_owned_leftovers`: every owned container/network/image was confirmed gone |

**It does not prove:**
- production readiness;
- real provider connectivity or production TLS/DB/keys;
- quota accounting against a real provider;
- anything about uncommitted changes;
- anything about a cached image (cached-smoke mode disclaims acceptance).

See also "Not covered" below.

---

## Cached‑smoke mode (`--no-build`)

`--no-build` skips source export and build. It pins the cached `--image`
(default `localhost/localrouter:0.3.0`) to its immutable Id and runs the same
container stages against that Id. It is a debugging aid:

- the outcome is the hard‑coded `cached_smoke_not_release_acceptance`, and the
  exit code is 1;
- `cached_smoke_not_release_acceptance: true` and `fresh_build_proven: false`
  appear in `report.json`;
- `build.skipped_cached_smoke` is a failed `limit` check, plus a limitation
  line;
- `--allow-build-network` and `--no-cache` are rejected with `--no-build`.

---

## Topology

```
     host loopback only                 rootless --internal bridge (no default route)
  ┌───────────────────────┐   -p   ┌────────────────────────────────────────────────┐
  │ https 127.0.0.1:      │◀──────▶│ lr-acc-net-<runid>   label lr-acc-run=<runid>  │
  │ <freeport>            │        │                                                │
  └───────────────────────┘        │  ┌───────────────┐  :8787   ┌───────────────┐  │
                                    │  │ nginx edge    │─────────▶│ router        │  │
                                    │  │ uid 1000, RO, │ upstream │ image Id, RO, │  │
                                    │  │ cap-drop ALL  │          │ uid 1000, no  │  │
                                    │  └───────────────┘          │ host port     │  │
                                    │                             └──────┬────────┘  │
                                    │  ┌───────────────┐  base_url        │           │
                                    │  │ fixture       │◀─────────────────┘           │
                                    │  │ python http,  │  alias `fixture`             │
                                    │  │ uid 65534, RO │                              │
                                    │  └───────────────┘                              │
                                    └────────────────────────────────────────────────┘
```

- **One `--internal` bridge for all three.** It has no default route, so none
  of the containers can reach anything off the bridge. Name resolution
  (`localrouter`, `fixture`) works through aardvark-dns on the internal
  network. Rootless port forwarding of the NGINX publish still works; this was
  verified live.
- NGINX is the **only** publisher, on `127.0.0.1` with an auto‑allocated high
  port (never 8787/8443/8081).
- The fixture is a local `http.server` and is the router's only upstream. Its
  account is `provider: openai_compat`, which makes no usage call.
- Egress isolation is proven **structurally**: `--internal`, attachment to that
  network only, and no default route. The harness deliberately makes **no**
  live outbound "negative probe", because that would itself be an outbound
  attempt.

---

## Usage

```bash
# unit tests (fast; stdlib unittest, no podman, no network)
python3 -m unittest scripts.tests.test_container_acceptance -v

# default: offline build of HEAD (currently fails closed on apk; see above)
python3 scripts/container-acceptance.py

# approved fresh-source acceptance: build-time dependency network, every layer re-run
python3 scripts/container-acceptance.py --allow-build-network --no-cache \
    --max-runtime 230 --build-timeout 200 --cleanup-timeout 60

# EXPLICIT cached smoke only (NOT acceptance; exit 1)
python3 scripts/container-acceptance.py --no-build

# machine-readable report on stdout / leave containers up for debugging
python3 scripts/container-acceptance.py --json
python3 scripts/container-acceptance.py --keep
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--artifacts <root>` | `~/.hermes/work/localrouter-roadmap/container-acceptance` | a unique `<runid>/` is created under it |
| `--max-runtime <s>` | 180 | shared deadline for **every** subprocess and HTTPS probe of the run |
| `--build-timeout <s>` | 150 | build cap (further clamped to the remaining run budget) |
| `--cleanup-timeout <s>` | 60 | **separate** teardown budget, started when the run ends |
| `--allow-build-network` | off | build `RUN` steps may fetch apk/Go dependencies; `--pull=never` stays |
| `--no-cache` | off | `--no-cache --layers=false` |
| `--no-build` | off | cached smoke only |
| `--image`, `--nginx-image`, `--provider-image` | see `--help` | `--image` is used only by `--no-build` |
| `--keep` | off | skip teardown; everything is reported in `remaining_resources` |
| `--json` | off | print the (sanitized) report |

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | `pass` or `pass_with_limitations` (no `gate` check failed) |
| `1` | `fail` (a gate failed, the run aborted, or cleanup left an owned resource) or `cached_smoke_not_release_acceptance` |
| `2` | podman not found: prints `{"outcome": "error", "error": "podman not found"}` to stdout and writes **no** run directory or report |

---

## Safety and isolation guarantees

- **Rootless only, checked first.** `podman info` must say rootless before
  anything is built or created.
- **No image pull, anywhere.** `--pull=never` appears in the build **and in
  every `podman run`**, in front of the image positional (unit-tested). The
  preflight requires every image cached.
- **Runtime has no egress.** The three containers share one `--internal`
  bridge (see Topology).
- **Hardened containers.** Every container gets `--read-only --cap-drop=ALL
  --security-opt=no-new-privileges`, and the result is verified from inspect
  (`EffectiveCaps == []`).
  - The router and NGINX run as uid 1000 (`keep-id`); the fixture runs as
    65534.
  - The image's own `USER` is asserted non-root **before** the `--user`
    override is applied.
- **Ownership by ID, not by name pattern.**
  - Every container and the network carry `--label lr-acc-run=<runid>`.
  - A resource is recorded as owned **only after creation succeeds**, using
    the ID podman returned: the container Id from `run -d`, the network Id
    from inspect, the image Id from `--iidfile`.
  - At teardown, a label sweep (`--filter label=lr-acc-run=<runid>`) also
    adopts anything this run created but could not record, for example a
    `podman run` killed mid-creation.
  - If the built image Id already existed before the build (an identical
    earlier build), only the per-run tag is ours: cleanup runs `podman untag`,
    never `rmi`.
  - **Pre-existing images are detected fail-closed.** Before building, the
    harness lists every image Id (`podman images -a -q --no-trunc`). An error,
    an unparsable Id, or an empty list (the preflight just saw the base
    images) **refuses the build**. It never guesses "nothing pre-existed".
    The per-run tag `localhost/localrouter-acc:<runid>` must be absent
    (`podman image exists` rc=1); otherwise the build is refused too.
  - **Failed and timed-out builds are adopted.** Once `podman build` is
    launched, cleanup checks the per-run tag and `--iidfile` on the
    **cleanup** budget, even when the build failed or was killed and used up
    the run budget. The tag was proven absent before, so an image under it is
    ours. Its Id is `rmi`'d when new and only `untag`ged when it pre-existed.
    An iidfile Id without the tag is owned only when it is new; a pre-existing
    one is never touched. A tag that exists but whose Id cannot be resolved is
    reported in `remaining_resources` and fails the run.
- **Cleanup never forces foreign removal.**
  - Containers: `podman rm -f -t 5 <owned-id>`.
  - Network: `podman network rm <owned-id>` **without `-f`**, so a network a
    foreign container joined is left in place and reported.
  - Image: `podman rmi <owned-id>` **without `-f`**.
- **Audit list ≠ remaining list.** `owned` in the report is the append-only
  audit of everything created and is never cleared. `remaining_resources`
  lists every owned resource that `podman … exists` did **not** confirm gone
  (rc=1). Anything unverified counts as remaining. A non-empty list fails the
  run (`cleanup.no_owned_leftovers`), except with `--keep`.
- **Bounded runtime, two budgets.**
  - `--max-runtime` is shared by every subprocess, including podman, git and
    openssl.
  - `run()` clamps each timeout to the remaining budget and refuses to start
    a command once it is exhausted.
  - Every command runs in its own session. On timeout its **whole process
    group** gets SIGTERM. The harness then drains and reaps the leader and
    polls the group (`killpg(pgid, 0)`) for up to 5s. If **any** member is
    still there, the group gets SIGKILL, whatever the leader or pipe state.
    That includes a SIGTERM-ignoring member with detached stdio, and the case
    where the leader was already reaped. SIGKILL is skipped only when the
    group is confirmed empty. The pgid cannot be recycled while the leader is
    unreaped or any member lives. Each phase is bounded, so a timed-out
    command can overshoot by at most ~10s.
  - Scope: escalation runs only on the **timeout** path. A command that exits
    normally is not group-killed, because podman's own long-lived helpers
    must survive a successful `podman run -d`.
  - HTTPS probes clamp their socket timeout to the remaining budget. The SSE
    probe re-clamps before **every** line, so an endless stream cannot outlive
    the deadline.
  - Wait loops check the deadline and sleep 0.5s.
  - Teardown has its **own** `--cleanup-timeout` budget, so an exhausted run
    budget never prevents cleanup.
  - Worst-case wall time ≈ `max-runtime + cleanup-timeout + ~20s`.
- **Throwaway secrets only.** These are a per-run self-signed cert/key and
  fake fixture bearer/provider keys. No real credential is read. The fixture
  key values and the TLS key body are redacted (`[REDACTED]`) from
  `report.json`, `--json` stdout, `build.log` and `cleanup.log`.
- **Contained scratch.** The build runs with `TMPDIR=<run>/build-tmp`, so
  buildah scratch stays inside the run directory. A failed rootless `RUN` step
  can leave a `buildah*` dir behind there.

---

## Artifacts

Every run writes a **unique, durable** directory named by its run id:

```
<artifacts>/<runid>/
├── report.json        # strict, sanitized JSON (schema below)
├── build.log          # sanitized build output
├── cleanup.log        # sanitized per-container logs + the exact rm/rmi/network rm commands and rc
├── image.iid          # podman build --iidfile output
├── context/           # `git archive <recorded sha>`: build context + mounted edge config
├── build-tmp/         # TMPDIR of the build (buildah scratch)
├── config/config.yaml # the generated fixture router config
├── secrets/           # fake fixture client/provider keys (0600)
├── tls/               # throwaway self-signed tls.crt / tls.key (0600)
├── data/              # the router's rw data dir
└── fixture/provider.py# the local streaming OpenAI-compatible fixture
```

Earlier run directories are never rewritten. An aborted run still leaves its
`report.json` (and `build.log` once the build has started).

`report.json` shape:

```json
{
  "run_id": "b1f3bca701",
  "outcome": "pass | pass_with_limitations | fail | cached_smoke_not_release_acceptance",
  "error": null,
  "generated_at": "2026-10-10T00:31:12+00:00",
  "elapsed_seconds": 60.68,
  "max_runtime_seconds": 230.0,
  "cleanup_elapsed_seconds": 6.27,
  "cleanup_timeout_seconds": 60.0,
  "artifacts_dir": "<artifacts>/<runid>",
  "executed_stages": ["host-preflight", "source", "build", "network", "router", "provider", "nginx"],
  "checks": [{"name": "router.image_matches_build", "ok": true, "detail": "...", "severity": "gate"}],
  "checks_passed": 43,
  "checks_total": 43,
  "gate_failures": [],
  "limitation_failures": [],
  "commands": ["podman build --pull=never ...", "podman network create --internal ...", "podman run -d --pull=never ..."],
  "limitations": ["..."],
  "source": {"repo": "...", "head": "<sha>", "tree": "<sha>", "describe": "...", "worktree_dirty": true, "dirty_entries": 8, "status_sha256": "...", "context_dir": "...", "context_sha256": "...", "context_files": 199, "containerfile_sha256": "...", "built_from": "git HEAD export (committed tree only)"},
  "build": {"network": "none | enabled", "pull": "never", "layer_cache": "enabled | disabled", "tag": "localhost/localrouter-acc:<runid>", "cpu_flag": "--cpu-quota", "version_arg": "...", "argv": ["podman", "build", "..."]},
  "router_image": "<image id the router container was started from, or null>",
  "image_under_test": "<same as router_image>",
  "image_built_artifact": {"reference": "<id>", "immutable_id": "<id>", "digest": "sha256:...", "created": "...", "size_bytes": 28029738, "version_label": "..."},
  "built_image_id": "<id or null>",
  "fresh_build_proven": true,
  "image_matches_build": true,
  "cached_smoke_not_release_acceptance": false,
  "nginx_image": "docker.io/nginxinc/nginx-unprivileged:1.30.5-alpine@sha256:15c9...",
  "provider_image": "docker.io/library/python:3.13-slim",
  "host_port": 50863,
  "edge_config": {"path": "<artifacts>/<runid>/context/packaging/nginx/localrouter.conf", "sha256": "...", "source": "git archive <sha>"},
  "owned": [{"kind": "image", "id": "<id>", "name": "localhost/localrouter-acc:<runid>", "preexisting": false}, {"kind": "network", "id": "<id>", "name": "lr-acc-net-<runid>"}, {"kind": "container", "id": "<id>", "name": "lr-acc-router-<runid>-1"}],
  "remaining_resources": []
}
```

Field semantics:
- **Image fields.** `router_image` and `image_under_test` are `null` whenever
  no router container was started (for example, a failed build).
  `image_matches_build` is `true`/`false` from the gate, or `null` when it was
  not evaluated. Image Ids are bare hex (podman's inspect format).
- **`source` and `build`.** `source` is `null` in `--no-build`. `build`
  describes the intended build even when the build did not run.
- **`edge_config`.** The NGINX config actually mounted, with its sha256 and
  origin. It is `null` when no edge was started.
- **`error`.** The abort reason (string) when the run aborted, otherwise
  `null`.
- **Unit-tested.** The unit test `test_documented_report_schema_matches_code`
  requires this block's keys to equal `build_report()`'s keys exactly.

---

## Test discipline

`scripts/tests/test_container_acceptance.py` uses **stdlib `unittest` only**.
It needs no podman and no network; the provenance tests use a throwaway local
`git` repo under `TMPDIR`. It asserts on the harness's own construction and
decisions. Fakes answer *commands* only; the harness decides every outcome.
It pins:

- **Argv.**
  - `--pull=never` (before the image), `--cap-drop=ALL`, `no-new-privileges`,
    `--read-only` and the run label are in every `podman run`.
  - The network is created `--internal` with the label.
  - The router publishes nothing; NGINX publishes `127.0.0.1:<port>:8443`
    only.
- **Build argv.**
  - `--pull=never` in both modes.
  - `--network=none` by default; `--allow-build-network` drops exactly that
    one flag.
  - `--no-cache` adds `--layers=false`.
  - `--iidfile` and `--build-arg VERSION` are passed.
  - No CPU flag is passed when detection fails (this used to wrongly emit
    `--cpus`).
- **Ordering.**
  - The rootless and missing-base-image refusals happen **before** any build
    or git export.
  - A failed build stops at `["host-preflight", "source", "build"]` with no
    container, network or owned resource.
  - An image whose own `USER` is root/unset is refused before the router
    starts.
- **Deadline.**
  - In a full fake run, **every** subprocess, HTTPS probe, SSE probe and git
    call receives the run's `Deadline`.
  - `run()` clamps, refuses on an exhausted budget, starts a new session, and
    `killpg`s the group on timeout.
  - Real processes, with a pid-file handshake written only after the
    survivor ignores SIGTERM: a SIGTERM-ignoring member with detached stdio,
    one holding the pipes, and one left after the leader was already reaped
    are all SIGKILLed. An obedient group returns without waiting out the
    grace.
  - `https_request` clamps its socket timeout; `sse_probe` raises once the
    budget runs out mid-stream.
- **Provenance.**
  - Router argv ends in the built image **Id**, not the tag.
  - The iidfile Id is recorded and owned.
  - A pre-existing identical Id is only untagged.
  - The HEAD export excludes dirty and untracked changes, has a stable digest
    and the right Containerfile hash, and the dirty flag is set.
  - Real git: HEAD is moved between provenance and export, and the live edge
    config is edited. The context and the mounted edge config remain the
    recorded commit's, and the export refuses `HEAD`, branch names and
    abbreviations. Tree and describe are derived from the recorded sha. A
    missing edge config in the export fails closed.
  - The pre-existing image listing fails closed (error, empty, unparsable),
    and so does an existing per-run tag. Neither runs a build.
  - A failed build that left the tag on a **new** Id gets `rmi <id>`; one on a
    **shared, pre-existing** Id gets only `untag`. A timed-out build's iidfile
    Id is adopted on the cleanup budget. A shared iidfile Id is never owned.
- **Cleanup.**
  - It works by ID, in reverse order, with no `-f` on network rm.
  - A failed network rm is reported in `remaining_resources`, not forced.
  - An exhausted cleanup budget spawns nothing and reports everything as
    remaining.
  - Cleanup uses its own `--cleanup-timeout` deadline even when the run
    aborted.
  - Leftovers fail the run.
- **main().**
  - A build failure exits 1 and writes a report whose fixture secrets are
    `[REDACTED]`, with `image_under_test: null`.
  - A full fake pass removes exactly the owned IDs.
  - A missing podman exits 2.
  - The documented schema equals the code's.
- **Inspect helpers.** `hardening_problems`, `has_default_route`,
  `assert_image_user`, the mount/port/user assertions, `host_allowed`
  (mirrors `internal/app/hostguard.go`), `sanitize`, and
  `render_router_config`.

---

## Not covered (remaining gates before any release sign-off)

1. **Default offline build still fails.** It fails closed (see above). Making
   it pass needs a cached `ca-certificates` source or a base that already
   carries it. That is a `Containerfile`/packaging change, outside this
   harness; the merged security review left the `apk` step network-dependent.
2. **Quadlet/systemd acceptance is NOT done.** The harness runs plain `podman
   run` with flags chosen for parity, but it does **not** install or run the
   Quadlet units. Known differences from the shipped units:
   - `--security-opt label=disable` instead of `:Z` relabelling;
   - a single `config.yaml` file mount instead of the `/etc/localrouter`
     directory;
   - an `--internal` test bridge instead of the shipped non-internal
     `localrouter.network`;
   - an ephemeral loopback host port (`127.0.0.1:<free>:8443`) instead of the
     NGINX unit's `PublishPort=127.0.0.1:8443:8443` (same loopback address;
     host port 8443 itself is never bound by the harness, and the operator's
     LAN/overlay address edit is not exercised).

   Not exercised at all:
   - `HealthCmd`/`HEALTHCHECK` (a local `podman build` writes OCI format,
     which drops the `Containerfile` `HEALTHCHECK` with a build warning; the
     Quadlet units set `HealthCmd` themselves);
   - `ReloadSignal=SIGHUP`, `ReloadCmd`;
   - `Restart=`, `StopTimeout`;
   - `ReadOnlyTmpfs`.
3. **Upgrade/rollback acceptance is NOT done.** That means old → new image
   against an existing `data_dir` ledger, and rollback.
4. **Also not done:** an arm64 image, CI wiring, key rotation / cert reload,
   and ingest-path checks.
5. **Layer-cache images from cached builds.** Intermediate layer images
   created by a build **with** the layer cache (the default, including the
   failing offline build) are the shared build cache. They are not owned by
   the run and are not removed. Use `--no-cache` (`--layers=false`) for runs
   that should add only the final, owned image.
6. **Stale pre-fix reports.** Earlier reports under
   `~/.hermes/work/localrouter-roadmap/container-acceptance/` came from older
   harness versions and are **not** acceptance evidence:
   - `2a61615ee2`: `pass_with_limitations` from the removed cached-fallback
     behaviour;
   - `77a017b303`, `7067a274ac`: cached smoke.

   They were left untouched.
