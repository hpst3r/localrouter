# Worker rules (read first)

You are implementing ONE package of LocalRouter in an isolated git worktree.
Read `docs/SPEC.md` (authoritative) and `internal/core/core.go` (frozen
contract) before writing code.

Hard rules:
- Edit ONLY files under your assigned package directory, plus `go.mod`/`go.sum`
  if you add an allowed dependency. Do NOT modify `internal/core`,
  `internal/config`, `docs/`, or other packages. If the contract is
  insufficient, work around it inside your package and report it under
  `contract_issues` — do not change core.
- Import only stdlib, `internal/core`, and the allowed deps in SPEC.md.
- No network access in tests; use `httptest` and fakes of core interfaces.
- Bounded builds: always run with `GOFLAGS=-p=4` and `go test -p 4`.
- Required before finishing, in your package dir:
  `gofmt -l .` (empty), `GOFLAGS=-p=4 go vet ./internal/<pkg>/...`,
  `GOFLAGS=-p=4 go test -race -count=1 -p 4 ./internal/<pkg>/...`.
- Never log or return secrets (tokens, keys) or prompt/response bodies.
- Keep it small and readable. Exported API documented with doc comments.
  Provide a constructor and a short `doc.go` package comment describing the
  public API the architect will wire in `cmd/localrouter`.
- Commit your work on the current branch with a conventional commit message
  (`feat(<pkg>): ...`). One or a few commits; do not push; do not merge.

Final answer: ONLY the JSON object required by the schema — no prose.
Distinguish verified facts (commands you ran and their results) from
assumptions.

NOTE: If your brief explicitly names additional files/packages you may edit (e.g. a NEW cmd/localrouter/admit.go), those are allowed; everything else in this list still applies.
