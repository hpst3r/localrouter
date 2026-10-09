package ledger

// Reconciled-PR8 regression coverage: the provider-reported cost precedence
// (from the reported-cost feature) must be applied by a generation-pinned
// PricingView on top of master's frozen price table, for both Record and
// RecordBatch, and must not depend on the ledger's mutable startup state.

import (
	"context"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// A PricingView must apply the provider-reported precedence (an explicit,
// finite, non-negative OpenRouter cost wins regardless of UsageKnown, stored
// with the reserved provenance) on top of its generation-pinned price table,
// for both Record and RecordBatch. When no usable reported cost is present it
// must fall back to the view's frozen table and frozen basis — never the
// ledger's mutable startup pricing/basis.
func TestPricingViewReportedCostPrecedence(t *testing.T) {
	l, _ := openTest(t, testPricing()) // startup table: model-a 2/10
	// Pin generation B (8/40 -> 48.00 for 1M/1M) and a view-specific basis, so
	// any fallback is provably costed by the view's generation, not the
	// ledger's startup pricing (which would be 12.00 / "metered").
	v := l.newPricingView(viewPricingB(), viewBasis)
	ctx := context.Background()
	now := time.Now()

	vR := []core.RequestRecord{
		// Paid reported cost wins over both the view table (48) and startup (12).
		{ID: "v-paid", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "acc", UsageKnown: true,
			Usage:           core.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			ReportedCostUSD: fp(0.42)},
		// Explicit zero wins even with no usage known.
		{ID: "v-zero", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "acc", ReportedCostUSD: fp(0)},
		// No reported cost: the view's pinned table prices it, with the view basis.
		{ID: "v-fallback", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "acc", UsageKnown: true,
			Usage: core.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}},
		// Non-OpenRouter reported cost is ignored; the view table prices it.
		{ID: "v-other", StartedAt: now, Provider: core.ProviderOpenAICompat, Model: "model-a",
			AccountID: "acc", UsageKnown: true,
			Usage:           core.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			ReportedCostUSD: fp(0.5)},
	}
	for _, r := range vR[:3] {
		if err := v.Record(ctx, r); err != nil {
			t.Fatalf("view.Record: %v", err)
		}
	}
	// RecordBatch must share the same pinned generation for the whole batch.
	if err := v.RecordBatch(ctx, vR[3:]); err != nil {
		t.Fatalf("view.RecordBatch: %v", err)
	}

	wantStoredCost(t, readStored(t, v.Ledger, "v-paid"), fp(0.42), wantBasisReported)
	wantStoredCost(t, readStored(t, v.Ledger, "v-zero"), fp(0), wantBasisReported)
	// Generation-pinned fallback: the view's table (B) and basis, not startup A.
	wantStoredCost(t, readStored(t, v.Ledger, "v-fallback"), fp(viewCostB), "view:acc")
	wantStoredCost(t, readStored(t, v.Ledger, "v-other"), fp(viewCostB), "view:acc")
}

// Even a view pinned to a nil price table stores a provider-reported zero as an
// explicit, priced zero with the reserved provenance; a record with neither
// usage nor a reported cost stays unpriced. Record and RecordBatch agree.
func TestPricingViewNilTableReportedZero(t *testing.T) {
	l, _ := openTest(t, testPricing())
	v := l.WithPricing(nil) // this view generation has no price table at all
	ctx := context.Background()
	now := time.Now()

	if err := v.Record(ctx, core.RequestRecord{
		ID: "n-zero", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
		AccountID: "acc", ReportedCostUSD: fp(0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordBatch(ctx, []core.RequestRecord{
		{ID: "n-none", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 1000}},
		{ID: "n-paid", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "acc", ReportedCostUSD: fp(0.25)},
	}); err != nil {
		t.Fatal(err)
	}

	// nil table: a reported zero is still an explicit, priced provider cost.
	wantStoredCost(t, readStored(t, v.Ledger, "n-zero"), fp(0), wantBasisReported)
	wantStoredCost(t, readStored(t, v.Ledger, "n-paid"), fp(0.25), wantBasisReported)
	// No reported cost and no table: the row stays unpriced.
	wantStoredCost(t, readStored(t, v.Ledger, "n-none"), nil, "")
}
