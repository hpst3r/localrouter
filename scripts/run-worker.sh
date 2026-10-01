#!/usr/bin/env bash
# Launch one Opus worker in its worktree. Usage: run-worker.sh <pkg>
set -euo pipefail
pkg="$1"
root="$HOME/projects/localrouter"
wt="$HOME/projects/localrouter-wt/$pkg"
out="$root/.runs/$pkg"
mkdir -p "$out"
schema="$(cat "$root/docs/worker-result.schema.json")"
prompt="$(cat "$wt/docs/WORKER.md")

Your assigned package brief:

$(cat "$wt/docs/briefs/$pkg.md")

Working directory is the worktree root ($wt). Start by reading docs/SPEC.md and internal/core/core.go."
cd "$wt"
export GOFLAGS=-p=4 GOMAXPROCS=4
exec nice -n 10 claude -p "$prompt" \
  --model opus \
  --permission-mode acceptEdits \
  --allowedTools "Read Edit Write Glob Grep Bash(go:*) Bash(env:*) Bash(gofmt:*) Bash(git add:*) Bash(git commit:*) Bash(git status:*) Bash(git diff:*) Bash(git log:*) Bash(ls:*) Bash(mkdir:*)" \
  --output-format json \
  --json-schema "$schema" \
  > "$out/result.json" 2> "$out/stderr.log"
