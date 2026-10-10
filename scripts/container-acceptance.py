#!/usr/bin/env python3
"""chunk5 container acceptance: rootless Podman + NGINX on an internal bridge.

Owned by this slice ONLY:
    scripts/container-acceptance.py
    scripts/tests/test_container_acceptance.py
    docs/CONTAINER-ACCEPTANCE.md

What it proves, by actually running containers:

  * host preflight runs FIRST: podman must be rootless and every base/runtime
    image must already be cached, before any build starts;
  * the build context is ``git archive`` of the HEAD commit id recorded once
    up front (uncommitted changes are NOT in it; the dirty state is recorded),
    with the commit, tree, a context digest and the Containerfile sha256 in the
    report; the NGINX edge config is mounted from that same export;
  * the image builds with ``--pull=never`` (never a base-image pull) and, by
    default, ``--network=none``. ``--allow-build-network`` is an explicit
    opt-in that lets the build's RUN steps fetch build-time apk/Go
    dependencies; it never relaxes ``--pull=never`` and never touches the
    runtime network. The build is FAIL-CLOSED: if it fails the run reports
    FAILED, exits non-zero and starts NO container -- there is never a
    cached-image fallback;
  * the image Id comes from ``--iidfile``; the router container is started
    from that immutable Id (not a tag) and is asserted to run exactly it;
  * the image's own ``USER`` is non-root before any ``--user`` override; every
    container runs ``--read-only --cap-drop=ALL --security-opt
    no-new-privileges --pull=never``; the router publishes NO host port;
  * router, fixture provider and NGINX share one ``--internal`` bridge (no
    default route, so no egress); NGINX publishes only on
    ``127.0.0.1:<freeport>`` (never 0.0.0.0, never 8787/8443/8081);
  * /readyz through the edge reports readiness, HTTPS auth is enforced,
    a known Host is accepted and an unknown Host is rejected with 403;
  * streaming (SSE) is not buffered by the edge, using a *real* local fixture
    provider on the internal bridge;
  * every subprocess and HTTPS probe is bound by one shared ``--max-runtime``
    deadline; cleanup runs in ``finally`` on its own ``--cleanup-timeout``
    budget and removes only resources this run created, by ID.

Hard limits honoured by design: no image pull, no runtime egress, no host
port 8787, no production certs/DB/keys, no systemd units, no credential reads.
The only secrets it ever writes are throwaway self-signed TLS material and
throwaway fixture API/bearer keys generated in a per-run artifact directory
under ``~/.hermes/work/localrouter-roadmap/container-acceptance/<runid>/``.
"""

from __future__ import annotations

import argparse
import contextlib
import datetime
import hashlib
import http.client
import ipaddress
import json
import os
import re
import secrets as pysecrets
import shutil
import signal
import socket
import ssl
import string
import subprocess
import sys
import tarfile
import time
import uuid

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

ROUTER_IMAGE_DEFAULT = "localhost/localrouter:0.3.0"  # cached worktree image
# The shipped digest pins (packaging/quadlet/localrouter-nginx.container Image=,
# Containerfile FROM lines), so preflight and runs never resolve a mutable tag.
# Unit tests fail when these drift from the shipped files.
NGINX_IMAGE_DEFAULT = (
    "docker.io/nginxinc/nginx-unprivileged:1.30.5-alpine"
    "@sha256:15c994d10d6d78658721c3bcafff14cb281fba2a4bdf9d5ba92c416a472516e3"
)
PROVIDER_IMAGE_DEFAULT = "docker.io/library/python:3.13-slim"
BUILD_BASE_IMAGES = (
    "docker.io/library/golang:1.27.0-alpine"
    "@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc",
    "docker.io/library/alpine:3.24"
    "@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6",
)

ROUTER_CONTAINER_PORT = 8787          # inside the container netns only
NGINX_TLS_PORT = 8443                 # nginx-unprivileged public listener
NGINX_HEALTH_PORT = 8081              # loopback-only in-container liveness
PROVIDER_PORT = 8080                  # internal bridge only
PROVIDER_USER = "65534:65534"         # fixture runs as nobody

# Host ports this harness must never bind/publish anywhere.
FORBIDDEN_HOST_PORTS = {8787, 8443, 8081}

KNOWN_HOST = "router.example.com"     # present in allowed_hosts
UNKNOWN_HOST = "evil.example.com"     # absent -> must be rejected
FIXTURE_MODEL = "fixture-model"
FIXTURE_BASE_URL = "http://fixture:%d/v1" % PROVIDER_PORT

DEFAULT_ARTIFACT_ROOT = os.path.expanduser(
    "~/.hermes/work/localrouter-roadmap/container-acceptance"
)
DEFAULT_MAX_RUNTIME = 180.0
DEFAULT_CLEANUP_TIMEOUT = 60.0
KILL_GRACE = 5.0                      # SIGTERM -> SIGKILL grace per process group

RUN_LABEL_KEY = "lr-acc-run"
HARDENING_ARGS = ["--cap-drop=ALL", "--security-opt=no-new-privileges"]

ALLOWED_ROUTER_MOUNT_DESTS = {
    "/etc/localrouter/config.yaml",
    "/run/secrets",
    "/var/lib/localrouter",
}
# Rootless podman always injects these; they are not interesting mounts.
ROUTER_MOUNT_IGNORE_DESTS = {
    "/etc/resolv.conf",
    "/etc/hosts",
    "/etc/hostname",
    "/etc/localtime",
    "/dev",
    "/dev/termination-log",
}

_IMAGE_ID_RE = re.compile(r"^[0-9a-f]{64}$")

RUN_ID = uuid.uuid4().hex[:10]
_COUNTER = [0]


class AcceptanceError(Exception):
    """A checked acceptance condition failed."""


class Deadline:
    """A bounded wall-clock budget."""

    def __init__(self, seconds: float):
        self.limit = float(seconds)
        self.start = time.monotonic()

    @property
    def elapsed(self) -> float:
        return time.monotonic() - self.start

    @property
    def remaining(self) -> float:
        return max(0.0, self.limit - self.elapsed)

    def check(self) -> None:
        if self.elapsed > self.limit:
            raise AcceptanceError(
                "max runtime %.0fs exceeded (elapsed %.1fs)" % (self.limit, self.elapsed)
            )


def norm_id(value) -> str:
    """Normalize an image Id: podman inspect reports bare hex, --iidfile and
    ``images --no-trunc`` report ``sha256:<hex>``."""
    value = str(value or "").strip().lower()
    return value[len("sha256:"):] if value.startswith("sha256:") else value


def run_label(run_id=None) -> str:
    return "%s=%s" % (RUN_LABEL_KEY, run_id or RUN_ID)


# ---------------------------------------------------------------------------
# Process helpers
# ---------------------------------------------------------------------------


def _group_alive(pgid) -> bool:
    """True while any process (the leader included) is still in ``pgid``."""
    try:
        os.killpg(pgid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def _await_group_exit(proc, pgid, grace) -> bool:
    """Drain/reap the leader and poll the group for up to ``grace`` seconds.

    Returns True only once the leader is reaped AND no group member is left.
    An unreaped leader keeps its pid, so ``pgid`` cannot be recycled meanwhile;
    a live member keeps ``pgid`` itself in use.
    """
    end = time.monotonic() + grace
    while True:
        left = end - time.monotonic()
        if proc.returncode is None:
            with contextlib.suppress(subprocess.TimeoutExpired):
                proc.communicate(timeout=max(0.0, min(left, 0.1)))
        if proc.returncode is not None and not _group_alive(pgid):
            return True
        if left <= 0:
            return False
        time.sleep(min(0.05, max(left, 0.0)))


def _kill_process_group(proc) -> None:
    """SIGTERM, then SIGKILL, the command's whole process group (its session).

    Escalation does not depend on the leader or the pipes: a member that
    ignores SIGTERM is SIGKILLed even when it detached its stdio or the leader
    was already reaped. SIGKILL is skipped only when the group is confirmed
    empty. Each phase is bounded by KILL_GRACE.
    """
    pgid = proc.pid
    with contextlib.suppress(ProcessLookupError, PermissionError):
        os.killpg(pgid, signal.SIGTERM)
    if _await_group_exit(proc, pgid, KILL_GRACE):
        return
    with contextlib.suppress(ProcessLookupError, PermissionError):
        os.killpg(pgid, signal.SIGKILL)
    _await_group_exit(proc, pgid, KILL_GRACE)


def run(cmd, *, timeout=60.0, env=None, check=False, deadline=None):
    """Run a command, returning a CompletedProcess. Never raises on rc!=0.

    When ``deadline`` (a :class:`Deadline`) is supplied the per-command timeout
    is clamped to the remaining budget, so no single command can silently
    overshoot it. The command runs in its own session; on timeout the whole
    process group is killed (SIGTERM, then SIGKILL) and TimeoutExpired is
    re-raised, so the overshoot is bounded by 2 * KILL_GRACE.
    """
    if deadline is not None:
        remaining = deadline.remaining
        if remaining <= 0:
            raise AcceptanceError(
                "max runtime budget exhausted before: %s" % " ".join(cmd)
            )
        timeout = min(float(timeout), remaining)
    proc = subprocess.Popen(
        cmd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=env,
        start_new_session=True,
    )
    try:
        out, err = proc.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        _kill_process_group(proc)
        raise
    p = subprocess.CompletedProcess(cmd, proc.returncode, out or "", err or "")
    if check and p.returncode != 0:
        raise AcceptanceError(
            "command failed rc=%d: %s\n%s" % (p.returncode, " ".join(cmd), p.stderr.strip())
        )
    return p


def podman_available() -> bool:
    return shutil.which("podman") is not None


def podman_rootless(*, deadline) -> bool:
    p = run(["podman", "info", "--format", "{{.Host.Security.Rootless}}"],
            timeout=30, deadline=deadline)
    return p.returncode == 0 and p.stdout.strip().lower() == "true"


def image_exists(image: str, *, deadline) -> bool:
    return run(["podman", "image", "exists", image], timeout=30,
               deadline=deadline).returncode == 0


def inspect_container(ref: str, *, deadline) -> dict:
    p = run(["podman", "container", "inspect", ref], timeout=30, deadline=deadline)
    if p.returncode != 0:
        raise AcceptanceError("podman inspect %s failed: %s" % (ref, p.stderr.strip()))
    data = json.loads(p.stdout)
    if not data:
        raise AcceptanceError("podman inspect %s returned nothing" % ref)
    return data[0]


def image_id(image: str, *, deadline) -> str:
    """Immutable image Id (bare hex) or "" when the image is absent."""
    p = run(["podman", "image", "inspect", "--format", "{{.Id}}", image],
            timeout=30, deadline=deadline)
    return norm_id(p.stdout) if p.returncode == 0 else ""


def image_config_user(image: str, *, deadline) -> str:
    """The image's own ``USER`` (Config.User), before any ``--user`` override."""
    p = run(["podman", "image", "inspect", "--format", "{{.Config.User}}", image],
            timeout=30, deadline=deadline)
    if p.returncode != 0:
        raise AcceptanceError("image inspect %s failed: %s" % (image, p.stderr.strip()))
    return p.stdout.strip()


def image_metadata(image: str, *, deadline) -> dict:
    """A small JSON-safe provenance record for a built image artifact."""
    meta = {"reference": image, "immutable_id": image_id(image, deadline=deadline)}
    for key, fmt in (("digest", "{{.Digest}}"), ("created", "{{.Created}}"),
                     ("size_bytes", "{{.Size}}"),
                     ("version_label",
                      '{{index .Labels "org.opencontainers.image.version"}}')):
        p = run(["podman", "image", "inspect", "--format", fmt, image],
                timeout=30, deadline=deadline)
        if p.returncode != 0:
            continue
        val = p.stdout.strip()
        if key == "size_bytes":
            try:
                meta[key] = int(val)
            except ValueError:
                meta[key] = None
        else:
            meta[key] = val
    return meta


def container_image_id(inspect_json: dict) -> str:
    """The immutable image Id a running container actually uses."""
    return str(inspect_json.get("Image") or inspect_json.get("ImageID") or "").strip()


def image_identity_ok(inspect_json: dict, expected_id: str) -> bool:
    """True only when the container's immutable image Id equals ``expected_id``."""
    expected = norm_id(expected_id)
    return bool(expected) and norm_id(container_image_id(inspect_json)) == expected


# ---------------------------------------------------------------------------
# Source provenance: the build context is an export of git HEAD
# ---------------------------------------------------------------------------


def _sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 16), b""):
            h.update(chunk)
    return h.hexdigest()


def context_digest(root: str) -> str:
    """sha256 over (relative path, exec bit, content sha256) of every file."""
    h = hashlib.sha256()
    entries = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames.sort()
        for fn in filenames:
            full = os.path.join(dirpath, fn)
            rel = os.path.relpath(full, root).replace(os.sep, "/")
            if os.path.islink(full):
                entries.append("%s\0L\0%s\n" % (rel, os.readlink(full)))
            else:
                mode = "x" if os.stat(full).st_mode & 0o111 else "-"
                entries.append("%s\0%s\0%s\n" % (rel, mode, _sha256_file(full)))
    for e in sorted(entries):
        h.update(e.encode())
    return h.hexdigest()


_COMMIT_RE = re.compile(r"^(?:[0-9a-f]{40}|[0-9a-f]{64})$")


def git_provenance(repo: str, *, deadline) -> dict:
    """HEAD commit/tree and the worktree's dirty state (names only, no content).

    The symbolic ``HEAD`` is resolved exactly once; tree and describe derive
    from that recorded commit, so a HEAD move mid-run cannot mix commits.
    """
    def git(*args):
        return run(["git", "-C", repo, *args], timeout=30, deadline=deadline, check=True)

    head = git("rev-parse", "HEAD").stdout.strip()
    if not _COMMIT_RE.match(head):
        raise AcceptanceError("git rev-parse HEAD gave no full commit id: %r" % head)
    tree = git("rev-parse", "%s^{tree}" % head).stdout.strip()
    status = git("status", "--porcelain=v1", "--untracked-files=all").stdout
    p = run(["git", "-C", repo, "describe", "--tags", "--always", head],
            timeout=30, deadline=deadline)
    describe = p.stdout.strip() if p.returncode == 0 else head[:12]
    entries = [ln for ln in status.splitlines() if ln.strip()]
    return {
        "repo": repo,
        "head": head,
        "tree": tree,
        "describe": describe,
        "worktree_dirty": bool(entries),
        "dirty_entries": len(entries),
        "status_sha256": hashlib.sha256(status.encode()).hexdigest(),
    }


def export_head_context(repo: str, dest: str, *, deadline, commit: str) -> dict:
    """Export the recorded commit's tree into ``dest`` (the build context).

    ``commit`` must be the full immutable id recorded by :func:`git_provenance`;
    a symbolic ref (``HEAD``, a branch) or abbreviation is refused, because it
    could name a different tree by the time ``git archive`` runs.
    """
    if not _COMMIT_RE.match(commit or ""):
        raise AcceptanceError("refusing to export a non-immutable ref: %r" % commit)
    tar_path = dest + ".tar"
    run(["git", "-C", repo, "archive", "--format=tar", "-o", tar_path, commit],
        timeout=60, deadline=deadline, check=True)
    os.makedirs(dest)
    try:
        with tarfile.open(tar_path) as tf:
            tf.extractall(dest, filter="data")
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.remove(tar_path)
    nfiles = sum(len(fs) for _d, _s, fs in os.walk(dest))
    cf = os.path.join(dest, "Containerfile")
    return {
        "context_dir": dest,
        "context_sha256": context_digest(dest),
        "context_files": nfiles,
        "containerfile_sha256": _sha256_file(cf) if os.path.exists(cf) else None,
    }


# ---------------------------------------------------------------------------
# Pure command-construction functions (unit-tested without podman)
# ---------------------------------------------------------------------------


def unique_name(prefix: str) -> str:
    _COUNTER[0] += 1
    return "lr-acc-%s-%s-%d" % (prefix, RUN_ID, _COUNTER[0])


def unique_network_name() -> str:
    return "lr-acc-net-%s" % RUN_ID


def _run_prefix(name, network):
    """Shared head of every ``podman run``: never pull, owned label, hardened."""
    return [
        "podman", "run", "-d",
        "--pull=never",
        "--name", name,
        "--label", run_label(),
        "--network", network,
        "--read-only",
        *HARDENING_ARGS,
        "--security-opt", "label=disable",
    ]


def router_run_args(*, name, network, image, config_path, secrets_dir, data_dir,
                    user="1000:1000"):
    """Argv for the isolated router container.

    Deliberately contains NO -p/--publish (no host port) and NO tls mount.
    ``image`` is the immutable image Id, never a mutable tag.
    """
    return _run_prefix(name, network) + [
        "--network-alias", "localrouter",
        "--tmpfs", "/tmp:rw,mode=1777",
        "--user", user,
        "--userns", "keep-id:uid=1000,gid=1000",
        "-v", "%s:/etc/localrouter/config.yaml:ro" % config_path,
        "-v", "%s:/run/secrets:ro" % secrets_dir,
        "-v", "%s:/var/lib/localrouter:rw" % data_dir,
        image,
    ]


def nginx_run_args(*, name, network, image, conf_path, tls_dir, host_port,
                   user="1000:1000"):
    """Argv for the NGINX TLS edge. Publishes on 127.0.0.1 only.

    Mirrors the shipped quadlet: keep-id maps host uid 1000 <-> container uid
    1000 so the read-only, operator-owned 0600 tls.key stays readable; the two
    tmpfs mounts (pid + cache) are the only writable paths.
    """
    return _run_prefix(name, network) + [
        "-p", "127.0.0.1:%d:%d" % (host_port, NGINX_TLS_PORT),
        "--tmpfs", "/tmp:rw,mode=1777",
        "--tmpfs", "/var/cache/nginx:rw,mode=1777",
        "--user", user,
        "--userns", "keep-id:uid=1000,gid=1000",
        "-v", "%s:/etc/nginx/conf.d/default.conf:ro" % conf_path,
        "-v", "%s:/etc/nginx/tls:ro" % tls_dir,
        image,
    ]


def provider_run_args(*, name, network, image, script_path):
    """Argv for the throwaway fixture provider (internal bridge only)."""
    return _run_prefix(name, network) + [
        "--network-alias", "fixture",
        "--tmpfs", "/tmp:rw,mode=1777",
        "--user", PROVIDER_USER,
        "-v", "%s:/app/provider.py:ro" % script_path,
        image,
        "python3", "/app/provider.py", str(PROVIDER_PORT),
    ]


def network_create_args(name):
    """Argv for the run's private bridge: ``--internal`` (no egress route)."""
    return ["podman", "network", "create", "--internal", "--label", run_label(), name]


def build_args(*, context, tag, iidfile=None, jobs=2, memory="6g", cpus=2,
               cpu_flag=None, allow_network=False, no_cache=False, version=None):
    """Argv for the image build.

    ``--pull=never`` is unconditional. ``--network=none`` is the default and is
    dropped ONLY when ``allow_network`` (``--allow-build-network``) is set.
    """
    args = ["podman", "build", "--pull=never"]
    if not allow_network:
        args.append("--network=none")
    if no_cache:
        # --layers=false: no intermediate layer images are committed, so a
        # no-cache build adds nothing to the shared cache beyond the final image.
        args += ["--no-cache", "--layers=false"]
    args += ["--jobs", str(jobs), "--memory", memory]
    if cpus:
        if cpu_flag == "--cpus":
            args += ["--cpus", str(cpus)]
        elif cpu_flag == "--cpu-quota":
            args += ["--cpu-quota", str(int(cpus) * 100000), "--cpu-period", "100000"]
    if version:
        args += ["--build-arg", "VERSION=%s" % version]
    if iidfile:
        args += ["--iidfile", iidfile]
    args += ["-t", tag, context]
    return args


def detect_build_cpu_flag(*, deadline):
    """Pick a real CPU-cap flag: podman has --cpu-quota, not --cpus."""
    p = run(["podman", "build", "--help"], timeout=30, deadline=deadline)
    txt = (p.stdout or "") + (p.stderr or "")
    if "--cpu-quota" in txt:
        return "--cpu-quota"
    if re.search(r"^\s+--cpus\b", txt, re.M):
        return "--cpus"
    return None


# ---------------------------------------------------------------------------
# Inspect-derived assertions (pure, unit-tested)
# ---------------------------------------------------------------------------


def published_ports(inspect_json: dict):
    """Return [(container_port, host_ip, host_port)] for real host bindings."""
    ports = (inspect_json.get("NetworkSettings") or {}).get("Ports") or {}
    out = []
    for cport, binds in ports.items():
        for b in (binds or []):
            if b.get("HostPort"):
                out.append((cport, b.get("HostIp", ""), b.get("HostPort", "")))
    return out


def assert_router_no_host_port(inspect_json: dict) -> None:
    pub = published_ports(inspect_json)
    if pub:
        raise AcceptanceError("router publishes host port(s): %r" % (pub,))


def assert_router_mounts(inspect_json: dict) -> None:
    for m in inspect_json.get("Mounts") or []:
        dst = m.get("Destination") or m.get("destination") or ""
        if dst in ALLOWED_ROUTER_MOUNT_DESTS:
            continue
        if dst in ROUTER_MOUNT_IGNORE_DESTS:
            continue
        raise AcceptanceError("router has unexpected writable/foreign mount: %s" % dst)
    blob = json.dumps(inspect_json.get("Mounts") or []).lower()
    for bad in ("/etc/nginx/tls", "tls.crt", "tls.key"):
        if bad in blob:
            raise AcceptanceError("router mounts TLS material (%s)" % bad)


def _assert_nonroot(user: str, what: str) -> str:
    user = (user or "").strip()
    if user == "" or user.split(":")[0] in ("0", "root"):
        raise AcceptanceError("%s is not a non-root uid: %r" % (what, user))
    return user


def assert_router_user(inspect_json: dict) -> str:
    """The container's effective Config.User (reflects the --user override)."""
    cfg = inspect_json.get("Config") or {}
    return _assert_nonroot(str(cfg.get("User") or cfg.get("user") or ""),
                           "router container user")


def assert_image_user(user: str) -> str:
    """The image's own USER, before any --user override, must be non-root."""
    return _assert_nonroot(user, "image USER")


def assert_readonly_rootfs(inspect_json: dict) -> None:
    hc = inspect_json.get("HostConfig") or {}
    if hc.get("ReadonlyRootfs") is not True:
        raise AcceptanceError("router root filesystem is not read-only")


def hardening_problems(inspect_json: dict):
    """Problems with read-only rootfs / no capabilities / no-new-privileges."""
    hc = inspect_json.get("HostConfig") or {}
    problems = []
    if hc.get("ReadonlyRootfs") is not True:
        problems.append("rootfs not read-only")
    if "EffectiveCaps" not in inspect_json or inspect_json.get("EffectiveCaps"):
        problems.append("effective caps not empty: %r" % (inspect_json.get("EffectiveCaps"),))
    if hc.get("CapAdd"):
        problems.append("caps added: %r" % (hc.get("CapAdd"),))
    if "no-new-privileges" not in [str(o).split(":")[0].split("=")[0]
                                   for o in hc.get("SecurityOpt") or []]:
        problems.append("no-new-privileges not set")
    return problems


def has_default_route(proc_net_route: str) -> bool:
    """True when /proc/net/route contains a default (0.0.0.0/0) route."""
    for line in proc_net_route.splitlines()[1:]:
        cols = line.split()
        if len(cols) >= 8 and cols[1] == "00000000" and cols[7] == "00000000":
            return True
    return False


def assert_nginx_loopback_only(inspect_json: dict, expect_port: int) -> None:
    pub = published_ports(inspect_json)
    if len(pub) != 1:
        raise AcceptanceError("nginx must publish exactly one port, got %r" % (pub,))
    cport, hip, hport = pub[0]
    if hip not in ("127.0.0.1", "localhost"):
        raise AcceptanceError("nginx published on non-loopback host ip %r" % hip)
    if int(hport) != expect_port:
        raise AcceptanceError("nginx published unexpected host port %r" % hport)
    if int(hport) in FORBIDDEN_HOST_PORTS:
        raise AcceptanceError("nginx published a forbidden host port %r" % hport)


# ---------------------------------------------------------------------------
# Small pure helpers (unit-tested)
# ---------------------------------------------------------------------------


def parse_readyz(body) -> bool:
    text = body.decode("utf-8") if isinstance(body, (bytes, bytearray)) else body
    data = json.loads(text)
    if not isinstance(data, dict) or "ready" not in data:
        raise AcceptanceError("readyz body has no 'ready' field: %r" % text[:200])
    return bool(data["ready"])


def free_port() -> int:
    for _ in range(32):
        with contextlib.closing(socket.socket(socket.AF_INET, socket.SOCK_STREAM)) as s:
            s.bind(("127.0.0.1", 0))
            port = s.getsockname()[1]
        if port not in FORBIDDEN_HOST_PORTS and port >= 1024:
            return port
    raise AcceptanceError("could not find a free non-forbidden host port")


def _split_host(hostport: str):
    """Mirror internal/app/hostguard.go parseHost()."""
    host = (hostport or "").strip()
    if not host:
        return None, ""
    if host.startswith("["):
        end = host.find("]")
        if end != -1:
            host = host[1:end]
    elif host.count(":") == 1:
        host = host.rsplit(":", 1)[0]
    host = host.strip().lower()
    try:
        ip = ipaddress.ip_address(host)
        return ip, ""
    except ValueError:
        pass
    if host.endswith("."):
        host = host[:-1]
    return None, host


def host_allowed(host_header: str, allowed) -> bool:
    """Mirror internal/app/hostguard.go HostGuard() decision."""
    names = set()
    ips = set()
    for a in allowed or []:
        ip, name = _split_host(a)
        if ip is not None:
            ips.add(str(ip))
        elif name:
            names.add(name)
    ip, name = _split_host(host_header)
    if ip is not None:
        return ip.is_loopback or str(ip) in ips
    return bool(name) and (name == "localhost" or name in names)


def sanitize(text: str, secret_values) -> str:
    """Redact throwaway fixture secrets from anything we persist."""
    for s in secret_values or []:
        if s and len(s) >= 8:
            text = text.replace(s, "[REDACTED]")
    return text


# ---------------------------------------------------------------------------
# Fixture / config rendering
# ---------------------------------------------------------------------------

ROUTER_CONFIG_TEMPLATE = string.Template(
    """# Throwaway chunk5 acceptance config (generated; not shipped).
listen: 0.0.0.0:8787
allow_non_loopback: true
allowed_hosts: [$allowed_hosts]
data_dir: /var/lib/localrouter
control:
  require_auth: true
quota:
  poll_interval: 5m
policy:
  stale_after: 10m
  safety_margin: 0.02
  inflight_estimate: 0.01
  max_failovers: 2
timeouts:
  header: 10s
  body: 30s
  idle: 120s
  shutdown: 30s
limits:
  max_concurrent: 8
clients:
  - { name: acc-interactive, class: interactive, host: client, key_file: /run/secrets/client-interactive.key }
  - { name: acc-background, class: background, host: worker, key_file: /run/secrets/client-background.key }
accounts:
  - id: acc-fixture
    provider: openai_compat
    base_url: $base_url
    api_key_file: /run/secrets/provider.key
    reserve: {}
routes:
  - name: acc-fixture
    models: [$models]
    interactive: [acc-fixture]
    background: [acc-fixture]
claude_logs:
  enabled: false
hermes_logs:
  enabled: false
"""
)

# A real, tiny streaming provider: no external calls, no dependencies.
FIXTURE_PROVIDER_SOURCE = '''\
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
GAP = float(sys.argv[2]) if len(sys.argv) > 2 else 3.0

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        if self.path.rstrip("/").endswith("healthz"):
            body = b"ok"
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            req = json.loads(raw or b"{}")
        except Exception:
            req = {}
        model = req.get("model", "fixture-model")
        if req.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "keep-alive")
            self.end_headers()
            for i, piece in enumerate(("alpha", "beta", "gamma")):
                obj = {"id": "fixture-1", "object": "chat.completion.chunk",
                       "model": model,
                       "choices": [{"index": 0, "delta": {"content": piece},
                                    "finish_reason": None}]}
                self.wfile.write(("data: %s\\n\\n" % json.dumps(obj)).encode())
                self.wfile.flush()
                time.sleep(GAP if i == 0 else 0.15)
            self.wfile.write(b"data: [DONE]\\n\\n")
            self.wfile.flush()
            return
        obj = {"id": "fixture-1", "object": "chat.completion", "model": model,
               "choices": [{"index": 0, "finish_reason": "stop",
                            "message": {"role": "assistant", "content": "fixture-ok"}}]}
        body = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
'''


def render_router_config(*, allowed_hosts, base_url=FIXTURE_BASE_URL,
                         models=(FIXTURE_MODEL,)):
    return ROUTER_CONFIG_TEMPLATE.substitute(
        allowed_hosts=", ".join(allowed_hosts),
        base_url=base_url,
        models=", ".join(models),
    )


# ---------------------------------------------------------------------------
# Owned resources + cleanup
# ---------------------------------------------------------------------------


class Owned:
    """Audit list of every resource this run created, by immutable ID.

    ``created`` is append-only: cleanup never clears it, so the report always
    shows what was owned. What cleanup could not confirm removed is returned
    separately as the remaining-resource list.
    """

    ORDER = ("container", "network", "image")

    def __init__(self):
        self.created = []

    def add(self, kind: str, rid: str, name: str, *, preexisting=False) -> str:
        if any(e["kind"] == kind and e["id"] == rid for e in self.created):
            return rid
        entry = {"kind": kind, "id": rid, "name": name}
        if kind == "image":
            entry["preexisting"] = bool(preexisting)
        self.created.append(entry)
        return rid

    def ids(self, kind: str):
        return [e["id"] for e in self.created if e["kind"] == kind]

    @staticmethod
    def remove_call(e):
        if e["kind"] == "container":
            return ["podman", "rm", "-f", "-t", "5", e["id"]]
        if e["kind"] == "network":
            # No -f: a network a foreign container joined is left alone.
            return ["podman", "network", "rm", e["id"]]
        if e.get("preexisting"):
            # The Id existed before this run: only our tag is ours.
            return ["podman", "untag", e["id"], e["name"]]
        return ["podman", "rmi", e["id"]]

    @staticmethod
    def exists_call(e):
        if e["kind"] == "container":
            return ["podman", "container", "exists", e["id"]]
        if e["kind"] == "network":
            return ["podman", "network", "exists", e["id"]]
        return ["podman", "image", "exists", e["name"] if e.get("preexisting") else e["id"]]

    def _ordered(self):
        return [e for kind in self.ORDER
                for e in reversed([x for x in self.created if x["kind"] == kind])]

    def cleanup_calls(self):
        """The exact argv lists cleanup will run: containers, networks, images;
        newest first within each kind."""
        return [self.remove_call(e) for e in self._ordered()]

    def cleanup(self, log, deadline):
        """Remove owned resources within ``deadline``; return what remains.

        A resource only counts as gone when ``exists`` answers rc=1; anything
        unverified (error, timeout, exhausted budget) is reported as remaining.
        """
        for e in self._ordered():
            cmd = self.remove_call(e)
            try:
                if e["kind"] == "container":
                    with contextlib.suppress(Exception):
                        log = _capture_logs(e["id"], log, deadline=deadline)
                p = run(cmd, timeout=30, deadline=deadline)
                log.append("cleanup: %s -> rc=%d %s" % (" ".join(cmd), p.returncode,
                                                         p.stderr.strip()))
            except (AcceptanceError, subprocess.TimeoutExpired, OSError) as exc:
                log.append("cleanup: %s -> NOT RUN/INCOMPLETE: %s" % (" ".join(cmd), exc))
        remaining = []
        for e in self.created:
            gone = False
            try:
                gone = run(self.exists_call(e), timeout=15,
                           deadline=deadline).returncode == 1
            except (AcceptanceError, subprocess.TimeoutExpired, OSError) as exc:
                log.append("cleanup: could not verify %s %s: %s" % (e["kind"], e["id"], exc))
            if not gone:
                remaining.append(dict(e))
        return remaining


def _capture_logs(ref: str, log, *, deadline):
    p = run(["podman", "logs", ref], timeout=30, deadline=deadline)
    log.append("---- logs %s (rc=%d) ----\n%s\n%s" % (
        ref, p.returncode, p.stdout, p.stderr))
    return log


# ---------------------------------------------------------------------------
# HTTP probes (real sockets to the isolated edge)
# ---------------------------------------------------------------------------


def _tls_context():
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


def probe_timeout(deadline, cap) -> float:
    """Clamp a socket timeout to the remaining run budget."""
    remaining = deadline.remaining
    if remaining <= 0:
        raise AcceptanceError("max runtime budget exhausted before HTTPS probe")
    return min(float(cap), remaining)


def https_request(port, method, path, *, deadline, host=KNOWN_HOST, key=None,
                  body=None, headers=None, timeout=15.0):
    conn = http.client.HTTPSConnection(
        "127.0.0.1", port, context=_tls_context(),
        timeout=probe_timeout(deadline, timeout),
    )
    hdrs = {"Host": host}
    if key:
        hdrs["Authorization"] = "Bearer %s" % key
    if body is not None:
        hdrs["Content-Type"] = "application/json"
    if headers:
        hdrs.update(headers)
    try:
        conn.request(method, path, body=body, headers=hdrs)
        resp = conn.getresponse()
        data = resp.read()
        return resp.status, data, dict(resp.getheaders())
    finally:
        with contextlib.suppress(Exception):
            conn.close()


def https_status(port, path, *, deadline, **kw):
    status, _data, _h = https_request(port, "GET", path, deadline=deadline, **kw)
    return status


def wait_https_ready(port, deadline, *, timeout=45.0):
    end = time.monotonic() + min(timeout, deadline.remaining)
    last = None
    while time.monotonic() < end:
        deadline.check()
        try:
            status, data, _h = https_request(port, "GET", "/readyz", host=KNOWN_HOST,
                                             deadline=deadline, timeout=5.0)
            if status == 200:
                return parse_readyz(data)
            last = "status=%s" % status
        except AcceptanceError:
            raise
        except Exception as exc:  # noqa: BLE001 - transient during startup
            last = repr(exc)
        time.sleep(0.5)
    raise AcceptanceError("edge never became ready over HTTPS: %s" % last)


def sse_probe(port, key, model=FIXTURE_MODEL, *, deadline, host=KNOWN_HOST,
              timeout=30.0):
    """POST a streaming request and timestamp each content chunk as it lands.

    Every socket read is clamped to the remaining run budget, so a stream that
    never ends cannot outlive the deadline.
    """
    payload = json.dumps({
        "model": model,
        "stream": True,
        "messages": [{"role": "user", "content": "hi"}],
    })
    conn = http.client.HTTPSConnection(
        "127.0.0.1", port, context=_tls_context(),
        timeout=probe_timeout(deadline, timeout),
    )
    try:
        conn.request("POST", "/v1/chat/completions", body=payload, headers={
            "Host": host,
            "Authorization": "Bearer %s" % key,
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        })
        sock = conn.sock
        resp = conn.getresponse()
        if resp.status != 200:
            return {"status": resp.status, "chunks": [], "arrivals": [], "spread": 0.0}
        arrivals, chunks = [], []
        while True:
            if sock is not None:
                sock.settimeout(probe_timeout(deadline, timeout))
            elif deadline.remaining <= 0:
                raise AcceptanceError("max runtime budget exhausted during SSE probe")
            line = resp.readline()
            if not line:
                break
            text = line.decode("utf-8", "replace").rstrip("\r\n")
            if not text.startswith("data:"):
                continue
            payload_line = text[5:].strip()
            if payload_line == "[DONE]":
                break
            arrivals.append(time.monotonic())
            with contextlib.suppress(Exception):
                chunks.append(json.loads(payload_line))
        spread = (arrivals[-1] - arrivals[0]) if len(arrivals) > 1 else 0.0
        return {"status": 200, "chunks": chunks, "arrivals": arrivals,
                "spread": spread}
    finally:
        with contextlib.suppress(Exception):
            conn.close()


# ---------------------------------------------------------------------------
# TLS material
# ---------------------------------------------------------------------------


def generate_self_signed(tls_dir, common_name=KNOWN_HOST, *, deadline):
    os.makedirs(tls_dir, exist_ok=True)
    crt = os.path.join(tls_dir, "tls.crt")
    key = os.path.join(tls_dir, "tls.key")
    cmd = [
        "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
        "-keyout", key, "-out", crt, "-days", "1",
        "-subj", "/CN=%s" % common_name,
        "-addext", "subjectAltName=DNS:%s,IP:127.0.0.1" % common_name,
    ]
    p = run(cmd, timeout=60, deadline=deadline)
    if p.returncode != 0 or not (os.path.exists(crt) and os.path.exists(key)):
        raise AcceptanceError("openssl self-signed generation failed: %s" % p.stderr.strip())
    os.chmod(key, 0o600)
    return crt, key


# ---------------------------------------------------------------------------
# Harness
# ---------------------------------------------------------------------------


class Harness:
    def __init__(self, args):
        self.args = args
        self.deadline = Deadline(args.max_runtime)
        self.owned = Owned()
        self.remaining = []
        self.run_dir = os.path.join(args.artifacts, RUN_ID)
        self.repo = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
        self.checks = []
        self.commands = []
        self.limitations = []
        self.executed_stages = []
        self.report = {}
        self.router_key = pysecrets.token_urlsafe(32)
        self.provider_key = pysecrets.token_urlsafe(32)
        self.background_key = pysecrets.token_urlsafe(32)
        self.secrets_used = [self.router_key, self.provider_key, self.background_key]
        self.network = None
        self.router_container = None
        self.nginx_container = None
        self.provider_container = None
        self.host_port = None
        self.context_dir = None
        self.source = None
        self.edge_config = None
        # Build-image adoption state (set once ``podman build`` is launched).
        self.build_tag = None
        self.build_iidfile = None
        self.preexisting_images = None
        self.unadopted = []
        self.build_info = {
            "network": "enabled" if args.allow_build_network else "none",
            "pull": "never",
            "layer_cache": "disabled" if args.no_cache else "enabled",
        }
        self.built_image = None
        self.built_image_id = None
        self.built_image_metadata = None
        self.cached_image_id = None
        self.cached_smoke = False
        self.cleanup_elapsed = None
        self.run_log = []

    # -- reporting ---------------------------------------------------------
    def check(self, name, ok, detail="", severity="gate"):
        """Record a check. severity='gate' decides the outcome; 'limit' does
        not gate the acceptance but is surfaced in limitations/report."""
        self.checks.append({"name": name, "ok": bool(ok), "detail": detail,
                            "severity": severity})
        return ok

    def check_result(self, name):
        for c in reversed(self.checks):
            if c["name"] == name:
                return c["ok"]
        return None

    def stage(self, name):
        self.executed_stages.append(name)

    # -- setup -------------------------------------------------------------
    def setup_dirs(self):
        os.makedirs(self.run_dir, exist_ok=True)
        for sub in ("config", "secrets", "data", "tls", "fixture"):
            os.makedirs(os.path.join(self.run_dir, sub), exist_ok=True)
        return self.run_dir

    def write_fixtures(self):
        cfg = render_router_config(allowed_hosts=[KNOWN_HOST, "localhost"])
        path = os.path.join(self.run_dir, "config", "config.yaml")
        with open(path, "w") as fh:
            fh.write(cfg)
        secrets_dir = os.path.join(self.run_dir, "secrets")
        for fname, value in (
            ("client-interactive.key", self.router_key),
            ("client-background.key", self.background_key),
            ("provider.key", self.provider_key),
        ):
            fp = os.path.join(secrets_dir, fname)
            with open(fp, "w") as fh:
                fh.write(value + "\n")
            os.chmod(fp, 0o600)
        script = os.path.join(self.run_dir, "fixture", "provider.py")
        with open(script, "w") as fh:
            fh.write(FIXTURE_PROVIDER_SOURCE)
        os.chmod(script, 0o644)   # read by the fixture's non-root uid
        return path, secrets_dir, script

    def write_tls(self):
        crt, key = generate_self_signed(os.path.join(self.run_dir, "tls"),
                                        deadline=self.deadline)
        with open(key) as fh:
            # Defence in depth: never persist the throwaway key body anywhere.
            self.secrets_used += [ln.strip() for ln in fh
                                  if ln.strip() and not ln.startswith("-----")]
        return crt

    # -- stages ------------------------------------------------------------
    def stage_host_preflight(self):
        """Rootless + cached-image preflight, BEFORE any build or container."""
        self.stage("host-preflight")
        rootless = self.check("preflight.podman_rootless",
                              podman_rootless(deadline=self.deadline), "podman rootless")
        if not rootless:
            raise AcceptanceError("refusing to run: podman is not rootless")
        images = []
        if self.args.build:
            images += [("preflight.build_base_image", i) for i in BUILD_BASE_IMAGES]
        else:
            images.append(("preflight.router_image", self.args.image))
        images += [("preflight.nginx_image", self.args.nginx_image),
                   ("preflight.provider_image", self.args.provider_image)]
        missing = []
        for name, image in images:
            if not self.check(name, image_exists(image, deadline=self.deadline),
                              "%s cached locally (never pulled)" % image):
                missing.append(image)
        if not self.check("preflight.openssl", shutil.which("openssl") is not None,
                          "openssl"):
            missing.append("openssl")
        if missing:
            raise AcceptanceError("preflight failed: not available locally: %s"
                                  % ", ".join(missing))

    def stage_source(self):
        """Export git HEAD as the build context and record its provenance."""
        self.stage("source")
        prov = git_provenance(self.repo, deadline=self.deadline)
        ctx = export_head_context(self.repo, os.path.join(self.run_dir, "context"),
                                  deadline=self.deadline, commit=prov["head"])
        self.source = dict(prov, **ctx)
        self.source["built_from"] = "git HEAD export (committed tree only)"
        self.context_dir = ctx["context_dir"]
        self.check("source.head_exported", True,
                   "head=%s tree=%s context_sha256=%s containerfile_sha256=%s" % (
                       prov["head"], prov["tree"], ctx["context_sha256"],
                       ctx["containerfile_sha256"]))
        if prov["worktree_dirty"]:
            self.limitations.append(
                "worktree has %d uncommitted/untracked entr%s; the image is built "
                "from HEAD %s only, so those changes are NOT in the image and are "
                "not accepted by this run" % (
                    prov["dirty_entries"], "y" if prov["dirty_entries"] == 1 else "ies",
                    prov["head"][:12]))

    def _build_check_name(self):
        return ("build.dependency_network_opt_in" if self.args.allow_build_network
                else "build.offline_nonetwork")

    def stage_build(self):
        """Build the exported HEAD context -- fail-closed.

        On failure this aborts the run: no containers, and NEVER a cached-image
        fallback. On success the image Id from ``--iidfile`` is recorded, the
        image is registered as owned (or only its tag, when an identical image
        already existed) and the router will be started from that Id.
        """
        self.stage("build")
        tag = "localhost/localrouter-acc:%s" % RUN_ID
        iidfile = os.path.join(self.run_dir, "image.iid")
        cpu_flag = detect_build_cpu_flag(deadline=self.deadline)
        if cpu_flag is None:
            self.limitations.append(
                "podman build exposes no --cpus/--cpu-quota; CPU cap not applied"
            )
        version = (self.source or {}).get("describe")
        argv = build_args(context=self.context_dir, tag=tag, iidfile=iidfile, jobs=2,
                          memory="6g", cpus=2, cpu_flag=cpu_flag,
                          allow_network=self.args.allow_build_network,
                          no_cache=self.args.no_cache, version=version)
        self.build_info.update({"tag": tag, "cpu_flag": cpu_flag, "version_arg": version,
                                "argv": argv})
        if self.args.allow_build_network:
            self.limitations.append(
                "--allow-build-network: the build's RUN steps had network access "
                "for build-time apk/Go dependency downloads (base images still "
                "--pull=never; runtime stays on the --internal bridge)")
        if not self.args.no_cache:
            self.limitations.append(
                "layer cache enabled: a step is reused when its instruction and "
                "inputs match, so dependency layers (apk add, go mod download) may "
                "come from an earlier build; use --no-cache to re-run every step")
        preexisting = self._preexisting_image_ids()
        self._assert_tag_absent(tag)
        # From here on the tag and iidfile are ours: cleanup adopts whatever
        # the build leaves behind, even if it fails or is killed.
        self.preexisting_images = preexisting
        self.build_tag = tag
        self.build_iidfile = iidfile
        self.commands.append(" ".join(argv))
        log_path = os.path.join(self.run_dir, "build.log")
        check_name = self._build_check_name()
        # buildah stages scratch under TMPDIR and can leave it behind when a RUN
        # step fails; keep that inside this run's directory.
        build_tmp = os.path.join(self.run_dir, "build-tmp")
        os.makedirs(build_tmp, exist_ok=True)
        env = dict(os.environ, TMPDIR=build_tmp)
        try:
            p = run(argv, timeout=self.args.build_timeout, deadline=self.deadline,
                    env=env)
            out = p.stdout + "\n" + p.stderr
            rc = p.returncode
        except subprocess.TimeoutExpired as exc:
            out = "BUILD TIMEOUT (process group killed): %r" % exc
            rc = 124
        with open(log_path, "w") as fh:
            fh.write(sanitize(out, self.secrets_used))

        if rc != 0:
            # FAIL CLOSED. A cached image is NOT a substitute for a build of the
            # current source, and no container is ever started.
            self.built_image = None
            self.built_image_id = None
            tail = out.strip().splitlines()[-6:]
            self.check(check_name, False,
                       "rc=%d (fail-closed; no cached-image fallback, no "
                       "containers started)" % rc, severity="gate")
            self.limitations.append(
                "build (--pull=never, network=%s) FAILED; refusing a cached-image "
                "fallback and starting no containers (fail-closed)"
                % self.build_info["network"]
            )
            self.limitations.append(
                "build.log tail: " + sanitize(" | ".join(tail), self.secrets_used))
            raise AcceptanceError(
                "build FAILED rc=%d; refusing cached-image fallback "
                "(fail-closed, no router containers started)" % rc)

        iid = ""
        with contextlib.suppress(OSError):
            with open(iidfile) as fh:
                iid = norm_id(fh.read())
        tag_id = image_id(tag, deadline=self.deadline)
        if tag_id:
            # Own what exists even if it then fails the gate, so it is cleaned.
            self.owned.add("image", tag_id, tag, preexisting=tag_id in preexisting)
        if not iid or iid != tag_id:
            self.check("build.image_id_recorded", False,
                       "iidfile=%r tag=%s id=%r" % (iid, tag, tag_id), severity="gate")
            raise AcceptanceError("built image %s has no consistent immutable Id" % tag)
        self.built_image = tag
        self.built_image_id = iid
        self.built_image_metadata = image_metadata(iid, deadline=self.deadline)
        self.check(check_name, True, "rc=0 tag=%s id=%s network=%s" % (
            tag, iid, self.build_info["network"]), severity="gate")
        self.check("build.image_id_recorded", True, "immutable_id=%s" % iid,
                   severity="gate")
        return True

    def _preexisting_image_ids(self):
        """Every image Id present BEFORE the build -- fail-closed.

        This set is what keeps cleanup from ``rmi``-ing a shared Id, so an
        unusable listing (error, unparsable, or empty although the preflight
        just saw the base images) refuses the build instead of guessing.
        """
        p = run(["podman", "images", "-a", "-q", "--no-trunc"], timeout=30,
                deadline=self.deadline)
        ids = p.stdout.split() if p.returncode == 0 else []
        if not ids or not all(_IMAGE_ID_RE.match(norm_id(x)) for x in ids):
            raise AcceptanceError(
                "cannot list pre-existing images (rc=%d %r); refusing to build "
                "because a shared image Id could not be told apart from ours"
                % (p.returncode, (p.stderr or p.stdout).strip()[:200]))
        return {norm_id(x) for x in ids}

    def _assert_tag_absent(self, tag):
        """The per-run tag must not exist yet, so whatever holds it later is ours."""
        p = run(["podman", "image", "exists", tag], timeout=30, deadline=self.deadline)
        if p.returncode != 1:
            raise AcceptanceError("per-run tag %s already exists or is unverifiable "
                                  "(rc=%d); refusing to build" % (tag, p.returncode))

    def adopt_build_image(self, deadline):
        """Own what a launched build left behind, success or not (idempotent).

        The per-run tag was proven absent before the build, so an image under
        it is ours to remove -- by ``rmi`` when its Id is new, by ``untag``
        only when that Id pre-existed. An ``--iidfile`` Id without the tag is
        owned only when it is new. Anything unresolvable is reported remaining.
        """
        if self.build_tag is None or self.preexisting_images is None:
            return
        tag, pre = self.build_tag, self.preexisting_images
        p = run(["podman", "image", "exists", tag], timeout=30, deadline=deadline)
        if p.returncode == 0:
            tid = image_id(tag, deadline=deadline)
            if _IMAGE_ID_RE.match(tid):
                self.owned.add("image", tid, tag, preexisting=tid in pre)
            else:
                self.unadopted.append({"kind": "image", "id": tid or "<unresolved>",
                                       "name": tag})
        elif p.returncode != 1:
            self.unadopted.append({"kind": "image", "id": "<unverified>", "name": tag})
        iid = ""
        with contextlib.suppress(OSError):
            with open(self.build_iidfile) as fh:
                iid = norm_id(fh.read())
        if not _IMAGE_ID_RE.match(iid) or iid in pre or iid in self.owned.ids("image"):
            return
        q = run(["podman", "image", "exists", iid], timeout=30, deadline=deadline)
        if q.returncode != 1:
            self.owned.add("image", iid, tag)

    def resolve_cached_image(self):
        """--no-build: pin the cached image to its immutable Id up front."""
        self.cached_image_id = image_id(self.args.image, deadline=self.deadline)
        if not self.cached_image_id:
            raise AcceptanceError("cached image %s is not inspectable" % self.args.image)

    def stage_network(self):
        self.stage("network")
        name = unique_network_name()
        argv = network_create_args(name)
        self.commands.append(" ".join(argv))
        p = run(argv, timeout=60, deadline=self.deadline)
        if p.returncode != 0:
            raise AcceptanceError("network create failed: %s" % p.stderr.strip())
        q = run(["podman", "network", "inspect", "--format", "{{.ID}}|{{.Internal}}", name],
                timeout=30, deadline=self.deadline)
        nid, _sep, internal = q.stdout.strip().partition("|")
        if q.returncode != 0 or not nid:
            raise AcceptanceError("network %s is not inspectable: %s" % (name, q.stderr.strip()))
        self.owned.add("network", nid, name)
        self.network = name
        if not self.check("network.internal", internal.strip() == "true",
                          "network=%s internal=%s" % (name, internal)):
            raise AcceptanceError("runtime network %s is not --internal" % name)
        return name

    def router_image(self):
        """The image under test as an immutable Id; None if none is known."""
        return self.built_image_id or self.cached_image_id

    def _start(self, role, argv):
        """Start a container; register its ID as owned only once it exists."""
        self.commands.append(" ".join(argv))
        p = run(argv, timeout=90, deadline=self.deadline)
        lines = p.stdout.strip().splitlines()
        cid = lines[-1].strip() if p.returncode == 0 and lines else ""
        if not cid:
            raise AcceptanceError("%s start failed: %s" % (role, p.stderr.strip()))
        self.owned.add("container", cid, argv[argv.index("--name") + 1])
        return cid

    def assert_image_user_check(self):
        """The image's own USER must be non-root BEFORE the --user override."""
        image = self.router_image()
        user = image_config_user(image, deadline=self.deadline)
        try:
            assert_image_user(user)
            self.check("router.image_user_nonroot", True,
                       "image %s Config.User=%r" % (image, user))
        except AcceptanceError as exc:
            self.check("router.image_user_nonroot", False, str(exc))
            raise

    def stage_router(self, net, config_path, secrets_dir):
        self.stage("router")
        self.assert_image_user_check()
        data_dir = os.path.join(self.run_dir, "data")
        argv = router_run_args(
            name=unique_name("router"), network=net, image=self.router_image(),
            config_path=config_path, secrets_dir=secrets_dir, data_dir=data_dir,
        )
        self.router_container = self._start("router", argv)
        return self.router_container, data_dir

    def stage_provider(self, net, script_path):
        self.stage("provider")
        argv = provider_run_args(name=unique_name("provider"), network=net,
                                 image=self.args.provider_image, script_path=script_path)
        self.provider_container = self._start("provider", argv)
        return self.provider_container

    def stage_nginx(self, net, conf_path, tls_dir):
        self.stage("nginx")
        self.host_port = free_port()
        argv = nginx_run_args(name=unique_name("nginx"), network=net,
                              image=self.args.nginx_image, conf_path=conf_path,
                              tls_dir=tls_dir, host_port=self.host_port)
        self.nginx_container = self._start("nginx", argv)
        return self.nginx_container

    def exec_in(self, ref, *cmd, timeout=20):
        return run(["podman", "exec", ref, *cmd], timeout=timeout, deadline=self.deadline)

    # -- waits -------------------------------------------------------------
    def wait_router_ready(self):
        end = time.monotonic() + min(60.0, self.deadline.remaining)
        last = None
        while time.monotonic() < end:
            self.deadline.check()
            p = self.exec_in(self.router_container, "wget", "-q", "-O", "-",
                             "http://127.0.0.1:%d/readyz" % ROUTER_CONTAINER_PORT)
            if p.returncode == 0 and p.stdout.strip():
                try:
                    return parse_readyz(p.stdout)
                except AcceptanceError as exc:
                    last = str(exc)
            else:
                last = p.stderr.strip() or "rc=%d" % p.returncode
            time.sleep(0.5)
        raise AcceptanceError("router /readyz never became ready: %s" % last)

    def wait_provider_ready(self):
        end = time.monotonic() + min(45.0, self.deadline.remaining)
        last = None
        while time.monotonic() < end:
            self.deadline.check()
            p = self.exec_in(self.router_container, "wget", "-q", "-O", "-",
                             "http://fixture:%d/healthz" % PROVIDER_PORT)
            if p.returncode == 0 and p.stdout.strip() == "ok":
                return True
            last = (p.stderr.strip() or p.stdout.strip() or "rc=%d" % p.returncode)
            time.sleep(0.5)
        raise AcceptanceError("fixture provider never became ready: %s" % last)

    # -- assertions --------------------------------------------------------
    def inspect(self, ref):
        return inspect_container(ref, deadline=self.deadline)

    def assert_image_identity(self):
        """Prove the RUNNING router is exactly the image this run built.

        A cached-smoke run (--no-build) has no build artifact to match and skips
        this; such a run is reported as cached smoke, never as acceptance.
        """
        if self.built_image is None or self.built_image_id is None:
            return
        ins = inspect_container(self.router_container, deadline=self.deadline)
        self.check("router.image_matches_build",
                   image_identity_ok(ins, self.built_image_id),
                   "running=%s built=%s" % (container_image_id(ins),
                                            self.built_image_id),
                   severity="gate")

    def assert_router_inspect(self):
        if self.router_container is None:
            raise AcceptanceError("router container was never started")
        ins = self.inspect(self.router_container)
        assert_router_no_host_port(ins)
        self.check("router.no_host_port", True, "published=%r" % published_ports(ins))
        try:
            assert_router_mounts(ins)
            self.check("router.mounts_allowlisted", True,
                       "mounts=%r" % [m.get("Destination") for m in ins.get("Mounts") or []])
        except AcceptanceError as exc:
            self.check("router.mounts_allowlisted", False, str(exc))
            raise
        user = assert_router_user(ins)
        self.check("router.nonroot_uid", True,
                   "container Config.User=%s (the --user override; the image's own "
                   "USER is router.image_user_nonroot)" % user)
        assert_readonly_rootfs(ins)
        self.check("router.readonly_rootfs", True, "ReadonlyRootfs=true")
        blob = json.dumps(ins.get("Mounts") or []).lower()
        self.check("router.no_tls_mount",
                   "/etc/nginx/tls" not in blob and "tls.crt" not in blob,
                   "no TLS material mounted into router")
        p = self.exec_in(self.router_container, "id", "-u")
        self.check("router.process_uid", p.returncode == 0 and p.stdout.strip() == "1000",
                   "id -u -> %r" % p.stdout.strip())

    def assert_isolation(self, role, ref):
        """Hardening + internal-bridge-only attachment + no default route."""
        ins = self.inspect(ref)
        problems = hardening_problems(ins)
        self.check("%s.hardened" % role, not problems,
                   "; ".join(problems) or "read-only, no caps, no-new-privileges")
        nets = sorted(((ins.get("NetworkSettings") or {}).get("Networks") or {}).keys())
        self.check("%s.internal_network_only" % role, nets == [self.network],
                   "networks=%r" % nets)
        p = self.exec_in(ref, "cat", "/proc/net/route")
        self.check("%s.no_default_route" % role,
                   p.returncode == 0 and not has_default_route(p.stdout),
                   "rc=%d routes=%r" % (p.returncode, p.stdout.strip().splitlines()[1:]))

    def assert_data_writable_root_immutable(self):
        probe = "/var/lib/localrouter/.acc-write-probe"
        p = self.exec_in(self.router_container, "sh", "-c",
                         "touch %s && echo WROTE" % probe)
        self.check("router.data_dir_writable",
                   p.returncode == 0 and "WROTE" in p.stdout,
                   (p.stdout + p.stderr).strip()[:200])
        q = self.exec_in(self.router_container, "sh", "-c",
                         "touch /acc-root-probe 2>&1 || echo RO_IMMUTABLE")
        self.check("router.root_immutable",
                   "RO_IMMUTABLE" in (q.stdout + q.stderr),
                   (q.stdout + q.stderr).strip()[:200])

    def assert_router_ready_checks(self):
        ready = self.wait_router_ready()
        self.check("router.readyz_ready", ready is True, "ready=%s" % ready)

    def assert_https_surface(self):
        d = self.deadline
        status, data, _h = https_request(self.host_port, "GET", "/readyz",
                                         host=KNOWN_HOST, deadline=d)
        ok = status == 200 and parse_readyz(data) is True
        self.check("edge.readyz_over_tls", ok, "status=%s body=%r" % (status, data[:80]))

        known = https_status(self.host_port, "/readyz", host=KNOWN_HOST, deadline=d)
        self.check("edge.known_host_accepted", known == 200, "status=%d" % known)

        unknown = https_status(self.host_port, "/readyz", host=UNKNOWN_HOST, deadline=d)
        self.check("edge.unknown_host_rejected", unknown == 403, "status=%d" % unknown)

        anon = https_status(self.host_port, "/control/v1/status", host=KNOWN_HOST,
                            deadline=d)
        self.check("edge.auth_required_401", anon == 401, "status=%d" % anon)

        auth = https_status(self.host_port, "/control/v1/status", host=KNOWN_HOST,
                            key=self.router_key, deadline=d)
        self.check("edge.auth_bearer_accepted", auth == 200, "status=%d" % auth)

        bad = https_status(self.host_port, "/control/v1/status", host=KNOWN_HOST,
                           key="not-a-real-key", deadline=d)
        self.check("edge.bad_key_rejected", bad == 401, "status=%d" % bad)

    def assert_nginx_inspect(self):
        if self.nginx_container is None or self.host_port is None:
            raise AcceptanceError("nginx container was never started")
        ins = self.inspect(self.nginx_container)
        assert_nginx_loopback_only(ins, self.host_port)
        self.check("edge.loopback_only_publish", True,
                   "host_port=%d loopback=127.0.0.1" % self.host_port)

    def assert_no_forbidden_host_ports(self):
        p = run(["podman", "ps", "--filter", "label=%s" % run_label(),
                 "--format", "{{.Names}} {{.Ports}}"], timeout=30, deadline=self.deadline)
        leaked = []
        for line in p.stdout.splitlines():
            for forbidden in FORBIDDEN_HOST_PORTS:
                if ("0.0.0.0:%d" % forbidden) in line or (":%d->" % forbidden) in line:
                    leaked.append(line.strip())
        self.check("host.no_forbidden_port_published", p.returncode == 0 and not leaked,
                   "rc=%d leaks=%r" % (p.returncode, leaked))

    def assert_sse_streaming(self):
        try:
            self.wait_provider_ready()
        except AcceptanceError as exc:
            self.check("edge.sse_streaming", False, "provider not ready: %s" % exc)
            self.limitations.append("SSE stage not run: fixture provider not reachable")
            return
        res = sse_probe(self.host_port, self.router_key, deadline=self.deadline)
        n = len(res["chunks"])
        spread = res["spread"]
        self.check("edge.sse_status_200", res["status"] == 200, "status=%s" % res["status"])
        self.check("edge.sse_chunks_streamed", n == 3, "chunks=%d" % n)
        self.check("edge.sse_not_buffered", spread >= 1.0,
                   "arrival spread=%.2fs (buffered would be ~0)" % spread)

    def assert_router_down_readiness(self):
        """Stopping the router must make the edge /readyz non-2xx (real signal)."""
        p = run(["podman", "stop", "-t", "5", self.router_container], timeout=40,
                deadline=self.deadline)
        if p.returncode != 0:
            self.check("edge.readyz_reflects_backend", False,
                       "could not stop router: %s" % p.stderr.strip())
            return
        end = time.monotonic() + min(10.0, self.deadline.remaining)
        status = None
        while time.monotonic() < end:
            try:
                status = https_status(self.host_port, "/readyz", host=KNOWN_HOST,
                                      deadline=self.deadline, timeout=5.0)
            except AcceptanceError:
                raise
            except Exception:  # noqa: BLE001
                status = 502
            if status is not None and status >= 400:
                break
            time.sleep(0.5)
        self.check("edge.readyz_reflects_backend", status is not None and status >= 400,
                   "status=%s after router stop (any >=400 accepted)" % status)
        run(["podman", "start", self.router_container], timeout=40, deadline=self.deadline)

    # -- orchestration -----------------------------------------------------
    def run_all(self):
        self.setup_dirs()
        # Rootless + cached images first: nothing is built on a rootful podman.
        self.stage_host_preflight()
        if self.args.build:
            # Fail-closed: a failed build raises here and NOTHING else runs --
            # no network, no router/edge containers, no fallback.
            self.stage_source()
            self.stage_build()
        else:
            self.cached_smoke = True
            self.stage("build-skipped")
            self.check("build.skipped_cached_smoke", False,
                       "build skipped (--no-build cached smoke mode)",
                       severity="limit")
            self.limitations.append(
                "cached smoke mode (--no-build): exercises cached image %s; this "
                "is NOT release acceptance and cannot prove a fresh source build"
                % self.args.image
            )
            self.resolve_cached_image()

        config_path, secrets_dir, script = self.write_fixtures()
        crt = self.write_tls()
        net = self.stage_network()
        self.stage_router(net, config_path, secrets_dir)
        self.assert_image_identity()
        self.assert_router_inspect()
        self.assert_router_ready_checks()
        self.assert_data_writable_root_immutable()

        self.stage_provider(net, script)
        self.stage_nginx(net, self._nginx_conf_path(), os.path.dirname(crt))
        for role, ref in (("router", self.router_container),
                          ("fixture", self.provider_container),
                          ("edge", self.nginx_container)):
            self.assert_isolation(role, ref)
        self.assert_nginx_inspect()
        wait_https_ready(self.host_port, self.deadline)
        self.assert_https_surface()
        self.assert_sse_streaming()
        self.assert_no_forbidden_host_ports()
        self.assert_router_down_readiness()
        return True

    def adopt_labelled(self, deadline):
        """Add any container/network carrying THIS run's unique label that is
        not yet tracked (e.g. a ``podman run`` killed mid-creation)."""
        label = "label=%s" % run_label()
        for kind, cmd in (
            ("container", ["podman", "ps", "-a", "--no-trunc", "--filter", label,
                           "--format", "{{.ID}} {{.Names}}"]),
            ("network", ["podman", "network", "ls", "--no-trunc", "--filter", label,
                         "--format", "{{.ID}} {{.Name}}"]),
        ):
            p = run(cmd, timeout=30, deadline=deadline)
            for line in p.stdout.splitlines() if p.returncode == 0 else []:
                rid, _sep, name = line.strip().partition(" ")
                if rid:
                    self.owned.add(kind, rid, name)

    def cleanup(self, cleanup_deadline):
        """Remove owned resources on the SEPARATE cleanup budget."""
        try:
            # On the cleanup budget: a timed-out build may have spent the run's.
            self.adopt_build_image(cleanup_deadline)
        except Exception as exc:  # noqa: BLE001
            self.run_log.append("cleanup: build image adoption failed: %s" % exc)
            if self.build_tag is not None:
                self.unadopted.append({"kind": "image", "id": "<unverified>",
                                       "name": self.build_tag})
        if self.args.keep:
            self.remaining = [dict(e) for e in self.owned.created] + self.unadopted
            self.limitations.append("--keep: owned resources deliberately left in place")
            return
        try:
            self.adopt_labelled(cleanup_deadline)
        except Exception as exc:  # noqa: BLE001
            self.run_log.append("cleanup: label sweep failed: %s" % exc)
        try:
            self.remaining = self.owned.cleanup(self.run_log, cleanup_deadline)
        except Exception as exc:  # noqa: BLE001
            self.limitations.append("cleanup problem: %s" % exc)
            self.remaining = [dict(e) for e in self.owned.created]
        self.remaining += self.unadopted
        self.check("cleanup.no_owned_leftovers", not self.remaining,
                   "remaining=%r" % [(e["kind"], e["id"]) for e in self.remaining])

    def _nginx_conf_path(self):
        """The SHIPPED edge config, verbatim, and record what was mounted.

        A build run mounts the copy inside the pinned HEAD export, so a live
        worktree edit (or HEAD move) after the source stage cannot reach the
        edge; a config missing from the export fails closed. Only cached smoke
        (``--no-build``, never acceptance) has no export and reads the worktree.
        """
        rel = os.path.join("packaging", "nginx", "localrouter.conf")
        if self.args.build:
            if self.context_dir is None:
                raise AcceptanceError("no pinned export to mount the edge config from")
            root = os.path.realpath(self.context_dir)
            path = os.path.join(self.context_dir, rel)
            real = os.path.realpath(path)
            if not real.startswith(root + os.sep) or not os.path.isfile(real):
                raise AcceptanceError("edge config %s is not a file inside the "
                                      "pinned export %s" % (rel, self.context_dir))
            origin = "git archive %s" % (self.source or {}).get("head")
        else:
            path = os.path.join(self.repo, rel)
            origin = "live worktree (--no-build cached smoke; not pinned)"
        self.edge_config = {"path": path, "sha256": _sha256_file(path), "source": origin}
        return path


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------


def classify_outcome(checks, *, cached_smoke=False):
    """Decide the run outcome -- fail-closed by construction.

    * a ``--no-build`` cached-smoke run is NEVER release acceptance;
    * any failed ``gate`` check is ``fail``;
    * failed ``limit`` checks only downgrade a real run to
      ``pass_with_limitations``.
    An aborted run (e.g. the build failed) is reported as ``fail`` by the
    caller and never reaches this function.
    """
    if cached_smoke:
        return "cached_smoke_not_release_acceptance"
    if any(not c["ok"] and c.get("severity", "gate") == "gate" for c in checks):
        return "fail"
    if any(not c["ok"] and c.get("severity") == "limit" for c in checks):
        return "pass_with_limitations"
    return "pass"


def build_report(h, *, outcome, error=None):
    passed = sum(1 for c in h.checks if c["ok"])
    ran = h.router_image() if h.router_container else None
    return {
        "run_id": RUN_ID,
        "outcome": outcome,
        "error": error,
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "elapsed_seconds": round(h.deadline.elapsed, 2),
        "max_runtime_seconds": h.deadline.limit,
        "cleanup_elapsed_seconds": h.cleanup_elapsed,
        "cleanup_timeout_seconds": float(h.args.cleanup_timeout),
        "artifacts_dir": h.run_dir,
        "executed_stages": h.executed_stages,
        "checks": h.checks,
        "checks_passed": passed,
        "checks_total": len(h.checks),
        "gate_failures": [c["name"] for c in h.checks
                          if not c["ok"] and c.get("severity", "gate") == "gate"],
        "limitation_failures": [c["name"] for c in h.checks
                                if not c["ok"] and c.get("severity") == "limit"],
        "commands": h.commands,
        "limitations": h.limitations,
        "source": h.source,
        "build": h.build_info,
        "router_image": ran,
        "image_under_test": ran,
        "image_built_artifact": h.built_image_metadata,
        "built_image_id": h.built_image_id,
        "fresh_build_proven": h.built_image is not None,
        "image_matches_build": h.check_result("router.image_matches_build"),
        "cached_smoke_not_release_acceptance": bool(h.cached_smoke),
        "nginx_image": h.args.nginx_image,
        "provider_image": h.args.provider_image,
        "host_port": h.host_port,
        "edge_config": h.edge_config,
        "owned": h.owned.created,
        "remaining_resources": h.remaining,
    }


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description=(__doc__ or "").splitlines()[0])
    ap.add_argument("--artifacts", default=DEFAULT_ARTIFACT_ROOT,
                    help="durable artifact root (a unique run dir is created under it)")
    ap.add_argument("--image", default=ROUTER_IMAGE_DEFAULT,
                    help="cached router image for --no-build smoke mode only")
    ap.add_argument("--nginx-image", default=NGINX_IMAGE_DEFAULT)
    ap.add_argument("--provider-image", default=PROVIDER_IMAGE_DEFAULT)
    ap.add_argument("--max-runtime", type=float, default=DEFAULT_MAX_RUNTIME,
                    help="shared deadline for every command and probe of the run")
    ap.add_argument("--cleanup-timeout", type=float, default=DEFAULT_CLEANUP_TIMEOUT,
                    help="separate budget for teardown after the run")
    ap.add_argument("--build-timeout", type=float, default=150.0)
    ap.add_argument("--build", dest="build", action="store_true", default=True)
    ap.add_argument("--no-build", dest="build", action="store_false",
                    help="explicit cached smoke mode: exercise the cached image "
                         "only; NOT release acceptance and never PASS")
    ap.add_argument("--allow-build-network", action="store_true", default=False,
                    help="explicit opt-in: let the build's RUN steps reach the "
                         "network for build-time apk/Go dependencies. Base images "
                         "are still never pulled (--pull=never) and the runtime "
                         "stays on the --internal fixture-only bridge")
    ap.add_argument("--no-cache", action="store_true", default=False,
                    help="build without reusing cached layers")
    ap.add_argument("--keep", action="store_true",
                    help="do not tear down containers (debugging only)")
    ap.add_argument("--json", action="store_true",
                    help="print the run report as JSON on stdout")
    args = ap.parse_args(argv)
    if not args.build and (args.allow_build_network or args.no_cache):
        ap.error("--allow-build-network/--no-cache need a build (drop --no-build)")
    return args


def main(argv=None):
    args = parse_args(argv)
    if not podman_available():
        print(json.dumps({"outcome": "error", "error": "podman not found"}))
        return 2
    h = Harness(args)
    aborted, error = False, None
    try:
        h.run_all()
    except AcceptanceError as exc:
        aborted, error = True, str(exc)
    except Exception as exc:  # noqa: BLE001
        aborted, error = True, "%s: %s" % (type(exc).__name__, exc)
    finally:
        cleanup_deadline = Deadline(args.cleanup_timeout)
        h.cleanup(cleanup_deadline)
        h.cleanup_elapsed = round(cleanup_deadline.elapsed, 2)

    # Fail-closed outcome: an aborted run is a hard fail; a cached-smoke run is
    # never acceptance; only a completed fresh-build run can pass.
    outcome = "fail" if aborted else classify_outcome(h.checks,
                                                      cached_smoke=h.cached_smoke)
    h.report = build_report(h, outcome=outcome, error=error)
    text = sanitize(json.dumps(h.report, indent=2, sort_keys=True), h.secrets_used)
    os.makedirs(h.run_dir, exist_ok=True)
    with open(os.path.join(h.run_dir, "report.json"), "w") as fh:
        fh.write(text + "\n")
    with open(os.path.join(h.run_dir, "cleanup.log"), "w") as fh:
        fh.write(sanitize("\n".join(h.run_log), h.secrets_used))
    if args.json:
        print(text)
    else:
        print("outcome=%s checks=%d/%d artifacts=%s" % (
            outcome, h.report["checks_passed"], h.report["checks_total"], h.run_dir))
    return 0 if outcome in ("pass", "pass_with_limitations") else 1


if __name__ == "__main__":
    sys.exit(main())
