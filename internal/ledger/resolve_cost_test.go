package ledger

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Once the proxy captures a Codex row's usage, the ledger prices and
// attributes it exactly like any other non-OpenRouter provider's row with the
// same usage: same price key (pricing_model, else model), same cost_usd, and
// the account's configured cost_basis. ResolveRecordCost (budget settlement)
// agrees.
func TestCodexRowsPriceLikeOtherProviders(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "localrouter.db"), attrPricing(),
		func(string) string { return "api_equivalent" })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	usage := core.Usage{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 500_000, ReasoningTokens: 100_000}
	type got struct {
		cost  sql.NullFloat64
		basis sql.NullString
		est   float64
		eb    string
	}
	read := func(provider, pricingModel string) got {
		id := provider + "/" + pricingModel
		rec := core.RequestRecord{
			ID: id, StartedAt: time.UnixMilli(1), Model: "gpt-x", PricingModel: pricingModel,
			Provider: provider, AccountID: "acc", Status: 200, UsageKnown: true, Usage: usage,
		}
		if err := l.Record(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
		var g got
		if err := l.db.QueryRow(`SELECT cost_usd, cost_basis FROM requests WHERE id = ?`, id).Scan(&g.cost, &g.basis); err != nil {
			t.Fatal(err)
		}
		c, b := ResolveRecordCost(rec, attrPricing())
		if c == nil {
			t.Fatalf("%s: ResolveRecordCost = nil (%s)", id, b)
		}
		g.est, g.eb = *c, b
		return g
	}
	for _, pm := range []string{"", "backend-a"} {
		want := read(core.ProviderOpenAICompat, pm)
		if !want.cost.Valid || want.basis.String != "api_equivalent" || want.eb != CostBasisEstimated || want.est != want.cost.Float64 {
			t.Fatalf("pricing_model %q: baseline %+v", pm, want)
		}
		for _, prov := range []string{core.ProviderCodex, core.ProviderOllama, core.ProviderClaude} {
			if g := read(prov, pm); g != want {
				t.Errorf("pricing_model %q: %s = %+v, want %+v", pm, prov, g, want)
			}
		}
	}
}
