Packages: internal/ledger and internal/control, plus a NEW file
cmd/localrouter/admit.go (you may NOT edit cmd/localrouter/main.go; the architect will
add the `case "admit":` dispatch line calling `cmdAdmit(os.Args[2:]) error`).

Implement "Ledger changes" and "Admit CLI + model-less admit" from docs/SPEC.md.

Ledger:
- Migration v2 adding cache_creation_input_tokens (existing DBs must upgrade in place —
  test by creating a v1 DB with the old schema SQL then Open()).
- Record idempotent on ID (ON CONFLICT DO NOTHING), nil error on duplicate; empty ID
  still gets a random ID.
- core.Usage now has CacheCreationInputTokens; core.UsageRow has
  CacheCreationInputTokens — populate both.
- Cost formula per SPEC with cache_creation_input price (default = input price; clamp
  so the uncached remainder is never negative). ImportLiteLLM maps
  cache_creation_input_token_cost. Update existing tests that assert column lists.

Control:
- admit accepts exactly one of model/account (400 otherwise); account must exist
  (404); dry-run that one account via Policy.DryRun(class, []string{account}).
- status: windows with kinds other than 5h/weekly must be emitted too (verify).
- Widget (internal/control/static/index.html): render any extra window kinds
  (e.g. weekly_fable) as additional bars; label provider "claude" accounts with a
  "quota only" badge; usage table adds a "cache write" column. Keep escaping rules.

cmd/localrouter/admit.go: func cmdAdmit(args []string) error per SPEC; flags --class
(default background), --account, --model, --url (default http://127.0.0.1:8787),
--json, --timeout (default 5s). Exit codes: return a typed error the architect's main
maps — define `type exitError struct{code int; msg string}` with Error() and a method
Code() int, and have cmdAdmit return exitError{1,...} on deny and exitError{2,...} on
errors. Print reason to stdout on deny/allow (one line), JSON when --json.
Test cmdAdmit against an httptest server (allow, deny, 404, unreachable).
