// Package ledger implements core.Ledger on SQLite (modernc.org/sqlite) and
// owns the pricing table used to estimate request cost.
//
// Public API wired by cmd/localrouter:
//
//	pricing, err := ledger.LoadPricing(cfg.PricingFile)          // missing file => empty
//	l, err := ledger.Open(filepath.Join(cfg.DataDir, "localrouter.db"), pricing,
//	        func(accountID string) string { return basisByAccount[accountID] })
//	defer l.Close()
//	l.Record(ctx, rec)                                           // computes cost_usd/cost_basis; no-op on existing rec.ID
//	l.RecordBatch(ctx, recs)                                     // core.BatchLedger: one tx, <= MaxBatch, idempotent
//	rows, err := l.Summary(ctx, since, "account")                // account|model|class|client|route|host|task|agent|day
//	res, err := l.Analytics(ctx, core.AnalyticsQuery{...})       // core.AnalyticsLedger; validation errors start "analytics: "
//	n, err := l.RelabelHost(ctx, "", cfg.HostName)               // `ledger relabel-host`; validation errors start "relabel: "
//
// Multi-user mode (identity enabled). Records carry their owner
// (RequestRecord.UserID/KeyID → nullable user_id/key_id; "" stores NULL):
//
//	l.RequireScope()                                             // once, before serving: unscoped Summary/Analytics now fail (core.ErrInvalidScope)
//	rows, err := l.SummaryScoped(ctx, since, "model", core.DataScope{UserID: uid}) // core.ScopedLedger; also groups user|key
//	q.Scope = &core.DataScope{AllUsers: true}                    // explicit global read (admin); unowned rows included
//	res, err := l.Analytics(ctx, q)                              // scoped q may group/filter by AnalyticsOwnerDimensions
//
// A UserID scope sees only rows with that user_id (never unowned/NULL rows);
// the predicate is applied to every aggregate before ranking, top-N,
// __other__ and totals. An invalid scope wraps core.ErrInvalidScope.
//
// Pricing import (`localrouter pricing import <litellm json>`):
//
//	m, err := ledger.ImportLiteLLM(f)
//	err = ledger.WritePricing(cfg.PricingFile, m)
//
// The requests table never stores prompt or response content. No price
// numbers ship with this package; unknown prices yield a NULL cost.
package ledger
