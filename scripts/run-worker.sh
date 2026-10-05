#!/usr/bin/env bash
# Launch one Opus worker in its worktree. Usage: run-worker.sh <pkg>
# WT_DIR overrides the worktree path (e.g. a separate repo for the plugin).
set -euo pipefail
pkg="$1"
root="$HOME/projects/localrouter"
wt="${WT_DIR:-$HOME/projects/localrouter-wt/$pkg}"
out="$root/.runs/$pkg"
mkdir -p "$out"

# Respect the interactive Claude reserve (exit 1 = deny; 2 = router unreachable -> proceed).
rc=0; localrouter admit --class background --account claude-max >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 1 ]; then echo "claude reserve reached; not launching $pkg" >&2; exit 3; fi

schema="$(cat "$root/docs/worker-result.schema.json")"
prompt="$(cat "$root/docs/WORKER.md")

Your assigned brief:

$(cat "$root/docs/briefs/$pkg.md")

Working directory is the worktree root ($wt). Your process tree is capped at 6 GB RAM; keep builds bounded (-p 4)."
cd "$wt"
export GOFLAGS=-p=4 GOMAXPROCS=4
exec systemd-run --user --scope -q -p MemoryMax=6G -p MemorySwapMax=0 \
  nice -n 10 claude -p "$prompt" \
  --model opus \
  --permission-mode acceptEdits \
  --allowedTools "Read Edit Write Glob Grep Bash(go:*) Bash(env:*) Bash(gofmt:*) Bash(python3:*) Bash(curl -s 127.0.0.1:*) Bash(curl -s http://127.0.0.1:*) Bash(git add:*) Bash(git commit:*) Bash(git status:*) Bash(git diff:*) Bash(git log:*) Bash(git show:*) Bash(ls:*) Bash(mkdir:*) Bash(git -C:*)" \
  --add-dir "$root" \
  --output-format json \
  --json-schema "$schema" \
  > "$out/result.json" 2> "$out/stderr.log"
