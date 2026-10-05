Packages: internal/ledger ONLY (+ tests). Read docs/SPEC.md section 'Analytics (time series + drill-down)' FIRST (authoritative). core (AnalyticsQuery/Result/Series/Ledger, AnalyticsDimensions, AnalyticsOtherKey) and config (host_name) are frozen and already contain the new types.
Implement (*Ledger).Analytics per SPEC exactly, plus `func (l *Ledger) RelabelHost(ctx, from, to string) (int64, error)` (single tx, returns rows changed; `to` must match [A-Za-z0-9._-]{1,64}).
Add `var _ core.AnalyticsLedger = (*Ledger)(nil)`.
Add migration v4 if you add an index (keep existing migrations untouched).
Tests (use t.Setenv("TZ", ...)? No — Go caches time.Local; instead make the local-day bucketing take a *time.Location internally and test with time.LoadLocation("America/New_York") across the 2026-03-08 and 2026-11-01 DST transitions; production passes time.Local):
- bucket alignment hour/day, DST 23h/25h days, BucketStarts coverage of [From,To)
- grouping by every dimension incl. empty key; filters incl. empty-value filter; multiple filters AND
- TopN + __other__ sums equal Totals; Breakdown complete and ranked; ties by key
- cost nil vs priced per point (same rule as Summary)
- validation errors (bad group/filter key/bucket, From>=To, >400d, hourly >31d, TopN>50)
- performance: insert 200k rows via RecordBatch across 90 days, Analytics(day, group=model) < 1s (skip under -short); report the measured time in your result
- RelabelHost.

Error convention (agreed with the API worker): every validation error from Analytics/RelabelHost must have a message starting with "analytics: " (RelabelHost: "relabel: "); internal/SQL errors must NOT use that prefix.
