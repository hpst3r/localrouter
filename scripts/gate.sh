#!/usr/bin/env bash
# Full verification gate for localrouter + hermes-localrouter.
set -uo pipefail
cd "$HOME/projects/localrouter"
fail=0
echo "--- gofmt"; out=$(gofmt -l .); [ -z "$out" ] || { echo "$out"; fail=1; }
echo "--- vet"; GOFLAGS=-p=4 go vet ./... || fail=1
echo "--- race x2"
systemd-run --user --scope -q -p MemoryMax=8G env GOFLAGS=-p=4 GOMAXPROCS=4 \
  go test -race -count=2 -p 4 ./... 2>&1 | grep -v "no test files" || true
GOFLAGS=-p=4 go test -count=1 -p 4 ./... >/dev/null 2>&1 || { echo "UNIT FAIL"; fail=1; }
echo "--- review mode"
LOCALROUTER_REVIEW=1 GOFLAGS=-p=4 go test -count=1 -p 4 -run Review ./... 2>&1 \
  | grep -v "no test files\|no tests to run"
LOCALROUTER_REVIEW=1 GOFLAGS=-p=4 go test -count=1 -p 4 -run Review ./... >/dev/null 2>&1 || { echo "REVIEW FAIL"; fail=1; }
echo "remaining reviewBug gates: $(grep -rn 'reviewBug(t, ' internal | wc -l)"
echo "--- plugin"
cd "$HOME/projects/hermes-localrouter"
python3 -m unittest discover -s tests 2>&1 | tail -1
LOCALROUTER_REVIEW=1 python3 -m unittest discover -s tests 2>&1 | tail -1
LOCALROUTER_REVIEW=1 python3 -m unittest discover -s tests >/dev/null 2>&1 || { echo "PLUGIN FAIL"; fail=1; }
echo "GATE fail=$fail"
exit $fail
