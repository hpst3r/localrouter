package ledger

import (
	"context"

	"github.com/hpst3r/localrouter/internal/core"
)

// PricingView is a generation-scoped writer over an existing *Ledger. It pins
// an immutable *Pricing (and an optional basis function) captured at creation
// and uses it for cost attribution of every record written through the view,
// while reading and writing the SAME *sql.DB under the SAME write mutex as the
// ledger it embeds. A view never reopens, replaces or closes the shared
// database, and it never mutates the ledger's own pricing or basis.
//
// This is the seam a hot reload uses: each configuration generation captures a
// view, so records admitted in that generation are costed at that generation's
// prices even if the price table changes while they are in flight. Because the
// view embeds *Ledger, the read-side API (Ping, Summary, SummaryScoped,
// Analytics, RelabelHost, …) is promoted unchanged — including the ledger's
// RequireScope mode; only the writing methods are
// overridden to select the frozen price table.
//
// Ownership: a PricingView does NOT own the database. Its Close is a no-op —
// see Close. The ledger's own Record/RecordBatch continue to use the startup
// pricing.
type PricingView struct {
	*Ledger
	pricing *Pricing
	basis   func(accountID string) string
}

// PricingView must satisfy the same ledger contracts as *Ledger. The read
// methods are promoted from the embedded ledger.
var (
	_ core.Ledger       = (*PricingView)(nil)
	_ core.BatchLedger  = (*PricingView)(nil)
	_ core.ScopedLedger = (*PricingView)(nil)
)

// WithPricing returns a view of l that attributes cost using p for every
// record written through it. The ledger's own pricing is not changed: l.Record
// and l.RecordBatch keep using the startup table. p may be nil, in which case
// records written through the view are unpriced. The view uses the ledger's
// basis function.
func (l *Ledger) WithPricing(p *Pricing) *PricingView {
	return l.newPricingView(p, l.basis)
}

// newPricingView builds a view with an explicit basis function. It is the
// internal constructor WithPricing layers on top of; a nil basis yields a NULL
// cost_basis.
func (l *Ledger) newPricingView(p *Pricing, basis func(accountID string) string) *PricingView {
	return &PricingView{Ledger: l, pricing: p, basis: basis}
}

// Pricing returns the immutable price table this view was pinned to. The
// returned table must not be mutated; *Pricing carries no public mutators.
func (v *PricingView) Pricing() *Pricing { return v.pricing }

// Record inserts one request row costed with the view's frozen pricing. It has
// the same idempotency and empty-ID semantics as Ledger.Record and writes to
// the shared database under the shared write mutex.
func (v *PricingView) Record(ctx context.Context, r core.RequestRecord) error {
	return v.recordArgs(ctx, insertArgs(r, v.pricing, v.basis))
}

// RecordBatch inserts up to MaxBatch rows in one transaction, costed with the
// view's frozen pricing. Semantics match Ledger.RecordBatch and agent ingestion
// through the view shares the view's generation for the whole batch.
func (v *PricingView) RecordBatch(ctx context.Context, rs []core.RequestRecord) error {
	if len(rs) > MaxBatch {
		return errBatchTooLarge(len(rs))
	}
	if len(rs) == 0 {
		return nil
	}
	args := make([][]any, len(rs))
	for i, r := range rs {
		args[i] = insertArgs(r, v.pricing, v.basis)
	}
	return v.recordBatchArgs(ctx, args)
}

// Close is a deliberate no-op returning nil. A PricingView is not the owner of
// the shared database — the embedded *Ledger is — so closing a view must never
// close the ledger out from under other generations. Callers close the *Ledger
// once, at shutdown; closing a view is always safe and idempotent. To make the
// read-side Close promoted from the embedded ledger unreachable, this method
// shadows it.
func (v *PricingView) Close() error { return nil }
