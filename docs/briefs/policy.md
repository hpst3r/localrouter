Package: internal/policy

Implement core.Policy exactly per SPEC "Admission / policy".

- `New(accounts []core.Account, quota core.QuotaSource, opts Options) *Policy`
  Options{StaleAfter, SafetyMargin, InflightEstimate, Clock core.Clock, Logger}.
- One mutex guards decision+lease creation (atomic Acquire). Leases are
  release-once (second Release is a no-op).
- Admission math (per window of latest snapshot): effective used = 0 if ResetAt
  non-zero and now >= ResetAt. floor = reserve[kind] for background else 0.
  margin = SafetyMargin for background else 0. Admit iff
  used + inflight*InflightEstimate + margin <= 1 - floor (use small epsilon 1e-9).
  If Snapshot.Allowed is false and no window has rolled past ResetAt -> deny all.
- Staleness per SPEC (absent or older than StaleAfter): reserved account -> deny
  background, allow interactive; unreserved -> allow both. openai_compat
  accounts with no snapshot and no reserve -> allow.
- Cooldown on Release: status 429 -> cooldown until max(o.ResetAt, earliest
  ResetAt among windows with used>=0.999 in latest snapshot, now+60s);
  401/403 -> now+60s. Cooldown cleared early if a snapshot newer than the
  cooldown start shows all windows below 1.0 (and Allowed not false). After 429
  call quota.RequestRefresh(id, true); after any release call
  quota.RequestRefresh(id, false).
- Unknown account ids in candidates are skipped with a reason.
- Acquire returns Decision with the chosen account or Allow=false and a reason that
  names each candidate's rejection (e.g. "codex-primary: background reserve 5h
  (used 0.91 > 0.88)"). Reasons must not include secrets.
- Status(id) returns core.AccountState (inflight, cooldown, stale, admissible per
  class via the same math, reason).
- Tests (table-driven + concurrency with -race): acceptance tests 1, 2 (exactly 1
  of 3 concurrent admits — use a barrier/goroutines and assert count), 3 (selection
  picks secondary), 6 (reset boundary), staleness both cases, Allowed=false deny,
  cooldown on 429 + early clear, double release, lease inflight accounting.
