#!/usr/bin/env bash
# Native binary release builder for LocalRouter.
#
# Usage: scripts/release.sh <output-dir> [version]
#
# Cross-compiles static (CGO_ENABLED=0) native binaries for Linux and macOS
# on amd64 and arm64, packages each with README.md and LICENSE (only when the
# file actually exists in the tree), and writes a SHA256SUMS manifest covering
# every archive produced.
#
#   <output-dir>  required, must be an absolute path and must NOT be /tmp (or
#                 anywhere beneath it): release artifacts need durable storage.
#   [version]     optional; defaults to `git describe --tags --always --dirty`,
#                 falling back to "dev". Used only for archive naming -- no
#                 linker flags are injected, because the build has no wired
#                 version symbol.
#
# This script only builds and packages. It never tags, pushes, or publishes.
set -euo pipefail

readonly TARGETS=(
  linux/amd64
  linux/arm64
  darwin/amd64
  darwin/arm64
)

readonly PKG="./cmd/localrouter"
readonly BIN_NAME="localrouter"

usage() {
  printf 'usage: %s <output-dir> [version]\n' "${0##*/}" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  usage
  exit 2
fi

out_dir="$1"
[ -n "$out_dir" ] || die "output directory must not be empty"

case "$out_dir" in
  /*) : ;;
  *) die "output directory must be an absolute path (got: $out_dir)" ;;
esac

# Refuse /tmp and its subdirectories, plus macOS's private symlink form.
case "${out_dir%/}" in
  /tmp|/tmp/*|/private/tmp|/private/tmp/*)
    die "refusing to write release artifacts under /tmp; pass a durable output directory"
    ;;
esac

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd -- "$repo_root"

command -v go >/dev/null 2>&1 || die "go toolchain not found in PATH"

if [ "$#" -ge 2 ] && [ -n "$2" ]; then
  version="$2"
else
  version="$(git describe --tags --always --dirty 2>/dev/null || printf 'dev')"
fi
[ -n "$version" ] || die "could not determine a version"

# Normalize a leading "v" so archive names stay consistent either way.
version_clean="${version#v}"

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$@"; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$@"; }
else
  die "no SHA-256 tool found (need sha256sum or shasum)"
fi

mkdir -p "$out_dir"
out_dir="$(cd -- "$out_dir" && pwd)"

stage_dir="$(mktemp -d)"
cleanup() { rm -rf "$stage_dir"; }
trap cleanup EXIT

archives=()

printf 'release %s\n' "$version_clean"
printf 'output  %s\n' "$out_dir"

for target in "${TARGETS[@]}"; do
  goos="${target%%/*}"
  goarch="${target##*/}"
  name="${BIN_NAME}_${version_clean}_${goos}_${goarch}"
  pkg_dir="$stage_dir/$name"

  mkdir -p "$pkg_dir"

  printf 'building %s/%s ...\n' "$goos" "$goarch"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOMAXPROCS=2 GOFLAGS=-p=2 \
    go build -trimpath -o "$pkg_dir/$BIN_NAME" "$PKG"

  [ -f "$pkg_dir/$BIN_NAME" ] || die "build did not produce $pkg_dir/$BIN_NAME"

  cp README.md "$pkg_dir/README.md"
  if [ -f LICENSE ]; then
    cp LICENSE "$pkg_dir/LICENSE"
  fi

  archive="$out_dir/$name.tar.gz"
  rm -f "$archive"
  tar -C "$stage_dir" -czf "$archive" "$name"
  archives+=("$archive")
done

# SHA256SUMS over the archives, stored with paths relative to <output-dir>.
checksum_file="$out_dir/SHA256SUMS"
rm -f "$checksum_file"
(
  cd -- "$out_dir"
  for archive in "${archives[@]}"; do
    sha256 "$(basename -- "$archive")"
  done
) > "$checksum_file"

printf 'wrote %d archives and %s\n' "${#archives[@]}" "$checksum_file"
