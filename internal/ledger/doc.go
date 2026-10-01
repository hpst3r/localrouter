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
//	rows, err := l.Summary(ctx, since, "account")                // account|model|class|client|route|day
//
// Pricing import (`localrouter pricing import <litellm json>`):
//
//	m, err := ledger.ImportLiteLLM(f)
//	err = ledger.WritePricing(cfg.PricingFile, m)
//
// The requests table never stores prompt or response content. No price
// numbers ship with this package; unknown prices yield a NULL cost.
package ledger
