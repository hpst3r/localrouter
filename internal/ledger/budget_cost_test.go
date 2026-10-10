package ledger

import (
	"math"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// ResolveRecordCost is the pure seam the budget runtime settles through: it
// reports the USD cost to book for one attempt and the provenance to book it
// under. Unlike Ledger.Record it reads no mutable ledger state, so a caller can
// resolve a cost against a generation-pinned price table.

func TestResolveRecordCostTrustsProviderReported(t *testing.T) {
	// A provider-reported OpenRouter cost wins over the price table even when
	// the table would compute a different number.
	r := core.RequestRecord{
		Provider:        core.ProviderOpenRouter,
		Model:           "model-a",
		AccountID:       "acc",
		UsageKnown:      true,
		Usage:           core.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		ReportedCostUSD: fp(0.42),
	}
	cost, basis := ResolveRecordCost(r, testPricing())
	if basis != CostBasisReported {
		t.Fatalf("basis = %q, want %q", basis, CostBasisReported)
	}
	if cost == nil || !approx(*cost, 0.42) {
		t.Fatalf("cost = %v, want 0.42", cost)
	}
}

func TestResolveRecordCostReportedZeroStaysReported(t *testing.T) {
	// An explicit reported zero is a known, reported cost: unknown usage and an
	// unpriced model must not demote it to unknown. Negative zero is still zero.
	for _, zero := range []*float64{fp(0), fp(math.Copysign(0, -1))} {
		r := core.RequestRecord{
			Provider:        core.ProviderOpenRouter,
			Model:           "not-priced",
			ReportedCostUSD: zero,
		}
		cost, basis := ResolveRecordCost(r, testPricing())
		if basis != CostBasisReported || cost == nil || *cost != 0 {
			t.Fatalf("reported %v: got (cost=%v basis=%q), want (0, %q)", *zero, cost, basis, CostBasisReported)
		}
	}
}

// TestResolveRecordCostInvalidReportedCostFallsBackToEstimate pins that a
// reported cost which is not an explicit, finite, non-negative amount is
// unusable: the record is priced from the table instead of being trusted.
// Only such invalid observations fall through; a valid reported zero does not.
func TestResolveRecordCostInvalidReportedCostFallsBackToEstimate(t *testing.T) {
	for _, bad := range []*float64{fp(-1), fp(math.NaN()), fp(math.Inf(1)), fp(math.Inf(-1))} {
		r := core.RequestRecord{
			Provider:        core.ProviderOpenRouter,
			Model:           "model-a",
			AccountID:       "acc",
			UsageKnown:      true,
			Usage:           core.Usage{InputTokens: 1_000_000},
			ReportedCostUSD: bad,
		}
		cost, basis := ResolveRecordCost(r, testPricing())
		if basis != CostBasisEstimated || cost == nil || !approx(*cost, 2) {
			t.Fatalf("invalid reported %v: got (cost=%v basis=%q), want (2, %q)", *bad, cost, basis, CostBasisEstimated)
		}
	}
}

// TestResolveRecordCostReportedWinsRegardlessOfUsageKnown pins that the
// trusted-reported precedence does not depend on UsageKnown: an OpenRouter
// observation resolves as reported even when token usage is unknown (and even
// when the table can price the model, which here it cannot).
func TestResolveRecordCostReportedWinsRegardlessOfUsageKnown(t *testing.T) {
	r := core.RequestRecord{
		Provider:        core.ProviderOpenRouter,
		Model:           "model-a",
		AccountID:       "acc",
		ReportedCostUSD: fp(0.42),
	}
	cost, basis := ResolveRecordCost(r, testPricing())
	if basis != CostBasisReported || cost == nil || !approx(*cost, 0.42) {
		t.Fatalf("got (cost=%v basis=%q), want (0.42, %q)", cost, basis, CostBasisReported)
	}
}

func TestResolveRecordCostIgnoresUntrustedReportedCost(t *testing.T) {
	// A cost field from any provider but OpenRouter is not a trustworthy
	// observation, so a known-usage record the table prices resolves as an
	// estimate instead.
	r := core.RequestRecord{
		Provider:        core.ProviderOpenAICompat,
		Model:           "model-a",
		AccountID:       "acc",
		UsageKnown:      true,
		Usage:           core.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		ReportedCostUSD: fp(0.5),
	}
	cost, basis := ResolveRecordCost(r, testPricing())
	if basis != CostBasisEstimated || cost == nil || !approx(*cost, 12) {
		t.Fatalf("got (cost=%v basis=%q), want (12, %q)", cost, basis, CostBasisEstimated)
	}
}

func TestResolveRecordCostUnknown(t *testing.T) {
	p := testPricing()
	cases := []struct {
		name string
		r    core.RequestRecord
	}{
		{"unpriced model", core.RequestRecord{Model: "no-such-model", AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 10}}},
		{"unknown usage", core.RequestRecord{Model: "model-a", AccountID: "acc"}},
		{"openrouter without a reported cost", core.RequestRecord{Provider: core.ProviderOpenRouter, Model: "no-such-model", UsageKnown: true, Usage: core.Usage{InputTokens: 10}}},
		{"openrouter with a negative reported cost", core.RequestRecord{Provider: core.ProviderOpenRouter, Model: "no-such-model", UsageKnown: true, Usage: core.Usage{InputTokens: 10}, ReportedCostUSD: fp(-0.5)}},
		{"openrouter with a NaN reported cost", core.RequestRecord{Provider: core.ProviderOpenRouter, Model: "no-such-model", UsageKnown: true, Usage: core.Usage{InputTokens: 10}, ReportedCostUSD: fp(math.NaN())}},
		{"openrouter with an infinite reported cost", core.RequestRecord{Provider: core.ProviderOpenRouter, Model: "no-such-model", UsageKnown: true, Usage: core.Usage{InputTokens: 10}, ReportedCostUSD: fp(math.Inf(1))}},
		{"unpriced model with an untrusted reported cost", core.RequestRecord{Provider: core.ProviderOpenAICompat, Model: "no-such-model", AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 10}, ReportedCostUSD: fp(0.5)}},
	}
	for _, tc := range cases {
		cost, basis := ResolveRecordCost(tc.r, p)
		if cost != nil || basis != CostBasisUnknown {
			t.Errorf("%s: got (cost=%v basis=%q), want (nil, %q)", tc.name, cost, basis, CostBasisUnknown)
		}
	}
}

func TestResolveRecordCostNilTable(t *testing.T) {
	// No table prices nothing, but a trusted reported cost still resolves.
	cost, basis := ResolveRecordCost(core.RequestRecord{Provider: core.ProviderOpenRouter, ReportedCostUSD: fp(1.25)}, nil)
	if basis != CostBasisReported || cost == nil || !approx(*cost, 1.25) {
		t.Fatalf("nil table, reported: got (cost=%v basis=%q)", cost, basis)
	}
	if cost, basis := ResolveRecordCost(core.RequestRecord{Model: "model-a", UsageKnown: true, Usage: core.Usage{InputTokens: 10}}, nil); cost != nil || basis != CostBasisUnknown {
		t.Fatalf("nil table, priced model: got (cost=%v basis=%q)", cost, basis)
	}
}
