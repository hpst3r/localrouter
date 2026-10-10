# Release acceptance (native artifacts)

This document describes the **offline native release-artifact acceptance
harness**, `scripts/release-acceptance.py`, and what it does and does not prove.
It covers *native binaries only*.

## Scope and ownership

In scope (this harness and its docs):

- `scripts/release-acceptance.py` — the harness.
- `scripts/tests/test_release_acceptance.py` — deterministic offline unit tests.
- `docs/RELEASE-ACCEPTANCE.md` — this document.

Explicitly **out of scope / not touched** by this slice:

- CI workflows (`.github/`).
- `Containerfile` and container images — covered separately by
  `scripts/container-acceptance.py` / `docs/CONTAINER-ACCEPTANCE.md`.
- Deployment units (`deploy/`, `packaging/quadlet`, `packaging/nginx`).
- Any service configuration, provider keys, or network access.
- Any `git` write to the repository (no tag, commit, push, index or ref
  update). Against the repository the harness only runs read-only
  `git rev-parse`, `git status`, `git describe`, and `git ls-tree`. Build
  modes also run `git clone --shared` from it into the per-run scratch dir;
  that reads the repository's objects and writes only the new clone.

## The release contract it validates

The harness validates the artifacts produced by the existing
`scripts/release.sh` contract. It does **not** redefine it; if the two ever
disagree, `scripts/release.sh` is authoritative.

| Aspect | Contract |
|---|---|
| Targets | `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` |
| Archive format | `.tar.gz` (there is no `windows` target and no `.zip`) |
| Archive name | `localrouter_<version>_<os>_<arch>.tar.gz` (leading `v` normalized off) |
| Member dir | `localrouter_<version>_<os>_<arch>/` |
| Required members | `localrouter` (binary), `README.md` |
| Optional member | `LICENSE` (only when present in the source tree) |
| Manifest | `SHA256SUMS`, paths relative to the output directory, one line per target |
| Build env | `CGO_ENABLED=0 GOOS/GOARCH GOMAXPROCS=2 GOFLAGS=-p=2`, `go build -trimpath ./cmd/localrouter` |

## Usage

```sh
# Release acceptance: drive scripts/release.sh (all four targets), then
# verify everything it wrote. Executes only the host target. <new-dir> must
# not exist yet, or be empty.
python3 scripts/release-acceptance.py \
  --out-dir <new-durable-dir> --build-mode script

# Verify existing release.sh output in place (never rewrites SHA256SUMS).
# A target subset may be deep-checked; every SHA256SUMS entry is still hashed.
python3 scripts/release-acceptance.py \
  --out-dir <dir> --version <ver> --arches host,arm64 --build-mode none

# Contract mirror smoke (default mode): the harness builds and packages the
# selected targets itself. NOT acceptance of release.sh output.
python3 scripts/release-acceptance.py \
  --out-dir <durable-dir> --arches host,arm64 --build-mode mirror
```

Other flags:

- `--max-runtime SECONDS` (default 1800) — global deadline; every subprocess
  timeout is clamped to what is left.
- `--scratch-dir DIR` — durable scratch parent (default
  `<out-dir>/.acceptance-scratch`). `/tmp`, `/private/tmp`, and `/dev/shm`
  are refused, including when reached through a symlink.
- `--keep-scratch` — keep the per-run scratch dir for debugging.
- `--report PATH` — report location (default
  `<out-dir>/acceptance-reports/release-acceptance-<run-id>.json`).

### Arch iteration (`--arches`)

`--arches` bounds which targets are deep-checked:

- `host,arm64` (default) — the host's own target plus `linux/arm64`.
- `all` — the full `scripts/release.sh` target set.
- `host` / `arm64` — a single target.
- explicit `os/arch` names (e.g. `linux/amd64,darwin/arm64`) — any subset of the
  contract; anything outside it is rejected before any work starts.

`--build-mode script` always checks all four targets, because that is what
`release.sh` produces.

### Build modes and what they prove

| `--build-mode` | Report `mode` | Meaning |
|---|---|---|
| `script` | `release_sh` | Runs `bash scripts/release.sh <out-dir> <version>`, then accepts its output. `SHA256SUMS` must list **exactly** the four contract targets. This is the release-acceptance mode. |
| `none` | `verify_only` | No build. Verifies existing artifacts and the **existing** `SHA256SUMS` in place. |
| `mirror` (default) | `contract_mirror_smoke` | Per-target Go build and Python `tarfile` packaging by the harness, which then writes `SHA256SUMS` itself. The checksum step cannot detect anything in this mode. This is a contract smoke test, **not** acceptance of `release.sh`. |

Both `script` and `mirror` run the build with `GOMAXPROCS=2 GOFLAGS=-p=2
GOPROXY=off GOTOOLCHAIN=local` and `TMPDIR` inside the per-run scratch dir. So
the build is offline: the Go module cache must already hold every dependency,
and the build fails cleanly otherwise.

### Output directory ownership (build modes)

`script` and `mirror` write archives and `SHA256SUMS` into `--out-dir`.
`release.sh` itself would `rm -f` and replace existing ones. So, before any
build or scratch dir is created, the harness requires `--out-dir` to be
either **missing**, in which case it creates and owns it, or an **existing
empty directory**. Anything else fails the run before the build, and nothing
in the directory is touched. That includes an existing release, a lone
`SHA256SUMS`, or a previous run's `acceptance-reports/`. The mirror build
also creates every archive and `SHA256SUMS` with exclusive create (`O_EXCL`),
so it never replaces a file. `--build-mode none` (verify-only) has no such
check: it never writes archives or manifests and keeps working on a populated
release directory. The report records `out_dir_preflight`
(`{"created": bool, "was_empty": true}`).

One exception remains: the report itself. By default it goes to
`<out-dir>/acceptance-reports/` under a per-run name, so a refused run still
adds a new report file there (never an overwrite). Pass `--report` to put it
elsewhere.

### Build source: committed-tree snapshot

Build modes never compile the live worktree. Before the build:

1. **Input policy (fail closed).** Every `git status --porcelain
   --untracked-files=all` entry is classified, with the harness's own
   out/scratch dirs excluded. An entry is a **build input** when its path (or
   a rename's original path) is:
   - any `*.go` file, anywhere;
   - `go.mod`, `go.sum`, `go.work`, or `go.work.sum`, or anything under
     `vendor/`;
   - a `release.sh` packaging input: `README.md`, `LICENSE`, or
     `scripts/release.sh`;
   - at or below any directory that holds a committed `.go` file.

   The last rule covers every file a package can compile, assemble or link
   (`.s`, `.syso`) or `//go:embed`, because embed patterns only reach the
   package directory and its subdirectories (e.g.
   `internal/control/static/index.html`). It over-approximates (tests,
   `testdata/`), but it never misses a compiled file. Any dirty build input
   (modified, staged, deleted, renamed, or untracked) **fails the run**,
   naming the paths. A committed-tree build would silently leave that work
   out. Other dirty paths can't affect `go build ./cmd/localrouter` or the
   archive contents. Examples are untracked harness scripts, docs outside
   package dirs, and `__pycache__/`. They are allowed, and they are listed in
   `build_source.excluded_dirty`.
2. **Snapshot.** `git clone --shared --no-checkout --template=` of the
   repository into the per-run scratch dir (durable storage, never `/tmp`).
   Then a detached checkout of the recorded `HEAD` commit, with hooks
   disabled. `--shared` reads the repository's objects and writes only the
   clone. Clone, checkout and the snapshot's own `git` queries run with
   `GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1`. So no user or system
   filter driver (git-lfs may fetch over the network), `autocrlf`, hook or
   template applies, and blobs are written raw. The build's own Go VCS
   stamping still runs with the normal environment.
3. **Content verification.** Every snapshot file is hashed as a git blob and
   compared against `git ls-tree -r` of that commit. The path set, blob ids,
   and exec/symlink modes must match exactly. Submodules and other non-file
   entries fail closed. So does any checkout filter or attribute that changes
   bytes. The clone's `HEAD` must be the recorded commit, and its
   `git status` must be clean. Snapshot directories are then made read-only.
4. **Build** runs `<snapshot>/scripts/release.sh` (which builds its own
   directory) or the mirror build, with the snapshot as the working directory.
   The clone has a real `.git` directory, so Go stamps
   `vcs.revision=<HEAD>` and `vcs.modified=false`.
5. **After the build**, the snapshot is hashed again. `content_sha256` before
   and after must be equal, and the clone must still be clean. The live
   worktree's `HEAD` and status hash must also be unchanged.

The report's `build_source` records `revision`, `tree`, `object_format`,
`files`, `content_sha256`, and `content_sha256_after_build` (sha256 over the
sorted `mode blob-id path` lines, so it pins exact contents),
`verified_against_tree`, and `excluded_dirty`. The snapshot is removed with
the rest of the per-run scratch (`--keep-scratch` keeps it). Anyone can
recreate it from the recorded commit, and `tree` and `content_sha256` let
them check it.

Limits of this policy:

- Ignored files are never enumerated and never part of the snapshot. An
  ignored `.go` file would be compiled by a plain `go build` in the worktree
  but not by this harness.
- `source.status_sha256` (live worktree) covers status lines (names and
  states), not contents. It is a before/after record only. Content guarantees
  come from the snapshot, which is the only tree that is compiled.
- The default `--version` comes from `git describe --dirty` in the live
  worktree. Tracked non-input edits (e.g. docs) therefore add `-dirty` to the
  archive *name*, although the build is the clean committed tree.

## Order of checks

Nothing from an archive is parsed, extracted, or executed until its bytes
have been matched against `SHA256SUMS`.

1. **Manifest integrity, first.** `SHA256SUMS` is read without following
   symlinks and is size-capped. Every line must be `<64 lowercase hex>  <name>`.
   Every name must be a plain contract archive name for this version (no
   paths, no `..`, no non-contract targets, no duplicates). **Every** listed
   archive is hashed (a regular file, never a symlink, size-capped) and must
   match. Every selected target must be listed. A genuine 4-entry `release.sh`
   manifest therefore works with any target subset, and a tampered
   *unselected* entry still fails the run. The selected archives are copied
   into a private snapshot **in the same pass that hashes them**. Every later
   step reads only the snapshot, never the out-dir file again.
2. **Archive shape** (on the snapshot), streamed, with no `extractall`:
   - exactly one member-dir entry, which must be a directory;
   - only `localrouter`, `README.md`, and `LICENSE`, each a regular file;
   - no symlinks, hardlinks, devices, FIFOs, or sparse members;
   - no duplicates, nesting, or traversal;
   - no setuid/setgid/sticky bits;
   - `localrouter` must carry the owner-exec bit **in the archive** (the
     harness never adds it);
   - size caps: binary 256 MiB, docs 4 MiB, archive 512 MiB, 16 members.

   Corrupt gzip/tar data is a clean failure, not a traceback. The binary is
   extracted once (exclusive create, then mode `0500`) and its SHA-256 is
   recorded.
3. **Architecture proof**, from two independent sources that must agree:
   - **Binary header.** Linux targets must be 64-bit little-endian ELF with a
     SYSV/Linux OSABI and `e_machine` `x86-64`/`AArch64`. Darwin targets must
     be 64-bit Mach-O `MH_EXECUTE` with cputype `x86_64` (`0x01000007`) or
     `arm64` (`0x0100000C`).
   - **Go build info** (`go version -m`, which reads the file and never runs
     it). It must report the target's `GOOS` **and** `GOARCH`. If build info
     is unavailable (for example, no `go` on `PATH`), the run fails rather
     than falling back to a header-only proof.
4. **LocalRouter identity.** Build info must report main path
   `<go.mod module>/cmd/localrouter`, module `<go.mod module>`,
   `CGO_ENABLED=0`, and `-trimpath=true`. Tying the binary to the source
   revision (`identity.revision_source` in the report):
   - `vcs_stamp`: build info carries `vcs.revision`, and it must equal the
     source tree's `HEAD`. `vcs.modified` is recorded. In `script` and
     `mirror` modes it must also be `false`, because those modes build a clean
     snapshot (see "Build source" above). That is the normal case for them.
   - `build_provenance`: build info has **no** `vcs.*` settings. Go omits
     them when it finds no `.git` *directory*. That happens in a **git
     worktree**, where `.git` is a file (`vcs.go` requires a directory; seen
     even with `-buildvcs=true` on go1.26.8). It also happens with a source
     tarball or with `-buildvcs=false`. This is accepted **only** in `script`
     and `mirror` modes, as a fallback, because the harness built the
     artifact itself in this run from the content-verified snapshot.
   - `verify_only` rejects unstamped binaries, because nothing ties
     pre-existing artifacts to the source. Artifacts that `release.sh` built
     directly in a git worktree are unstamped and so cannot pass. Artifacts
     from this harness's build modes, or from `release.sh` in a regular
     checkout, are stamped. Run verify-only from a checkout of the revision
     the artifacts were built from.
5. **Host execution.** Runs only for the host target, and only after every
   selected target has passed steps 1–4. It uses the verified snapshot
   extraction, which is re-hashed immediately before running. It runs
   `<binary> --help` (must exit 0) and `<binary> check -config <placeholder>`
   (must exit 0 and print `config OK`), with a minimal environment
   (`PATH`, `HOME`/`TMPDIR` = private workdir, `GOMAXPROCS=2`). The
   placeholder config is keyless and provider-free: one client with a local
   `0600` key file, no accounts, no `api_key`/`api_key_env`, no `base_url`,
   no routes. So nothing can reach a network provider.
6. **Post-run re-check.** `SHA256SUMS` and every listed out-dir archive are
   hashed again. Any change during the run fails acceptance.

## Cross-compiled targets: proof vs. execution

Only the host target is ever executed. Every other target, including
`linux/arm64` on an amd64 host, is proven by its binary header plus Go build
info and identity, and is **never executed**. No emulation (qemu or binfmt) is
used or detected. The report marks each such target `"executed": false` with
a `not_executed_reason`, and the PASS line lists them as
`NOT EXECUTED (header+buildinfo proof only)`.

## Report

Every run writes a JSON report, whether it passes, fails, or hits an internal
error (written atomically). Key fields:

```json
{
  "outcome": "pass | fail | error",
  "error": "message or null",
  "mode": "release_sh | verify_only | contract_mirror_smoke",
  "flags": {"argv": [], "build_mode": "", "arches": "", "version": "",
            "version_source": "flag | git describe | fallback",
            "max_runtime": 1800, "scratch_dir": null, "keep_scratch": false,
            "out_dir": "", "repo_root": "", "report": ""},
  "source": {"revision": "<HEAD>", "dirty": true, "status_entries": 0,
             "excluded_entries": 0, "status_sha256": "", "describe": "",
             "toplevel": "", "entries": [{"status": "??", "path": ""}],
             "entries_truncated": false},
  "source_after_build": {"...": "same shape; script/mirror only"},
  "out_dir_preflight": {"created": true, "was_empty": true},
  "build_source": {"policy": "committed_tree_snapshot", "revision": "",
                   "tree": "", "object_format": "sha1", "files": 0,
                   "content_sha256": "", "content_sha256_after_build": "",
                   "verified_against_tree": true, "dirty_build_inputs": [],
                   "excluded_dirty": [], "excluded_dirty_count": 0,
                   "root": "", "path": ""},
  "host_target": "linux/amd64",
  "build": {"command": [], "cwd": "", "exit": 0, "env": {}, "stdout": "",
            "stderr": ""},
  "manifest": {"sha256": "", "entries": {"<archive>": {"target": "",
               "sha256": "", "size": 0, "selected": true}},
               "rechecked_after_run": true},
  "archives": {"linux/amd64": {"archive_sha256": "", "members": [],
               "binary_sha256": "", "proof": {"header": {}, "buildinfo": {}},
               "identity": {"revision_source": "vcs_stamp | build_provenance",
                            "path": "", "mod": "", "vcs.revision": "",
                            "vcs.modified": ""},
               "executed": true,
               "host_run": {"help_exit": 0, "check_exit": 0,
                            "binary_sha256": ""}},
               "linux/arm64": {"executed": false,
                               "not_executed_reason": "..."}},
  "scratch_removed": true,
  "started_at": "", "finished_at": "", "elapsed_seconds": 0
}
```

`source.dirty` reflects `git status --porcelain --untracked-files=all` of the
live worktree. In verify-only mode a dirty tree is recorded and warned about,
but does not fail the run on its own. In build modes, dirty *build inputs*
fail the run, and other dirty paths are recorded (see "Build source").
`vcs.modified` describes the tree Go actually compiled, not the live
worktree. For this harness's build modes that tree is the clean snapshot, so
it is `false`. When Go builds directly in a git worktree, no `vcs.*` setting
is stamped at all, so `vcs.revision` and `vcs.modified` are `null` in the
report. That was the case for all four targets of the earlier live-worktree
`release.sh` run.

## Resource bounds

- Builds run with `GOMAXPROCS=2` and `GOFLAGS=-p=2`, sequentially (never all
  cores). No more than two build jobs are in flight (`MAX_JOBS = 2`).
- Every subprocess (`release.sh`, `go build`, `go version -m`, `git`, and host
  runs) has its own timeout, clamped to the global `--max-runtime` deadline.
  Each runs in its own process group (new session). Captured output is capped
  at 64 KiB per stream. Timeouts, missing executables, and `noexec` errors
  are reported as acceptance failures.
- Process-group cleanup runs after **every** command, both when the leader
  exits and on timeout. If any live member is left in the group, the whole
  group gets `SIGTERM` and up to 2 s to drain. Then the whole group gets
  `SIGKILL` unconditionally. So members that ignore `SIGTERM`, or that
  detached their stdio from the harness's pipes, die too. Only then is the
  leader reaped. Until that point, the unreaped leader keeps its PID, and so
  the group ID, reserved. That means `killpg` can never reach an unrelated
  group that reused the number. Live members are found via `/proc` on Linux
  (`ps` elsewhere). Limits: a process that moves itself to another session or
  group (`setsid`/`setpgid`) is out of reach. Without `os.waitid` (macOS
  before Python 3.13), the leader is reaped before the group is signalled,
  which reopens a tiny PID-reuse window.
- Output, report, and scratch paths must be **absolute and durable**. The
  per-run scratch dir (`run-*`) holds the source snapshot, the archive
  snapshot, extracted binaries, the host workdir, and `release.sh`'s
  `TMPDIR`. It is removed at the end unless `--keep-scratch` is given, and the
  report records `scratch_removed`. Cleanup only ever chmods real
  directories, opened with `O_NOFOLLOW`, to get write access back to the
  read-only snapshot dirs. It never chmods through a symlink, so files and
  dirs outside scratch keep their modes.

## Failure behaviour (no fabrication)

The harness never substitutes or invents results. Any failure prints
`ACCEPTANCE FAILED: …`, writes the report, and exits 1. Negative cases the
tests cover deterministically:

- missing archive, binary, or `README.md` member; wrong member directory;
- extra, nested, traversal, or duplicate members; symlink, hardlink, or
  directory in place of the binary (including dangling links); a member dir
  that is not a directory;
- non-executable or setuid binary mode; oversized members or archives;
  corrupt gzip;
- wrong architecture from the header (ELF `e_machine`, ELF class, Mach-O
  cputype); build info `GOOS` or `GOARCH` mismatch; missing build info; a
  darwin binary in a linux archive and vice versa;
- foreign main package or module, CGO build, untrimmed build, source
  revision mismatch, an unstamped binary in verify-only mode, a
  `vcs.modified=true` stamp in a build mode, or a source tree that changed
  during the build;
- build modes: a populated out-dir (a release, or just a `SHA256SUMS`) is
  refused before the build and left byte-identical. Uncommitted build inputs
  fail closed before the build: an untracked `.go` file (in a package or
  elsewhere), a modified embedded asset, `README.md`, `go.mod`, or a deleted
  `.go` file. The build compiles the committed snapshot, never the live
  worktree (real git). A content edit inside the compiled tree during the
  build is detected even when the live path was already dirty. A user
  (global) git filter driver is never run by the snapshot checkout;
- process groups (real subprocesses, PID handshake): `SIGTERM`-ignoring
  grandchildren die after the leader exits, whether they hold the pipes or
  detached their stdio. They also die on timeout. The group is signalled only
  while its leader is unreaped, and an unrelated process group survives;
- cleanup never changes the mode of a symlink target outside scratch;
- wrong or missing checksum; archive absent from the manifest; manifest
  listing an absent archive; unsafe, duplicate, or non-contract manifest
  names; a symlinked archive; a tampered unselected entry;
- a tampered host archive is **not executed** (sentinel test); an archive
  swapped after verification is not executed, and the run fails on the
  post-run re-check;
- `release.sh` output without exactly four entries; `release.sh` timeout
  (process group killed, report written);
- unknown or out-of-contract target names (e.g. `windows/amd64`);
- host binary whose `check` exits non-zero, which hangs, or which is not
  executable;
- scratch under `/tmp`.

## Tests

```sh
RA_TEST_TMPDIR=<durable exec-capable dir> \
  python3 -m unittest scripts/tests/test_release_acceptance.py

# Opt-in, slower: compile real stdlib-only Go sentinels offline (host and
# darwin/arm64) to check the same properties against real build info, and
# mirror-build a sentinel module from a real git repo through the snapshot
# (stamped with the committed revision, vcs.modified=false).
RA_REAL_GO=1 RA_TEST_TMPDIR=<dir> \
  python3 -m unittest scripts.tests.test_release_acceptance.RealGoTests
```

The default tests are fixture-based: synthetic `.tar.gz` archives, ELF and
Mach-O headers, `SHA256SUMS` files, a fake `release.sh`, small executable
stubs, and faked `go version -m` / git source state. They need no network, no
container runtime, and no Go toolchain. Every temp dir is created under
`$RA_TEST_TMPDIR` (or the normal `TMPDIR` rules) and removed afterwards. Tests
that would execute a fake binary use a non-host target, so they behave the
same on amd64 and arm64 hosts.

## Remaining gates (not covered here)

- darwin binaries are proven by header and build info only; they are never
  executed.
- arm64 is never executed (no emulation).
- No upgrade or rollback acceptance (old binary to new binary with an existing
  `data_dir`/ledger).
- No signing or provenance attestation for archives. `release.sh` output is
  not byte-reproducible: two runs at the same revision gave different archive
  digests (tar mtimes), and no reproducibility comparison is made.
- `release.sh` run directly in a git worktree produces unstamped binaries,
  which a later verify-only run cannot accept. The harness's build modes
  avoid this by building from a snapshot clone with a real `.git` directory.
- Build modes accept only committed source. Work in progress has to be
  committed first, even on a local branch. Ignored files are not checked
  (see "Build source").
- No CI wiring.
