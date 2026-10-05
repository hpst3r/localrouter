FIX-WORKER variant (see WORKER.md). Review tests are gated by `reviewBug(t, ...)` which skips unless LOCALROUTER_REVIEW=1. Done = remove the reviewBug(...) line from each test you fix (do NOT weaken assertions), and both `go test -race -count=1 -p 4 ./...` AND `LOCALROUTER_REVIEW=1 go test -count=1 -p 4 -run Review ./<your pkgs>/...` pass. Add regression notes in code comments only where non-obvious.

Scope: internal/app/hostguard.go (+tests) only. Also make sure IPv6 literals with zones/brackets still work and empty Host is rejected.

Findings:
[SEVERITY low] internal/app/hostguard.go:50 — hostPart does not strip a trailing dot, so 'router.tail.', 'router.tail.:8787' and 'localhost.:8787' get 403 even when 'Router.Tail' is in allowed_hosts. This fails closed and is not a rebinding bypass, only a usability issue. — evidence: app TestReviewHostGuardTrailingDot — minimal fix: apply strings.TrimSuffix(host, ".") to both the allowed entries and the request host.