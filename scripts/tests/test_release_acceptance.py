#!/usr/bin/env python3
"""Deterministic unit tests for scripts/release-acceptance.py.

These tests are fixture-based and require NO network and NO container
runtime. They exercise the validation helpers directly with synthetic
archives / manifest / ELF and Mach-O headers, plus small executable stubs for
the host-binary invocation plumbing. ``go version -m`` and the git source state
are faked where a test needs them.

Scratch: every temp dir is created under ``$RA_TEST_TMPDIR`` (falling back to
the normal ``TMPDIR`` rules) and removed by ``addCleanup``. Host-run stubs are
executed from there, so it must be on an exec-capable filesystem.

The optional ``RealGoTests`` compile a stdlib-only sentinel with the local Go
toolchain (offline: GOPROXY=off, GOTOOLCHAIN=local). They are slow and only
run when ``RA_REAL_GO=1`` is set.
"""
import contextlib
import gzip
import importlib.util
import io
import json
import os
import shutil
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
MODULE_PATH = os.path.join(os.path.dirname(HERE), "release-acceptance.py")

spec = importlib.util.spec_from_file_location("release_acceptance", MODULE_PATH)
ra = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ra)

TEST_TMP = os.environ.get("RA_TEST_TMPDIR") or None
VERSION = "1.2.3"
MODULE = "github.com/hpst3r/localrouter"
MAIN_PKG = MODULE + "/cmd/localrouter"
REV = "0123456789abcdef0123456789abcdef01234567"
ALL = [tuple(t.split("/")) for t in ra.DEFAULT_TARGETS]
HOST = ra.detect_host_target()
# A target that is never the host, so fake-header archives are proven but
# never executed regardless of whether the test host is amd64 or arm64.
CROSS = ("darwin", "arm64") if HOST[0] == "linux" else ("linux", "arm64")
CROSS_T = "%s/%s" % CROSS
NON_HOST_SPEC = ",".join(t for t in ra.DEFAULT_TARGETS if t != "%s/%s" % HOST)


# --------------------------------------------------------------------------
# fixture helpers
# --------------------------------------------------------------------------
def elf64_header(machine, ei_class=2):
    """Minimal 64-byte ELF header carrying only e_ident + e_type/e_machine."""
    ident = b"\x7fELF" + bytes([ei_class, 1, 1, 0]) + b"\x00" * 8  # LE
    body = struct.pack("<H", 2) + struct.pack("<H", machine)  # ET_EXEC, e_machine
    return ident + body + b"\x00" * (64 - len(ident) - len(body))


MACHO_CPU = {"amd64": 0x01000007, "arm64": 0x0100000C}


def macho64_header(goarch):
    # MH_MAGIC_64 stored little-endian (CF FA ED FE), cputype, subtype, MH_EXECUTE
    return struct.pack("<IIII", 0xFEEDFACF, MACHO_CPU[goarch], 0, 2) + b"\x00" * 16


def native_header(goos, goarch):
    if goos == "darwin":
        return macho64_header(goarch)
    return elf64_header(ra.ELF_MACHINE[goarch])


def make_archive(dest_dir, version=VERSION, goos="linux", goarch="amd64",
                 binary=elf64_header(ra.ELF_MACHINE["amd64"]),
                 include_binary=True, include_readme=True, member_dir=None):
    """Package like release.sh does: one member dir holding the files."""
    name = ra.archive_name(version, goos, goarch)
    default_member = ra.archive_member_dir(version, goos, goarch)
    member = member_dir if member_dir is not None else default_member
    stage = tempfile.mkdtemp(dir=dest_dir, prefix="stage-")
    pkg = os.path.join(stage, member)
    os.makedirs(pkg, exist_ok=True)
    os.chmod(pkg, 0o755)
    if include_binary:
        with open(os.path.join(pkg, ra.BIN_NAME), "wb") as f:
            f.write(binary)
        os.chmod(os.path.join(pkg, ra.BIN_NAME), 0o755)
    if include_readme:
        with open(os.path.join(pkg, "README.md"), "w") as f:
            f.write("# readme\n")
    archive_path = os.path.join(dest_dir, name)
    with tarfile.open(archive_path, "w:gz") as tf:
        tf.add(pkg, arcname=member)
    shutil.rmtree(stage)
    return archive_path


def build_tar(path, entries):
    """entries: list of dicts {name, type: file|dir|sym|lnk, data, mode, link}."""
    with tarfile.open(path, "w:gz") as tf:
        for e in entries:
            ti = tarfile.TarInfo(e["name"])
            ti.mode = e.get("mode", 0o755)
            kind = e.get("type", "file")
            if kind == "file":
                data = e.get("data", b"")
                ti.size = len(data)
                tf.addfile(ti, io.BytesIO(data))
            elif kind == "dir":
                ti.type = tarfile.DIRTYPE
                tf.addfile(ti)
            elif kind == "sym":
                ti.type = tarfile.SYMTYPE
                ti.linkname = e["link"]
                tf.addfile(ti)
            elif kind == "lnk":
                ti.type = tarfile.LNKTYPE
                ti.linkname = e["link"]
                tf.addfile(ti)
    return path


def std_archive(dest, goos, goarch, binary, extra=(), bin_entry=None,
                version=VERSION, pre=(), readme=True):
    """Archive with explicit member order/types (for shape-rejection tests)."""
    md = ra.archive_member_dir(version, goos, goarch)
    entries = [{"name": md, "type": "dir"}]
    for e in pre:  # members that must precede the binary (link targets)
        e = dict(e)
        e["name"] = md + "/" + e["name"]
        entries.append(e)
    if bin_entry is None:
        entries.append({"name": md + "/localrouter", "data": binary})
    else:
        e = dict(bin_entry)
        e["name"] = md + "/localrouter"
        entries.append(e)
    if readme:
        entries.append({"name": md + "/README.md", "data": b"# readme\n",
                        "mode": 0o644})
    for e in extra:
        e = dict(e)
        e["name"] = md + "/" + e["name"]
        entries.append(e)
    return build_tar(os.path.join(dest, ra.archive_name(version, goos, goarch)),
                     entries)


def write_manifest(dest_dir, archive_paths):
    lines = []
    for p in archive_paths:
        lines.append("%s  %s" % (ra.sha256_file(p), os.path.basename(p)))
    path = os.path.join(dest_dir, "SHA256SUMS")
    with open(path, "w") as f:
        f.write("\n".join(lines) + "\n")
    return path


def write_raw_manifest(dest_dir, entries):
    """entries: list of (digest, name) written verbatim."""
    path = os.path.join(dest_dir, "SHA256SUMS")
    with open(path, "w") as f:
        f.write("".join("%s  %s\n" % e for e in entries))
    return path


def release_sh_shaped(dest_dir, version=VERSION):
    """Four archives + 4-entry SHA256SUMS, the exact shape release.sh writes."""
    paths = [make_archive(dest_dir, version=version, goos=goos, goarch=goarch,
                          binary=native_header(goos, goarch))
             for goos, goarch in ALL]
    write_manifest(dest_dir, paths)
    return paths


def make_stub_binary(dest_dir, name, check_exit=0, print_usage=True):
    path = os.path.join(dest_dir, name)
    body = "#!/bin/sh\n"
    body += 'case "$1" in\n'
    body += '  -h|--help|help) ' + ('echo "localrouter usage"; ' if print_usage else '') + 'exit 0;;\n'
    body += '  check) echo "config OK: 1 clients, 0 accounts, 0 routes; data_dir=/x"; exit %d;;\n' % check_exit
    body += '  *) echo "localrouter usage" >&2; exit 2;;\n'
    body += "esac\n"
    with open(path, "w") as f:
        f.write(body)
    os.chmod(path, 0o755)
    return path


def sentinel_stub(sentinel, tag="EXECUTED"):
    return ("#!/bin/sh\n"
            "echo \"%s $*\" >> '%s'\n"
            "case \"$1\" in\n"
            "  --help) echo usage; exit 0;;\n"
            "  check) echo 'config OK: sentinel'; exit 0;;\n"
            "esac\nexit 0\n" % (tag, sentinel)).encode()


def fake_bi(goos, goarch, path=MAIN_PKG, mod=MODULE, revision=REV,
            modified="false", cgo="0", trimpath="true", calls=None):
    """Stand-in for go_buildinfo: what `go version -m` reports."""
    def _bi(binary, *a, **k):
        if calls is not None:
            calls.append(binary)
        settings = {"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": cgo,
                    "-trimpath": trimpath}
        if revision is not None:  # None: unstamped (e.g. git worktree build)
            settings.update({"vcs": "git", "vcs.revision": revision,
                             "vcs.modified": modified})
        return {"goos": goos, "goarch": goarch, "path": path, "mod": mod,
                "go_version": "go1.26.8", "settings": settings}
    return _bi


def by_header_bi(**kw):
    """Fake buildinfo that agrees with whatever header the file carries."""
    def _bi(binary, *a, **k):
        with open(binary, "rb") as f:
            head = f.read(20)
        if head[:4] == b"\x7fELF":
            goos = "linux"
            machine = struct.unpack("<H", head[18:20])[0]
            goarch = {0x3E: "amd64", 0xB7: "arm64"}[machine]
        else:
            goos = "darwin"
            cpu = struct.unpack("<I", head[4:8])[0]
            goarch = {v: k for k, v in MACHO_CPU.items()}[cpu]
        return fake_bi(goos, goarch, **kw)(binary)
    return _bi


def fake_source(revision=REV, dirty=False, changes_after=None):
    """Fake git source state; ``changes_after`` = call count after which the
    status hash changes (simulates the tree changing during a build)."""
    calls = []

    def _src(repo_root, *a, **k):
        calls.append(k.get("exclude"))
        changed = changes_after is not None and len(calls) > changes_after
        return {"revision": revision, "dirty": dirty, "status_entries": 0,
                "status_sha256": "changed" if changed else "same",
                "describe": "v-test"}
    _src.calls = calls
    return _src


def quiet(*a, **k):
    return None


def read_text(path):
    if not os.path.exists(path):
        return ""
    with open(path) as f:
        return f.read()


def fake_snapshot(repo_root, source, dest, deadline=None):
    """Stand-in for the committed-tree snapshot: a plain copy of repo_root
    (the fixture repos used with it are not git repositories)."""
    shutil.copytree(repo_root, dest, symlinks=True)
    return {"policy": "committed_tree_snapshot", "path": dest,
            "revision": source["revision"], "tree": "f" * 40,
            "content_sha256": "c" * 64, "dirty_build_inputs": [],
            "excluded_dirty": []}


def _force_rmtree(path):
    """Test-side cleanup that copes with read-only snapshot dirs."""
    for root, dirs, _files in os.walk(path):
        for d in dirs:
            p = os.path.join(root, d)
            if not os.path.islink(p):
                os.chmod(p, 0o700)
    shutil.rmtree(path, True)


class ScratchCase(unittest.TestCase):
    def mkdtemp(self, prefix="ra-test-"):
        d = tempfile.mkdtemp(prefix=prefix, dir=TEST_TMP)
        self.addCleanup(_force_rmtree, d)
        return d

    @contextlib.contextmanager
    def faked(self, bi, source=None, real_git=False):
        """Fake go buildinfo, plus (unless ``real_git``) the git source state
        and source snapshot (create=True so these tests load against any
        harness revision)."""
        with contextlib.ExitStack() as stack:
            stack.enter_context(mock.patch.object(ra, "go_buildinfo", bi))
            if not real_git:
                stack.enter_context(mock.patch.object(
                    ra, "source_state", source or fake_source(), create=True))
                stack.enter_context(mock.patch.object(
                    ra, "source_snapshot", fake_snapshot, create=True))
                stack.enter_context(mock.patch.object(
                    ra, "recheck_snapshot",
                    lambda rec, **k: rec["content_sha256"], create=True))
            yield


# --------------------------------------------------------------------------
# contract
# --------------------------------------------------------------------------
class ContractTests(unittest.TestCase):
    def test_archive_name_matches_release_contract(self):
        self.assertEqual(ra.archive_name("1.2.3", "linux", "amd64"),
                         "localrouter_1.2.3_linux_amd64.tar.gz")
        self.assertEqual(ra.archive_name("v1.2.3", "darwin", "arm64"),
                         "localrouter_1.2.3_darwin_arm64.tar.gz")

    def test_default_targets_match_release_sh(self):
        self.assertEqual(ra.DEFAULT_TARGETS,
                         ("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"))

    def test_unknown_target_rejected(self):
        for bad in ("windows/amd64", "linux/386", "linux", "windows"):
            with self.assertRaises(ra.AcceptanceError):
                ra.validate_target(bad)

    def test_known_targets_accepted(self):
        for t in ra.DEFAULT_TARGETS:
            self.assertEqual(ra.validate_target(t), t)

    def test_resolve_arches_first_slice(self):
        self.assertEqual(ra.resolve_arches("host,arm64", ("linux", "amd64")),
                         [("linux", "amd64"), ("linux", "arm64")])

    def test_resolve_arches_all(self):
        self.assertEqual(ra.resolve_arches("all", ("linux", "amd64")),
                         [("linux", "amd64"), ("linux", "arm64"),
                          ("darwin", "amd64"), ("darwin", "arm64")])

    def test_resolve_arches_rejects_unknown(self):
        with self.assertRaises(ra.AcceptanceError):
            ra.resolve_arches("windows/amd64", ("linux", "amd64"))


# --------------------------------------------------------------------------
# archive shape
# --------------------------------------------------------------------------
class ArchiveTests(ScratchCase):
    def setUp(self):
        self.tmp = self.mkdtemp()

    def test_good_archive_accepted(self):
        p = make_archive(self.tmp)
        info = ra.verify_archive(p, "1.2.3", "linux", "amd64")
        self.assertIn(ra.BIN_NAME, info["files"])
        self.assertIn("README.md", info["files"])

    def test_missing_archive_rejected(self):
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_archive(os.path.join(self.tmp, "nope.tar.gz"),
                              "1.2.3", "linux", "amd64")

    def test_missing_binary_rejected(self):
        p = make_archive(self.tmp, include_binary=False)
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_archive(p, "1.2.3", "linux", "amd64")

    def test_missing_readme_rejected(self):
        p = make_archive(self.tmp, include_readme=False)
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_archive(p, "1.2.3", "linux", "amd64")

    def test_wrong_member_dir_rejected(self):
        p = make_archive(self.tmp, member_dir="localrouter_9.9.9_linux_amd64")
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_archive(p, "1.2.3", "linux", "amd64")

    def test_path_traversal_and_nested_members_rejected(self):
        md = ra.archive_member_dir(VERSION, "linux", "arm64")
        elf = elf64_header(ra.ELF_MACHINE["arm64"])
        for bad in ("../evil", "/abs/evil", md + "/../evil", md + "/sub/evil"):
            a = os.path.join(self.tmp, ra.archive_name(VERSION, "linux", "arm64"))
            build_tar(a, [{"name": md, "type": "dir"},
                          {"name": md + "/localrouter", "data": elf},
                          {"name": md + "/README.md", "data": b"r"},
                          {"name": bad, "data": b"x"}])
            with self.assertRaises(ra.AcceptanceError, msg=bad):
                ra.verify_archive(a, VERSION, "linux", "arm64")


class ArchiveShapeRegressionTests(ScratchCase):
    """Ported from the audit verify repro (F6): member types, allowlist,
    duplicates, exec mode, size caps, and clean (AcceptanceError) failures."""
    GOOS, GOARCH = "linux", "arm64"

    def setUp(self):
        self.out = self.mkdtemp("f6-")
        self.elf = elf64_header(ra.ELF_MACHINE[self.GOARCH])
        self.md = ra.archive_member_dir(VERSION, self.GOOS, self.GOARCH)

    def _accept(self, archive):
        ra.verify_archive(archive, VERSION, self.GOOS, self.GOARCH)
        return ra.verify_arch_proof(archive, VERSION, self.GOOS, self.GOARCH,
                                    use_buildinfo=False)

    def _reject(self, archive, fragment):
        with self.assertRaises(ra.AcceptanceError) as cm:
            self._accept(archive)
        self.assertIn(fragment, str(cm.exception))

    def test_extra_top_level_file_rejected(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH, self.elf,
                        extra=[{"name": "evil.sh", "data": b"#!/bin/sh\n"}])
        self._reject(a, "unexpected member")

    def test_symlink_binary_rejected(self):
        # The link points at an allowed sibling, so only the member TYPE is
        # wrong (no extra payload member masks the type check).
        a = std_archive(self.out, self.GOOS, self.GOARCH, None,
                        pre=[{"name": "README.md", "data": self.elf,
                              "mode": 0o644}],
                        bin_entry={"type": "sym", "link": "README.md"},
                        readme=False)
        self._reject(a, "not a regular file")

    def test_hardlink_binary_rejected(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH, None,
                        pre=[{"name": "README.md", "data": self.elf,
                              "mode": 0o755}],
                        bin_entry={"type": "lnk", "link": self.md + "/README.md"},
                        readme=False)
        self._reject(a, "not a regular file")

    def test_dangling_absolute_symlink_is_clean_acceptance_error(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH, None,
                        bin_entry={"type": "sym", "link": "/usr/bin/true"})
        self._reject(a, "not a regular file")

    def test_directory_binary_is_clean_acceptance_error(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH, None,
                        bin_entry={"type": "dir"})
        self._reject(a, "not a regular file")

    def test_non_executable_binary_mode_rejected(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH, None,
                        bin_entry={"data": self.elf, "mode": 0o644})
        self._reject(a, "not executable")

    def test_setuid_binary_mode_rejected(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH, None,
                        bin_entry={"data": self.elf, "mode": 0o4755})
        self._reject(a, "mode")

    def test_duplicate_binary_member_rejected(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH, self.elf,
                        extra=[{"name": "localrouter",
                                "data": self.elf + b"second"}])
        with tarfile.open(a, "r:gz") as tf:
            self.assertEqual(tf.getnames().count(self.md + "/localrouter"), 2)
        self._reject(a, "duplicate")

    def test_member_dir_symlink_rejected(self):
        a = os.path.join(self.out, ra.archive_name(VERSION, self.GOOS, self.GOARCH))
        build_tar(a, [{"name": self.md, "type": "sym", "link": "/etc"},
                      {"name": self.md + "/localrouter", "data": self.elf},
                      {"name": self.md + "/README.md", "data": b"r"}])
        self._reject(a, "not a directory")

    def test_oversized_binary_member_rejected(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH,
                        self.elf + b"\x00" * 4096)
        with mock.patch.object(ra, "MAX_BINARY_BYTES", 1024, create=True):
            self._reject(a, "exceeds")

    def test_oversized_doc_member_rejected(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH, self.elf,
                        extra=[{"name": "LICENSE", "data": b"L" * 4096,
                                "mode": 0o644}])
        with mock.patch.object(ra, "MAX_DOC_BYTES", 1024, create=True):
            self._reject(a, "exceeds")

    def test_decompression_bound_covers_pax_headers(self):
        # tarfile reads a pax header's payload into memory before any member
        # check runs; the decompressed stream itself must be bounded.
        a = os.path.join(self.out, ra.archive_name(VERSION, self.GOOS, self.GOARCH))
        with tarfile.open(a, "w:gz", format=tarfile.PAX_FORMAT) as tf:
            ti = tarfile.TarInfo(self.md)
            ti.type = tarfile.DIRTYPE
            ti.pax_headers = {"comment": "x" * (256 << 10)}
            tf.addfile(ti)
        with mock.patch.object(ra, "MAX_EXPANDED_BYTES", 64 << 10, create=True):
            self._reject(a, "expands beyond")

    def test_corrupt_gzip_is_clean_acceptance_error(self):
        a = os.path.join(self.out, ra.archive_name(VERSION, self.GOOS, self.GOARCH))
        with open(a, "wb") as f:
            f.write(gzip.compress(b"this is not a tar stream" * 40)[:-9])
        with self.assertRaises(ra.AcceptanceError):
            self._accept(a)

    def test_license_member_allowed(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH, self.elf,
                        extra=[{"name": "LICENSE", "data": b"MIT\n",
                                "mode": 0o644}])
        self.assertEqual(self._accept(a)["arch"], "arm64")

    def test_extracted_binary_keeps_exec_bits_from_archive_only(self):
        a = std_archive(self.out, self.GOOS, self.GOARCH, self.elf)
        d = self.mkdtemp("f6-ext-")
        p = ra.extract_member(a, ra.BIN_NAME, d)
        mode = stat.S_IMODE(os.stat(p).st_mode)
        self.assertTrue(mode & 0o100)
        self.assertFalse(mode & 0o022, oct(mode))


# --------------------------------------------------------------------------
# architecture proof
# --------------------------------------------------------------------------
class ElfTests(ScratchCase):
    def setUp(self):
        self.tmp = self.mkdtemp("ra-elf-")

    def test_aarch64_detected(self):
        p = os.path.join(self.tmp, "b")
        with open(p, "wb") as f:
            f.write(elf64_header(ra.ELF_MACHINE["arm64"]))
        self.assertEqual(ra.elf_machine_arch(ra.elf_machine(p)), "arm64")

    def test_amd64_detected(self):
        p = os.path.join(self.tmp, "b")
        with open(p, "wb") as f:
            f.write(elf64_header(ra.ELF_MACHINE["amd64"]))
        self.assertEqual(ra.elf_machine_arch(ra.elf_machine(p)), "amd64")

    def test_non_elf_rejected(self):
        p = os.path.join(self.tmp, "b")
        with open(p, "wb") as f:
            f.write(b"not an elf file at all")
        self.assertIsNone(ra.elf_machine(p))

    def test_wrong_arch_rejected(self):
        # archive claims amd64 but the embedded ELF is AArch64
        p = make_archive(self.tmp, goos="linux", goarch="amd64",
                         binary=elf64_header(ra.ELF_MACHINE["arm64"]))
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_arch_proof(p, "1.2.3", "linux", "amd64", use_buildinfo=False)

    def test_matching_arch_accepted(self):
        p = make_archive(self.tmp, goos="linux", goarch="amd64",
                         binary=elf64_header(ra.ELF_MACHINE["amd64"]))
        proof = ra.verify_arch_proof(p, "1.2.3", "linux", "amd64", use_buildinfo=False)
        self.assertEqual(proof["arch"], "amd64")

    def test_elf32_rejected_for_64bit_target(self):
        p = make_archive(self.tmp, goos="linux", goarch="arm64",
                         binary=elf64_header(ra.ELF_MACHINE["arm64"], ei_class=1))
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_arch_proof(p, "1.2.3", "linux", "arm64", use_buildinfo=False)

    def test_macho_cputype_proves_darwin_arch_without_buildinfo(self):
        for goarch in ("amd64", "arm64"):
            p = make_archive(self.tmp, goos="darwin", goarch=goarch,
                             binary=macho64_header(goarch))
            proof = ra.verify_arch_proof(p, "1.2.3", "darwin", goarch,
                                         use_buildinfo=False)
            self.assertEqual(proof["arch"], goarch)

    def test_macho_wrong_cputype_rejected(self):
        p = make_archive(self.tmp, goos="darwin", goarch="arm64",
                         binary=macho64_header("amd64"))
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_arch_proof(p, "1.2.3", "darwin", "arm64", use_buildinfo=False)


class ArchProofBuildinfoRegressionTests(ScratchCase):
    """Ported from the audit verify repro (F2): with build info present the
    proof must still check GOOS and read the ELF / Mach-O header itself."""

    def setUp(self):
        self.out = self.mkdtemp("f2-")

    def test_darwin_binary_in_linux_arm64_archive_rejected(self):
        a = std_archive(self.out, "linux", "arm64", macho64_header("arm64"))
        with mock.patch.object(ra, "go_buildinfo", fake_bi("darwin", "arm64")):
            with self.assertRaises(ra.AcceptanceError):
                ra.verify_arch_proof(a, VERSION, "linux", "arm64")

    def test_linux_binary_in_darwin_arm64_archive_rejected(self):
        a = std_archive(self.out, "darwin", "arm64",
                        elf64_header(ra.ELF_MACHINE["arm64"]))
        with mock.patch.object(ra, "go_buildinfo", fake_bi("linux", "arm64")):
            with self.assertRaises(ra.AcceptanceError):
                ra.verify_arch_proof(a, VERSION, "darwin", "arm64")

    def test_buildinfo_goos_mismatch_rejected_even_when_header_matches(self):
        a = std_archive(self.out, "linux", "arm64",
                        elf64_header(ra.ELF_MACHINE["arm64"]))
        with mock.patch.object(ra, "go_buildinfo", fake_bi("darwin", "arm64")):
            with self.assertRaises(ra.AcceptanceError) as cm:
                ra.verify_arch_proof(a, VERSION, "linux", "arm64")
        self.assertIn("GOOS", str(cm.exception))

    def test_elf_machine_disagreeing_with_buildinfo_rejected(self):
        a = std_archive(self.out, "linux", "arm64",
                        elf64_header(ra.ELF_MACHINE["amd64"]))
        with mock.patch.object(ra, "go_buildinfo", fake_bi("linux", "arm64")):
            with self.assertRaises(ra.AcceptanceError):
                ra.verify_arch_proof(a, VERSION, "linux", "arm64")

    def test_proof_records_both_sources(self):
        a = std_archive(self.out, "linux", "arm64",
                        elf64_header(ra.ELF_MACHINE["arm64"]))
        with mock.patch.object(ra, "go_buildinfo", fake_bi("linux", "arm64")):
            proof = ra.verify_arch_proof(a, VERSION, "linux", "arm64")
        self.assertEqual(proof["machine"], ra.ELF_MACHINE["arm64"])
        self.assertEqual(proof["buildinfo"]["goos"], "linux")

    def test_missing_buildinfo_fails_closed(self):
        a = std_archive(self.out, "linux", "arm64",
                        elf64_header(ra.ELF_MACHINE["arm64"]))
        with mock.patch.object(ra, "go_buildinfo", lambda *a, **k: None):
            with self.assertRaises(ra.AcceptanceError):
                ra.verify_arch_proof(a, VERSION, "linux", "arm64")

    def test_end_to_end_verify_only_rejects_darwin_in_linux_arm64(self):
        a = std_archive(self.out, "linux", "arm64", macho64_header("arm64"))
        write_manifest(self.out, [a])
        with self.faked(fake_bi("darwin", "arm64")):
            with self.assertRaises(ra.AcceptanceError):
                ra.run_acceptance(self.out, VERSION, "arm64", "none", REPO,
                                  log=quiet)


class IdentityRegressionTests(ScratchCase):
    """The artifact must be the LocalRouter main package from this source."""

    def setUp(self):
        self.out = self.mkdtemp("id-")
        release_sh_shaped(self.out)

    def _run(self, bi, source=None):
        with self.faked(bi, source):
            return ra.run_acceptance(self.out, VERSION, CROSS_T, "none", REPO,
                                     log=quiet)

    def test_foreign_main_package_rejected(self):
        with self.assertRaises(ra.AcceptanceError) as cm:
            self._run(by_header_bi(path="command-line-arguments"))
        self.assertIn("path", str(cm.exception))

    def test_unstamped_binary_rejected_in_verify_only(self):
        # Without a vcs stamp nothing ties pre-existing artifacts to source.
        with self.assertRaises(ra.AcceptanceError) as cm:
            self._run(by_header_bi(revision=None))
        self.assertIn("vcs.revision", str(cm.exception))

    def test_foreign_module_rejected(self):
        with self.assertRaises(ra.AcceptanceError):
            self._run(by_header_bi(mod="example.com/evil"))

    def test_revision_mismatch_rejected(self):
        with self.assertRaises(ra.AcceptanceError) as cm:
            self._run(by_header_bi(revision="f" * 40))
        self.assertIn("revision", str(cm.exception))

    def test_cgo_build_rejected(self):
        with self.assertRaises(ra.AcceptanceError):
            self._run(by_header_bi(cgo="1"))

    def test_untrimmed_build_rejected(self):
        with self.assertRaises(ra.AcceptanceError):
            self._run(by_header_bi(trimpath="false"))

    def test_matching_identity_accepted_and_recorded(self):
        report = self._run(by_header_bi(modified="true"),
                           fake_source(dirty=True))
        entry = report["archives"][CROSS_T]
        self.assertEqual(entry["identity"]["vcs.revision"], REV)
        self.assertEqual(entry["identity"]["vcs.modified"], "true")
        self.assertEqual(report["source"]["revision"], REV)
        self.assertTrue(report["source"]["dirty"])


# --------------------------------------------------------------------------
# manifest
# --------------------------------------------------------------------------
class ManifestTests(ScratchCase):
    def setUp(self):
        self.tmp = self.mkdtemp("ra-manifest-")

    def test_good_manifest_accepted(self):
        a = make_archive(self.tmp, goos="linux", goarch="amd64")
        b = make_archive(self.tmp, goos="linux", goarch="arm64",
                         binary=elf64_header(ra.ELF_MACHINE["arm64"]))
        write_manifest(self.tmp, [a, b])
        ra.verify_manifest(self.tmp, [a, b])

    def test_wrong_checksum_rejected(self):
        a = make_archive(self.tmp, goos="linux", goarch="amd64")
        write_manifest(self.tmp, [a])
        with open(a, "ab") as f:
            f.write(b"\x00tamper")
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_manifest(self.tmp, [a])

    def test_missing_manifest_rejected(self):
        a = make_archive(self.tmp, goos="linux", goarch="amd64")
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_manifest(self.tmp, [a])

    def test_archive_absent_from_manifest_rejected(self):
        a = make_archive(self.tmp, goos="linux", goarch="amd64")
        b = make_archive(self.tmp, goos="linux", goarch="arm64",
                         binary=elf64_header(ra.ELF_MACHINE["arm64"]))
        write_manifest(self.tmp, [a])  # b missing
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_manifest(self.tmp, [a, b])

    def test_manifest_lists_absent_archive_rejected(self):
        a = make_archive(self.tmp, goos="linux", goarch="amd64")
        write_manifest(self.tmp, [a])
        with open(os.path.join(self.tmp, "SHA256SUMS"), "a") as f:
            f.write("%s  localrouter_1.2.3_darwin_arm64.tar.gz\n" % ("0" * 64))
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_manifest(self.tmp, [a])

    def test_release_sh_four_entry_manifest_accepts_selected_subset(self):
        paths = release_sh_shaped(self.tmp)
        ra.verify_manifest(self.tmp, paths[1:2])

    def test_tampered_unselected_entry_still_rejected(self):
        paths = release_sh_shaped(self.tmp)
        with open(paths[3], "ab") as f:
            f.write(b"\x00tamper")
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_manifest(self.tmp, paths[1:2])

    def test_unsafe_manifest_names_rejected(self):
        a = make_archive(self.tmp, goos="linux", goarch="amd64")
        digest = ra.sha256_file(a)
        for bad in ("../" + os.path.basename(a), "sub/" + os.path.basename(a),
                    "/etc/passwd", "localrouter_1.2.3_windows_amd64.tar.gz",
                    "README.md"):
            write_raw_manifest(self.tmp, [(digest, os.path.basename(a)),
                                          (digest, bad)])
            with self.assertRaises(ra.AcceptanceError, msg=bad):
                ra.verify_manifest(self.tmp, [a], version=VERSION)

    def test_duplicate_manifest_entry_rejected(self):
        a = make_archive(self.tmp, goos="linux", goarch="amd64")
        d = ra.sha256_file(a)
        write_raw_manifest(self.tmp, [(d, os.path.basename(a)),
                                      (d, os.path.basename(a))])
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_manifest(self.tmp, [a])

    def test_symlinked_archive_rejected(self):
        real = make_archive(self.mkdtemp("ra-real-"), goos="linux", goarch="amd64")
        link = os.path.join(self.tmp, os.path.basename(real))
        os.symlink(real, link)
        write_manifest(self.tmp, [real])
        with self.assertRaises(ra.AcceptanceError):
            ra.verify_manifest(self.tmp, [link])

    def test_oversized_archive_rejected(self):
        a = make_archive(self.tmp, goos="linux", goarch="amd64")
        write_manifest(self.tmp, [a])
        with mock.patch.object(ra, "MAX_ARCHIVE_BYTES", 16, create=True):
            with self.assertRaises(ra.AcceptanceError):
                ra.verify_manifest(self.tmp, [a])


class VerifyOnlyModeTests(ScratchCase):
    """Verify-only mode must validate the *existing* manifest, never rewrite it."""

    def setUp(self):
        self.tmp = self.mkdtemp("ra-none-")

    def _cross_archive(self):
        return make_archive(self.tmp, version="1.2.3", goos=CROSS[0],
                            goarch=CROSS[1], binary=native_header(*CROSS))

    def test_verify_only_accepts_existing_good_manifest(self):
        a = self._cross_archive()
        write_manifest(self.tmp, [a])
        with self.faked(fake_bi(*CROSS)):
            report = ra.run_acceptance(self.tmp, "1.2.3", CROSS_T, "none",
                                       REPO, log=quiet)
        self.assertIn(CROSS_T, report["archives"])
        self.assertIs(report["archives"][CROSS_T]["executed"], False)

    def test_verify_only_rejects_tampered_archive(self):
        a = self._cross_archive()
        write_manifest(self.tmp, [a])
        with open(a, "ab") as f:
            f.write(b"\x00tamper")
        with self.faked(fake_bi(*CROSS)):
            with self.assertRaises(ra.AcceptanceError):
                ra.run_acceptance(self.tmp, "1.2.3", CROSS_T, "none", REPO,
                                  log=quiet)

    def test_verify_only_accepts_genuine_release_sh_output_with_subset(self):
        # Documented invocation: --build-mode none with a target subset
        # against the 4-entry SHA256SUMS release.sh writes.
        release_sh_shaped(self.tmp)
        with self.faked(by_header_bi()):
            report = ra.run_acceptance(self.tmp, VERSION, CROSS_T, "none",
                                       REPO, log=quiet)
        self.assertEqual(list(report["archives"]), [CROSS_T])
        self.assertEqual(len(report["manifest"]["entries"]), 4)

    def test_verify_only_does_not_rewrite_manifest(self):
        release_sh_shaped(self.tmp)
        mpath = os.path.join(self.tmp, "SHA256SUMS")
        with open(mpath, "rb") as f:
            before = f.read()
        with self.faked(by_header_bi()):
            ra.run_acceptance(self.tmp, VERSION, NON_HOST_SPEC, "none", REPO,
                              log=quiet)
        with open(mpath, "rb") as f:
            self.assertEqual(f.read(), before)


# --------------------------------------------------------------------------
# ordering / TOCTOU (F1)
# --------------------------------------------------------------------------
class HostTamperOrderingRegressionTests(ScratchCase):
    """Ported from the audit verify repro (F1): nothing from a host archive is
    executed unless its bytes match SHA256SUMS, and the executed bytes are
    the verified snapshot rather than a re-opened out-dir file."""

    def setUp(self):
        self.host = ra.detect_host_target()
        self.out = self.mkdtemp("f1-out-")
        self.sentinel = os.path.join(self.out, "SENTINEL_EXECUTED")
        self.header = {"format": "elf" if self.host[0] == "linux" else "macho",
                       "goos": self.host[0], "goarch": self.host[1]}

    def _host_header(self):
        # Shell-script stubs are not ELF; fake the header reader so the run
        # gets as far as execution when everything else is valid.
        return mock.patch.object(ra, "binary_header",
                                 lambda path: dict(self.header), create=True)

    def test_tampered_host_archive_is_not_executed(self):
        goos, goarch = self.host
        archive = std_archive(self.out, goos, goarch,
                              sentinel_stub(self.sentinel))
        write_raw_manifest(self.out, [("0" * 64, os.path.basename(archive))])
        calls = []
        with self.faked(fake_bi(goos, goarch, calls=calls)), self._host_header():
            with self.assertRaises(ra.AcceptanceError) as cm:
                ra.run_acceptance(self.out, VERSION, "host", "none", REPO,
                                  log=quiet)
        self.assertIn("checksum mismatch", str(cm.exception))
        self.assertFalse(
            os.path.exists(self.sentinel),
            "host binary was EXECUTED before the checksum was rejected; "
            "sentinel contents: %r" % read_text(self.sentinel))
        self.assertEqual(calls, [], "archive bytes were parsed before checksum")

    def test_tampered_unselected_manifest_entry_blocks_host_execution(self):
        goos, goarch = self.host
        paths = []
        for g, a in ALL:
            data = (sentinel_stub(self.sentinel) if (g, a) == self.host
                    else native_header(g, a))
            paths.append(std_archive(self.out, g, a, data))
        write_manifest(self.out, paths)
        other = [p for p in paths if "_%s_%s." % self.host not in p][0]
        with open(other, "ab") as f:
            f.write(b"\x00tamper")
        with self.faked(fake_bi(goos, goarch)), self._host_header():
            with self.assertRaises(ra.AcceptanceError):
                ra.run_acceptance(self.out, VERSION, "host", "none", REPO,
                                  log=quiet)
        self.assertFalse(os.path.exists(self.sentinel))

    def test_archive_swapped_after_verification_is_not_executed(self):
        goos, goarch = self.host
        good = std_archive(self.out, goos, goarch,
                           sentinel_stub(self.sentinel, "GOOD"))
        write_manifest(self.out, [good])
        evil_dir = self.mkdtemp("f1-evil-")
        evil = std_archive(evil_dir, goos, goarch,
                           sentinel_stub(self.sentinel, "EVIL"))
        swapped = []
        real_bi = fake_bi(goos, goarch)

        def swapping_bi(binary, *a, **k):
            if not swapped:  # attacker replaces the out-dir file mid-run
                shutil.copyfile(evil, good)
                swapped.append(True)
            return real_bi(binary)

        with self.faked(swapping_bi), self._host_header():
            with self.assertRaises(ra.AcceptanceError) as cm:
                ra.run_acceptance(self.out, VERSION, "host", "none", REPO,
                                  log=quiet)
        log = read_text(self.sentinel)
        self.assertNotIn("EVIL", log)
        self.assertIn("changed", str(cm.exception))

    def test_verified_host_archive_is_executed_and_flagged(self):
        goos, goarch = self.host
        good = std_archive(self.out, goos, goarch,
                           sentinel_stub(self.sentinel, "GOOD"))
        write_manifest(self.out, [good])
        with self.faked(fake_bi(goos, goarch)), self._host_header():
            report = ra.run_acceptance(self.out, VERSION, "host", "none", REPO,
                                       log=quiet)
        entry = report["archives"]["%s/%s" % self.host]
        self.assertIs(entry["executed"], True)
        self.assertEqual(entry["host_run"]["help_exit"], 0)
        self.assertEqual(entry["host_run"]["check_exit"], 0)
        self.assertEqual(entry["host_run"]["binary_sha256"],
                         entry["binary_sha256"])
        lines = read_text(self.sentinel).splitlines()
        self.assertEqual([l.split()[:2] for l in lines],
                         [["GOOD", "--help"], ["GOOD", "check"]])


# --------------------------------------------------------------------------
# host binary invocation
# --------------------------------------------------------------------------
class HostRunTests(ScratchCase):
    def setUp(self):
        self.tmp = self.mkdtemp("ra-run-")

    def test_good_binary_check_passes(self):
        b = make_stub_binary(self.tmp, "good")
        res = ra.run_binary_check(b, self.tmp)
        self.assertEqual(res["help_exit"], 0)
        self.assertEqual(res["check_exit"], 0)
        self.assertIn("config OK", res["check_stdout"])

    def test_failing_check_rejected(self):
        b = make_stub_binary(self.tmp, "bad", check_exit=1)
        with self.assertRaises(ra.AcceptanceError):
            ra.run_binary_check(b, self.tmp)

    def test_placeholder_config_has_no_provider_keys(self):
        cfg = ra.write_placeholder_config(self.tmp)
        with open(cfg) as f:
            text = f.read()
        for forbidden in ("api_key", "base_url", "accounts:"):
            self.assertNotIn(forbidden, text)
        keyfile = os.path.join(self.tmp, "keys", "acceptance.key")
        self.assertTrue(os.path.exists(keyfile))
        mode = stat.S_IMODE(os.stat(keyfile).st_mode)
        self.assertEqual(mode, 0o600)

    def test_hanging_binary_is_clean_timeout(self):
        path = os.path.join(self.tmp, "hang")
        with open(path, "w") as f:
            f.write("#!/bin/sh\nsleep 30\n")
        os.chmod(path, 0o755)
        start = time.monotonic()
        with self.assertRaises(ra.AcceptanceError) as cm:
            ra.run_binary_check(path, self.tmp, deadline=ra.Deadline(1.5))
        self.assertLess(time.monotonic() - start, 10)
        self.assertIn("timed out", str(cm.exception))

    def test_unexecutable_binary_is_clean_acceptance_error(self):
        path = os.path.join(self.tmp, "noexec")
        with open(path, "w") as f:
            f.write("#!/bin/sh\nexit 0\n")
        os.chmod(path, 0o644)
        with self.assertRaises(ra.AcceptanceError):
            ra.run_binary_check(path, self.tmp)

    def test_binary_changed_before_exec_rejected(self):
        b = make_stub_binary(self.tmp, "good")
        with self.assertRaises(ra.AcceptanceError):
            ra.run_binary_check(b, self.tmp, expected_sha256="0" * 64)


# --------------------------------------------------------------------------
# script mode, deadlines, scratch, report
# --------------------------------------------------------------------------
FAKE_RELEASE_SH = r'''#!/usr/bin/env bash
set -euo pipefail
out="$1"; ver="${2#v}"
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
printf '%s\n' "$root" > "$out/.fake-release-root"
if [ -f "$root/docs/notes.md" ]; then cp "$root/docs/notes.md" "$out/.fake-release-notes"; fi
if [ -n "${FAKE_RELEASE_TOUCH:-}" ]; then echo "edited during build" >> "$root/$FAKE_RELEASE_TOUCH"; fi
printf '%s\n' "${TMPDIR:-unset}" > "$out/.fake-release-tmpdir"
printf '%s %s %s\n' "${GOPROXY:-unset}" "${GOMAXPROCS:-unset}" "${GOFLAGS:-unset}" > "$out/.fake-release-env"
if [ -n "${FAKE_RELEASE_SLEEP:-}" ]; then sleep "$FAKE_RELEASE_SLEEP"; fi
cp "$FIXTURE_DIR"/*.tar.gz "$out"/
cp "$FIXTURE_DIR"/SHA256SUMS "$out"/SHA256SUMS
echo "wrote 4 archives"
'''


class ScriptModeTests(ScratchCase):
    def setUp(self):
        self.repo = self.mkdtemp("ra-repo-")
        os.makedirs(os.path.join(self.repo, "scripts"))
        with open(os.path.join(self.repo, "scripts", "release.sh"), "w") as f:
            f.write(FAKE_RELEASE_SH)
        with open(os.path.join(self.repo, "go.mod"), "w") as f:
            f.write("module %s\n\ngo 1.26\n" % MODULE)
        self.fixture = self.mkdtemp("ra-fixture-")
        release_sh_shaped(self.fixture)
        self.out = self.mkdtemp("ra-out-")
        self.reports = self.mkdtemp("ra-reports-")
        self.env = mock.patch.dict(os.environ, {"FIXTURE_DIR": self.fixture})
        self.env.start()
        self.addCleanup(self.env.stop)

    def _main(self, *extra, bi=None, source=None):
        report = os.path.join(self.reports, "report.json")
        argv = ["--out-dir", self.out, "--version", VERSION,
                "--build-mode", "script", "--repo-root", self.repo,
                "--report", report] + list(extra)
        out, err = io.StringIO(), io.StringIO()
        with self.faked(bi or by_header_bi(), source), \
                mock.patch.object(ra, "run_binary_check",
                                  lambda b, w, **k: {"help_exit": 0,
                                                     "check_exit": 0,
                                                     "check_stdout": "config OK",
                                                     "binary_sha256": k.get("expected_sha256")}), \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = ra.main(argv)
        with open(report) as f:
            data = json.load(f)
        return rc, out.getvalue(), err.getvalue(), data

    def test_script_mode_runs_release_sh_offline_with_durable_tmpdir(self):
        rc, out, err, data = self._main("--scratch-dir",
                                        os.path.join(self.out, "scratch"))
        self.assertEqual(rc, 0, err)
        with open(os.path.join(self.out, ".fake-release-tmpdir")) as f:
            tmpdir = f.read().strip()
        self.assertTrue(tmpdir.startswith(os.path.join(self.out, "scratch")), tmpdir)
        self.assertFalse(tmpdir.startswith("/tmp"))
        with open(os.path.join(self.out, ".fake-release-env")) as f:
            self.assertEqual(f.read().split(), ["off", "2", "-p=2"])
        self.assertEqual(data["mode"], "release_sh")
        self.assertEqual(sorted(data["archives"]), sorted(ra.DEFAULT_TARGETS))
        # scratch is cleaned after the run
        self.assertEqual(os.listdir(os.path.join(self.out, "scratch")), [])

    def test_script_mode_report_flags_execution_per_target(self):
        rc, out, err, data = self._main()
        self.assertEqual(rc, 0, err)
        host = "%s/%s" % ra.detect_host_target()
        for t, entry in data["archives"].items():
            self.assertIs(entry["executed"], t == host, t)
            self.assertEqual(len(entry["archive_sha256"]), 64)
            self.assertIn("header", entry["proof"])
            self.assertIn("buildinfo", entry["proof"])
        self.assertEqual(data["outcome"], "pass")
        self.assertEqual(data["flags"]["build_mode"], "script")
        self.assertEqual(data["flags"]["version"], VERSION)
        self.assertIn("max_runtime", data["flags"])
        self.assertEqual(data["source"]["revision"], REV)
        self.assertIn("dirty", data["source"])
        self.assertIn("NOT EXECUTED", out)

    def test_script_mode_timeout_is_clean_failure_with_report(self):
        os.environ["FAKE_RELEASE_SLEEP"] = "30"
        self.addCleanup(os.environ.pop, "FAKE_RELEASE_SLEEP", None)
        start = time.monotonic()
        rc, out, err, data = self._main("--max-runtime", "2")
        self.assertLess(time.monotonic() - start, 15)
        self.assertEqual(rc, 1)
        self.assertIn("timed out", err)
        self.assertEqual(data["outcome"], "fail")
        self.assertIn("timed out", data["error"])

    def test_script_mode_requires_full_contract_manifest(self):
        # release.sh always writes four entries; fewer means it is not the
        # genuine release.sh output.
        with open(os.path.join(self.fixture, "SHA256SUMS")) as f:
            lines = f.read().splitlines()
        with open(os.path.join(self.fixture, "SHA256SUMS"), "w") as f:
            f.write("\n".join(lines[:2]) + "\n")
        for p in os.listdir(self.fixture):
            if p.endswith(".tar.gz") and not any(p in l for l in lines[:2]):
                os.remove(os.path.join(self.fixture, p))
        rc, out, err, data = self._main()
        self.assertEqual(rc, 1)
        self.assertEqual(data["outcome"], "fail")

    def test_unstamped_release_sh_build_tied_by_build_provenance(self):
        # Go does not stamp vcs.* when building from a git worktree; the
        # harness built these itself from a recorded, unchanged source state.
        source = fake_source()
        rc, out, err, data = self._main(bi=by_header_bi(revision=None),
                                        source=source)
        self.assertEqual(rc, 0, err)
        for t, entry in data["archives"].items():
            self.assertEqual(entry["identity"]["revision_source"],
                             "build_provenance", t)
            self.assertIsNone(entry["identity"]["vcs.revision"])
        self.assertEqual(len(source.calls), 2)
        self.assertIn(self.out, source.calls[1])

    def test_stamped_release_sh_build_uses_vcs_stamp(self):
        rc, out, err, data = self._main()
        self.assertEqual(rc, 0, err)
        for entry in data["archives"].values():
            self.assertEqual(entry["identity"]["revision_source"], "vcs_stamp")

    def test_build_mode_rejects_stamp_from_modified_tree(self):
        # The build compiles a clean committed-tree snapshot; a stamp saying
        # "modified" would claim the revision for content it does not hold.
        rc, out, err, data = self._main(bi=by_header_bi(modified="true"))
        self.assertEqual(rc, 1)
        self.assertIn("vcs.modified", data["error"] or "")

    def test_source_change_during_build_rejected(self):
        rc, out, err, data = self._main(bi=by_header_bi(revision=None),
                                        source=fake_source(changes_after=1))
        self.assertEqual(rc, 1)
        self.assertIn("changed during the build", data["error"])

    def test_script_mode_refuses_populated_out_dir_before_building(self):
        # A real release dir (four archives + SHA256SUMS) must never be
        # overwritten or rebuilt over; the refusal comes before release.sh.
        release_sh_shaped(self.out, version="9.9.9")
        before = {n: ra.sha256_file(os.path.join(self.out, n))
                  for n in os.listdir(self.out)}
        rc, out, err, data = self._main()
        self.assertEqual(rc, 1)
        self.assertIn("not empty", data["error"])
        self.assertFalse(os.path.exists(os.path.join(self.out,
                                                     ".fake-release-tmpdir")),
                         "release.sh ran against a populated out-dir")
        after = {n: ra.sha256_file(os.path.join(self.out, n))
                 for n in os.listdir(self.out)}
        self.assertEqual(after, before)

    def test_script_mode_refuses_out_dir_with_only_a_manifest(self):
        with open(os.path.join(self.out, "SHA256SUMS"), "w") as f:
            f.write("keep me\n")
        rc, out, err, data = self._main()
        self.assertEqual(rc, 1)
        self.assertEqual(read_text(os.path.join(self.out, "SHA256SUMS")),
                         "keep me\n")
        self.assertFalse(os.path.exists(os.path.join(self.out,
                                                     ".fake-release-tmpdir")))

    def test_script_mode_creates_and_owns_new_out_dir(self):
        self.out = os.path.join(self.out, "new", "release")
        rc, out, err, data = self._main()
        self.assertEqual(rc, 0, err)
        self.assertEqual(data.get("out_dir_preflight", {}).get("created"), True)
        self.assertEqual(sorted(data["archives"]), sorted(ra.DEFAULT_TARGETS))


class MirrorOwnershipTests(ScratchCase):
    def setUp(self):
        self.out = self.mkdtemp("ra-mirror-")
        self.calls = []

    def _fake_build(self, repo_root, out_dir, version, goos, goarch, tmpdir,
                    deadline=None):
        self.calls.append(repo_root)
        return make_archive(out_dir, version=version, goos=goos, goarch=goarch,
                            binary=native_header(goos, goarch))

    def test_mirror_mode_refuses_populated_out_dir_before_building(self):
        paths = release_sh_shaped(self.out)
        mpath = os.path.join(self.out, "SHA256SUMS")
        manifest = read_text(mpath)
        digests = [ra.sha256_file(p) for p in paths]
        with self.faked(by_header_bi()), \
                mock.patch.object(ra, "build_target", self._fake_build):
            with self.assertRaises(ra.AcceptanceError) as cm:
                ra.run_acceptance(self.out, VERSION, CROSS_T, "mirror", REPO,
                                  log=quiet)
        self.assertEqual(self.calls, [], "mirror build ran in a populated dir")
        self.assertIn("not empty", str(cm.exception))
        self.assertEqual(read_text(mpath), manifest)
        self.assertEqual([ra.sha256_file(p) for p in paths], digests)

    def test_mirror_mode_builds_from_snapshot_into_empty_out_dir(self):
        with self.faked(by_header_bi()), \
                mock.patch.object(ra, "build_target", self._fake_build):
            report = ra.run_acceptance(self.out, VERSION, CROSS_T, "mirror",
                                       REPO, log=quiet)
        self.assertEqual(len(self.calls), 1)
        self.assertNotEqual(os.path.realpath(self.calls[0]),
                            os.path.realpath(REPO),
                            "mirror build compiled the live worktree")
        self.assertEqual(report["archives"][CROSS_T]["executed"], False)


class SourceStateTests(ScratchCase):
    @unittest.skipIf(shutil.which("git") is None, "git not on PATH")
    def test_out_dir_inside_repo_excluded_from_dirty_state(self):
        repo = self.mkdtemp("ra-git-")
        env = {"GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1",
               "HOME": repo}
        with mock.patch.dict(os.environ, env):
            def git(*args):
                subprocess.run(["git", "-C", repo, "-c", "user.name=t",
                                "-c", "user.email=t@example.invalid"] + list(args),
                               check=True, capture_output=True, timeout=30)
            git("init", "-q")
            with open(os.path.join(repo, "a.txt"), "w") as f:
                f.write("a\n")
            git("add", "a.txt")
            git("commit", "-q", "-m", "init")
            dist = os.path.join(repo, "dist")
            os.makedirs(dist)
            with open(os.path.join(dist, "x.tar.gz"), "w") as f:
                f.write("x")
            clean = ra.source_state(repo, exclude=[dist])
            dirty = ra.source_state(repo)
        self.assertFalse(clean["dirty"])
        self.assertTrue(dirty["dirty"])
        self.assertEqual(len(clean["revision"]), 40)


NOTES_COMMITTED = "committed notes\n"


@unittest.skipIf(shutil.which("git") is None, "git not on PATH")
class SnapshotProvenanceTests(ScratchCase):
    """Build modes compile an immutable snapshot of the committed tree, never
    the live worktree; uncommitted build inputs fail closed (real git)."""

    def setUp(self):
        self.repo = self.mkdtemp("ra-src-")
        self.fixture = self.mkdtemp("ra-fixture-")
        release_sh_shaped(self.fixture)
        patcher = mock.patch.dict(os.environ, {
            "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1",
            "HOME": self.repo, "FIXTURE_DIR": self.fixture})
        patcher.start()
        self.addCleanup(patcher.stop)
        files = {
            "go.mod": "module %s\n\ngo 1.26\n" % MODULE,
            "README.md": "# readme\n",
            "cmd/localrouter/main.go": "package main\n\nfunc main() {}\n",
            "internal/control/control.go": "package control\n",
            "internal/control/static/index.html": "<html></html>\n",
            "docs/notes.md": NOTES_COMMITTED,
            "scripts/release.sh": FAKE_RELEASE_SH,
        }
        for rel, body in files.items():
            self.write(rel, body)
        os.chmod(os.path.join(self.repo, "scripts", "release.sh"), 0o755)
        self.git("init", "-q")
        self.git("add", "-A")
        self.git("commit", "-q", "-m", "init")
        self.rev = self.git("rev-parse", "HEAD").strip()
        self.tree = self.git("rev-parse", "HEAD^{tree}").strip()
        self.out = os.path.join(self.mkdtemp("ra-out-"), "release")
        self.reports = self.mkdtemp("ra-reports-")

    def write(self, rel, body):
        p = os.path.join(self.repo, rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "w") as f:
            f.write(body)

    def git(self, *args):
        return subprocess.run(["git", "-C", self.repo, "-c", "user.name=t",
                               "-c", "user.email=t@example.invalid"]
                              + list(args), check=True, capture_output=True,
                              text=True, timeout=30).stdout

    def _main(self):
        report = os.path.join(self.reports, "report.json")
        argv = ["--out-dir", self.out, "--version", VERSION,
                "--build-mode", "script", "--repo-root", self.repo,
                "--report", report]
        err = io.StringIO()
        with self.faked(by_header_bi(revision=self.rev), real_git=True), \
                mock.patch.object(ra, "run_binary_check",
                                  lambda b, w, **k: {"help_exit": 0,
                                                     "check_exit": 0,
                                                     "check_stdout": "config OK",
                                                     "binary_sha256": k.get("expected_sha256")}), \
                contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(err):
            rc = ra.main(argv)
        with open(report) as f:
            return rc, err.getvalue(), json.load(f)

    def built_from(self):
        return read_text(os.path.join(self.out, ".fake-release-root")).strip()

    def test_build_compiles_committed_snapshot_not_live_worktree(self):
        # Unrelated dirt (harness files, docs) is allowed and recorded; it is
        # not a Go or packaging input, and the build never sees it.
        self.write("docs/notes.md", "LIVE EDIT, not committed\n")
        self.write("scripts/tool.py", "print('harness')\n")
        self.write("scripts/__pycache__/tool.cpython-311.pyc", "x")
        rc, err, data = self._main()
        self.assertEqual(rc, 0, err)
        root = self.built_from()
        self.assertTrue(root, "fake release.sh did not run")
        self.assertNotEqual(os.path.realpath(root), os.path.realpath(self.repo),
                            "release.sh compiled the live worktree")
        self.assertEqual(read_text(os.path.join(self.out, ".fake-release-notes")),
                         NOTES_COMMITTED)
        bs = data.get("build_source") or {}
        self.assertEqual(bs.get("revision"), self.rev)
        self.assertEqual(bs.get("tree"), self.tree)
        self.assertEqual(bs.get("dirty_build_inputs"), [])
        self.assertEqual(sorted(bs.get("excluded_dirty", [])),
                         ["docs/notes.md",
                          "scripts/__pycache__/tool.cpython-311.pyc",
                          "scripts/tool.py"])
        self.assertEqual(len(bs.get("content_sha256", "")), 64)
        self.assertEqual(bs.get("content_sha256_after_build"),
                         bs.get("content_sha256"))
        self.assertTrue(data["source"]["dirty"])
        self.assertFalse(os.path.exists(root), "snapshot was not cleaned up")

    def test_uncommitted_build_inputs_fail_closed_before_building(self):
        cases = {
            "untracked Go file": ("cmd/localrouter/feature.go",
                                  lambda p: self.write(p, "package main\n")),
            "untracked Go file elsewhere": ("tools/gen.go",
                                            lambda p: self.write(p, "package x\n")),
            "embedded asset": ("internal/control/static/index.html",
                               lambda p: self.write(p, "<html>new</html>\n")),
            "packaged README": ("README.md",
                                lambda p: self.write(p, "# changed\n")),
            "module file": ("go.mod",
                            lambda p: self.write(p, "module %s\n\ngo 1.26\n// x\n"
                                                 % MODULE)),
            "deleted Go file": ("internal/control/control.go",
                                lambda p: os.remove(os.path.join(self.repo, p))),
        }
        for label, (rel, mutate) in cases.items():
            with self.subTest(label):
                self.git("reset", "-q", "--hard")
                self.git("clean", "-qfdx")
                _force_rmtree(self.out)
                mutate(rel)
                rc, err, data = self._main()
                self.assertEqual(rc, 1, "%s: run passed with dirty %s"
                                 % (label, rel))
                self.assertIn(rel, data["error"] or "")
                self.assertEqual(self.built_from(), "",
                                 "%s: release.sh ran despite dirty input" % label)

    def test_snapshot_checkout_ignores_user_git_filters(self):
        # A user/global filter driver (git-lfs is one, and may fetch over the
        # network) must not run for the snapshot; blobs are checked out raw.
        marker = os.path.join(self.mkdtemp("ra-filter-"), "SMUDGE_RAN")
        cfg = os.path.join(self.mkdtemp("ra-gitcfg-"), "gitconfig")
        with open(cfg, "w") as f:
            f.write('[filter "probe"]\n\tsmudge = "touch \'%s\'; cat"\n'
                    '\tclean = cat\n' % marker)
        self.write(".gitattributes", "docs/notes.md filter=probe\n")
        self.git("add", ".gitattributes")
        self.git("commit", "-q", "-m", "attributes")
        self.rev = self.git("rev-parse", "HEAD").strip()
        with mock.patch.dict(os.environ, {"GIT_CONFIG_GLOBAL": cfg}):
            rc, err, data = self._main()
        self.assertFalse(os.path.exists(marker),
                         "a user git filter ran during the snapshot checkout")
        self.assertEqual(rc, 0, data.get("error"))

    def test_edit_to_already_dirty_file_during_build_detected(self):
        # Names-only status hashing cannot see this; the content-verified
        # snapshot does.
        self.write("docs/notes.md", "LIVE EDIT, not committed\n")
        os.environ["FAKE_RELEASE_TOUCH"] = "docs/notes.md"
        self.addCleanup(os.environ.pop, "FAKE_RELEASE_TOUCH", None)
        rc, err, data = self._main()
        self.assertEqual(rc, 1, "content edit during the build went undetected")
        self.assertIn("changed during the build", data["error"])


class MainCleanFailureTests(ScratchCase):
    def test_corrupt_archive_exits_1_without_traceback(self):
        out = self.mkdtemp("ra-main-")
        a = os.path.join(out, ra.archive_name(VERSION, "linux", "arm64"))
        with open(a, "wb") as f:
            f.write(b"\x1f\x8b garbage that is not gzip")
        write_manifest(out, [a])
        err = io.StringIO()
        with self.faked(fake_bi("linux", "arm64")), \
                contextlib.redirect_stderr(err), \
                contextlib.redirect_stdout(io.StringIO()):
            rc = ra.main(["--out-dir", out, "--version", VERSION,
                          "--arches", "arm64", "--build-mode", "none",
                          "--repo-root", REPO])
        self.assertEqual(rc, 1)
        self.assertIn("ACCEPTANCE FAILED", err.getvalue())
        self.assertNotIn("Traceback", err.getvalue())

    def test_relative_report_path_refused_and_not_written(self):
        out = self.mkdtemp("ra-main-")
        cwd = self.mkdtemp("ra-cwd-")
        err = io.StringIO()
        old = os.getcwd()
        os.chdir(cwd)
        self.addCleanup(os.chdir, old)
        with contextlib.redirect_stderr(err), \
                contextlib.redirect_stdout(io.StringIO()):
            rc = ra.main(["--out-dir", out, "--version", VERSION,
                          "--build-mode", "none", "--repo-root", REPO,
                          "--report", "rel/report.json"])
        self.assertEqual(rc, 1)
        self.assertEqual(os.listdir(cwd), [])

    def test_tmp_scratch_dir_refused(self):
        out = self.mkdtemp("ra-main-")
        err = io.StringIO()
        with contextlib.redirect_stderr(err), \
                contextlib.redirect_stdout(io.StringIO()):
            rc = ra.main(["--out-dir", out, "--version", VERSION,
                          "--build-mode", "none", "--repo-root", REPO,
                          "--scratch-dir", "/tmp/ra-scratch"])
        self.assertEqual(rc, 1)
        self.assertIn("/tmp", err.getvalue())


class DeadlineTests(unittest.TestCase):
    def test_expired_deadline_refuses_new_commands(self):
        d = ra.Deadline(0)
        with self.assertRaises(ra.AcceptanceError):
            ra.run_cmd(["true"], timeout=5, deadline=d)

    def test_timeout_kills_process_group(self):
        start = time.monotonic()
        with self.assertRaises(ra.AcceptanceError) as cm:
            ra.run_cmd(["sh", "-c", "sleep 30 & sleep 30; wait"], timeout=1)
        self.assertLess(time.monotonic() - start, 10)
        self.assertIn("timed out", str(cm.exception))

    def test_missing_executable_is_acceptance_error(self):
        with self.assertRaises(ra.AcceptanceError):
            ra.run_cmd(["/nonexistent/definitely-not-here"], timeout=5)

    def test_output_is_bounded(self):
        res = ra.run_cmd(["sh", "-c", "head -c 3000000 /dev/zero"], timeout=20,
                         output_cap=1024)
        self.assertEqual(res.returncode, 0)
        self.assertLessEqual(len(res.stdout), 1024 + 64)


# The grandchild ignores SIGTERM, then publishes its PID (the handshake: the
# file only appears once the trap is in place) and execs a uniquely-tagged
# sleep. The leader waits for the handshake before it exits or sleeps.
_GRANDCHILD = ('trap "" TERM; echo $$ > "$1/pid.tmp"; mv "$1/pid.tmp" "$1/pid"; '
               'exec sleep "$2"')
_LEADER = {
    # leader exits 0; the grandchild has detached its stdio from our pipes
    "detached_after_exit": ('sh -c \'%s\' gc "$1" "$2" </dev/null >/dev/null 2>&1 &\n'
                            'while [ ! -e "$1/pid" ]; do sleep 0.01; done\n'
                            'exit 0\n' % _GRANDCHILD),
    # leader exits 0; the grandchild still holds stdout/stderr
    "pipe_holder_after_exit": ('sh -c \'%s\' gc "$1" "$2" &\n'
                               'while [ ! -e "$1/pid" ]; do sleep 0.01; done\n'
                               'exit 0\n' % _GRANDCHILD),
    # leader is still running at the timeout and obeys SIGTERM
    "pipe_holder_timeout": ('sh -c \'%s\' gc "$1" "$2" &\n'
                            'while [ ! -e "$1/pid" ]; do sleep 0.01; done\n'
                            'touch "$1/ready"\n'
                            'exec sleep 300\n' % _GRANDCHILD),
}


def _proc_state(pid):
    """Linux /proc state letter and cmdline for pid, or (None, '')."""
    try:
        with open("/proc/%d/stat" % pid, "rb") as f:
            stat_line = f.read()
        with open("/proc/%d/cmdline" % pid, "rb") as f:
            cmdline = f.read().replace(b"\0", b" ").decode("utf-8", "replace")
    except OSError:
        return None, ""
    return stat_line[stat_line.rfind(b")") + 2:].split()[0].decode(), cmdline


@unittest.skipUnless(os.path.isdir("/proc/self"), "needs Linux /proc")
class ProcessGroupCleanupTests(ScratchCase):
    """Real subprocesses: nothing in the command's process group survives
    run_cmd, including SIGTERM-ignoring grandchildren after the leader has
    exited or detached its stdio; nothing outside the group is signalled."""

    def setUp(self):
        self.tmp = self.mkdtemp("ra-pg-")
        # unique sleep argument, so liveness checks cannot match another process
        self.tag = "%d.%06d" % (400 + os.getpid() % 400, int(time.time() * 1e6) % 10**6)

    def _alive(self, pid, tag):
        state, cmdline = _proc_state(pid)
        return state is not None and state != "Z" and tag in cmdline

    def _reap_later(self, pidfile, tag):
        def reap():
            pid = int(read_text(pidfile).strip() or 0)
            if pid and self._alive(pid, tag):
                os.kill(pid, 9)
        self.addCleanup(reap)

    def _grandchild(self):
        pid = int(read_text(os.path.join(self.tmp, "pid")).strip())
        state, cmdline = _proc_state(pid)
        return pid, state, cmdline

    def _run(self, case, timeout):
        self._reap_later(os.path.join(self.tmp, "pid"), self.tag)
        return ra.run_cmd(["sh", "-c", _LEADER[case], "leader", self.tmp,
                           self.tag], timeout=timeout)

    def assertGone(self, case):
        self.assertTrue(os.path.exists(os.path.join(self.tmp, "pid")),
                        "%s: handshake never completed" % case)
        pid, state, cmdline = self._grandchild()
        self.assertFalse(state not in (None, "Z") and self.tag in cmdline,
                         "%s: SIGTERM-ignoring grandchild %d survived run_cmd "
                         "(state %s, %r)" % (case, pid, state, cmdline))

    def test_detached_grandchild_killed_after_leader_exits(self):
        res = self._run("detached_after_exit", timeout=20)
        self.assertEqual(res.returncode, 0)
        self.assertGone("detached_after_exit")

    def test_pipe_holding_grandchild_killed_after_leader_exits(self):
        start = time.monotonic()
        res = self._run("pipe_holder_after_exit", timeout=20)
        self.assertEqual(res.returncode, 0)
        self.assertGone("pipe_holder_after_exit")
        self.assertLess(time.monotonic() - start, 15)

    def test_timeout_kills_term_ignoring_grandchild(self):
        start = time.monotonic()
        with self.assertRaises(ra.AcceptanceError) as cm:
            self._run("pipe_holder_timeout", timeout=4)
        self.assertIn("timed out", str(cm.exception))
        self.assertTrue(os.path.exists(os.path.join(self.tmp, "ready")),
                        "leader never reached the handshake before the timeout")
        self.assertGone("pipe_holder_timeout")
        self.assertLess(time.monotonic() - start, 20)

    def test_group_signals_only_while_leader_is_unreaped(self):
        # While the leader is an unreaped zombie its PID, and so the process
        # group ID, cannot be reused by an unrelated group.
        calls = []
        real_killpg = os.killpg

        def spy(pgid, sig):
            calls.append((pgid, sig, os.path.exists("/proc/%d" % pgid)))
            return real_killpg(pgid, sig)

        with mock.patch.object(ra.os, "killpg", spy):
            self._run("pipe_holder_after_exit", timeout=20)
        self.assertTrue(calls, "the leftover group was never signalled")
        self.assertEqual([c for c in calls if not c[2]], [],
                         "process group signalled after its leader was reaped")
        self.assertIn(9, [c[1] for c in calls])

    def test_unrelated_process_group_untouched(self):
        bystander = subprocess.Popen(["sleep", self.tag + "1"],
                                     start_new_session=True,
                                     stdout=subprocess.DEVNULL,
                                     stderr=subprocess.DEVNULL)
        self.addCleanup(bystander.wait)
        self.addCleanup(bystander.kill)
        self._run("detached_after_exit", timeout=20)
        self.assertIsNone(bystander.poll(), "an unrelated process group was killed")


class SymlinkSafeCleanupTests(ScratchCase):
    def test_rmtree_never_chmods_through_symlinks(self):
        outside = self.mkdtemp("ra-outside-")
        target_file = os.path.join(outside, "keep.txt")
        with open(target_file, "w") as f:
            f.write("external\n")
        os.chmod(target_file, 0o444)
        target_dir = os.path.join(outside, "dir")
        os.mkdir(target_dir, 0o555)
        self.addCleanup(os.chmod, target_dir, 0o700)
        modes = {p: stat.S_IMODE(os.stat(p).st_mode)
                 for p in (target_file, target_dir)}

        scratch = self.mkdtemp("ra-rm-")
        tree = os.path.join(scratch, "run")
        for name, target in (("f", target_file), ("d", target_dir)):
            ro = os.path.join(tree, name)
            os.makedirs(ro)
            os.symlink(target, os.path.join(ro, "link"))
            os.chmod(ro, 0o555)   # read-only: unlinking the link fails first
        ra._rmtree(tree)
        self.assertFalse(os.path.lexists(tree), "scratch tree not removed")
        self.assertEqual({p: stat.S_IMODE(os.stat(p).st_mode) for p in modes},
                         modes, "cleanup changed the mode of a symlink target")


# --------------------------------------------------------------------------
# Real Go toolchain variants (no stubbed buildinfo). Offline; opt-in.
# --------------------------------------------------------------------------
GO = shutil.which("go")
# NB: %s placeholder filled with a JSON string literal (valid Go syntax); the
# audit repro used Go's %q inside a Python %-format, which Python rejects.
GO_SRC = r'''package main

import "os"

func main() {
	f, err := os.OpenFile(SENTINEL_PATH, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		line := "EXECUTED"
		for _, a := range os.Args[1:] {
			line += " " + a
		}
		f.WriteString(line + "\n")
		f.Close()
	}
	if len(os.Args) > 1 && os.Args[1] == "check" {
		os.Stdout.WriteString("config OK: sentinel\n")
	}
}
'''


def go_build(scratch, src_dir, out, goos, goarch):
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"),
           "HOME": scratch, "GOCACHE": os.path.join(scratch, "gocache"),
           "GOPATH": os.path.join(scratch, "gopath"),
           "GOMODCACHE": os.path.join(scratch, "gomod"),
           "GOTMPDIR": scratch, "TMPDIR": scratch,
           "GOPROXY": "off", "GOTOOLCHAIN": "local", "GOWORK": "off",
           "GOFLAGS": "-p=2", "GOMAXPROCS": "2", "CGO_ENABLED": "0",
           "GOOS": goos, "GOARCH": goarch, "GO111MODULE": "on"}
    proc = subprocess.run([GO, "build", "-trimpath", "-o", out, "main.go"],
                          cwd=src_dir, env=env, capture_output=True, text=True,
                          timeout=300)
    if proc.returncode != 0:
        raise RuntimeError("go build %s/%s failed: %s" % (goos, goarch, proc.stderr))
    return out


@unittest.skipUnless(GO and os.environ.get("RA_REAL_GO") == "1",
                     "set RA_REAL_GO=1 (and have go on PATH) for real-Go tests")
class RealGoTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.host = ra.detect_host_target()
        cls.work = tempfile.mkdtemp(prefix="realgo-", dir=TEST_TMP)
        cls.sentinel = os.path.join(cls.work, "SENTINEL_EXECUTED_GO")
        src = os.path.join(cls.work, "src")
        os.makedirs(src)
        with open(os.path.join(src, "main.go"), "w") as f:
            f.write(GO_SRC.replace("SENTINEL_PATH", json.dumps(cls.sentinel)))
        cls.host_bin = go_build(cls.work, src, os.path.join(cls.work, "host.bin"),
                                *cls.host)
        cls.darwin_bin = go_build(cls.work, src,
                                  os.path.join(cls.work, "darwin_arm64.bin"),
                                  "darwin", "arm64")

    @classmethod
    def tearDownClass(cls):
        subprocess.run(["chmod", "-R", "u+w", cls.work])
        shutil.rmtree(cls.work, ignore_errors=True)

    def setUp(self):
        if os.path.exists(self.sentinel):
            os.remove(self.sentinel)

    def _read(self, p):
        with open(p, "rb") as f:
            return f.read()

    def _out(self, prefix):
        d = tempfile.mkdtemp(prefix=prefix, dir=self.work)
        return d

    def _std(self, out, goos, goarch, data):
        return std_archive(out, goos, goarch, None,
                           bin_entry={"data": data, "mode": 0o755})

    def test_real_go_tampered_host_archive_not_executed(self):
        out = self._out("f1-")
        a = self._std(out, *self.host, self._read(self.host_bin))
        write_raw_manifest(out, [("0" * 64, os.path.basename(a))])
        with self.assertRaises(ra.AcceptanceError) as cm:
            ra.run_acceptance(out, VERSION, "host", "none", REPO, log=quiet)
        self.assertIn("checksum mismatch", str(cm.exception))
        self.assertFalse(os.path.exists(self.sentinel))

    def test_real_go_foreign_binary_with_valid_checksum_not_executed(self):
        # Correct GOOS/GOARCH and header, valid SHA256SUMS, but it is not the
        # LocalRouter main package: identity must stop it before execution.
        out = self._out("f1id-")
        a = self._std(out, *self.host, self._read(self.host_bin))
        write_manifest(out, [a])
        with self.assertRaises(ra.AcceptanceError) as cm:
            ra.run_acceptance(out, VERSION, "host", "none", REPO, log=quiet)
        self.assertIn("command-line-arguments", str(cm.exception))
        self.assertFalse(os.path.exists(self.sentinel))

    def test_real_go_darwin_binary_in_linux_arm64_archive_rejected(self):
        out = self._out("f2-")
        a = self._std(out, "linux", "arm64", self._read(self.darwin_bin))
        with self.assertRaises(ra.AcceptanceError) as cm:
            ra.verify_arch_proof(a, VERSION, "linux", "arm64")
        self.assertRegex(str(cm.exception), "ELF|GOOS")

    def test_real_go_mirror_build_compiles_clean_committed_snapshot(self):
        # Live repo has an unrelated untracked harness file. A live-tree build
        # would be stamped vcs.modified=true for content it never compiled;
        # the snapshot build is stamped with the exact committed revision and
        # vcs.modified=false, and its tree/content digest is recorded.
        repo = self._out("snaprepo-")
        with open(os.path.join(repo, "go.mod"), "w") as f:
            f.write("module %s\n\ngo 1.26\n" % MODULE)
        with open(os.path.join(repo, "README.md"), "w") as f:
            f.write("# sentinel\n")
        os.makedirs(os.path.join(repo, "cmd", "localrouter"))
        with open(os.path.join(repo, "cmd", "localrouter", "main.go"), "w") as f:
            f.write(GO_SRC.replace("SENTINEL_PATH", json.dumps(self.sentinel)))
        env = {"GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1",
               "HOME": self.work, "GOCACHE": os.path.join(self.work, "gocache"),
               "GOPATH": os.path.join(self.work, "gopath"),
               "GOMODCACHE": os.path.join(self.work, "gomod"),
               "GOWORK": "off", "GO111MODULE": "on"}
        with mock.patch.dict(os.environ, env):
            def git(*args):
                return subprocess.run(["git", "-C", repo, "-c", "user.name=t",
                                       "-c", "user.email=t@example.invalid"]
                                      + list(args), check=True, text=True,
                                      capture_output=True, timeout=30).stdout
            git("init", "-q")
            git("add", "-A")
            git("commit", "-q", "-m", "sentinel")
            rev = git("rev-parse", "HEAD").strip()
            with open(os.path.join(repo, "harness.py"), "w") as f:
                f.write("print('untracked harness file')\n")
            out = os.path.join(self._out("snapout-"), "release")
            report = ra.run_acceptance(out, VERSION, "host", "mirror", repo,
                                       log=quiet)
        entry = report["archives"]["%s/%s" % self.host]
        self.assertEqual(entry["identity"]["vcs.revision"], rev)
        self.assertEqual(entry["identity"]["vcs.modified"], "false")
        self.assertEqual(entry["identity"]["revision_source"], "vcs_stamp")
        self.assertIs(entry["executed"], True)
        bs = report.get("build_source") or {}
        self.assertEqual(bs.get("excluded_dirty"), ["harness.py"])
        self.assertEqual(bs.get("content_sha256_after_build"),
                         bs.get("content_sha256"))
        self.assertTrue(report["scratch_removed"])

    def test_real_go_buildinfo_parsed(self):
        bi = ra.go_buildinfo(self.darwin_bin)
        self.assertEqual((bi["goos"], bi["goarch"]), ("darwin", "arm64"))
        self.assertEqual(bi["path"], "command-line-arguments")
        self.assertEqual(bi["settings"]["CGO_ENABLED"], "0")
        self.assertEqual(bi["settings"]["-trimpath"], "true")
        hdr = ra.binary_header(self.darwin_bin)
        self.assertEqual((hdr["goos"], hdr["goarch"]), ("darwin", "arm64"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
