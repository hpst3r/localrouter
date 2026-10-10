#!/usr/bin/env python3
"""Unit tests for scripts/container-acceptance.py -- stdlib unittest only.

No podman, no network, no pytest import. These pin the safety-critical
*construction* of the acceptance harness and its FAIL-CLOSED behaviour:

  * every ``podman run`` argv carries ``--pull=never``, ``--cap-drop=ALL``,
    ``no-new-privileges``, ``--read-only`` and the per-run ownership label;
    the router argv contains no host port; NGINX publishes loopback-only;
  * the build argv keeps ``--pull=never`` in BOTH modes; ``--network=none`` is
    the default and is dropped ONLY by the explicit ``--allow-build-network``;
  * rootless + cached-image preflight runs BEFORE any build;
  * a failed build is a hard ``gate`` failure that ABORTS the run before any
    router/edge container starts -- there is NO cached-image fallback;
  * the build context is an export of git HEAD with recorded provenance, the
    image Id comes from ``--iidfile`` and the router runs that immutable Id;
  * the image's own ``USER`` is asserted non-root before any ``--user``
    override, and the runtime bridge is ``--internal``;
  * every subprocess and HTTPS probe is bound by the shared run deadline;
    cleanup has its own, separate bound; a timed-out command's whole process
    group is killed;
  * cleanup removes only owned resources BY ID (no forced network removal),
    keeps the audit list intact and reports what remains;
  * the persisted report is sanitized and matches the documented schema.

Run:  python3 -m unittest scripts.tests.test_container_acceptance -v
or:   python3 scripts/tests/test_container_acceptance.py
"""

from __future__ import annotations

import contextlib
import hashlib
import importlib.util
import io
import json
import os
import pathlib
import re
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from contextlib import redirect_stdout
from unittest import mock

SCRIPT = pathlib.Path(__file__).resolve().parents[1] / "container-acceptance.py"
DOC = pathlib.Path(__file__).resolve().parents[2] / "docs" / "CONTAINER-ACCEPTANCE.md"
_spec = importlib.util.spec_from_file_location("container_acceptance", SCRIPT)
assert _spec is not None and _spec.loader is not None
mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mod)
_REAL_RUN = mod.run

BUILT_HEX = "b" * 64
BUILT_ID = "sha256:" + BUILT_HEX
CACHED_HEX = "c" * 64
NET_ID = "e" * 64
ROUTE_NO_DEFAULT = (
    "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
    "eth0\t0000590A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"
)
ROUTE_WITH_DEFAULT = ROUTE_NO_DEFAULT + (
    "eth0\t00000000\t0100590A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
)


# ---------------------------------------------------------------------------
# Fakes -- pure command capture, never a fabricated acceptance result
# ---------------------------------------------------------------------------


class _Proc:
    def __init__(self, returncode=0, stdout="", stderr=""):
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr


def _write_iidfile(cmd, value):
    if "--iidfile" in cmd:
        with open(cmd[cmd.index("--iidfile") + 1], "w") as fh:
            fh.write(value)


BASE_IMAGE_ID = "sha256:" + "a1" * 32   # a cached base image in every listing
TAG_PREFIX = "localhost/localrouter-acc:"


def make_fake_podman(*, build_rc=0, built_id=BUILT_ID, record=None,
                     cpu_help="      --cpu-quota int\n", preexisting=BASE_IMAGE_ID + "\n",
                     images_rc=0, tag_exists_before=False):
    """A fake ``run`` for the stage-level tests. Only *commands* are faked; the
    module's own logic decides every outcome from the (fake) command results."""
    state = {"tagged": tag_exists_before}

    def fake_run(cmd, **kw):
        if record is not None:
            record.append((list(cmd), kw))
        if list(cmd[:3]) == ["podman", "build", "--help"]:
            return _Proc(stdout=cpu_help)
        if list(cmd[:2]) == ["podman", "build"]:
            if build_rc == 0:
                _write_iidfile(cmd, built_id)
                state["tagged"] = True
            return _Proc(returncode=build_rc,
                         stdout="BUILD LOG LINE",
                         stderr="" if build_rc == 0 else "apk: no such package")
        if list(cmd[:2]) == ["podman", "images"]:
            return _Proc(returncode=images_rc, stdout=preexisting,
                         stderr="" if images_rc == 0 else "storage error")
        if list(cmd[:3]) == ["podman", "image", "exists"] and cmd[3].startswith(TAG_PREFIX):
            return _Proc(returncode=0 if state["tagged"] else 1)
        if list(cmd[:3]) == ["podman", "image", "inspect"]:
            fmt = cmd[cmd.index("--format") + 1] if "--format" in cmd else ""
            if fmt == "{{.Id}}":
                hexid = mod.norm_id(built_id)
                return _Proc(returncode=0 if hexid else 125, stdout=hexid)
            if fmt == "{{.Size}}":
                return _Proc(returncode=0, stdout="12345")
            return _Proc(returncode=0, stdout="meta")
        if list(cmd[:2]) == ["podman", "network"]:
            return _Proc(returncode=0)
        return _Proc(returncode=0)

    return fake_run


def new_harness(tmpdir, *extra):
    args = mod.parse_args(["--artifacts", tmpdir, *extra])
    h = mod.Harness(args)
    h.setup_dirs()
    h.context_dir = os.path.join(tmpdir, "ctx")
    os.makedirs(h.context_dir, exist_ok=True)
    return h


def _norm(x):
    x = (x or "").strip().lower()
    return x[7:] if x.startswith("sha256:") else x


class FakeWorld:
    """A stateful fake podman/openssl world for whole-harness orchestration.

    It answers commands the way podman would (IDs from ``run -d``, iidfile
    writes, inspect JSON derived from the argv actually used) and records every
    call and its kwargs, so tests can assert on what the harness *did*.
    """

    def __init__(self, *, rootless="true", build_rc=0, built_id=BUILT_ID,
                 image_user="1000:1000", missing_images=(), preexisting=(BASE_IMAGE_ID,),
                 network_internal="true", network_rm_rc=0, route=ROUTE_NO_DEFAULT,
                 build_stderr="", exec_hook=None, dirty=False, build_timeout=False,
                 failed_tag_id=None, failed_iid=None, real_git=False):
        self.rootless = rootless
        self.build_rc = build_rc
        self.built_id = built_id
        # A failed/killed build may still have committed and tagged an image
        # (failed_tag_id) or written --iidfile (failed_iid) before it died.
        self.build_timeout = build_timeout
        self.failed_tag_id = failed_tag_id
        self.failed_iid = failed_iid
        self.tag_id = None              # what the per-run tag points at, if anything
        self.real_git = real_git        # delegate git to the real run() (real repo)
        self.image_user = image_user
        self.missing = set(missing_images)
        self.preexisting = list(preexisting)
        self.network_internal = network_internal
        self.network_rm_rc = network_rm_rc
        self.route = route
        self.build_stderr = build_stderr
        self.exec_hook = exec_hook
        self.dirty = dirty
        self.calls = []
        self.https_calls = []
        self.sse_calls = []
        self.git_calls = []
        self.export_commits = []
        self.containers = {}
        self.networks = {}
        self.removed = set()
        self.router_stopped = False

    # -- helpers --------------------------------------------------------------
    def _container_by(self, ref):
        if ref in self.containers:
            return self.containers[ref]
        for c in self.containers.values():
            if c["name"] == ref:
                return c
        return None

    def _inspect_json(self, c):
        argv = c["argv"]
        mounts = []
        for i, tok in enumerate(argv):
            if tok == "-v":
                mounts.append({"Destination": argv[i + 1].split(":")[1]})
        ports = {}
        if "-p" in argv:
            hip, hport, cport = argv[argv.index("-p") + 1].split(":")
            ports["%s/tcp" % cport] = [{"HostIp": hip, "HostPort": hport}]
        dropped = "--cap-drop=ALL" in argv
        label = argv[argv.index("--label") + 1] if "--label" in argv else ""
        return {
            "Id": c["id"],
            "Image": c["image"],
            "Config": {"User": argv[argv.index("--user") + 1] if "--user" in argv else "",
                       "Labels": dict([label.split("=", 1)]) if label else {}},
            "HostConfig": {
                "ReadonlyRootfs": "--read-only" in argv,
                "CapDrop": ["CAP_CHOWN", "CAP_KILL"] if dropped else [],
                "SecurityOpt": (["no-new-privileges"]
                                if "--security-opt=no-new-privileges" in argv else []),
            },
            "EffectiveCaps": [] if dropped else ["CAP_CHOWN"],
            "NetworkSettings": {
                "Networks": {argv[argv.index("--network") + 1]: {}},
                "Ports": ports,
            },
            "Mounts": mounts,
        }

    def _image_token(self, argv):
        for tok in argv:
            if tok == _norm(self.built_id) or tok == CACHED_HEX:
                return tok
            if tok.startswith("docker.io/") or tok.startswith("localhost/"):
                return tok
        return ""

    # -- fake run ---------------------------------------------------------------
    def _image_present(self, ref):
        if ref in self.missing or ref in self.removed or _norm(ref) in self.removed:
            return False
        if ref.startswith(TAG_PREFIX):
            return self.tag_id is not None
        return True

    def run(self, cmd, **kw):
        c = list(cmd)
        self.calls.append((c, kw))
        if c[:1] == ["git"] and self.real_git:
            return _REAL_RUN(cmd, **kw)
        if c[:2] == ["podman", "info"]:
            return _Proc(stdout=self.rootless)
        if c[:3] == ["podman", "image", "exists"]:
            return _Proc(returncode=0 if self._image_present(c[3]) else 1)
        if c[:2] == ["podman", "images"]:
            return _Proc(stdout="\n".join(self.preexisting))
        if c[:3] == ["podman", "build", "--help"]:
            return _Proc(stdout="      --cpu-quota int\n")
        if c[:2] == ["podman", "build"]:
            if self.build_rc == 0 and not self.build_timeout:
                _write_iidfile(c, self.built_id)
                self.tag_id = _norm(self.built_id)
            else:
                if self.failed_iid:
                    _write_iidfile(c, self.failed_iid)
                if self.failed_tag_id:
                    self.tag_id = _norm(self.failed_tag_id)
            if self.build_timeout:
                raise subprocess.TimeoutExpired(c, kw.get("timeout"))
            return _Proc(returncode=self.build_rc, stdout="BUILD LOG LINE",
                         stderr=self.build_stderr)
        if c[:3] == ["podman", "image", "inspect"]:
            fmt = c[c.index("--format") + 1]
            ref = c[-1]
            if fmt == "{{.Id}}":
                if not self._image_present(ref):
                    return _Proc(returncode=125, stderr="image not known")
                if ref.startswith(TAG_PREFIX):
                    return _Proc(stdout=self.tag_id)
                if _norm(ref) == _norm(self.built_id):
                    return _Proc(stdout=_norm(self.built_id))
                return _Proc(stdout=CACHED_HEX)
            if fmt == "{{.Config.User}}":
                return _Proc(stdout=self.image_user)
            if fmt == "{{.Size}}":
                return _Proc(stdout="12345")
            return _Proc(stdout="meta")
        if c[:3] == ["podman", "network", "create"]:
            self.networks[NET_ID] = c[-1]
            return _Proc(stdout=c[-1])
        if c[:3] == ["podman", "network", "inspect"]:
            return _Proc(stdout="%s|%s" % (NET_ID, self.network_internal))
        if c[:3] == ["podman", "network", "rm"]:
            if self.network_rm_rc == 0:
                self.removed.add(c[3])
            return _Proc(returncode=self.network_rm_rc,
                         stderr="" if self.network_rm_rc == 0 else "network is in use")
        if c[:3] == ["podman", "network", "exists"]:
            return _Proc(returncode=1 if c[3] in self.removed else 0)
        if c[:3] == ["podman", "network", "ls"]:
            return _Proc(stdout="")
        if c[:2] == ["podman", "ps"]:
            return _Proc(stdout="")
        if c[:2] == ["podman", "run"]:
            cid = "%064x" % (len(self.containers) + 1)
            self.containers[cid] = {"id": cid, "name": c[c.index("--name") + 1],
                                    "argv": c, "image": self._image_token(c)}
            return _Proc(stdout=cid + "\n")
        if c[:2] == ["podman", "inspect"] or c[:3] == ["podman", "container", "inspect"]:
            ct = self._container_by(c[-1])
            if ct is None:
                return _Proc(returncode=125, stderr="no such container")
            return _Proc(stdout=json.dumps([self._inspect_json(ct)]))
        if c[:2] == ["podman", "exec"]:
            if self.exec_hook is not None:
                self.exec_hook(c)
            joined = " ".join(c)
            if "/proc/net/route" in joined:
                return _Proc(stdout=self.route)
            if c[-2:] == ["id", "-u"]:
                return _Proc(stdout="1000\n")
            if "readyz" in joined:
                return _Proc(stdout='{"ready": true}')
            if "healthz" in joined:
                return _Proc(stdout="ok")
            if "acc-write-probe" in joined:
                return _Proc(stdout="WROTE\n")
            if "acc-root-probe" in joined:
                return _Proc(stdout="touch: /acc-root-probe: Read-only file system\nRO_IMMUTABLE\n")
            return _Proc()
        if c[:2] == ["podman", "stop"]:
            self.router_stopped = True
            return _Proc()
        if c[:2] == ["podman", "start"]:
            self.router_stopped = False
            return _Proc()
        if c[:2] == ["podman", "rm"]:
            self.removed.add(c[-1])
            return _Proc()
        if c[:3] == ["podman", "container", "exists"]:
            return _Proc(returncode=1 if c[3] in self.removed else 0)
        if c[:2] in (["podman", "rmi"], ["podman", "untag"]):
            self.removed.add(c[2] if c[1] == "rmi" else c[3])
            return _Proc()
        if c[:1] == ["openssl"]:
            key = c[c.index("-keyout") + 1]
            crt = c[c.index("-out") + 1]
            with open(key, "w") as fh:
                fh.write("-----BEGIN PRIVATE KEY-----\nFAKEKEYBODYLINEabcdefgh12345678\n"
                         "-----END PRIVATE KEY-----\n")
            with open(crt, "w") as fh:
                fh.write("-----BEGIN CERTIFICATE-----\nFAKECERT\n-----END CERTIFICATE-----\n")
            return _Proc()
        return _Proc()

    # -- fake probes (record the deadline they were bound to) -----------------
    def https_request(self, port, method, path, *, deadline, host=mod.KNOWN_HOST,
                      key=None, body=None, headers=None, timeout=15.0):
        self.https_calls.append({"path": path, "host": host, "deadline": deadline})
        if path == "/readyz" and self.router_stopped:
            return 502, b"bad gateway", {}
        if host != mod.KNOWN_HOST:
            return 403, b"", {}
        if path == "/readyz":
            return 200, b'{"ready": true}', {}
        if key is None or key == "not-a-real-key":
            return 401, b"", {}
        return 200, b"{}", {}

    def sse_probe(self, port, key, model=mod.FIXTURE_MODEL, *, deadline,
                  host=mod.KNOWN_HOST, timeout=30.0):
        self.sse_calls.append({"deadline": deadline})
        return {"status": 200, "chunks": [{}, {}, {}], "arrivals": [0.0, 3.0, 3.2],
                "spread": 3.2}

    # -- fake source export -----------------------------------------------------
    def git_provenance(self, repo, *, deadline):
        self.git_calls.append(("provenance", deadline))
        return {"repo": repo, "head": "a" * 40, "tree": "d" * 40, "describe": "v0.3.0-1-gaaaa",
                "worktree_dirty": self.dirty, "dirty_entries": 2 if self.dirty else 0,
                "status_sha256": "0" * 64}

    EDGE_CONF = "# pinned edge config from the HEAD export\n"

    def export_head_context(self, repo, dest, *, deadline, commit=None):
        self.git_calls.append(("export", deadline))
        self.export_commits.append(commit)
        os.makedirs(os.path.join(dest, "packaging", "nginx"), exist_ok=True)
        with open(os.path.join(dest, "Containerfile"), "w") as fh:
            fh.write("FROM scratch\n")
        with open(os.path.join(dest, "packaging", "nginx", "localrouter.conf"), "w") as fh:
            fh.write(self.EDGE_CONF)
        return {"context_dir": dest, "context_sha256": "1" * 64, "context_files": 2,
                "containerfile_sha256": "2" * 64}

    def patches(self):
        ps = [
            mock.patch.object(mod, "run", self.run),
            mock.patch.object(mod, "https_request", self.https_request),
            mock.patch.object(mod, "sse_probe", self.sse_probe),
            mock.patch.object(mod, "podman_available", lambda: True),
            mock.patch.object(mod.time, "sleep", lambda s: None),
        ]
        if not self.real_git:
            ps += [
                mock.patch.object(mod, "git_provenance", self.git_provenance),
                mock.patch.object(mod, "export_head_context", self.export_head_context),
            ]
        return ps

    def podman_runs(self):
        return [c for c, _kw in self.calls if c[:2] == ["podman", "run"]]


class _Patched:
    def __init__(self, world):
        self.ps = world.patches()

    def __enter__(self):
        for p in self.ps:
            p.start()
        return self

    def __exit__(self, *exc):
        for p in reversed(self.ps):
            p.stop()
        return False


def run_main(tmp, world, *extra, secrets=None):
    """Run main() in the fake world; returns (rc, report, stdout)."""
    out = io.StringIO()
    tok = mock.patch.object(mod.pysecrets, "token_urlsafe",
                            side_effect=list(secrets)) if secrets else None
    with _Patched(world):
        if tok:
            tok.start()
        try:
            with redirect_stdout(out):
                rc = mod.main(["--artifacts", tmp, *extra])
        finally:
            if tok:
                tok.stop()
    run_dir = os.path.join(tmp, mod.RUN_ID)
    raw = pathlib.Path(run_dir, "report.json").read_text()
    return rc, raw, out.getvalue()


# ---------------------------------------------------------------------------
# Router argv: isolation and hardening
# ---------------------------------------------------------------------------


def _router_args(**kw):
    base = dict(
        name="lr-acc-router-x",
        network="lr-acc-net-x",
        image=BUILT_HEX,
        config_path="/art/config/config.yaml",
        secrets_dir="/art/secrets",
        data_dir="/art/data",
    )
    base.update(kw)
    return mod.router_run_args(**base)


def _nginx_args():
    return mod.nginx_run_args(
        name="n", network="net", image="nginx:latest",
        conf_path="/art/nginx.conf", tls_dir="/art/tls", host_port=54321,
    )


def _provider_args():
    return mod.provider_run_args(name="p", network="net", image="python:3.13-slim",
                                 script_path="/art/provider.py")


class TestRouterArgv(unittest.TestCase):
    def test_publish_no_host_port(self):
        argv = _router_args()
        joined = " ".join(argv)
        self.assertNotIn("-p", argv)
        self.assertNotIn("--publish", argv)
        self.assertNotIn("8787:", joined)
        self.assertFalse(any(a.startswith("127.0.0.1:") for a in argv))

    def test_are_hardened(self):
        argv = _router_args()
        self.assertIn("--read-only", argv)
        self.assertTrue(argv[argv.index("--tmpfs") + 1].startswith("/tmp"))
        self.assertEqual(argv[argv.index("--user") + 1], "1000:1000")
        self.assertEqual(argv[argv.index("--userns") + 1],
                         "keep-id:uid=1000,gid=1000")
        self.assertIn("/art/config/config.yaml:/etc/localrouter/config.yaml:ro", argv)
        self.assertIn("/art/secrets:/run/secrets:ro", argv)
        self.assertIn("/art/data:/var/lib/localrouter:rw", argv)
        self.assertNotIn("--privileged", argv)

    def test_mount_no_tls_material(self):
        joined = " ".join(_router_args())
        self.assertNotIn("/etc/nginx/tls", joined)
        self.assertNotIn("tls.crt", joined)
        self.assertNotIn("tls.key", joined)


# ---------------------------------------------------------------------------
# NGINX / provider argv
# ---------------------------------------------------------------------------


class TestNginxProviderArgv(unittest.TestCase):
    def test_nginx_publish_loopback_only(self):
        argv = _nginx_args()
        spec = argv[argv.index("-p") + 1]
        self.assertEqual(spec, "127.0.0.1:54321:8443")
        self.assertFalse(spec.startswith("0.0.0.0"))
        self.assertIn("--read-only", argv)
        self.assertEqual(argv[argv.index("--user") + 1], "1000:1000")
        self.assertEqual(argv[argv.index("--userns") + 1],
                         "keep-id:uid=1000,gid=1000")
        self.assertIn("/art/nginx.conf:/etc/nginx/conf.d/default.conf:ro", argv)
        self.assertIn("/art/tls:/etc/nginx/tls:ro", argv)

    def test_provider_use_private_alias_only(self):
        argv = _provider_args()
        self.assertNotIn("-p", argv)
        self.assertNotIn("--publish", argv)
        self.assertEqual(argv[argv.index("--network-alias") + 1], "fixture")


# ---------------------------------------------------------------------------
# Every podman run: --pull=never, hardening, ownership label
# ---------------------------------------------------------------------------


class TestEveryRunArgv(unittest.TestCase):
    CASES = (
        ("router", _router_args, BUILT_HEX),
        ("nginx", _nginx_args, "nginx:latest"),
        ("provider", _provider_args, "python:3.13-slim"),
    )

    def test_pull_never_before_image_positional(self):
        for role, fn, image in self.CASES:
            argv = fn()
            with self.subTest(role=role):
                self.assertIn("--pull=never", argv)
                self.assertLess(argv.index("--pull=never"), argv.index(image))
                self.assertFalse(any(a.startswith("--pull") and a != "--pull=never"
                                     for a in argv))

    def test_cap_drop_all_no_new_privileges_read_only(self):
        for role, fn, _image in self.CASES:
            argv = fn()
            with self.subTest(role=role):
                self.assertIn("--cap-drop=ALL", argv)
                self.assertIn("--security-opt=no-new-privileges", argv)
                self.assertIn("--read-only", argv)
                self.assertNotIn("--privileged", argv)
                self.assertFalse(any(a.startswith("--cap-add") for a in argv))

    def test_ownership_label_on_every_container(self):
        for role, fn, _image in self.CASES:
            argv = fn()
            with self.subTest(role=role):
                self.assertEqual(argv[argv.index("--label") + 1],
                                 "lr-acc-run=%s" % mod.RUN_ID)

    def test_provider_runs_non_root(self):
        argv = _provider_args()
        user = argv[argv.index("--user") + 1]
        self.assertNotIn(user.split(":")[0], ("0", "root"))

    def test_network_create_is_internal_and_labelled(self):
        argv = mod.network_create_args("lr-acc-net-x")
        self.assertEqual(argv[:3], ["podman", "network", "create"])
        self.assertIn("--internal", argv)
        self.assertEqual(argv[argv.index("--label") + 1], "lr-acc-run=%s" % mod.RUN_ID)
        self.assertEqual(argv[-1], "lr-acc-net-x")


# ---------------------------------------------------------------------------
# Build argv: --pull=never always; network only by explicit opt-in
# ---------------------------------------------------------------------------


class TestBuildArgv(unittest.TestCase):
    def test_are_offline(self):
        argv = mod.build_args(context="/repo", tag="localhost/lr:test",
                              cpu_flag="--cpus")
        self.assertIn("--pull=never", argv)
        self.assertIn("--network=none", argv)
        self.assertEqual(argv[argv.index("--jobs") + 1], "2")
        self.assertEqual(argv[argv.index("--memory") + 1], "6g")
        self.assertEqual(argv[argv.index("--cpus") + 1], "2")
        self.assertEqual(argv[-2:], ["localhost/lr:test", "/repo"])

    def test_cpu_quota_fallback(self):
        argv = mod.build_args(context="/repo", tag="t", cpu_flag="--cpu-quota")
        self.assertEqual(argv[argv.index("--cpu-quota") + 1], "200000")
        self.assertEqual(argv[argv.index("--cpu-period") + 1], "100000")

    def test_no_cpu_flag_when_detection_failed(self):
        argv = mod.build_args(context="/repo", tag="t", cpu_flag=None)
        for flag in ("--cpus", "--cpu-quota", "--cpu-period"):
            self.assertNotIn(flag, argv)

    def test_network_opt_in_drops_only_network_none(self):
        offline = mod.build_args(context="/repo", tag="t", cpu_flag=None)
        online = mod.build_args(context="/repo", tag="t", cpu_flag=None,
                                allow_network=True)
        self.assertIn("--network=none", offline)
        self.assertFalse(any(a.startswith("--network") for a in online))
        self.assertIn("--pull=never", online)
        self.assertEqual([a for a in offline if a != "--network=none"], online)

    def test_iidfile_no_cache_and_version(self):
        argv = mod.build_args(context="/repo", tag="t", cpu_flag=None,
                              iidfile="/art/iid", no_cache=True, version="v1-gabc")
        self.assertEqual(argv[argv.index("--iidfile") + 1], "/art/iid")
        self.assertIn("--no-cache", argv)
        self.assertEqual(argv[argv.index("--build-arg") + 1], "VERSION=v1-gabc")
        self.assertNotIn("--no-cache", mod.build_args(context="/repo", tag="t",
                                                      cpu_flag=None))

    def test_no_cache_also_skips_intermediate_layer_images(self):
        argv = mod.build_args(context="/repo", tag="t", cpu_flag=None, no_cache=True)
        self.assertIn("--layers=false", argv)
        self.assertNotIn("--layers=false",
                         mod.build_args(context="/repo", tag="t", cpu_flag=None))

    def test_network_flag_default_off(self):
        self.assertFalse(mod.parse_args([]).allow_build_network)
        self.assertTrue(mod.parse_args(["--allow-build-network"]).allow_build_network)
        self.assertFalse(mod.parse_args([]).no_cache)


REPO = pathlib.Path(__file__).resolve().parents[2]


class TestShippedImagePins(unittest.TestCase):
    """Preflight/run refs are the shipped digest pins, never a mutable tag."""

    def test_build_base_images_match_containerfile_from_lines(self):
        refs = []
        for line in (REPO / "Containerfile").read_text().splitlines():
            words = line.split()
            if words and words[0].upper() == "FROM":
                refs.append([w for w in words[1:] if not w.startswith("--")][0])
        self.assertEqual(list(mod.BUILD_BASE_IMAGES), refs)
        for ref in refs:
            self.assertRegex(ref, r"@sha256:[0-9a-f]{64}$")

    def test_nginx_default_matches_quadlet_image(self):
        quadlet = (REPO / "packaging" / "quadlet" / "localrouter-nginx.container")
        images = [ln.split("=", 1)[1].strip() for ln in quadlet.read_text().splitlines()
                  if ln.startswith("Image=")]
        self.assertEqual(images, [mod.NGINX_IMAGE_DEFAULT])
        self.assertRegex(mod.NGINX_IMAGE_DEFAULT, r"@sha256:[0-9a-f]{64}$")


# ---------------------------------------------------------------------------
# Inspect-derived assertions
# ---------------------------------------------------------------------------


GOOD_HARDENED = {
    "HostConfig": {"ReadonlyRootfs": True, "CapDrop": ["CAP_CHOWN"],
                   "SecurityOpt": ["no-new-privileges"]},
    "EffectiveCaps": [],
}


class TestInspectAssertions(unittest.TestCase):
    def test_published_ports_empty_when_unpublished(self):
        ins = {"NetworkSettings": {"Ports": {"8787/tcp": None}}}
        self.assertEqual(mod.published_ports(ins), [])
        mod.assert_router_no_host_port(ins)

    def test_published_ports_detect_binding(self):
        ins = {"NetworkSettings": {"Ports": {
            "8443/tcp": [{"HostIp": "127.0.0.1", "HostPort": "54321"}],
            "8787/tcp": None,
        }}}
        self.assertEqual(mod.published_ports(ins),
                         [("8443/tcp", "127.0.0.1", "54321")])
        with self.assertRaises(mod.AcceptanceError):
            mod.assert_router_no_host_port(ins)

    def test_mounts_accepts_allowlist(self):
        ins = {"Mounts": [
            {"Destination": "/etc/localrouter/config.yaml"},
            {"Destination": "/run/secrets"},
            {"Destination": "/var/lib/localrouter"},
            {"Destination": "/etc/resolv.conf"},
        ]}
        mod.assert_router_mounts(ins)

    def test_mounts_rejects_foreign_mount(self):
        with self.assertRaises(mod.AcceptanceError):
            mod.assert_router_mounts({"Mounts": [{"Destination": "/etc/nginx/tls"}]})

    def test_mounts_rejects_tls_blob(self):
        with self.assertRaises(mod.AcceptanceError):
            mod.assert_router_mounts(
                {"Mounts": [{"Destination": "/run/secrets", "Source": "/x/tls.crt"}]})

    def test_router_user(self):
        self.assertEqual(
            mod.assert_router_user({"Config": {"User": "1000:1000"}}), "1000:1000")
        for bad in ("0", "root", "", "0:0"):
            with self.assertRaises(mod.AcceptanceError):
                mod.assert_router_user({"Config": {"User": bad}})

    def test_image_user_must_be_non_root(self):
        self.assertEqual(mod.assert_image_user("1000:1000"), "1000:1000")
        self.assertEqual(mod.assert_image_user("1000"), "1000")
        for bad in ("", "0", "root", "0:0", "root:root", "  "):
            with self.subTest(user=bad), self.assertRaises(mod.AcceptanceError):
                mod.assert_image_user(bad)

    def test_readonly_rootfs(self):
        mod.assert_readonly_rootfs({"HostConfig": {"ReadonlyRootfs": True}})
        with self.assertRaises(mod.AcceptanceError):
            mod.assert_readonly_rootfs({"HostConfig": {"ReadonlyRootfs": False}})

    def test_hardening_problems(self):
        self.assertEqual(mod.hardening_problems(GOOD_HARDENED), [])
        no_ro = json.loads(json.dumps(GOOD_HARDENED))
        no_ro["HostConfig"]["ReadonlyRootfs"] = False
        caps = json.loads(json.dumps(GOOD_HARDENED))
        caps["EffectiveCaps"] = ["CAP_NET_RAW"]
        nnp = json.loads(json.dumps(GOOD_HARDENED))
        nnp["HostConfig"]["SecurityOpt"] = []
        for bad in (no_ro, caps, nnp, {}):
            with self.subTest(bad=bad):
                self.assertTrue(mod.hardening_problems(bad))

    def test_default_route_detection(self):
        self.assertFalse(mod.has_default_route(ROUTE_NO_DEFAULT))
        self.assertTrue(mod.has_default_route(ROUTE_WITH_DEFAULT))

    def test_nginx_loopback_only(self):
        ok = {"NetworkSettings": {"Ports": {
            "8443/tcp": [{"HostIp": "127.0.0.1", "HostPort": "50000"}]}}}
        mod.assert_nginx_loopback_only(ok, 50000)
        exposed = {"NetworkSettings": {"Ports": {
            "8443/tcp": [{"HostIp": "0.0.0.0", "HostPort": "50000"}]}}}
        with self.assertRaises(mod.AcceptanceError):
            mod.assert_nginx_loopback_only(exposed, 50000)
        forbidden = {"NetworkSettings": {"Ports": {
            "8443/tcp": [{"HostIp": "127.0.0.1", "HostPort": "8787"}]}}}
        with self.assertRaises(mod.AcceptanceError):
            mod.assert_nginx_loopback_only(forbidden, 8787)


# ---------------------------------------------------------------------------
# Pure helpers
# ---------------------------------------------------------------------------


class TestPureHelpers(unittest.TestCase):
    def test_parse_readyz(self):
        self.assertIs(mod.parse_readyz('{"ready": true}'), True)
        self.assertIs(mod.parse_readyz(b'{"ready": false}'), False)
        with self.assertRaises(mod.AcceptanceError):
            mod.parse_readyz('{"status": "ok"}')
        with self.assertRaises(Exception):
            mod.parse_readyz("not json")

    def test_free_port_is_safe(self):
        port = mod.free_port()
        self.assertTrue(1024 <= port <= 65535)
        self.assertNotIn(port, mod.FORBIDDEN_HOST_PORTS)

    def test_host_allowed_mirrors_hostguard(self):
        allowed = ["router.example.com", "localhost"]
        self.assertIs(mod.host_allowed("127.0.0.1:8443", allowed), True)
        self.assertIs(mod.host_allowed("127.0.0.1", allowed), True)
        self.assertIs(mod.host_allowed("[::1]:8443", allowed), True)
        self.assertIs(mod.host_allowed("localhost", allowed), True)
        self.assertIs(mod.host_allowed("router.example.com", allowed), True)
        self.assertIs(mod.host_allowed("router.example.com:8443", allowed), True)
        self.assertIs(mod.host_allowed("ROUTER.EXAMPLE.COM", allowed), True)
        self.assertIs(mod.host_allowed("evil.example.com", allowed), False)
        self.assertIs(mod.host_allowed("10.0.0.5", allowed), False)
        self.assertIs(mod.host_allowed("", allowed), False)

    def test_unique_names_are_unique_and_prefixed(self):
        a, b = mod.unique_name("router"), mod.unique_name("router")
        self.assertNotEqual(a, b)
        self.assertTrue(a.startswith("lr-acc-router-"))
        self.assertTrue(mod.unique_network_name().startswith("lr-acc-net-"))

    def test_sanitize_redacts_secrets(self):
        secret = "sk-live-abcdefghijklmnop"
        text = "Authorization: Bearer %s and short" % secret
        out = mod.sanitize(text, [secret, "short"])
        self.assertNotIn(secret, out)
        self.assertIn("[REDACTED]", out)
        self.assertIn("short", out)  # too short to be treated as a secret

    def test_render_router_config_shape(self):
        cfg = mod.render_router_config(allowed_hosts=["router.example.com", "localhost"])
        self.assertIn("allow_non_loopback: true", cfg)
        self.assertIn("require_auth: true", cfg)
        self.assertIn("listen: 0.0.0.0:8787", cfg)
        self.assertIn("allowed_hosts: [router.example.com, localhost]", cfg)
        self.assertNotIn("tls_cert_file", cfg)
        self.assertNotIn("tls_key_file", cfg)
        self.assertIn(mod.FIXTURE_BASE_URL, cfg)
        self.assertIn("data_dir: /var/lib/localrouter", cfg)

    def test_norm_id(self):
        self.assertEqual(mod.norm_id("sha256:ABC"), "abc")
        self.assertEqual(mod.norm_id(" abc\n"), "abc")
        self.assertEqual(mod.norm_id(""), "")


# ---------------------------------------------------------------------------
# Cleanup: owned IDs only, audit list kept, remaining reported
# ---------------------------------------------------------------------------


class TestCleanupOwnership(unittest.TestCase):
    def _owned(self):
        owned = mod.Owned()
        owned.add("container", "cid1", "lr-acc-router-1")
        owned.add("container", "cid2", "lr-acc-nginx-2")
        owned.add("network", "nid1", "lr-acc-net-x")
        owned.add("image", BUILT_HEX, "localhost/localrouter-acc:x")
        return owned

    def test_cleanup_calls_are_by_id_reversed_and_never_force_network(self):
        self.assertEqual(self._owned().cleanup_calls(), [
            ["podman", "rm", "-f", "-t", "5", "cid2"],
            ["podman", "rm", "-f", "-t", "5", "cid1"],
            ["podman", "network", "rm", "nid1"],
            ["podman", "rmi", BUILT_HEX],
        ])

    def test_preexisting_image_id_is_only_untagged(self):
        owned = mod.Owned()
        owned.add("image", BUILT_HEX, "localhost/localrouter-acc:x", preexisting=True)
        self.assertEqual(owned.cleanup_calls(),
                         [["podman", "untag", BUILT_HEX, "localhost/localrouter-acc:x"]])

    def test_cleanup_keeps_audit_and_reports_nothing_remaining(self):
        world = FakeWorld()
        owned = self._owned()
        world.containers = {}
        with mock.patch.object(mod, "run", world.run):
            remaining = owned.cleanup([], mod.Deadline(30))
        self.assertEqual(remaining, [])
        self.assertEqual([e["id"] for e in owned.created],
                         ["cid1", "cid2", "nid1", BUILT_HEX])
        exists = [c for c, _ in world.calls if c[2:3] == ["exists"]]
        self.assertEqual(len(exists), 4)

    def test_failed_network_rm_is_reported_not_forced(self):
        world = FakeWorld(network_rm_rc=2)
        owned = self._owned()
        with mock.patch.object(mod, "run", world.run):
            remaining = owned.cleanup([], mod.Deadline(30))
        self.assertEqual([e["id"] for e in remaining], ["nid1"])
        net_rms = [c for c, _ in world.calls if c[:3] == ["podman", "network", "rm"]]
        self.assertEqual(net_rms, [["podman", "network", "rm", "nid1"]])
        self.assertFalse(any("-f" in c or "--force" in c for c in net_rms))

    def test_exhausted_cleanup_budget_reports_everything_remaining(self):
        owned = self._owned()

        def no_spawn(*a, **kw):
            raise AssertionError("nothing may be spawned on an exhausted budget")

        # The REAL run(): it must refuse before spawning anything.
        with mock.patch.object(mod.subprocess, "Popen", no_spawn):
            remaining = owned.cleanup([], mod.Deadline(0.0))
        self.assertEqual(len(remaining), 4)

    def test_duplicate_add_is_ignored(self):
        owned = mod.Owned()
        owned.add("container", "cid1", "a")
        owned.add("container", "cid1", "a")
        self.assertEqual(len(owned.created), 1)


# ---------------------------------------------------------------------------
# Build provenance helpers: immutable Id
# ---------------------------------------------------------------------------


class TestImageProvenance(unittest.TestCase):
    def test_container_image_id(self):
        self.assertEqual(mod.container_image_id({"Image": "sha256:abc"}), "sha256:abc")
        self.assertEqual(mod.container_image_id({"ImageID": "sha256:xyz"}), "sha256:xyz")
        self.assertEqual(mod.container_image_id({}), "")

    def test_image_identity_ok_requires_exact_match(self):
        self.assertTrue(mod.image_identity_ok({"Image": "sha256:abc"}, "sha256:abc"))
        self.assertFalse(mod.image_identity_ok({"Image": "sha256:abc"}, "sha256:def"))
        self.assertFalse(mod.image_identity_ok({"Image": "sha256:abc"}, ""))
        self.assertFalse(mod.image_identity_ok({}, "sha256:abc"))

    def test_image_identity_ignores_sha256_prefix(self):
        # podman container inspect reports bare hex; --iidfile writes sha256:.
        self.assertTrue(mod.image_identity_ok({"Image": BUILT_HEX}, BUILT_ID))


# ---------------------------------------------------------------------------
# Source provenance: HEAD export, dirty flag, digests (real local git)
# ---------------------------------------------------------------------------


class TestSourceProvenance(unittest.TestCase):
    GIT_ENV = dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM="1")

    def _git(self, repo, *args):
        return subprocess.run(
            ["git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.invalid",
             "-c", "commit.gpgsign=false", *args],
            check=True, capture_output=True, text=True, env=self.GIT_ENV,
        ).stdout.strip()

    EDGE = os.path.join("packaging", "nginx", "localrouter.conf")

    def _repo(self, tmp, *, edge=False):
        repo = os.path.join(tmp, "repo")
        os.makedirs(repo)
        self._git(repo, "init", "-q")
        pathlib.Path(repo, "Containerfile").write_text("FROM scratch\n")
        pathlib.Path(repo, "main.go").write_text("package main\n")
        if edge:
            os.makedirs(os.path.join(repo, "packaging", "nginx"))
            pathlib.Path(repo, self.EDGE).write_text("# committed edge v1\n")
        self._git(repo, "add", "-A")
        self._git(repo, "commit", "-q", "-m", "init")
        return repo

    def _move_head(self, repo):
        """Commit new content so HEAD now names a different tree."""
        pathlib.Path(repo, "main.go").write_text("package main // MOVED\n")
        edge = pathlib.Path(repo, self.EDGE)
        if edge.exists():
            edge.write_text("# committed edge v2 (HEAD moved)\n")
        self._git(repo, "commit", "-q", "-am", "move")
        return self._git(repo, "rev-parse", "HEAD")

    def test_export_pins_recorded_commit_when_head_moves(self):
        # HEAD moves between provenance and export: the context must still be
        # exactly the recorded commit, never whatever symbolic HEAD names now.
        with tempfile.TemporaryDirectory() as tmp:
            repo = self._repo(tmp, edge=True)
            pinned = self._git(repo, "rev-parse", "HEAD")
            real_prov = mod.git_provenance
            moved = []

            def prov_then_move(r, *, deadline):
                p = real_prov(r, deadline=deadline)
                moved.append(self._move_head(repo))
                return p

            h = new_harness(tmp)
            h.repo = repo
            with mock.patch.dict(os.environ, self.GIT_ENV), \
                    mock.patch.object(mod, "git_provenance", prov_then_move):
                h.stage_source()
            self.assertNotEqual(moved[0], pinned)
            self.assertEqual(h.source["head"], pinned)
            self.assertEqual(pathlib.Path(h.context_dir, "main.go").read_text(),
                             "package main\n")
            self.assertEqual(pathlib.Path(h.context_dir, self.EDGE).read_text(),
                             "# committed edge v1\n")

    def test_tree_and_describe_derive_from_the_recorded_commit(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = self._repo(tmp)
            git_args = []

            def spy(cmd, **kw):
                git_args.append(cmd)
                return _REAL_RUN(cmd, **kw)

            with mock.patch.dict(os.environ, self.GIT_ENV), \
                    mock.patch.object(mod, "run", spy):
                prov = mod.git_provenance(repo, deadline=mod.Deadline(30))
            # Only the first call resolves the symbolic ref; the rest use the sha.
            self.assertEqual(git_args[0][-2:], ["rev-parse", "HEAD"])
            for cmd in git_args[1:]:
                with self.subTest(cmd=cmd):
                    if "status" in cmd:
                        continue
                    self.assertNotIn("HEAD", cmd)
                    self.assertNotIn("HEAD^{tree}", cmd)
            self.assertIn("%s^{tree}" % prov["head"], git_args[1])

    def test_export_refuses_a_symbolic_ref(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = self._repo(tmp)
            for ref in ("HEAD", "master", "abc123", ""):
                with self.subTest(ref=ref), mock.patch.dict(os.environ, self.GIT_ENV), \
                        self.assertRaises(mod.AcceptanceError):
                    mod.export_head_context(repo, os.path.join(tmp, "ctx-%s" % len(ref)),
                                            deadline=mod.Deadline(30), commit=ref)

    def test_nginx_config_is_mounted_from_the_pinned_export(self):
        # A live (uncommitted) edit to the shipped edge config after the export
        # must not reach the mounted file; its digest is recorded.
        with tempfile.TemporaryDirectory() as tmp:
            repo = self._repo(tmp, edge=True)
            h = new_harness(tmp)
            h.repo = repo
            with mock.patch.dict(os.environ, self.GIT_ENV):
                h.stage_source()
            pathlib.Path(repo, self.EDGE).write_text("# LIVE EDIT, not committed\n")
            path = h._nginx_conf_path()
            self.assertTrue(path.startswith(h.context_dir + os.sep), path)
            self.assertEqual(pathlib.Path(path).read_text(), "# committed edge v1\n")
            self.assertEqual(h.edge_config["sha256"],
                             hashlib.sha256(b"# committed edge v1\n").hexdigest())
            self.assertEqual(h.edge_config["path"], path)
            self.assertIn(h.source["head"], h.edge_config["source"])

    def test_missing_edge_config_in_export_fails_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = self._repo(tmp)          # no packaging/nginx in the commit
            h = new_harness(tmp)
            h.repo = repo
            pathlib.Path(repo, "packaging", "nginx").mkdir(parents=True)
            pathlib.Path(repo, self.EDGE).write_text("# untracked live file\n")
            with mock.patch.dict(os.environ, self.GIT_ENV):
                h.stage_source()
            with self.assertRaises(mod.AcceptanceError):
                h._nginx_conf_path()

    def test_clean_head_provenance(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = self._repo(tmp)
            with mock.patch.dict(os.environ, self.GIT_ENV):
                prov = mod.git_provenance(repo, deadline=mod.Deadline(30))
            self.assertEqual(prov["head"], self._git(repo, "rev-parse", "HEAD"))
            self.assertEqual(prov["tree"], self._git(repo, "rev-parse", "HEAD^{tree}"))
            self.assertFalse(prov["worktree_dirty"])
            self.assertEqual(prov["dirty_entries"], 0)

    def test_export_is_head_only_and_digest_is_stable(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = self._repo(tmp)
            pathlib.Path(repo, "main.go").write_text("package main // DIRTY\n")
            pathlib.Path(repo, "untracked.txt").write_text("not committed\n")
            with mock.patch.dict(os.environ, self.GIT_ENV):
                prov = mod.git_provenance(repo, deadline=mod.Deadline(30))
                a = mod.export_head_context(repo, os.path.join(tmp, "ctx-a"),
                                            deadline=mod.Deadline(30), commit=prov["head"])
                b = mod.export_head_context(repo, os.path.join(tmp, "ctx-b"),
                                            deadline=mod.Deadline(30), commit=prov["head"])
            self.assertTrue(prov["worktree_dirty"])
            self.assertEqual(prov["dirty_entries"], 2)
            self.assertEqual(pathlib.Path(a["context_dir"], "main.go").read_text(),
                             "package main\n")
            self.assertFalse(pathlib.Path(a["context_dir"], "untracked.txt").exists())
            self.assertEqual(a["context_sha256"], b["context_sha256"])
            self.assertEqual(a["containerfile_sha256"],
                             hashlib.sha256(b"FROM scratch\n").hexdigest())
            self.assertEqual(a["context_files"], 2)
            self.assertFalse(os.path.exists(a["context_dir"] + ".tar"))

    def test_context_digest_changes_with_content(self):
        with tempfile.TemporaryDirectory() as tmp:
            pathlib.Path(tmp, "x").write_text("1")
            d1 = mod.context_digest(tmp)
            pathlib.Path(tmp, "x").write_text("2")
            self.assertNotEqual(d1, mod.context_digest(tmp))


# ---------------------------------------------------------------------------
# FAIL-CLOSED build: the core regression
# ---------------------------------------------------------------------------


class TestBuildFailClosed(unittest.TestCase):
    def test_build_failure_is_gate_and_no_fallback(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            with mock.patch.object(mod, "run", make_fake_podman(build_rc=1)):
                with self.assertRaises(mod.AcceptanceError) as ctx:
                    h.stage_build()
            self.assertIn("fail-closed", str(ctx.exception).lower())
            # No cached image is ever substituted.
            self.assertIsNone(h.built_image)
            self.assertIsNone(h.built_image_id)
            # The build check is a GATE failure, not a soft "limit".
            build_checks = [c for c in h.checks
                            if c["name"] == "build.offline_nonetwork"]
            self.assertEqual(len(build_checks), 1)
            self.assertFalse(build_checks[0]["ok"])
            self.assertEqual(build_checks[0]["severity"], "gate")
            # The failure is surfaced honestly in limitations.
            self.assertTrue(any("fail-closed" in lim for lim in h.limitations))
            # It must never tell the reader an old cached image is proof.
            self.assertFalse(any("using cached" in lim for lim in h.limitations))
            # build.log was written and sanitized.
            log = pathlib.Path(h.run_dir, "build.log").read_text()
            self.assertIn("BUILD LOG LINE", log)
            self.assertEqual(h.owned.created, [])

    def test_build_success_records_iidfile_id_and_owns_image(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            rec = []
            with mock.patch.object(mod, "run", make_fake_podman(record=rec)):
                self.assertTrue(h.stage_build())
            self.assertEqual(h.built_image,
                             "localhost/localrouter-acc:%s" % mod.RUN_ID)
            self.assertEqual(h.built_image_id, BUILT_HEX)
            self.assertEqual(h.built_image_metadata["immutable_id"], BUILT_HEX)
            ok = {c["name"]: c for c in h.checks}
            self.assertTrue(ok["build.offline_nonetwork"]["ok"])
            self.assertTrue(ok["build.image_id_recorded"]["ok"])
            self.assertEqual(ok["build.image_id_recorded"]["severity"], "gate")
            build_cmds = [c for c, _kw in rec if c[:2] == ["podman", "build"]
                          and c[2] != "--help"]
            self.assertEqual(len(build_cmds), 1)
            self.assertIn("--network=none", build_cmds[0])
            self.assertIn("--pull=never", build_cmds[0])
            self.assertIn("--iidfile", build_cmds[0])
            self.assertEqual(build_cmds[0][-1], h.context_dir)
            self.assertEqual(h.owned.created, [{
                "kind": "image", "id": BUILT_HEX, "name": h.built_image,
                "preexisting": False}])

    def test_build_tmpdir_is_contained_in_the_run_dir(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            rec = []
            with mock.patch.object(mod, "run", make_fake_podman(record=rec)):
                h.stage_build()
            kw = [kw for c, kw in rec if c[:2] == ["podman", "build"] and c[2] != "--help"][0]
            tmpdir = kw["env"]["TMPDIR"]
            self.assertTrue(tmpdir.startswith(h.run_dir + os.sep), tmpdir)
            self.assertTrue(os.path.isdir(tmpdir))
            self.assertEqual(kw["env"].get("PATH"), os.environ.get("PATH"))

    def test_preexisting_identical_image_is_not_owned_for_rmi(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            with mock.patch.object(mod, "run", make_fake_podman(preexisting=BUILT_ID + "\n")):
                h.stage_build()
            self.assertTrue(h.owned.created[0]["preexisting"])

    def test_preexisting_image_listing_failure_fails_closed_before_build(self):
        # Without a trustworthy pre-build listing, a shared Id could later be
        # mistaken for ours and rmi'd: refuse to build at all.
        for kw in (dict(images_rc=125), dict(preexisting=""),
                   dict(preexisting="not-an-image-id\n")):
            with self.subTest(**kw), tempfile.TemporaryDirectory() as tmp:
                h = new_harness(tmp)
                rec = []
                with mock.patch.object(mod, "run", make_fake_podman(record=rec, **kw)):
                    with self.assertRaises(mod.AcceptanceError) as ctx:
                        h.stage_build()
                self.assertIn("pre-existing", str(ctx.exception))
                self.assertFalse([c for c, _ in rec
                                  if c[:2] == ["podman", "build"] and c[2] != "--help"])
                self.assertEqual(h.owned.created, [])

    def test_existing_per_run_tag_fails_closed_before_build(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            rec = []
            with mock.patch.object(mod, "run",
                                   make_fake_podman(record=rec, tag_exists_before=True)):
                with self.assertRaises(mod.AcceptanceError):
                    h.stage_build()
            self.assertFalse([c for c, _ in rec
                              if c[:2] == ["podman", "build"] and c[2] != "--help"])

    def test_build_cpu_flag_absent_when_undetected(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            rec = []
            with mock.patch.object(mod, "run",
                                   make_fake_podman(record=rec, cpu_help="nothing")):
                h.stage_build()
            build = [c for c, _ in rec if c[:2] == ["podman", "build"] and c[2] != "--help"][0]
            self.assertNotIn("--cpus", build)
            self.assertNotIn("--cpu-quota", build)
            self.assertTrue(any("CPU cap not applied" in lim for lim in h.limitations))

    def test_network_opt_in_build(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp, "--allow-build-network")
            rec = []
            with mock.patch.object(mod, "run", make_fake_podman(record=rec)):
                h.stage_build()
            build = [c for c, _ in rec if c[:2] == ["podman", "build"] and c[2] != "--help"][0]
            self.assertNotIn("--network=none", build)
            self.assertIn("--pull=never", build)
            names = {c["name"]: c for c in h.checks}
            self.assertNotIn("build.offline_nonetwork", names)
            self.assertTrue(names["build.dependency_network_opt_in"]["ok"])
            self.assertEqual(h.build_info["network"], "enabled")
            self.assertEqual(h.build_info["pull"], "never")

    def test_build_without_immutable_id_fails_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            with mock.patch.object(
                    mod, "run", make_fake_podman(build_rc=0, built_id="")):
                with self.assertRaises(mod.AcceptanceError):
                    h.stage_build()
            self.assertIsNone(h.built_image)
            failed = [c for c in h.checks if not c["ok"]]
            self.assertTrue(any(c["name"] == "build.image_id_recorded" for c in failed))

    def test_router_image_is_immutable_id(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            self.assertIsNone(h.router_image())
            h.built_image = "localhost/localrouter-acc:run"
            h.built_image_id = BUILT_HEX
            self.assertEqual(h.router_image(), BUILT_HEX)

    def test_assert_image_identity_gate(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            h.router_container = "lr-acc-router-x"
            h.built_image = "tag"
            h.built_image_id = "sha256:built"
            with mock.patch.object(mod, "inspect_container",
                                   lambda name, **kw: {"Image": "sha256:built"}):
                h.assert_image_identity()
            match = [c for c in h.checks if c["name"] == "router.image_matches_build"]
            self.assertTrue(match and match[0]["ok"])
            self.assertEqual(match[0]["severity"], "gate")
            # A mismatch must fail the gate.
            h2 = new_harness(tmp)
            h2.router_container = "c"
            h2.built_image, h2.built_image_id = "tag", "sha256:built"
            with mock.patch.object(mod, "inspect_container",
                                   lambda name, **kw: {"Image": "sha256:other"}):
                h2.assert_image_identity()
            bad = [c for c in h2.checks if c["name"] == "router.image_matches_build"]
            self.assertTrue(bad and not bad[0]["ok"])

    def test_cached_smoke_skips_identity_check(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp, "--no-build")
            h.router_container = "c"
            h.assert_image_identity()  # no build artifact -> no-op, no raise
            self.assertEqual(
                [c for c in h.checks if c["name"] == "router.image_matches_build"],
                [])


# ---------------------------------------------------------------------------
# Orchestration: preflight order, deadline everywhere, isolation, provenance
# ---------------------------------------------------------------------------


class TestOrchestration(unittest.TestCase):
    def test_rootless_refused_before_any_build(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(rootless="false")
            h = new_harness(tmp)
            with _Patched(world), self.assertRaises(mod.AcceptanceError):
                h.run_all()
            self.assertEqual(h.executed_stages, ["host-preflight"])
            self.assertFalse(any(c[:2] == ["podman", "build"] and c[2:3] != ["--help"]
                                 for c, _ in world.calls))
            self.assertEqual(world.git_calls, [])

    def test_missing_base_image_refused_before_build(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(missing_images=[mod.BUILD_BASE_IMAGES[1]])
            h = new_harness(tmp)
            with _Patched(world), self.assertRaises(mod.AcceptanceError):
                h.run_all()
            self.assertEqual(h.executed_stages, ["host-preflight"])
            self.assertFalse(any(c[:2] == ["podman", "build"] for c, _ in world.calls
                                 if c[2:3] != ["--help"]))

    def test_run_all_aborts_before_any_container(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(build_rc=1)
            h = new_harness(tmp)
            with _Patched(world), self.assertRaises(mod.AcceptanceError):
                h.run_all()
            self.assertEqual(h.executed_stages, ["host-preflight", "source", "build"])
            self.assertEqual(h.owned.created, [])
            self.assertEqual(world.podman_runs(), [])
            self.assertIsNone(h.router_container)
            self.assertIsNone(h.host_port)

    def test_full_run_every_call_is_bound_to_the_run_deadline(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld()
            h = new_harness(tmp)
            with _Patched(world):
                self.assertTrue(h.run_all())
            unbound = [c for c, kw in world.calls if kw.get("deadline") is not h.deadline]
            self.assertEqual(unbound, [])
            self.assertTrue(world.https_calls)
            self.assertTrue(all(c["deadline"] is h.deadline for c in world.https_calls))
            self.assertEqual([c["deadline"] for c in world.sse_calls], [h.deadline])
            self.assertTrue(all(d is h.deadline for _k, d in world.git_calls))
            self.assertEqual(mod.classify_outcome(h.checks), "pass",
                             [c for c in h.checks if not c["ok"]])

    def test_full_run_uses_immutable_id_internal_net_and_pull_never(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld()
            h = new_harness(tmp)
            with _Patched(world):
                h.run_all()
            runs = world.podman_runs()
            self.assertEqual(len(runs), 3)
            for argv in runs:
                self.assertIn("--pull=never", argv)
                self.assertEqual(argv[argv.index("--network") + 1],
                                 mod.unique_network_name())
            router = [a for a in runs if "--network-alias" in a
                      and a[a.index("--network-alias") + 1] == "localrouter"][0]
            self.assertEqual(router[-1], BUILT_HEX)
            self.assertNotIn(h.built_image, router)
            create = [c for c, _ in world.calls if c[:3] == ["podman", "network", "create"]]
            self.assertEqual(len(create), 1)
            self.assertIn("--internal", create[0])
            names = {c["name"]: c for c in h.checks}
            for name in ("network.internal", "router.image_user_nonroot",
                         "router.hardened", "edge.hardened", "fixture.hardened",
                         "router.internal_network_only", "router.no_default_route",
                         "fixture.no_default_route", "router.process_uid",
                         "source.head_exported"):
                with self.subTest(check=name):
                    self.assertIn(name, names)
                    self.assertTrue(names[name]["ok"], names[name])
                    self.assertEqual(names[name]["severity"], "gate")

    def test_root_image_user_refuses_to_start_router(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(image_user="")
            h = new_harness(tmp)
            with _Patched(world), self.assertRaises(mod.AcceptanceError):
                h.run_all()
            check = [c for c in h.checks if c["name"] == "router.image_user_nonroot"]
            self.assertTrue(check and not check[0]["ok"])
            self.assertEqual(world.podman_runs(), [])

    def test_non_internal_network_or_default_route_fails_gate(self):
        for kw, name in ((dict(network_internal="false"), "network.internal"),
                         (dict(route=ROUTE_WITH_DEFAULT), "router.no_default_route")):
            with self.subTest(check=name), tempfile.TemporaryDirectory() as tmp:
                world = FakeWorld(**kw)
                h = new_harness(tmp)
                with _Patched(world):
                    try:
                        h.run_all()
                    except mod.AcceptanceError:
                        pass
                bad = [c for c in h.checks if c["name"] == name]
                self.assertTrue(bad and not bad[0]["ok"])
                self.assertEqual(mod.classify_outcome(h.checks), "fail")

    def test_cached_smoke_runs_cached_image_id_and_never_passes(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld()
            rc, raw, _out = run_main(tmp, world, "--no-build")
            report = json.loads(raw)
            self.assertEqual(rc, 1)
            self.assertEqual(report["outcome"], "cached_smoke_not_release_acceptance")
            self.assertIsNone(report["source"])
            self.assertEqual(report["image_under_test"], CACHED_HEX)
            self.assertFalse(any(c[:2] == ["podman", "build"] for c, _ in world.calls))
            router = [a for a in world.podman_runs() if "localrouter" in a][0]
            self.assertEqual(router[-1], CACHED_HEX)

    def test_build_only_flags_rejected_with_no_build(self):
        for flag in ("--allow-build-network", "--no-cache"):
            with self.subTest(flag=flag), redirect_stdout(io.StringIO()), \
                    mock.patch("sys.stderr", io.StringIO()), \
                    self.assertRaises(SystemExit):
                mod.parse_args(["--no-build", flag])

    def test_full_run_exports_recorded_sha_and_mounts_pinned_edge_config(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld()
            rc, raw, _out = run_main(tmp, world)
            report = json.loads(raw)
            self.assertEqual(rc, 0, report["gate_failures"])
            self.assertEqual(world.export_commits, ["a" * 40])
            nginx = [a for a in world.podman_runs() if "-p" in a][0]
            mounts = [nginx[i + 1] for i, t in enumerate(nginx) if t == "-v"]
            conf = [m.split(":")[0] for m in mounts
                    if m.endswith(":/etc/nginx/conf.d/default.conf:ro")]
            ctx = report["source"]["context_dir"]
            self.assertEqual(conf, [os.path.join(ctx, "packaging", "nginx",
                                                 "localrouter.conf")])
            self.assertEqual(report["edge_config"]["path"], conf[0])
            self.assertEqual(report["edge_config"]["sha256"],
                             hashlib.sha256(FakeWorld.EDGE_CONF.encode()).hexdigest())

    def test_real_git_head_move_and_live_edit_do_not_alter_pinned_source(self):
        # Real git repo, fake podman: HEAD moves and the live edge config is
        # edited after the source stage; the build context and the mounted
        # edge config remain the recorded commit's.
        sp = TestSourceProvenance()
        with tempfile.TemporaryDirectory() as tmp:
            repo = sp._repo(tmp, edge=True)
            pinned = sp._git(repo, "rev-parse", "HEAD")
            world = FakeWorld(real_git=True)
            h = new_harness(tmp)
            h.repo = repo
            real_export = mod.export_head_context

            def export_then_drift(r, dest, *, deadline, commit=None):
                sp._move_head(repo)
                pathlib.Path(repo, sp.EDGE).write_text("# LIVE EDIT\n")
                return real_export(r, dest, deadline=deadline, commit=commit)

            with mock.patch.dict(os.environ, sp.GIT_ENV), _Patched(world), \
                    mock.patch.object(mod, "export_head_context", export_then_drift):
                self.assertTrue(h.run_all())
            self.assertEqual(h.source["head"], pinned)
            self.assertEqual(pathlib.Path(h.context_dir, "main.go").read_text(),
                             "package main\n")
            self.assertEqual(pathlib.Path(h.edge_config["path"]).read_text(),
                             "# committed edge v1\n")
            nginx = [a for a in world.podman_runs() if "-p" in a][0]
            self.assertIn("%s:/etc/nginx/conf.d/default.conf:ro" % h.edge_config["path"],
                          nginx)

    def test_dirty_worktree_is_recorded_as_limitation(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(dirty=True)
            h = new_harness(tmp)
            with _Patched(world):
                h.run_all()
            self.assertTrue(h.source["worktree_dirty"])
            self.assertTrue(any("NOT in the image" in lim for lim in h.limitations))


# ---------------------------------------------------------------------------
# main(): report written, sanitized, schema, separate cleanup budget
# ---------------------------------------------------------------------------


class TestMain(unittest.TestCase):
    SECRETS = ("SECRETrouterKEY0123456789", "SECRETproviderKEY012345",
               "SECRETbackgroundKEY01234")

    def test_build_failure_exit1_report_written_and_sanitized(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(build_rc=1,
                              build_stderr="leak %s here" % self.SECRETS[0])
            rc, raw, _out = run_main(tmp, world, secrets=self.SECRETS)
            self.assertEqual(rc, 1)
            for s in self.SECRETS:
                self.assertNotIn(s, raw)
            self.assertIn("[REDACTED]", raw)
            report = json.loads(raw)
            self.assertEqual(report["outcome"], "fail")
            self.assertIn("fail-closed", report["error"])
            self.assertIsNone(report["image_under_test"])
            self.assertIsNone(report["router_image"])
            self.assertIsNone(report["image_matches_build"])
            self.assertFalse(report["fresh_build_proven"])
            self.assertEqual(report["owned"], [])
            self.assertEqual(report["remaining_resources"], [])
            self.assertEqual(report["build"]["network"], "none")

    def test_pass_cleans_up_by_id_and_keeps_audit(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld()
            rc, raw, _out = run_main(tmp, world, "--allow-build-network")
            report = json.loads(raw)
            self.assertEqual(rc, 0, report["gate_failures"])
            self.assertEqual(report["outcome"], "pass")
            kinds = sorted(e["kind"] for e in report["owned"])
            self.assertEqual(kinds, ["container"] * 3 + ["image", "network"])
            self.assertEqual(report["remaining_resources"], [])
            self.assertTrue(report["image_matches_build"])
            self.assertEqual(report["image_under_test"], BUILT_HEX)
            self.assertEqual(report["build"]["network"], "enabled")
            self.assertEqual(report["source"]["head"], "a" * 40)
            removals = [c for c, _ in world.calls
                        if c[:2] in (["podman", "rm"], ["podman", "rmi"])
                        or c[:3] == ["podman", "network", "rm"]]
            owned_ids = {e["id"] for e in report["owned"]}
            self.assertEqual({c[-1] for c in removals}, owned_ids)
            self.assertFalse(any(c[:3] == ["podman", "network", "rm"] and "-f" in c
                                 for c in removals))
            self.assertTrue(any(c["name"] == "cleanup.no_owned_leftovers" and c["ok"]
                                for c in report["checks"]))

    def test_cleanup_has_its_own_bounded_budget(self):
        def boom(cmd):
            raise subprocess.TimeoutExpired(cmd, 1)

        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(exec_hook=boom)
            rc, raw, _out = run_main(tmp, world, "--cleanup-timeout", "17")
            report = json.loads(raw)
            self.assertEqual(rc, 1)
            self.assertEqual(report["remaining_resources"], [])
            self.assertEqual(report["cleanup_timeout_seconds"], 17.0)
            cleanup_kws = [kw for c, kw in world.calls
                           if c[:2] in (["podman", "rm"], ["podman", "rmi"])
                           or c[:3] == ["podman", "network", "rm"]]
            self.assertTrue(cleanup_kws)
            for kw in cleanup_kws:
                self.assertEqual(kw["deadline"].limit, 17.0)

    def test_leftover_resource_fails_the_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(network_rm_rc=2)
            rc, raw, _out = run_main(tmp, world)
            report = json.loads(raw)
            self.assertEqual(rc, 1)
            self.assertEqual([e["kind"] for e in report["remaining_resources"]],
                             ["network"])
            self.assertIn("cleanup.no_owned_leftovers", report["gate_failures"])

    def _removals(self, world):
        return [c for c, _ in world.calls
                if c[:2] in (["podman", "rmi"], ["podman", "untag"])]

    def test_failed_build_leaving_owned_tag_is_adopted_and_rmi_by_id(self):
        fresh = "f" * 64
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(build_rc=1, failed_tag_id=fresh)
            rc, raw, _out = run_main(tmp, world)
            report = json.loads(raw)
            self.assertEqual(rc, 1)
            self.assertEqual(report["executed_stages"], ["host-preflight", "source", "build"])
            self.assertEqual(world.podman_runs(), [])
            self.assertEqual(report["owned"], [{
                "kind": "image", "id": fresh, "preexisting": False,
                "name": "localhost/localrouter-acc:%s" % mod.RUN_ID}])
            self.assertEqual(self._removals(world), [["podman", "rmi", fresh]])
            self.assertEqual(report["remaining_resources"], [])

    def test_failed_build_tag_on_shared_preexisting_id_is_only_untagged(self):
        shared = "5" * 64
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(build_rc=1, failed_tag_id=shared,
                              preexisting=(BASE_IMAGE_ID, "sha256:" + shared))
            rc, raw, _out = run_main(tmp, world)
            report = json.loads(raw)
            self.assertEqual(rc, 1)
            tag = "localhost/localrouter-acc:%s" % mod.RUN_ID
            self.assertEqual(self._removals(world), [["podman", "untag", shared, tag]])
            self.assertTrue(report["owned"][0]["preexisting"])
            self.assertEqual(report["remaining_resources"], [])

    def test_timed_out_build_iidfile_image_is_adopted_on_cleanup_budget(self):
        # The build used the whole run budget and was killed after writing
        # --iidfile but before tagging: adoption must run on the cleanup budget.
        fresh = "9" * 64
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(build_timeout=True, failed_iid="sha256:" + fresh)
            rc, raw, _out = run_main(tmp, world, "--cleanup-timeout", "23")
            report = json.loads(raw)
            self.assertEqual(rc, 1)
            self.assertIn("rc=124", json.dumps(report["checks"]))
            self.assertEqual(self._removals(world), [["podman", "rmi", fresh]])
            self.assertEqual([e["id"] for e in report["owned"]], [fresh])
            adopt_kws = [kw for c, kw in world.calls
                         if c[:3] in (["podman", "image", "exists"],
                                      ["podman", "image", "inspect"])
                         and (fresh in c or c[-1].startswith(TAG_PREFIX))]
            self.assertTrue(adopt_kws)
            self.assertTrue(all(kw["deadline"].limit == 23.0 for kw in adopt_kws[-2:]))

    def test_failed_build_iidfile_naming_shared_id_is_never_owned(self):
        shared = "6" * 64
        with tempfile.TemporaryDirectory() as tmp:
            world = FakeWorld(build_rc=1, failed_iid="sha256:" + shared,
                              preexisting=(BASE_IMAGE_ID, "sha256:" + shared))
            rc, raw, _out = run_main(tmp, world)
            report = json.loads(raw)
            self.assertEqual(rc, 1)
            self.assertEqual(report["owned"], [])
            self.assertEqual(self._removals(world), [])

    def test_podman_missing_exit2(self):
        out = io.StringIO()
        with mock.patch.object(mod, "podman_available", lambda: False), \
                redirect_stdout(out):
            self.assertEqual(mod.main(["--artifacts", "/nonexistent-never-created"]), 2)
        self.assertEqual(json.loads(out.getvalue())["outcome"], "error")

    def test_documented_report_schema_matches_code(self):
        text = DOC.read_text()
        m = re.search(r"`report\.json` shape:\s*```json\n(.*?)\n```", text, re.S)
        self.assertIsNotNone(m, "docs must contain the report.json shape block")
        documented = set(json.loads(m.group(1)).keys())
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            actual = set(mod.build_report(h, outcome="fail").keys())
        self.assertEqual(documented, actual)


# ---------------------------------------------------------------------------
# Outcome classification -- fail-closed, no PASS for cached smoke
# ---------------------------------------------------------------------------


class TestOutcomeClassification(unittest.TestCase):
    def test_cached_smoke_is_never_pass(self):
        checks = [{"name": "x", "ok": True, "severity": "gate"}]
        self.assertEqual(
            mod.classify_outcome(checks, cached_smoke=True),
            "cached_smoke_not_release_acceptance")

    def test_build_gate_failure_is_fail(self):
        checks = [{"name": "build.offline_nonetwork", "ok": False, "severity": "gate"}]
        self.assertEqual(mod.classify_outcome(checks), "fail")

    def test_limit_failure_only_degrades(self):
        checks = [{"name": "x", "ok": False, "severity": "limit"}]
        self.assertEqual(mod.classify_outcome(checks), "pass_with_limitations")

    def test_all_green_is_pass(self):
        checks = [{"name": "x", "ok": True, "severity": "gate"}]
        self.assertEqual(mod.classify_outcome(checks), "pass")

    def test_default_args_enable_build(self):
        self.assertTrue(mod.parse_args([]).build)
        self.assertFalse(mod.parse_args(["--no-build"]).build)


# ---------------------------------------------------------------------------
# Global runtime budget is shared with subprocess timeouts and probes
# ---------------------------------------------------------------------------


class _FakePopen:
    instances = []

    def __init__(self, cmd, **kw):
        self.cmd = cmd
        self.kw = kw
        self.pid = 424242
        self.returncode = None
        self.calls = 0
        _FakePopen.instances.append(self)

    def communicate(self, timeout=None):
        self.calls += 1
        self.last_timeout = timeout
        if self.cmd[0] == "hang" and self.calls == 1:
            raise subprocess.TimeoutExpired(self.cmd, timeout)
        self.returncode = 0 if self.cmd[0] != "hang" else -9
        return "", ""


class TestDeadlineBudget(unittest.TestCase):
    def setUp(self):
        _FakePopen.instances = []

    def test_run_clamps_timeout_to_deadline(self):
        deadline = mod.Deadline(10.0)
        with mock.patch.object(mod.subprocess, "Popen", _FakePopen):
            mod.run(["true"], timeout=100.0, deadline=deadline)
        p = _FakePopen.instances[0]
        self.assertLessEqual(p.last_timeout, 10.0)
        self.assertGreater(p.last_timeout, 0.0)
        self.assertTrue(p.kw.get("start_new_session"))

    def test_timeout_kills_the_whole_process_group(self):
        # The leader is reaped right after SIGTERM, but the group still answers
        # killpg(0): a survivor remains, so SIGKILL must follow regardless.
        killed = []
        with mock.patch.object(mod.subprocess, "Popen", _FakePopen), \
                mock.patch.object(mod, "KILL_GRACE", 0.2), \
                mock.patch.object(mod.os, "killpg",
                                  lambda pid, sig: killed.append((pid, sig))):
            with self.assertRaises(subprocess.TimeoutExpired):
                mod.run(["hang"], timeout=1.0)
        sent = [k for k in killed if k[1] != 0]
        self.assertEqual(sent, [(424242, signal.SIGTERM), (424242, signal.SIGKILL)])

    def test_run_refuses_when_budget_exhausted(self):
        deadline = mod.Deadline(0.0)
        with self.assertRaises(mod.AcceptanceError):
            mod.run(["true"], timeout=5.0, deadline=deadline)

    def test_deadline_check_raises_when_exceeded(self):
        d = mod.Deadline(0.0)
        with self.assertRaises(mod.AcceptanceError):
            d.check()

    def test_https_request_timeout_is_clamped(self):
        seen = {}

        class FakeConn:
            def __init__(self, host, port, context=None, timeout=None):
                seen["timeout"] = timeout

            def request(self, *a, **kw):
                pass

            def getresponse(self):
                class R:
                    status = 200

                    def read(self):
                        return b"{}"

                    def getheaders(self):
                        return []
                return R()

            def close(self):
                pass

        with mock.patch.object(mod.http.client, "HTTPSConnection", FakeConn):
            mod.https_request(1, "GET", "/readyz", deadline=mod.Deadline(2.0), timeout=15.0)
            self.assertLessEqual(seen["timeout"], 2.0)
            with self.assertRaises(mod.AcceptanceError):
                mod.https_request(1, "GET", "/readyz", deadline=mod.Deadline(0.0))

    def test_sse_probe_is_bounded_by_the_deadline(self):
        timeouts = []

        class FakeSock:
            def settimeout(self, t):
                timeouts.append(t)

        class FakeResp:
            status = 200
            n = 0

            def readline(self):
                FakeResp.n += 1
                if FakeResp.n > 400:   # safety valve for the RED run only
                    return b""
                time.sleep(0.002)
                return b'data: {"x": 1}\n'

        class FakeConn:
            def __init__(self, *a, **kw):
                self.sock = None

            def request(self, *a, **kw):
                self.sock = FakeSock()

            def getresponse(self):
                return FakeResp()

            def close(self):
                pass

        with mock.patch.object(mod.http.client, "HTTPSConnection", FakeConn):
            with self.assertRaises(mod.AcceptanceError):
                mod.sse_probe(1, "k", deadline=mod.Deadline(0.1), timeout=30.0)
        self.assertTrue(timeouts)
        self.assertTrue(all(t <= 0.1 for t in timeouts))


# ---------------------------------------------------------------------------
# Real process-group escalation (real processes, pid-file handshake)
# ---------------------------------------------------------------------------

# The survivor ignores SIGTERM, THEN publishes its pid: once the pid file exists
# the SIGTERM-ignoring state is guaranteed, so the test is deterministic.
SURVIVOR_SRC = (
    "import os, signal, sys, time\n"
    "signal.signal(signal.SIGTERM, signal.SIG_IGN)\n"
    "tmp = sys.argv[1] + '.tmp'\n"
    "with open(tmp, 'w') as fh:\n"
    "    fh.write(str(os.getpid()))\n"
    "os.rename(tmp, sys.argv[1])\n"
    "time.sleep(120)\n"
)


def _pid_dead(pid):
    try:
        with open("/proc/%d/stat" % pid) as fh:
            return fh.read().rsplit(")", 1)[1].split()[0] == "Z"
    except (FileNotFoundError, ProcessLookupError):   # gone mid-read
        return True


class TestRealKillEscalation(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.pidfile = os.path.join(self.tmp.name, "survivor.pid")
        # LIFO: the survivor is reaped (pid file read) before the dir goes.
        self.addCleanup(self.tmp.cleanup)
        self.addCleanup(self._reap_survivor)
        p = mock.patch.object(mod, "KILL_GRACE", 0.5)
        p.start()
        self.addCleanup(p.stop)

    def _reap_survivor(self):
        with contextlib.suppress(Exception):
            os.kill(int(pathlib.Path(self.pidfile).read_text()), signal.SIGKILL)

    def _cmd(self, *, detached, leader_exits):
        redirect = "</dev/null >/dev/null 2>&1" if detached else ""
        tail = "exit 0" if leader_exits else "exec sleep 120"
        script = ('"$1" -c "$2" "$3" %s & '
                  'while [ ! -s "$3" ]; do sleep 0.02; done; %s' % (redirect, tail))
        return ["sh", "-c", script, "sh", sys.executable, SURVIVOR_SRC, self.pidfile]

    def _survivor_pid(self):
        self.assertTrue(os.path.exists(self.pidfile),
                        "handshake never completed: survivor did not start in time")
        return int(pathlib.Path(self.pidfile).read_text())

    def _assert_dies(self, pid):
        end = time.monotonic() + 5.0
        while time.monotonic() < end and not _pid_dead(pid):
            time.sleep(0.05)
        self.assertTrue(_pid_dead(pid), "SIGTERM-ignoring group member %d survived" % pid)

    def test_timeout_kills_term_ignoring_detached_survivor(self):
        # Leader dies on SIGTERM and the survivor holds no pipe: communicate()
        # returns at once, yet SIGKILL must still reach the group.
        with self.assertRaises(subprocess.TimeoutExpired):
            mod.run(self._cmd(detached=True, leader_exits=False), timeout=3.0)
        self._assert_dies(self._survivor_pid())

    def test_timeout_kills_term_ignoring_pipe_holder(self):
        t0 = time.monotonic()
        with self.assertRaises(subprocess.TimeoutExpired):
            mod.run(self._cmd(detached=False, leader_exits=False), timeout=3.0)
        self._assert_dies(self._survivor_pid())
        self.assertLess(time.monotonic() - t0, 3.0 + 2 * 0.5 + 2.0)

    def test_escalates_even_when_the_leader_was_already_reaped(self):
        proc = subprocess.Popen(self._cmd(detached=True, leader_exits=True),
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                text=True, start_new_session=True)
        proc.communicate(timeout=10)
        self.assertEqual(proc.returncode, 0)          # leader reaped
        pid = self._survivor_pid()
        self.assertFalse(_pid_dead(pid))              # survivor detached + alive
        mod._kill_process_group(proc)
        self._assert_dies(pid)

    def test_obedient_group_returns_promptly(self):
        t0 = time.monotonic()
        with self.assertRaises(subprocess.TimeoutExpired):
            mod.run(["sh", "-c", "sleep 30 & exec sleep 30"], timeout=0.5)
        self.assertLess(time.monotonic() - t0, 0.5 + 0.5)


# ---------------------------------------------------------------------------
# Report schema / provenance surface
# ---------------------------------------------------------------------------


class TestReportSchema(unittest.TestCase):
    def test_build_report_schema_and_provenance(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            h.check("x", True, "d")
            report = mod.build_report(h, outcome="pass")
            for key in ("outcome", "checks", "commands", "executed_stages",
                        "limitations", "artifacts_dir", "checks_passed",
                        "checks_total", "owned", "remaining_resources",
                        "image_built_artifact", "built_image_id",
                        "fresh_build_proven", "image_matches_build", "source",
                        "build", "cached_smoke_not_release_acceptance"):
                self.assertIn(key, report)
            self.assertEqual(report["checks_passed"], 1)
            self.assertFalse(report["fresh_build_proven"])
            self.assertFalse(report["cached_smoke_not_release_acceptance"])

    def test_report_is_json_serializable(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = new_harness(tmp)
            report = mod.build_report(h, outcome="fail")
            json.dumps(report)  # must not raise


if __name__ == "__main__":
    unittest.main(verbosity=2)
