Task: ADVERSARIAL REVIEW of the multi-host feature (merged on master) + the Hermes plugin.
READ-ONLY for production code. Prove each suspected bug with a focused failing test committed as `*_review_test.go` (Go) kept skipped via `t.Skip("BUG: ...")`, or for the plugin `tests/test_review.py` with `@unittest.skip("BUG: ...")`. Report each finding in contract_issues as `[SEVERITY critical|high|medium|low] path:line — problem — evidence (test name) — minimal fix`.

Scope (read docs/SPEC.md 'Multi-host' + 'Hermes plugin' first):
- internal/control/ingest.go: authz (ingest flag, 401/403), validation completeness (negative/overflowing usage, ID length/charset, host regex, schema_version, body limit, JSON trailing garbage, unknown accounts, quota_source), partial-write guarantees, overwrite of Host/Client/Provider, snapshot future-skew & staleness counting, any way a client key WITHOUT ingest can write rows or snapshots, any way to poison another account.
- internal/app/hostguard.go: Host-header parsing (ports, IPv6 literals, trailing dots, case, empty Host, absolute-form request URIs), DNS-rebinding bypass.
- internal/ledger: migration v3 on an existing v2 DB with rows; RecordBatch dedupe + atomicity; group=host.
- internal/quota/ingest.go: concurrency with Latest/refresh (go test -race), deep copies, agent accounts truly never polled (incl. RequestRefresh/ObserveHeaders paths).
- internal/claudelog batch path: offsets never advance past unsent records under ANY failure ordering; partial chunk failure; restart semantics.
- internal/agent + cmd/localrouter/agent.go: token never logged/returned in errors; backoff; --once exit codes; config validation; keychain/file selection; server URL with userinfo; what happens when the server returns 400 for a batch (poison pill -> infinite retry? data loss?).
- policy deny reason readability: currently prints 'background reserve 5h (used 0.00 > -0.01)' for a 0.999 reserve — propose a clearer wording (this one is known; include a test).
- Plugin (~/projects/hermes-localrouter): hook never raises, fail_open semantics, timeouts, key never in messages/logs, malformed JSON, non-dict args, register() against the real PluginContext signatures in /home/wporter/.hermes/hermes-agent/hermes_cli/plugins.py (register_command signature!).
Run `go test -race -count=1 -p 4 ./...` (Go) and `python3 -m unittest discover -s tests` (plugin) to confirm only your skipped tests are new. Commit your review tests in BOTH repos on their current branches (localrouter worktree branch; for the plugin, commit in ~/projects/hermes-localrouter on main).
