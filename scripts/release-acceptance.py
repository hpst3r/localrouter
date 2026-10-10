#!/usr/bin/env python3
"""Offline release-artifact acceptance harness for LocalRouter (native slice).

This harness validates the artifacts produced by the *existing* release
contract in ``scripts/release.sh``:

  * target set      linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
  * archive format  .tar.gz, member dir ``localrouter_<version>_<os>_<arch>/``
  * archive members ``localrouter`` (binary) + ``README.md`` (+ ``LICENSE`` if
                    present in the source tree)
  * manifest        ``SHA256SUMS`` with paths relative to the output directory
  * build env       CGO_ENABLED=0 GOOS/GOARCH GOMAXPROCS=2 GOFLAGS=-p=2

Build modes (script, mirror) first refuse a non-empty out-dir, then compile
only an immutable, content-verified snapshot of the committed tree (never
the live worktree); uncommitted Go/packaging inputs fail the run closed.

Order of checks (nothing is parsed, extracted, or executed before step 1):

  1. every ``SHA256SUMS`` entry is hashed and the selected archives are
     copied into a private snapshot while being hashed;
  2. each snapshot is checked for strict archive shape, ELF/Mach-O header
     arch, Go build info GOOS/GOARCH, and LocalRouter build identity;
  3. only the host target is executed, from the verified snapshot;
  4. the out-dir archives are re-hashed to prove they did not change.

It is deliberately dependency-free (Python standard library only) so it can run
wherever Python 3 is available, and it never fabricates results: if a build or
verification step fails, the harness reports the failure and exits non-zero.
Every run writes a JSON report (pass or fail).

Scope: native release artifacts ONLY. It does not touch CI, the Containerfile,
container images, deployment units, or any network/provider/key configuration.

Run ``python3 scripts/release-acceptance.py --help`` for usage.
"""
from __future__ import annotations

import argparse
import datetime
import gzip
import hashlib
import json
import os
import platform
import posixpath
import re
import secrets
import shutil
import signal
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import zlib
from collections import namedtuple

# --- release contract (must stay in lock-step with scripts/release.sh) -------
BIN_NAME = "localrouter"
PKG = "./cmd/localrouter"
MAIN_SUBPATH = "cmd/localrouter"
ARCHIVE_EXT = ".tar.gz"
DEFAULT_TARGETS = ("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64")
DEFAULT_ARCHES = "host,arm64"          # first bounded slice: host + linux/arm64
MAX_JOBS = 2
REQUIRED_MEMBERS = (BIN_NAME, "README.md")
OPTIONAL_MEMBERS = ("LICENSE",)

# --- resource bounds ----------------------------------------------------------
MAX_ARCHIVE_BYTES = 512 << 20
MAX_BINARY_BYTES = 256 << 20
MAX_DOC_BYTES = 4 << 20
MAX_MANIFEST_BYTES = 64 << 10
MAX_MEMBERS = 16
# decompressed tar stream cap: binary + docs + headers (bounds pax/GNU long
# headers too, which tarfile reads into memory before any member check)
MAX_EXPANDED_BYTES = MAX_BINARY_BYTES + 2 * MAX_DOC_BYTES + (1 << 20)
OUTPUT_CAP = 64 << 10
DEFAULT_MAX_RUNTIME = 1800
TIMEOUT_RELEASE_SH = 1800
TIMEOUT_GO_BUILD = 900
TIMEOUT_BUILDINFO = 60
TIMEOUT_HOST_RUN = 30
TIMEOUT_GIT = 30
TIMEOUT_GIT_CLONE = 300
MAX_SOURCE_FILE_BYTES = 64 << 20
MAX_TREE_LISTING = 16 << 20
MAX_REPORTED_PATHS = 500

# Paths (relative to the module root) whose change alters a release build
# besides Go package directories; see is_build_input.
GO_MODULE_FILES = ("go.mod", "go.sum", "go.work", "go.work.sum")
PACKAGING_INPUTS = ("README.md", "LICENSE", "scripts/release.sh")

# Offline, bounded build environment layered over the caller's environment
# for both release.sh and the mirror build.
BUILD_ENV = {"GOMAXPROCS": "2", "GOFLAGS": "-p=2", "GOPROXY": "off",
             "GOTOOLCHAIN": "local"}
NON_DURABLE_ROOTS = ("/tmp", "/private/tmp", "/dev/shm")
SCRATCH_SUBDIR = ".acceptance-scratch"

MODE_LABELS = {"script": "release_sh", "none": "verify_only",
               "mirror": "contract_mirror_smoke"}

ELF_MACHINE = {"amd64": 0x3E, "arm64": 0xB7, "386": 0x03, "arm": 0x28}
_MACHINE_TO_ARCH = {v: k for k, v in ELF_MACHINE.items()}
ELF_OSABI_LINUX = (0, 3)               # SYSV (what Go emits) or GNU/Linux
MACHO_MAGICS = (0xFEEDFACE, 0xFEEDFACF, 0xCEFAEDFE, 0xCFFAEDFE,
                0xCAFEBABE, 0xBEBAFECA)
MACHO_CPUTYPE = {0x01000007: "amd64", 0x0100000C: "arm64"}
MH_MAGIC_64 = 0xFEEDFACF
MH_EXECUTE = 2

_ARCHIVE_RE = re.compile(r"^localrouter_(.+)_(linux|darwin)_(amd64|arm64)\.tar\.gz$")
_SUMS_LINE_RE = re.compile(r"^([0-9a-f]{64}) [ *](.+)$")


class AcceptanceError(Exception):
    """Raised when an artifact, manifest, or invocation fails acceptance."""


# --------------------------------------------------------------------------
# bounded subprocesses
# --------------------------------------------------------------------------
class Deadline:
    """Global wall-clock budget; every subprocess timeout is clamped to it."""

    def __init__(self, seconds):
        self.seconds = seconds
        self.expires = time.monotonic() + seconds

    def remaining(self):
        return self.expires - time.monotonic()

    def clamp(self, timeout, what):
        left = self.remaining()
        if left <= 0:
            raise AcceptanceError("global deadline (%ss) exceeded before %s"
                                  % (self.seconds, what))
        return min(timeout, left)


CmdResult = namedtuple("CmdResult", "returncode stdout stderr seconds truncated")


TERM_GRACE = 2.0      # seconds a group gets to exit after SIGTERM
KILL_WAIT = 5.0       # bound on waiting for the group to vanish after SIGKILL
_POLL_MAX = 0.02
_HAVE_WAITID = hasattr(os, "waitid") and hasattr(os, "WNOWAIT")


def _leader_exited(proc):
    """True once the group leader has exited, *without* reaping it.

    An unreaped (zombie) leader keeps its PID, and with it the process group
    ID, reserved: no unrelated process or group can take that number until
    the leader is reaped, so killpg cannot reach a foreign group. Without
    os.waitid (e.g. macOS before Python 3.13) the leader is reaped here.
    """
    if _HAVE_WAITID:
        try:
            return os.waitid(os.P_PID, proc.pid,
                             os.WEXITED | os.WNOHANG | os.WNOWAIT) is not None
        except ChildProcessError:
            return True
    return proc.poll() is not None


def _group_members(pgid):
    """Live (non-zombie) PIDs in process group ``pgid``; None if unknown."""
    try:
        names = os.listdir("/proc")
    except OSError:
        names = None
    if names is not None and os.path.exists("/proc/self/stat"):
        live = []
        for name in names:
            if not name.isdigit():
                continue
            try:
                with open("/proc/%s/stat" % name, "rb") as f:
                    data = f.read()
            except OSError:
                continue
            # "pid (comm) state ppid pgrp ..."; comm may contain spaces/parens
            fields = data[data.rfind(b")") + 2:].split()
            if len(fields) >= 3 and int(fields[2]) == pgid and fields[0] != b"Z":
                live.append(int(name))
        return live
    try:
        res = subprocess.run(["ps", "-A", "-o", "pid=,pgid=,stat="],
                             stdin=subprocess.DEVNULL, capture_output=True,
                             text=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        return None
    if res.returncode != 0:
        return None
    live = []
    for line in res.stdout.splitlines():
        f = line.split()
        if len(f) >= 3 and f[1] == str(pgid) and not f[2].startswith("Z"):
            live.append(int(f[0]))
    return live


def _wait_drained(pgid, limit, unknown_wait):
    end = time.monotonic() + limit
    while True:
        members = _group_members(pgid)
        if members == []:
            return True
        left = end - time.monotonic()
        if left <= 0:
            return False
        if members is None:
            time.sleep(min(unknown_wait, left))
            return False
        time.sleep(min(0.05, left))


def _kill_group(proc, grace=TERM_GRACE):
    """Kill everything left in proc's process group, then reap the leader.

    SIGTERM goes to the group first (skipped when no live member is left),
    the group gets up to ``grace`` seconds to drain, then SIGKILL goes to the
    whole group unconditionally, so members that ignore SIGTERM or detached
    their stdio die as well. Only this command's own group is signalled, and
    only while its leader is still unreaped (see _leader_exited). A member
    that moved itself to another session or group is out of reach.
    """
    pgid = proc.pid

    def signal_group(sig):
        try:
            os.killpg(pgid, sig)
        except (ProcessLookupError, PermissionError):
            pass

    members = _group_members(pgid)
    if members is None or members:
        signal_group(signal.SIGTERM)
        _wait_drained(pgid, grace, unknown_wait=grace)
    signal_group(signal.SIGKILL)
    _wait_drained(pgid, KILL_WAIT, unknown_wait=0.1)
    try:
        proc.wait(timeout=KILL_WAIT)
    except subprocess.TimeoutExpired:
        pass


def run_cmd(argv, timeout, deadline=None, cwd=None, env=None,
            output_cap=OUTPUT_CAP):
    """Run argv in its own process group with a hard timeout.

    Output is drained continuously but only the first ``output_cap`` bytes of
    each stream are kept. When the leader exits, or on timeout, whatever is
    left in the group is killed before the leader is reaped. Every failure to
    run (missing binary, noexec, timeout) is an AcceptanceError.
    """
    what = os.path.basename(str(argv[0]))
    if deadline is not None:
        timeout = deadline.clamp(timeout, what)
    start = time.monotonic()
    try:
        proc = subprocess.Popen(argv, cwd=cwd, env=env,
                                stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                start_new_session=True)
    except OSError as e:
        raise AcceptanceError("cannot run %s: %s" % (argv[0], e))

    bufs = [bytearray(), bytearray()]
    truncated = [False, False]

    def drain(stream, i):
        try:
            while True:
                chunk = stream.read(1 << 16)
                if not chunk:
                    break
                room = output_cap - len(bufs[i])
                if room > 0:
                    bufs[i] += chunk[:room]
                if len(chunk) > max(room, 0):
                    truncated[i] = True
        finally:
            stream.close()

    threads = [threading.Thread(target=drain, args=(proc.stdout, 0), daemon=True),
               threading.Thread(target=drain, args=(proc.stderr, 1), daemon=True)]
    for t in threads:
        t.start()
    end = start + timeout
    timed_out = False
    delay = 0.001
    while not _leader_exited(proc):
        left = end - time.monotonic()
        if left <= 0:
            timed_out = True
            break
        time.sleep(min(delay, left))
        delay = min(delay * 2, _POLL_MAX)
    # Anything still in the group now is ours: the timed-out command, or a
    # leftover background child (even one ignoring SIGTERM or with detached
    # stdio). The group is always cleaned up before the leader is reaped.
    _kill_group(proc)
    for t in threads:
        t.join(timeout=5)
    if timed_out:
        raise AcceptanceError("%s timed out after %.0fs (process group killed)"
                              % (what, timeout))
    return CmdResult(proc.returncode,
                     bytes(bufs[0]).decode("utf-8", "replace"),
                     bytes(bufs[1]).decode("utf-8", "replace"),
                     round(time.monotonic() - start, 3), any(truncated))


# --------------------------------------------------------------------------
# contract helpers
# --------------------------------------------------------------------------
def normalize_version(version: str) -> str:
    return version[1:] if version.startswith("v") else version


def archive_name(version: str, goos: str, goarch: str) -> str:
    return "%s_%s_%s_%s%s" % (BIN_NAME, normalize_version(version), goos,
                              goarch, ARCHIVE_EXT)


def archive_member_dir(version: str, goos: str, goarch: str) -> str:
    return archive_name(version, goos, goarch)[: -len(ARCHIVE_EXT)]


def parse_archive_name(name: str):
    """Return (version, goos, goarch) for a contract archive basename."""
    m = _ARCHIVE_RE.match(name)
    if m is None or "/" in name:
        raise AcceptanceError("%r is not a release archive name" % name)
    return m.group(1), m.group(2), m.group(3)


def validate_target(target: str) -> str:
    if target not in DEFAULT_TARGETS:
        raise AcceptanceError(
            "target %r is outside the release.sh contract %s"
            % (target, ", ".join(DEFAULT_TARGETS)))
    return target


def detect_host_target():
    sysname = platform.system().lower()
    goos = {"linux": "linux", "darwin": "darwin"}.get(sysname)
    if goos is None:
        raise AcceptanceError("unsupported host OS for host slice: %s" % sysname)
    machine = platform.machine().lower()
    goarch = {"x86_64": "amd64", "amd64": "amd64",
              "aarch64": "arm64", "arm64": "arm64"}.get(machine)
    if goarch is None:
        raise AcceptanceError("unsupported host arch for host slice: %s" % machine)
    return (goos, goarch)


def resolve_arches(spec: str, host_target):
    """Expand an arch spec into an ordered list of (goos, goarch) tuples.

    Accepts ``all`` (the full release.sh target set), ``host`` (the host's own
    target), ``arm64`` (linux/arm64), or explicit ``os/arch`` names. The default
    ``host,arm64`` bounds the first slice to two targets.
    """
    out = []
    for raw in spec.split(","):
        tok = raw.strip()
        if not tok:
            continue
        if tok == "all":
            for t in DEFAULT_TARGETS:
                pair = tuple(t.split("/"))
                if pair not in out:
                    out.append(pair)
        elif tok == "host":
            if tuple(host_target) not in out:
                out.append(tuple(host_target))
        elif tok == "arm64":
            if ("linux", "arm64") not in out:
                out.append(("linux", "arm64"))
        else:
            pair = tuple(validate_target(tok).split("/"))
            if pair not in out:
                out.append(pair)
    if not out:
        raise AcceptanceError("no valid targets resolved from %r" % spec)
    return out


def check_durable(path: str, what: str) -> str:
    """Refuse relative paths and tmpfs-style locations (also via symlinks)."""
    if not os.path.isabs(path):
        raise AcceptanceError("%s must be an absolute path" % what)
    for candidate in (path, os.path.realpath(path)):
        norm = candidate.rstrip("/") or "/"
        for root in NON_DURABLE_ROOTS:
            if norm == root or norm.startswith(root + "/"):
                raise AcceptanceError("refusing %s under %s; use durable storage"
                                      % (what, root))
    return path


# --------------------------------------------------------------------------
# hashing / manifest
# --------------------------------------------------------------------------
def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _open_regular(path: str):
    """Open a regular file without following a final symlink."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC)
    except FileNotFoundError:
        raise AcceptanceError("missing file: %s" % path)
    except OSError as e:
        raise AcceptanceError("cannot open %s (symlink or unreadable): %s"
                              % (path, e))
    st = os.fstat(fd)
    if not stat.S_ISREG(st.st_mode):
        os.close(fd)
        raise AcceptanceError("%s is not a regular file" % path)
    return fd


def hash_file(path: str, max_bytes: int, copy_to: str = None):
    """SHA-256 a regular file in one bounded pass, optionally copying the
    exact bytes hashed into ``copy_to`` (created exclusively, mode 0600)."""
    fd = _open_regular(path)
    h = hashlib.sha256()
    total = 0
    out = None
    try:
        if copy_to is not None:
            out = os.open(copy_to, os.O_WRONLY | os.O_CREAT | os.O_EXCL
                          | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
        while True:
            chunk = os.read(fd, 1 << 20)
            if not chunk:
                break
            total += len(chunk)
            if total > max_bytes:
                raise AcceptanceError("%s exceeds the %d-byte limit"
                                      % (os.path.basename(path), max_bytes))
            h.update(chunk)
            if out is not None:
                view = memoryview(chunk)
                while view:
                    view = view[os.write(out, view):]
    finally:
        os.close(fd)
        if out is not None:
            os.close(out)
    return h.hexdigest(), total


def parse_sha256sums(text: str) -> dict:
    entries = {}
    for lineno, line in enumerate(text.splitlines(), 1):
        if not line.strip():
            continue
        m = _SUMS_LINE_RE.match(line)
        if m is None:
            raise AcceptanceError("SHA256SUMS line %d is malformed: %r"
                                  % (lineno, line))
        digest, name = m.group(1), m.group(2)
        if name in entries:
            raise AcceptanceError("SHA256SUMS line %d duplicates %r"
                                  % (lineno, name))
        entries[name] = digest
    if not entries:
        raise AcceptanceError("SHA256SUMS has no entries")
    return entries


def manifest_target(name: str, version: str = None) -> str:
    """Validate one SHA256SUMS name: a plain contract archive basename."""
    if (name != os.path.basename(name) or "\\" in name or "\0" in name
            or name.startswith(".")):
        raise AcceptanceError("SHA256SUMS entry %r is not a plain file name" % name)
    ver, goos, goarch = parse_archive_name(name)
    target = validate_target("%s/%s" % (goos, goarch))
    if version is not None and name != archive_name(version, goos, goarch):
        raise AcceptanceError("SHA256SUMS entry %r does not match version %s"
                              % (name, normalize_version(version)))
    return target


def write_manifest(out_dir: str, archives) -> str:
    path = os.path.join(out_dir, "SHA256SUMS")
    lines = ["%s  %s" % (sha256_file(a), os.path.basename(a)) for a in archives]
    with _create_exclusive(path, "w") as f:
        f.write("\n".join(lines) + "\n")
    return path


def verify_manifest(out_dir: str, archives, version: str = None,
                    snapshot_dir: str = None) -> dict:
    """Check SHA256SUMS before anything else looks at the archives.

    Every manifest entry must be a contract archive name and must hash to its
    listed digest (release.sh writes all four targets; a target subset may be
    selected, but unselected entries are still verified). Every selected
    archive must be listed. When ``snapshot_dir`` is given, the selected
    archives are copied there in the same pass that hashes them, so later
    steps work on exactly the verified bytes.
    """
    manifest = os.path.join(out_dir, "SHA256SUMS")
    if not os.path.lexists(manifest):
        raise AcceptanceError("missing SHA256SUMS in %s" % out_dir)
    fd = _open_regular(manifest)
    try:
        raw = os.read(fd, MAX_MANIFEST_BYTES + 1)
    finally:
        os.close(fd)
    if len(raw) > MAX_MANIFEST_BYTES:
        raise AcceptanceError("SHA256SUMS exceeds %d bytes" % MAX_MANIFEST_BYTES)
    try:
        entries = parse_sha256sums(raw.decode("utf-8"))
    except UnicodeDecodeError:
        raise AcceptanceError("SHA256SUMS is not UTF-8 text")
    targets = {name: manifest_target(name, version) for name in entries}

    selected = set()
    for a in archives:
        name = os.path.basename(a)
        if os.path.dirname(os.path.abspath(a)) != os.path.abspath(out_dir):
            raise AcceptanceError("%s is not in %s" % (a, out_dir))
        if name not in entries:
            raise AcceptanceError("%s is not listed in SHA256SUMS" % name)
        selected.add(name)

    result = {"path": manifest, "sha256": hashlib.sha256(raw).hexdigest(),
              "entries": {}}
    for name in sorted(entries):
        path = os.path.join(out_dir, name)
        if not os.path.lexists(path):
            raise AcceptanceError("SHA256SUMS lists %s but it is missing" % name)
        snap = (os.path.join(snapshot_dir, name)
                if snapshot_dir is not None and name in selected else None)
        actual, size = hash_file(path, MAX_ARCHIVE_BYTES, copy_to=snap)
        if actual != entries[name]:
            raise AcceptanceError("checksum mismatch for %s: manifest=%s actual=%s"
                                  % (name, entries[name], actual))
        result["entries"][name] = {"target": targets[name], "sha256": actual,
                                   "size": size, "selected": name in selected,
                                   "snapshot": snap}
    return result


def recheck_manifest(out_dir: str, manifest: dict):
    """Fail if SHA256SUMS or any listed archive changed since verification."""
    path = os.path.join(out_dir, "SHA256SUMS")
    actual, _ = hash_file(path, MAX_MANIFEST_BYTES)
    if actual != manifest["sha256"]:
        raise AcceptanceError("SHA256SUMS changed during acceptance")
    for name, e in manifest["entries"].items():
        actual, _ = hash_file(os.path.join(out_dir, name), MAX_ARCHIVE_BYTES)
        if actual != e["sha256"]:
            raise AcceptanceError("%s changed during acceptance (verified %s, "
                                  "now %s)" % (name, e["sha256"], actual))


# --------------------------------------------------------------------------
# archive shape
# --------------------------------------------------------------------------
_TAR_TYPES = {tarfile.REGTYPE: "file", tarfile.AREGTYPE: "file",
              tarfile.DIRTYPE: "dir", tarfile.SYMTYPE: "symlink",
              tarfile.LNKTYPE: "hardlink", tarfile.CHRTYPE: "chardev",
              tarfile.BLKTYPE: "blockdev", tarfile.FIFOTYPE: "fifo",
              tarfile.CONTTYPE: "contiguous", tarfile.GNUTYPE_SPARSE: "sparse"}


class _BoundedStream:
    """Seekable view of a decompressed stream that refuses to go past limit."""

    def __init__(self, raw, limit, label):
        self.raw, self.limit, self.label = raw, limit, label

    def _check(self, end):
        if end > self.limit:
            raise AcceptanceError("%s expands beyond the %d-byte limit"
                                  % (self.label, self.limit))

    def read(self, n=-1):
        pos = self.raw.tell()
        if n is None or n < 0:
            n = self.limit - pos + 1
        self._check(pos + n)
        return self.raw.read(n)

    def seek(self, pos, whence=0):
        target = {0: pos, 1: self.raw.tell() + pos}.get(whence)
        if target is None:
            raise AcceptanceError("%s: unsupported seek" % self.label)
        self._check(target)
        return self.raw.seek(target)

    def tell(self):
        return self.raw.tell()


def _extract_regular(tf, ti, dest_dir, cap):
    src = tf.extractfile(ti)
    out = os.path.join(dest_dir, BIN_NAME)
    fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
                 | os.O_CLOEXEC, 0o600)
    h = hashlib.sha256()
    total = 0
    with os.fdopen(fd, "wb") as fh:
        while True:
            chunk = src.read(1 << 20)
            if not chunk:
                break
            total += len(chunk)
            if total > cap:
                raise AcceptanceError("%s exceeds the %d-byte limit"
                                      % (ti.name, cap))
            h.update(chunk)
            fh.write(chunk)
    if total != ti.size:
        raise AcceptanceError("%s is truncated (%d of %d bytes)"
                              % (ti.name, total, ti.size))
    os.chmod(out, 0o500)   # owner read/exec only; nothing may rewrite it
    return out, h.hexdigest(), total


def scan_archive(archive_path: str, version: str, goos: str, goarch: str,
                 extract_dir: str = None) -> dict:
    """Strictly validate one archive and optionally extract its binary.

    Exactly one member directory plus regular files from the allowlist
    (localrouter, README.md, optional LICENSE); no links, devices,
    duplicates, nesting, or special mode bits; the binary must carry an
    executable mode; every member is size-capped and streamed. Any tar/gzip
    failure is an AcceptanceError.
    """
    expected_name = archive_name(version, goos, goarch)
    if os.path.basename(archive_path) != expected_name:
        raise AcceptanceError("archive name %s does not match target %s/%s (%s)"
                              % (os.path.basename(archive_path), goos, goarch,
                                 expected_name))
    member_dir = archive_member_dir(version, goos, goarch)
    allowed = set(REQUIRED_MEMBERS) | set(OPTIONAL_MEMBERS)
    seen = set()
    members = []
    out = {"archive": expected_name, "binary": None, "binary_sha256": None,
           "binary_size": None}
    try:
        with gzip.open(archive_path, "rb") as gz, \
                tarfile.open(fileobj=_BoundedStream(gz, MAX_EXPANDED_BYTES,
                                                    expected_name),
                             mode="r:") as tf:
            for ti in tf:
                if len(members) >= MAX_MEMBERS:
                    raise AcceptanceError("%s has more than %d members"
                                          % (expected_name, MAX_MEMBERS))
                kind = _TAR_TYPES.get(ti.type, repr(ti.type))
                members.append({"name": ti.name, "type": kind,
                                "mode": "%04o" % ti.mode, "size": ti.size})
                norm = ti.name.rstrip("/")
                if norm == member_dir:
                    rel = ""
                    if not ti.isdir():
                        raise AcceptanceError(
                            "%s member dir %r is not a directory (%s)"
                            % (expected_name, ti.name, kind))
                else:
                    prefix = member_dir + "/"
                    if not norm.startswith(prefix):
                        raise AcceptanceError(
                            "%s contains %r outside expected member dir %r"
                            % (expected_name, ti.name, member_dir))
                    rel = norm[len(prefix):]
                    if "/" in rel:
                        raise AcceptanceError(
                            "%s contains an unexpected nested path %r"
                            % (expected_name, ti.name))
                    if rel not in allowed:
                        raise AcceptanceError("%s contains unexpected member %r"
                                              % (expected_name, ti.name))
                if rel in seen:
                    raise AcceptanceError("%s has a duplicate member %r"
                                          % (expected_name, ti.name))
                seen.add(rel)
                if rel == "":
                    continue
                if ti.type not in (tarfile.REGTYPE, tarfile.AREGTYPE):
                    raise AcceptanceError("%s member %r is not a regular file (%s)"
                                          % (expected_name, ti.name, kind))
                if ti.mode & 0o7000:
                    raise AcceptanceError(
                        "%s member %r has setuid/setgid/sticky mode %04o"
                        % (expected_name, ti.name, ti.mode))
                cap = MAX_BINARY_BYTES if rel == BIN_NAME else MAX_DOC_BYTES
                if ti.size > cap:
                    raise AcceptanceError("%s member %r size %d exceeds the "
                                          "%d-byte limit"
                                          % (expected_name, ti.name, ti.size, cap))
                if rel == BIN_NAME:
                    if not ti.mode & 0o100:
                        raise AcceptanceError(
                            "%s binary %r is not executable in the archive "
                            "(mode %04o)" % (expected_name, ti.name, ti.mode))
                    if extract_dir is not None:
                        (out["binary"], out["binary_sha256"],
                         out["binary_size"]) = _extract_regular(tf, ti,
                                                                extract_dir, cap)
    except AcceptanceError:
        raise
    except (tarfile.TarError, EOFError, zlib.error, OSError, ValueError) as e:
        raise AcceptanceError("%s could not be read as .tar.gz: %s"
                              % (expected_name, e))
    if "" not in seen:
        raise AcceptanceError("%s has no member dir entry %r"
                              % (expected_name, member_dir))
    for req in REQUIRED_MEMBERS:
        if req not in seen:
            raise AcceptanceError("%s is missing required member %r"
                                  % (expected_name, req))
    out["members"] = members
    out["files"] = sorted(seen - {""})
    return out


def verify_archive(archive_path: str, version: str, goos: str, goarch: str) -> dict:
    if not os.path.isfile(archive_path):
        raise AcceptanceError("missing archive: %s" % archive_path)
    info = scan_archive(archive_path, version, goos, goarch)
    return {"archive": info["archive"],
            "members": [m["name"] for m in info["members"]],
            "files": info["files"]}


def extract_member(archive_path: str, member: str, dest_dir: str) -> str:
    """Strictly validate the archive and extract its binary into dest_dir."""
    if member != BIN_NAME:
        raise AcceptanceError("only %r is ever extracted" % BIN_NAME)
    version, goos, goarch = parse_archive_name(os.path.basename(archive_path))
    return scan_archive(archive_path, version, goos, goarch,
                        extract_dir=dest_dir)["binary"]


# --------------------------------------------------------------------------
# arch / identity proofs (never execute the binary)
# --------------------------------------------------------------------------
def elf_machine(path: str):
    """Return the ELF e_machine integer, or None if the file is not ELF."""
    with open(path, "rb") as f:
        head = f.read(20)
    if len(head) < 20 or head[:4] != b"\x7fELF":
        return None
    data_enc = head[5]
    endian = "<" if data_enc == 1 else ">"
    return struct.unpack(endian + "H", head[18:20])[0]


def elf_machine_arch(machine):
    if machine is None:
        return None
    return _MACHINE_TO_ARCH.get(machine)


def macho_magic(path: str):
    with open(path, "rb") as f:
        head = f.read(4)
    if len(head) < 4:
        return None
    return struct.unpack(">I", head)[0]


def binary_header(path: str) -> dict:
    """Read GOOS/GOARCH straight from the ELF or Mach-O header.

    Only the 64-bit little-endian forms Go emits for the contract targets map
    to an arch; anything else yields goarch None (a mismatch, never a guess).
    """
    with open(path, "rb") as f:
        head = f.read(64)
    if head[:4] == b"\x7fELF":
        if len(head) < 20:
            raise AcceptanceError("truncated ELF header")
        ei_class, ei_data, ei_osabi = head[4], head[5], head[7]
        endian = "<" if ei_data == 1 else ">"
        e_type, e_machine = struct.unpack(endian + "HH", head[16:20])
        sixty_four_le = ei_class == 2 and ei_data == 1
        return {"format": "elf",
                "goos": "linux" if ei_osabi in ELF_OSABI_LINUX else None,
                "goarch": _MACHINE_TO_ARCH.get(e_machine) if sixty_four_le else None,
                "elf_class": ei_class, "elf_data": ei_data,
                "elf_osabi": ei_osabi, "elf_type": e_type,
                "elf_machine": e_machine}
    if len(head) >= 4 and struct.unpack(">I", head[:4])[0] in MACHO_MAGICS:
        info = {"format": "macho", "goos": "darwin", "goarch": None,
                "macho_magic": "0x%08X" % struct.unpack("<I", head[:4])[0]}
        if struct.unpack("<I", head[:4])[0] == MH_MAGIC_64 and len(head) >= 16:
            cputype, _sub, filetype = struct.unpack("<III", head[4:16])
            info.update({"macho_cputype": "0x%08X" % cputype,
                         "macho_filetype": filetype})
            if filetype == MH_EXECUTE:
                info["goarch"] = MACHO_CPUTYPE.get(cputype)
        return info
    raise AcceptanceError("not an ELF or Mach-O binary")


def go_buildinfo(path: str, go_bin: str = "go", deadline=None):
    """Parse ``go version -m <binary>`` into a dict (None if unavailable).

    ``go version -m`` only reads the file; it never runs it.
    """
    exe = shutil.which(go_bin)
    if exe is None:
        return None
    env = os.environ.copy()
    env.update({"GOTOOLCHAIN": "local", "GOFLAGS": ""})
    res = run_cmd([exe, "version", "-m", path], timeout=TIMEOUT_BUILDINFO,
                  deadline=deadline, cwd=os.path.dirname(path) or None, env=env)
    if res.returncode != 0:
        return None
    info = {"goos": None, "goarch": None, "path": None, "mod": None,
            "go_version": None, "settings": {}}
    for line in res.stdout.splitlines():
        if not line.startswith("\t"):
            if ": " in line:
                info["go_version"] = line.rsplit(": ", 1)[1].strip()
            continue
        fields = line.strip("\t").split("\t")
        if len(fields) < 2:
            continue
        key, val = fields[0].strip(), fields[1].strip()
        if key == "path":
            info["path"] = val
        elif key == "mod":
            info["mod"] = val
        elif key == "build" and "=" in val:
            k, v = val.split("=", 1)
            info["settings"][k] = v
    info["goos"] = info["settings"].get("GOOS")
    info["goarch"] = info["settings"].get("GOARCH")
    return info


def verify_identity(bi: dict, expected: dict, label: str) -> dict:
    """The binary must be LocalRouter's main package, built per release.sh
    (CGO_ENABLED=0, -trimpath) from the expected source revision."""
    s = bi.get("settings") or {}
    if bi.get("path") != expected["path"]:
        raise AcceptanceError("%s build info main path is %r, expected %r"
                              % (label, bi.get("path"), expected["path"]))
    if bi.get("mod") != expected["module"]:
        raise AcceptanceError("%s build info module is %r, expected %r"
                              % (label, bi.get("mod"), expected["module"]))
    if s.get("CGO_ENABLED") != "0":
        raise AcceptanceError("%s was built with CGO_ENABLED=%s, release "
                              "contract is 0" % (label, s.get("CGO_ENABLED")))
    if s.get("-trimpath") != "true":
        raise AcceptanceError("%s was not built with -trimpath" % label)
    rev = s.get("vcs.revision")
    if rev:
        if rev != expected["revision"]:
            raise AcceptanceError("%s was built from revision %s but the source "
                                  "revision is %s"
                                  % (label, rev, expected["revision"]))
        if (expected.get("require_unmodified")
                and s.get("vcs.modified") != "false"):
            raise AcceptanceError("%s build info says vcs.modified=%s, but this "
                                  "run built it from a clean snapshot of %s"
                                  % (label, s.get("vcs.modified"), rev))
        revision_source = "vcs_stamp"
    elif expected.get("allow_unstamped"):
        # Go leaves vcs.* out when it finds no usable .git directory (or with
        # -buildvcs=false). Accepted only when this run built the artifact
        # itself from a verified snapshot of the recorded revision.
        revision_source = "build_provenance"
    else:
        raise AcceptanceError("%s build info has no vcs.revision; cannot tie it "
                              "to the source" % label)
    return {"revision_source": revision_source,
            "path": bi.get("path"), "mod": bi.get("mod"),
            "go_version": bi.get("go_version"), "CGO_ENABLED": s.get("CGO_ENABLED"),
            "-trimpath": s.get("-trimpath"), "vcs.revision": rev,
            "vcs.modified": s.get("vcs.modified"), "vcs.time": s.get("vcs.time")}


def prove_binary(binary: str, goos: str, goarch: str, use_buildinfo: bool = True,
                 identity: dict = None, deadline=None, label: str = None) -> dict:
    """Independent arch proofs; any disagreement fails.

    The ELF/Mach-O header is always read. Go build info (GOOS and GOARCH) is
    required unless ``use_buildinfo`` is False, and the LocalRouter identity
    is checked when ``identity`` is given.
    """
    label = label or os.path.basename(binary)
    proof = {"archive": label, "expected": "%s/%s" % (goos, goarch),
             "sources": [], "header": None, "machine": None, "arch": None,
             "buildinfo": None}
    hdr = binary_header(binary)
    proof["header"] = hdr
    proof["machine"] = hdr.get("elf_machine")
    if hdr.get("goos") != goos:
        raise AcceptanceError("%s header is %s (GOOS %s) but target is %s/%s"
                              % (label, hdr.get("format"), hdr.get("goos"),
                                 goos, goarch))
    if hdr.get("goarch") != goarch:
        raise AcceptanceError("%s %s header arch is %s (%s) but target is %s/%s"
                              % (label, hdr.get("format"), hdr.get("goarch"),
                                 hdr.get("elf_machine", hdr.get("macho_cputype")),
                                 goos, goarch))
    proof["arch"] = hdr["goarch"]
    proof["sources"].append("header")
    if not use_buildinfo:
        return proof
    bi = go_buildinfo(binary, deadline=deadline)
    if not bi:
        raise AcceptanceError("%s: no Go build info (`go version -m` failed or go "
                              "is not on PATH); refusing a header-only proof"
                              % label)
    proof["buildinfo"] = bi
    if bi.get("goos") != goos:
        raise AcceptanceError("%s build info says GOOS=%s but target is %s/%s"
                              % (label, bi.get("goos"), goos, goarch))
    if bi.get("goarch") != goarch:
        raise AcceptanceError("%s build info says GOARCH=%s but target is %s/%s"
                              % (label, bi.get("goarch"), goos, goarch))
    proof["sources"].append("buildinfo")
    if identity is not None:
        proof["identity"] = verify_identity(bi, identity, label)
    return proof


def verify_arch_proof(archive_path: str, version: str, goos: str, goarch: str,
                      use_buildinfo: bool = True, identity: dict = None,
                      deadline=None, scratch: str = None) -> dict:
    """Prove the embedded binary's architecture matches the archive target.

    Both the binary header and (unless disabled) Go build info must agree with
    the target. This never executes the binary, so it is valid for
    cross-built targets on a host without emulation.
    """
    tmp = tempfile.mkdtemp(prefix="ra-proof-", dir=scratch)
    try:
        info = scan_archive(archive_path, version, goos, goarch, extract_dir=tmp)
        return prove_binary(info["binary"], goos, goarch, use_buildinfo,
                            identity, deadline, label=info["archive"])
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


# --------------------------------------------------------------------------
# source state
# --------------------------------------------------------------------------
def read_go_module(repo_root: str) -> str:
    try:
        with open(os.path.join(repo_root, "go.mod")) as f:
            for line in f:
                parts = line.split()
                if len(parts) == 2 and parts[0] == "module":
                    return parts[1].strip('"')
    except OSError as e:
        raise AcceptanceError("cannot read go.mod: %s" % e)
    raise AcceptanceError("go.mod has no module directive")


def _under(path: str, roots) -> bool:
    real = os.path.realpath(path)
    for root in roots:
        r = os.path.realpath(root)
        if real == r or real.startswith(r.rstrip("/") + "/"):
            return True
    return False


def _git_env(isolated=False):
    env = os.environ.copy()
    env["GIT_OPTIONAL_LOCKS"] = "0"
    if isolated:
        # Snapshot-side git ignores user/system config: no filter drivers
        # (git-lfs may fetch over the network), autocrlf, hooks or templates.
        env.update({"GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"})
    return env


def _git_out(repo_root, args, deadline=None, timeout=TIMEOUT_GIT,
             cap=OUTPUT_CAP, isolated=False):
    git = shutil.which("git")
    if git is None:
        raise AcceptanceError("git not found; cannot record the source revision")
    env = _git_env(isolated)
    res = run_cmd([git, "-C", repo_root] + list(args), timeout=timeout,
                  deadline=deadline, env=env, output_cap=cap)
    if res.returncode != 0 or res.truncated:
        raise AcceptanceError("git %s failed: %s" % (args[0], res.stderr.strip()))
    return res.stdout


def source_state(repo_root: str, deadline=None, exclude=()) -> dict:
    """HEAD revision and dirty state of the live worktree (read-only git).

    Status entries under ``exclude`` (the harness's own out/scratch dirs, if
    they live inside the repo) are ignored. ``status_sha256`` covers status
    lines (names and states), not file contents; build modes never compile
    the worktree, they compile a content-verified snapshot (source_snapshot).
    """
    def git_out(*args, cap=OUTPUT_CAP):
        return _git_out(repo_root, args, deadline, cap=cap)

    rev = git_out("rev-parse", "--verify", "HEAD").strip()
    top = git_out("rev-parse", "--show-toplevel").strip()
    raw = git_out("status", "--porcelain=v1", "-z", "--untracked-files=all",
                  cap=4 << 20)
    kept, entries, excluded = [], [], 0
    toks = raw.split("\0")
    i = 0
    while i < len(toks):
        tok = toks[i]
        i += 1
        if not tok:
            continue
        entry = {"status": tok[:2], "path": tok[3:]}
        if tok[0] in "RC":   # rename/copy: the original path follows
            entry["orig"] = toks[i] if i < len(toks) else ""
            i += 1
        if exclude and _under(os.path.join(top, entry["path"]), exclude):
            excluded += 1
            continue
        kept.append(tok)
        entries.append(entry)
    try:
        describe = git_out("describe", "--tags", "--always", "--dirty").strip()
    except AcceptanceError:
        describe = None
    return {"revision": rev, "dirty": bool(kept), "status_entries": len(kept),
            "excluded_entries": excluded,
            "status_sha256": hashlib.sha256("\0".join(kept).encode()).hexdigest(),
            "describe": describe, "toplevel": top,
            "entries": entries[:MAX_REPORTED_PATHS],
            "entries_truncated": len(entries) > MAX_REPORTED_PATHS,
            "_all_entries": entries}


def is_build_input(path: str, go_dirs, prefix: str = "") -> bool:
    """Could a change at ``path`` (relative to the repo top) alter the build?

    Inputs: any ``.go`` file; go.mod/go.sum/go.work/go.work.sum; vendor/;
    release.sh's packaging inputs README.md, LICENSE and scripts/release.sh;
    and anything at or below a directory holding a committed ``.go`` file.
    The last rule covers every file a package can compile, assemble, link
    (.s, .syso) or embed: //go:embed only reaches the package directory and
    its subdirectories. It over-approximates (tests, testdata), never under.
    """
    if (posixpath.basename(path) in GO_MODULE_FILES or path.endswith(".go")
            or path in {posixpath.join(prefix, p) for p in PACKAGING_INPUTS}
            or path.startswith(posixpath.join(prefix, "vendor") + "/")):
        return True
    d = posixpath.dirname(path)
    while True:
        if d in go_dirs:
            return True
        if not d:
            return False
        d = posixpath.dirname(d)


def _tree_entries(repo_root, rev, deadline=None) -> dict:
    """{path: (mode, blob id)} for every entry of the committed tree."""
    raw = _git_out(repo_root, ["ls-tree", "-r", "-z", "--full-tree", rev],
                   deadline, cap=MAX_TREE_LISTING)
    out = {}
    for rec in raw.split("\0"):
        if not rec:
            continue
        meta, _, path = rec.partition("\t")
        mode, kind, oid = meta.split(" ")
        if kind != "blob" or mode not in ("100644", "100755", "120000"):
            raise AcceptanceError("committed entry %s is a %s (mode %s); only "
                                  "regular files and symlinks are supported"
                                  % (path, kind, mode))
        out[path] = (mode, oid)
    return out


def _snapshot_entries(root, algo) -> dict:
    """{path: (mode, blob id)} computed from the bytes on disk under root
    (git's own blob hashing, so it compares directly with ls-tree)."""
    found = {}
    for dirpath, dirnames, filenames in os.walk(root):
        if dirpath == root and ".git" in dirnames:
            dirnames.remove(".git")
        for name in dirnames + filenames:
            p = os.path.join(dirpath, name)
            st = os.lstat(p)
            rel = os.path.relpath(p, root).replace(os.sep, "/")
            if stat.S_ISDIR(st.st_mode):
                continue
            if stat.S_ISLNK(st.st_mode):
                mode, data = "120000", os.fsencode(os.readlink(p))
            elif stat.S_ISREG(st.st_mode):
                if st.st_size > MAX_SOURCE_FILE_BYTES:
                    raise AcceptanceError("source file %s exceeds %d bytes"
                                          % (rel, MAX_SOURCE_FILE_BYTES))
                fd = _open_regular(p)
                with os.fdopen(fd, "rb") as f:
                    data = f.read(MAX_SOURCE_FILE_BYTES + 1)
                mode = "100755" if st.st_mode & 0o100 else "100644"
            else:
                raise AcceptanceError("source snapshot holds a special file: %s"
                                      % rel)
            h = hashlib.new(algo)
            h.update(b"blob %d\0" % len(data))
            h.update(data)
            found[rel] = (mode, h.hexdigest())
    return found


def _content_digest(entries) -> str:
    h = hashlib.sha256()
    for path in sorted(entries):
        mode, oid = entries[path]
        h.update(("%s %s %s\0" % (mode, oid, path))
                 .encode("utf-8", "surrogateescape"))
    return h.hexdigest()


def _diff_entries(expected, found):
    return {"missing": sorted(set(expected) - set(found))[:10],
            "unexpected": sorted(set(found) - set(expected))[:10],
            "changed": sorted(p for p in set(found) & set(expected)
                              if found[p] != expected[p])[:10]}


def source_snapshot(repo_root, source, dest, deadline=None) -> dict:
    """Immutable, content-verified snapshot of the committed tree to build.

    Fails closed if the worktree holds uncommitted build inputs (see
    is_build_input), since the committed-tree build would silently leave them
    out. Other dirt (harness scripts, docs, caches) is recorded as excluded.
    The snapshot is a ``--shared`` clone (reads the repo's objects, writes
    only under ``dest``) checked out detached at the recorded revision; every
    file is then hashed as a git blob and must match ``git ls-tree`` of that
    revision exactly. Directories are made read-only; recheck_snapshot proves
    the contents did not change during the build. Because the clone has a
    real .git directory, Go stamps vcs.revision and vcs.modified=false.
    """
    rev = source["revision"]
    top = source.get("toplevel") or _git_out(
        repo_root, ["rev-parse", "--show-toplevel"], deadline).strip()
    prefix = os.path.relpath(os.path.realpath(repo_root), os.path.realpath(top))
    prefix = "" if prefix == "." else prefix.replace(os.sep, "/")
    try:
        algo = _git_out(repo_root, ["rev-parse", "--show-object-format"],
                        deadline).strip()
    except AcceptanceError:
        algo = "sha1"
    if algo not in ("sha1", "sha256"):
        raise AcceptanceError("unsupported git object format %r" % algo)
    tree = _git_out(repo_root, ["rev-parse", "--verify", rev + "^{tree}"],
                    deadline).strip()
    expected = _tree_entries(repo_root, rev, deadline)
    go_dirs = {posixpath.dirname(p) for p in expected if p.endswith(".go")}

    dirty_inputs, excluded = [], []
    for e in source.get("_all_entries", source.get("entries", [])):
        paths = [e["path"]] + ([e["orig"]] if e.get("orig") else [])
        if any(is_build_input(p, go_dirs, prefix) for p in paths):
            dirty_inputs.append(e["path"])
        else:
            excluded.append(e["path"])
    if dirty_inputs:
        raise AcceptanceError(
            "uncommitted build input(s) in the worktree: %s. The release build "
            "compiles only the committed tree of %s, so these would be silently "
            "left out; commit them or build from a clean checkout"
            % (", ".join(sorted(dirty_inputs)[:20]), rev[:12]))

    git = shutil.which("git")
    env = _git_env(isolated=True)
    for argv in ([git, "clone", "--quiet", "--shared", "--no-checkout",
                  "--template=", top, dest],
                 [git, "-C", dest, "-c", "core.hooksPath=" + os.devnull,
                  "-c", "advice.detachedHead=false", "checkout", "--quiet",
                  "--detach", rev]):
        res = run_cmd(argv, timeout=TIMEOUT_GIT_CLONE, deadline=deadline,
                      env=env)
        if res.returncode != 0:
            raise AcceptanceError("source snapshot: git %s failed: %s"
                                  % (argv[1] if argv[1] != "-C" else "checkout",
                                     res.stderr.strip()))
    head = _git_out(dest, ["rev-parse", "--verify", "HEAD"], deadline,
                    isolated=True).strip()
    if head != rev:
        raise AcceptanceError("source snapshot is at %s, expected %s"
                              % (head, rev))
    found = _snapshot_entries(dest, algo)
    if found != expected:
        raise AcceptanceError("source snapshot does not match the committed "
                              "tree %s: %s" % (tree, _diff_entries(expected, found)))
    if _git_out(dest, ["status", "--porcelain=v1", "-z",
                       "--untracked-files=all"], deadline,
                isolated=True).strip("\0"):
        raise AcceptanceError("source snapshot checkout is not clean")
    for dirpath, dirnames, _files in os.walk(dest, topdown=False):
        rel = os.path.relpath(dirpath, dest)
        if rel == ".git" or rel.startswith(".git" + os.sep):
            continue
        _chmod_dir_nofollow(dirpath, 0o555)
    return {"policy": "committed_tree_snapshot", "root": dest,
            "path": os.path.join(dest, prefix) if prefix else dest,
            "revision": rev, "tree": tree, "object_format": algo,
            "files": len(found), "content_sha256": _content_digest(found),
            "verified_against_tree": True,
            "dirty_build_inputs": [],
            "excluded_dirty": sorted(excluded)[:MAX_REPORTED_PATHS],
            "excluded_dirty_count": len(excluded)}


def recheck_snapshot(record, deadline=None) -> str:
    """Re-hash the snapshot after the build; any change fails acceptance."""
    root = record["root"]
    digest = _content_digest(_snapshot_entries(root, record["object_format"]))
    if digest != record["content_sha256"]:
        raise AcceptanceError("source snapshot changed during the build "
                              "(content %s -> %s)"
                              % (record["content_sha256"][:16], digest[:16]))
    status = _git_out(root, ["status", "--porcelain=v1", "-z",
                             "--untracked-files=all"], deadline, isolated=True)
    if status.strip("\0"):
        raise AcceptanceError("source snapshot changed during the build: %r"
                              % status[:200])
    return digest


# --------------------------------------------------------------------------
# host binary invocation (isolated placeholder config, no provider keys)
# --------------------------------------------------------------------------
def write_placeholder_config(workdir: str) -> str:
    """Write a keyless, provider-free config that satisfies config.Validate.

    The single client uses a local placeholder key file (mode 0600); there are
    no accounts, no api_key/api_key_env, no base_url and no routes, so nothing
    can reach a network provider.
    """
    keys_dir = os.path.join(workdir, "keys")
    os.makedirs(keys_dir, exist_ok=True)
    keyfile = os.path.join(keys_dir, "acceptance.key")
    with open(keyfile, "w") as f:
        f.write(hashlib.sha256(b"localrouter-acceptance-placeholder")
                .hexdigest() + "\n")
    os.chmod(keyfile, 0o600)
    cfg = os.path.join(workdir, "config.yaml")
    with open(cfg, "w") as f:
        f.write(
            "listen: 127.0.0.1:0\n"
            "data_dir: %s\n"
            "clients:\n"
            "  - name: acceptance\n"
            "    class: interactive\n"
            "    key_file: keys/acceptance.key\n" % workdir)
    return cfg


def run_binary_check(binary_path: str, workdir: str, deadline=None,
                     expected_sha256: str = None) -> dict:
    """Run the host-native binary offline: ``--help`` then ``check``.

    The release contract has no ``version`` subcommand, so binary identity is
    proven separately through build info (``go version -m``) and the binary
    header; here we only prove the binary executes and validates a
    placeholder config. When ``expected_sha256`` is given the file is
    re-hashed first and must still be the verified bytes.
    """
    actual = sha256_file(binary_path)
    if expected_sha256 is not None and actual != expected_sha256:
        raise AcceptanceError("binary changed between verification and execution "
                              "(verified %s, now %s)" % (expected_sha256, actual))
    cfg = write_placeholder_config(workdir)
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"),
           "HOME": workdir, "TMPDIR": workdir, "GOMAXPROCS": "2"}
    res = {"binary": binary_path, "binary_sha256": actual, "help_exit": None,
           "check_exit": None, "check_stdout": ""}

    help_run = run_cmd([binary_path, "--help"], timeout=TIMEOUT_HOST_RUN,
                       deadline=deadline, cwd=workdir, env=env)
    res["help_exit"] = help_run.returncode
    if help_run.returncode != 0:
        raise AcceptanceError("host binary --help exited %d: %s"
                              % (help_run.returncode, help_run.stderr.strip()))

    check_run = run_cmd([binary_path, "check", "-config", cfg],
                        timeout=TIMEOUT_HOST_RUN, deadline=deadline,
                        cwd=workdir, env=env)
    res["check_exit"] = check_run.returncode
    res["check_stdout"] = check_run.stdout.strip()
    if check_run.returncode != 0:
        raise AcceptanceError("host binary check exited %d: %s"
                              % (check_run.returncode, check_run.stderr.strip()))
    if "config OK" not in check_run.stdout:
        raise AcceptanceError("host binary check did not report 'config OK': %r"
                              % check_run.stdout)
    return res


# --------------------------------------------------------------------------
# bounded build (mirrors scripts/release.sh per-target work)
# --------------------------------------------------------------------------
def build_env(tmpdir: str) -> dict:
    env = os.environ.copy()
    env.update(BUILD_ENV)
    env["TMPDIR"] = tmpdir
    return env


def build_target(repo_root: str, out_dir: str, version: str, goos: str,
                 goarch: str, tmpdir: str, deadline=None) -> str:
    go_bin = shutil.which("go")
    if go_bin is None:
        raise AcceptanceError("go toolchain not found in PATH")
    name = archive_name(version, goos, goarch)
    member_dir = archive_member_dir(version, goos, goarch)
    pkg_dir = os.path.join(tmpdir, member_dir)
    os.makedirs(pkg_dir, exist_ok=True)

    env = build_env(tmpdir)
    env.update({"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch})
    res = run_cmd([go_bin, "build", "-trimpath", "-o",
                   os.path.join(pkg_dir, BIN_NAME), PKG],
                  timeout=TIMEOUT_GO_BUILD, deadline=deadline, cwd=repo_root,
                  env=env)
    if res.returncode != 0:
        raise AcceptanceError("go build failed for %s/%s (exit %d):\n%s"
                              % (goos, goarch, res.returncode, res.stderr))
    if not os.path.isfile(os.path.join(pkg_dir, BIN_NAME)):
        raise AcceptanceError("build produced no binary for %s/%s" % (goos, goarch))

    readme = os.path.join(repo_root, "README.md")
    if os.path.isfile(readme):
        shutil.copy(readme, os.path.join(pkg_dir, "README.md"))
    license_file = os.path.join(repo_root, "LICENSE")
    if os.path.isfile(license_file):
        shutil.copy(license_file, os.path.join(pkg_dir, "LICENSE"))

    archive = os.path.join(out_dir, name)
    fh = _create_exclusive(archive, "wb")
    with fh, tarfile.open(name=archive, mode="w:gz", fileobj=fh) as tf:
        tf.add(pkg_dir, arcname=member_dir)
    return archive


def _create_exclusive(path, mode):
    """Open a new file for writing; an existing file is never replaced."""
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
                     | os.O_CLOEXEC, 0o644)
    except FileExistsError:
        raise AcceptanceError("refusing to overwrite existing %s" % path)
    return os.fdopen(fd, mode)


def preflight_build_out_dir(out_dir: str) -> dict:
    """Build modes write archives and SHA256SUMS into out_dir. Refuse any
    out_dir that already holds anything, so an existing release (or any other
    file) is never overwritten; a missing out_dir is created and owned."""
    if not os.path.lexists(out_dir):
        try:
            os.makedirs(out_dir)
        except FileExistsError:
            raise AcceptanceError("output directory %s appeared during preflight"
                                  % out_dir)
        except OSError as e:
            raise AcceptanceError("cannot create output directory %s: %s"
                                  % (out_dir, e))
        return {"created": True, "was_empty": True}
    if not os.path.isdir(out_dir):
        raise AcceptanceError("output directory %s is not a directory" % out_dir)
    entries = sorted(os.listdir(out_dir))
    if entries:
        raise AcceptanceError(
            "output directory %s is not empty (%d entries, e.g. %s); build modes "
            "never overwrite existing archives or SHA256SUMS. Use a new or empty "
            "directory, or --build-mode none to verify what is there"
            % (out_dir, len(entries), ", ".join(entries[:5])))
    return {"created": False, "was_empty": True}


# --------------------------------------------------------------------------
# orchestration
# --------------------------------------------------------------------------
def run_acceptance(out_dir, version, arches, build_mode, repo_root, log=print,
                   scratch_dir=None, deadline=None, report=None,
                   keep_scratch=False):
    """Build (optionally) and accept the artifacts; returns the report dict.

    ``report`` (if given) is filled in place so a caller still has the
    partial evidence when an AcceptanceError is raised.
    """
    deadline = deadline or Deadline(DEFAULT_MAX_RUNTIME)
    report = report if report is not None else {}
    check_durable(out_dir, "output directory")
    scratch_root = scratch_dir or os.path.join(out_dir, SCRATCH_SUBDIR)
    check_durable(scratch_root, "scratch directory")
    if build_mode in ("script", "mirror"):
        # before anything is created inside out_dir (e.g. the default scratch)
        report["out_dir_preflight"] = preflight_build_out_dir(out_dir)
    os.makedirs(out_dir, exist_ok=True)

    host = tuple(detect_host_target())
    targets = resolve_arches(arches, host)
    if build_mode == "script":
        targets = [tuple(t.split("/")) for t in DEFAULT_TARGETS]
    report.update({"mode": MODE_LABELS[build_mode],
                   "version": normalize_version(version), "out_dir": out_dir,
                   "host_target": "%s/%s" % host,
                   "targets": ["%s/%s" % t for t in targets], "archives": {}})
    if build_mode == "mirror":
        report["mode_note"] = ("contract mirror smoke: the harness rebuilt and "
                               "packaged the targets itself; this is NOT "
                               "acceptance of scripts/release.sh output")
    log("targets: " + ", ".join("%s/%s" % t for t in targets))

    module = read_go_module(repo_root)
    exclude = [out_dir, scratch_root]
    source = source_state(repo_root, deadline=deadline, exclude=exclude)
    report["source"] = _public(source)
    built_here = build_mode in ("script", "mirror")
    identity = {"module": module, "path": module + "/" + MAIN_SUBPATH,
                "revision": source["revision"], "exclude": exclude,
                "allow_unstamped": built_here,
                "require_unmodified": built_here}
    if source.get("dirty"):
        log("warning: source tree is dirty (%d status entries)"
            % source.get("status_entries", 0))

    made_root = scratch_dir is None and not os.path.isdir(scratch_root)
    os.makedirs(scratch_root, exist_ok=True)
    run_scratch = tempfile.mkdtemp(prefix="run-", dir=scratch_root)
    report["scratch"] = run_scratch
    try:
        _accept(out_dir, version, targets, build_mode, repo_root, log, host,
                identity, deadline, report, run_scratch, source)
    finally:
        if not keep_scratch:
            _rmtree(run_scratch)
            if made_root:
                try:
                    os.rmdir(scratch_root)
                except OSError:
                    pass
        report["scratch_removed"] = not os.path.exists(run_scratch)
    return report


def _chmod_dir_nofollow(path, mode=0o700):
    """chmod a real directory; never acts through a symlink."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
                     | os.O_CLOEXEC)
    except OSError:
        return
    try:
        os.fchmod(fd, mode)
    except OSError:
        pass
    finally:
        os.close(fd)


def _rmtree(path):
    def onerror(func, p, _exc):
        # Removing an entry needs write access to its parent directory (the
        # snapshot's directories are read-only). Only real directories are
        # ever chmod-ed, so a symlink's target outside scratch is untouched.
        _chmod_dir_nofollow(os.path.dirname(p))
        _chmod_dir_nofollow(p)
        try:
            func(p)
        except OSError:
            pass
    shutil.rmtree(path, onerror=onerror)


def _public(state):
    return {k: v for k, v in state.items() if not k.startswith("_")}


def _accept(out_dir, version, targets, build_mode, repo_root, log, host,
            identity, deadline, report, scratch, source):
    if build_mode in ("script", "mirror"):
        # Build only from an immutable, content-verified snapshot of the
        # committed tree; the live worktree is never compiled.
        src_snap = source_snapshot(repo_root, source,
                                   os.path.join(scratch, "source"),
                                   deadline=deadline)
        report["build_source"] = src_snap
        build_root = src_snap["path"]
        log("source snapshot: %s tree %s, verified (%s files; %d unrelated "
            "dirty path(s) excluded)"
            % (src_snap["revision"][:12], src_snap["tree"][:12],
               src_snap.get("files"), len(src_snap.get("excluded_dirty", []))))
    if build_mode == "script":
        script = os.path.join(build_root, "scripts", "release.sh")
        tmp = os.path.join(scratch, "release-sh-tmp")
        os.makedirs(tmp)
        env = build_env(tmp)
        log("running scripts/release.sh (all targets; %s) ..."
            % " ".join("%s=%s" % kv for kv in sorted(BUILD_ENV.items())))
        res = run_cmd(["bash", script, out_dir, version],
                      timeout=TIMEOUT_RELEASE_SH, deadline=deadline,
                      cwd=build_root, env=env)
        report["build"] = {"command": ["bash", "scripts/release.sh", out_dir,
                                       version],
                           "cwd": "build_source.path (committed-tree snapshot)",
                           "exit": res.returncode, "seconds": res.seconds,
                           "env": dict(BUILD_ENV, TMPDIR=tmp,
                                       GOCACHE=env.get("GOCACHE")),
                           "stdout": res.stdout[-4000:],
                           "stderr": res.stderr[-4000:]}
        log(res.stdout.strip())
        if res.returncode != 0:
            raise AcceptanceError("scripts/release.sh failed (exit %d):\n%s"
                                  % (res.returncode, res.stderr))
    elif build_mode == "mirror":
        tmpdir = os.path.join(scratch, "mirror-build")
        os.makedirs(tmpdir)
        built = []
        for goos, goarch in targets:
            log("building %s/%s (GOMAXPROCS=2 GOFLAGS=-p=2) ..." % (goos, goarch))
            built.append(build_target(build_root, out_dir, version, goos, goarch,
                                      tmpdir, deadline=deadline))
        write_manifest(out_dir, built)
        report["build"] = {"command": "mirror go build per target",
                           "cwd": "build_source.path (committed-tree snapshot)",
                           "env": dict(BUILD_ENV, TMPDIR=tmpdir)}
    if build_mode in ("script", "mirror"):
        # The snapshot that was compiled must hold exactly what was verified
        # before the build (content digest before == after) ...
        src_snap["content_sha256_after_build"] = recheck_snapshot(
            src_snap, deadline=deadline)
        # ... and the live worktree's HEAD and status lines must be unchanged
        # too (a names-only record: the worktree is never compiled).
        after = source_state(repo_root, deadline=deadline,
                             exclude=identity["exclude"])
        report["source_after_build"] = _public(after)
        if (after["revision"], after["status_sha256"]) != (
                source["revision"], source["status_sha256"]):
            raise AcceptanceError("source tree changed during the build "
                                  "(revision %s -> %s)"
                                  % (source["revision"], after["revision"]))

    archives = [os.path.join(out_dir, archive_name(version, g, a))
                for g, a in targets]

    # 1. Integrity before anything else: hash every SHA256SUMS entry and
    #    snapshot the selected archives from the same bytes that were hashed.
    snap = os.path.join(scratch, "snapshot")
    os.mkdir(snap, 0o700)
    manifest = verify_manifest(out_dir, archives, version=version,
                               snapshot_dir=snap)
    if build_mode == "script":
        listed = sorted(e["target"] for e in manifest["entries"].values())
        if listed != sorted(DEFAULT_TARGETS):
            raise AcceptanceError("release.sh SHA256SUMS lists %s, expected "
                                  "exactly %s" % (listed, list(DEFAULT_TARGETS)))
    report["manifest"] = {
        "path": manifest["path"], "sha256": manifest["sha256"],
        "entries": {n: {k: v for k, v in e.items() if k != "snapshot"}
                    for n, e in manifest["entries"].items()}}
    log("SHA256SUMS verified for %d listed archive(s)" % len(manifest["entries"]))

    # 2. Shape + header + build info + identity, all on the snapshot.
    binaries = {}
    for goos, goarch in targets:
        t = "%s/%s" % (goos, goarch)
        name = archive_name(version, goos, goarch)
        m = manifest["entries"][name]
        xdir = tempfile.mkdtemp(prefix="x-", dir=scratch)
        info = scan_archive(m["snapshot"], version, goos, goarch, extract_dir=xdir)
        proof = prove_binary(info["binary"], goos, goarch, identity=identity,
                             deadline=deadline, label=name)
        report["archives"][t] = {
            "archive": name, "archive_sha256": m["sha256"],
            "archive_size": m["size"], "files": info["files"],
            "members": info["members"], "binary_sha256": info["binary_sha256"],
            "binary_size": info["binary_size"], "proof": proof,
            "identity": proof.get("identity"), "executed": False}
        binaries[t] = info["binary"]
        log("%s: shape, %s header and build info OK" % (t, proof["header"]["format"]))

    # 3. Execute only the host target, from the verified snapshot extraction.
    for goos, goarch in targets:
        t = "%s/%s" % (goos, goarch)
        entry = report["archives"][t]
        if (goos, goarch) == host:
            workdir = tempfile.mkdtemp(prefix="host-", dir=scratch)
            entry["host_run"] = run_binary_check(
                binaries[t], workdir, deadline=deadline,
                expected_sha256=entry["binary_sha256"])
            entry["executed"] = True
            log("%s: executed --help and check (config OK)" % t)
        else:
            entry["not_executed_reason"] = (
                "cross target on a %s host: proven by %s header and Go build "
                "info only; never executed (no emulation used)"
                % (report["host_target"], entry["proof"]["header"]["format"]))

    # 4. What remains in out_dir must still be exactly what was verified.
    recheck_manifest(out_dir, manifest)
    report["manifest"]["rechecked_after_run"] = True


def _utc_now():
    return datetime.datetime.now(datetime.timezone.utc).strftime(
        "%Y-%m-%dT%H:%M:%SZ")


def write_report(path: str, report: dict):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".partial"
    with open(tmp, "w") as f:
        json.dump(report, f, indent=2, sort_keys=True)
        f.write("\n")
    os.replace(tmp, path)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--out-dir", required=True,
                        help="durable output directory (absolute, not /tmp); "
                             "script/mirror require it to be missing or empty")
    parser.add_argument("--version", default=None,
                        help="release version (default: git describe --tags --always --dirty)")
    parser.add_argument("--arches", default=DEFAULT_ARCHES,
                        help="arch spec: 'host,arm64' (default), 'all', or os/arch list")
    parser.add_argument("--build-mode", choices=("mirror", "script", "none"),
                        default="mirror",
                        help="mirror=per-target bounded build, a contract smoke "
                             "(default); script=drive scripts/release.sh "
                             "(release acceptance); none=verify only")
    parser.add_argument("--repo-root", default=os.path.dirname(
                        os.path.dirname(os.path.abspath(__file__))),
                        help="repository root (default: parent of scripts/)")
    parser.add_argument("--max-runtime", type=float, default=DEFAULT_MAX_RUNTIME,
                        help="global deadline in seconds for the whole run "
                             "(default %d)" % DEFAULT_MAX_RUNTIME)
    parser.add_argument("--scratch-dir", default=None,
                        help="durable scratch parent (default: <out-dir>/%s); "
                             "the per-run dir inside it is removed afterwards"
                             % SCRATCH_SUBDIR)
    parser.add_argument("--keep-scratch", action="store_true",
                        help="keep the per-run scratch dir for debugging")
    parser.add_argument("--report", default=None,
                        help="report JSON path (default: <out-dir>/"
                             "acceptance-reports/release-acceptance-<run>.json)")
    args = parser.parse_args(argv)

    run_id = "%s-%s" % (datetime.datetime.now(datetime.timezone.utc)
                        .strftime("%Y%m%dT%H%M%SZ"), secrets.token_hex(3))
    report_path = args.report or os.path.join(
        args.out_dir, "acceptance-reports", "release-acceptance-%s.json" % run_id)
    deadline = Deadline(args.max_runtime)
    started = time.monotonic()
    report = {"schema": 1, "tool": "scripts/release-acceptance.py",
              "run_id": run_id, "started_at": _utc_now(), "outcome": None,
              "error": None,
              "flags": {"argv": list(sys.argv[1:] if argv is None else argv),
                        "out_dir": args.out_dir, "build_mode": args.build_mode,
                        "arches": args.arches, "version": args.version,
                        "version_source": "flag" if args.version else None,
                        "repo_root": args.repo_root,
                        "max_runtime": args.max_runtime,
                        "scratch_dir": args.scratch_dir,
                        "keep_scratch": args.keep_scratch,
                        "report": report_path}}
    rc = 1
    report_ok = False
    try:
        check_durable(report_path, "report path")
        report_ok = True
        check_durable(args.out_dir, "output directory")
        if args.scratch_dir:
            check_durable(args.scratch_dir, "scratch directory")
        version = args.version
        if not version:
            try:
                res = run_cmd(["git", "describe", "--tags", "--always", "--dirty"],
                              timeout=TIMEOUT_GIT, deadline=deadline,
                              cwd=args.repo_root)
                version = res.stdout.strip() if res.returncode == 0 else ""
            except AcceptanceError:
                version = ""
            report["flags"]["version_source"] = ("git describe" if version
                                                 else "fallback")
            version = version or "dev"
            report["flags"]["version"] = version
        run_acceptance(args.out_dir, version, args.arches, args.build_mode,
                       args.repo_root, scratch_dir=args.scratch_dir,
                       deadline=deadline, report=report,
                       keep_scratch=args.keep_scratch)
        report["outcome"] = "pass"
        rc = 0
    except AcceptanceError as e:
        report["outcome"] = "fail"
        report["error"] = str(e)
        print("ACCEPTANCE FAILED: %s" % e, file=sys.stderr)
    except Exception as e:  # never a bare traceback; still record the run
        report["outcome"] = "error"
        report["error"] = "internal error: %s: %s" % (type(e).__name__, e)
        print("ACCEPTANCE FAILED (internal error): %s: %s"
              % (type(e).__name__, e), file=sys.stderr)
    finally:
        report["finished_at"] = _utc_now()
        report["elapsed_seconds"] = round(time.monotonic() - started, 3)
        try:
            if not report_ok:
                raise OSError("report path is not absolute and durable")
            write_report(report_path, report)
            print("report: %s" % report_path)
        except OSError as e:
            print("could not write report %s: %s" % (report_path, e),
                  file=sys.stderr)
            rc = 1
    if rc == 0:
        executed = [t for t, e in report["archives"].items() if e["executed"]]
        proven = [t for t, e in report["archives"].items() if not e["executed"]]
        print("ACCEPTANCE PASS [%s]: version=%s executed=%s "
              "NOT EXECUTED (header+buildinfo proof only)=%s source=%s%s"
              % (report["mode"], report["version"], ",".join(executed) or "none",
                 ",".join(proven) or "none", report["source"]["revision"][:12],
                 "+dirty" if report["source"].get("dirty") else ""))
    return rc


if __name__ == "__main__":
    sys.exit(main())
