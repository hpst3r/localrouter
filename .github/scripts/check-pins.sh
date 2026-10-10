#!/usr/bin/env bash
# Fail if any supply-chain reference is mutable: GitHub Actions must be pinned
# to a full commit SHA, and container base/helper images to an @sha256 digest.
# Run from the repository root (CI runs it on every push and pull request).
set -euo pipefail
bad=0

# Every action reference: 40-char commit SHA plus a "# vX.Y.Z" comment that
# Dependabot keeps in step with the SHA. Local ./ actions are exempt.
if grep -nE '^\s*-?\s*uses:' .github/workflows/*.yml |
  grep -vE 'uses:\s*\./' |
  grep -vE '@[0-9a-f]{40}\s+#\s*v[0-9]'; then
  echo "action reference(s) above are not pinned to a commit SHA with a # vX.Y.Z comment" >&2
  bad=1
fi

# Base images and Quadlet images carry an @sha256 digest. The project's own
# image is exempt: the unit ships the literal VERSION placeholder.
if grep -nE '^FROM\s' Containerfile | grep -vE '@sha256:[0-9a-f]{64}'; then
  echo "Containerfile FROM line(s) above lack an @sha256 digest" >&2
  bad=1
fi
if grep -nE '^Image=' packaging/quadlet/*.container |
  grep -v '/localrouter:VERSION$' |
  grep -vE '@sha256:[0-9a-f]{64}'; then
  echo "Quadlet Image= line(s) above lack an @sha256 digest" >&2
  bad=1
fi

# Helper images pulled by setup-qemu-action and setup-buildx-action.
if ! grep -qE 'image:\s*\S*tonistiigi/binfmt\S*@sha256:[0-9a-f]{64}' .github/workflows/release.yml ||
  ! grep -qE 'image=\S*moby/buildkit\S*@sha256:[0-9a-f]{64}' .github/workflows/release.yml; then
  echo "release.yml must pin the binfmt and buildkit images by digest" >&2
  bad=1
fi

exit "$bad"
