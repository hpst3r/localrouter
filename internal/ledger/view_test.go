package ledger

// Frozen pricing view tests (chunk 2 reload).
//
// A PricingView is a generation-scoped writer over the SAME *Ledger (same
// *sql.DB, same write mutex). It never reopens or closes the shared database
// and never mutates shared ledger state: it only pins an immutable *Pricing
// to be used for cost attribution of records written through it.

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Generation fixtures. Numbers are arbitrary, not real prices.
//
//	generation A: input 2 USD/1M, output 10 USD/1M  -> 1M/1M = 12.00
//	generation B: input 8 USD/1M, output 40 USD/1M -> 1M/1M = 48.00
func viewPricingA() *Pricing {
	return NewPricing(map[string]ModelPrice{"model-a": {Input: 2, Output: 10}})
}

func viewPricingB() *Pricing {
	return NewPricing(map[string]ModelPrice{"model-a": {Input: 8, Output: 40}})
}

const (
	viewCostA = 12.0
	viewCostB = 48.0
)

// viewBasis tags rows written through a view so tests can tell the frozen
// writer's basis function apart from the ledger's startup basis.
func viewBasis(accountID string) string { return "view:" + accountID }

// viewRecord is one fully-priced fixture record (1M input + 1M output),
// stamped at the current time so Summary windows cover it.
func viewRecord(id string) core.RequestRecord {
	return core.RequestRecord{
		ID:         id,
		StartedAt:  time.Now(),
		AccountID:  "acct",
		Model:      "model-a",
		Provider:   "test",
		UsageKnown: true,
		Usage:      core.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
	}
}

// viewRow reads back cost_usd/cost_basis for one id.
func viewRow(t *testing.T, l *Ledger, id string) (sql.NullFloat64, sql.NullString) {
	t.Helper()
	var cost sql.NullFloat64
	var basis sql.NullString
	if err := l.db.QueryRow(`SELECT cost_usd, cost_basis FROM requests WHERE id = ?`, id).
		Scan(&cost, &basis); err != nil {
		t.Fatalf("read row %q: %v", id, err)
	}
	return cost, basis
}

// TestPricingViewFreezesCostAcrossReload is the core requirement: two views
// over the same ledger, each pinned to a different generation, attribute the
// same usage at their own generation's price, all rows landing in one shared
// table.
func TestPricingViewFreezesCostAcrossReload(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t, viewPricingA()) // startup generation == A
	now := time.Now()

	old := l.WithPricing(viewPricingA())
	if err := old.Record(ctx, viewRecord("old")); err != nil {
		t.Fatalf("old view record: %v", err)
	}
	newv := l.WithPricing(viewPricingB())
	if err := newv.Record(ctx, viewRecord("new")); err != nil {
		t.Fatalf("new view record: %v", err)
	}
	// The ledger's own (startup) pricing must be untouched by view creation.
	if err := l.Record(ctx, viewRecord("startup")); err != nil {
		t.Fatalf("ledger record: %v", err)
	}

	if c, _ := viewRow(t, l, "old"); !c.Valid || !approx(c.Float64, viewCostA) {
		t.Errorf("old view cost = %v, want %.2f", c, viewCostA)
	}
	if c, _ := viewRow(t, l, "new"); !c.Valid || !approx(c.Float64, viewCostB) {
		t.Errorf("new view cost = %v, want %.2f", c, viewCostB)
	}
	if c, _ := viewRow(t, l, "startup"); !c.Valid || !approx(c.Float64, viewCostA) {
		t.Errorf("startup cost = %v, want %.2f (view creation mutated ledger pricing)", c, viewCostA)
	}

	// All three rows share one table: the view is a writer over the same DB.
	rows, err := l.Summary(ctx, now.Add(-time.Hour), "account")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(rows) != 1 || rows[0].Requests != 3 {
		t.Fatalf("summary rows = %+v, want one row with 3 requests", rows)
	}
	if rows[0].CostUSD == nil || !approx(*rows[0].CostUSD, viewCostA+viewCostB+viewCostA) {
		t.Errorf("summary cost = %v, want %.2f", rows[0].CostUSD, viewCostA+viewCostB+viewCostA)
	}
}

// TestPricingViewBatchSamePrices checks RecordBatch through a view uses the
// frozen generation consistently for every row in the batch.
func TestPricingViewBatchSamePrices(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t, viewPricingA())
	now := time.Now()

	batch := make([]core.RequestRecord, 0, 5)
	for i := range 5 {
		batch = append(batch, viewRecord(fmt.Sprintf("batch-%d", i)))
	}
	v := l.WithPricing(viewPricingB())
	if err := v.RecordBatch(ctx, batch); err != nil {
		t.Fatalf("view batch: %v", err)
	}
	for i := range 5 {
		id := fmt.Sprintf("batch-%d", i)
		if c, _ := viewRow(t, l, id); !c.Valid || !approx(c.Float64, viewCostB) {
			t.Errorf("batch row %s cost = %v, want %.2f", id, c, viewCostB)
		}
	}
	rows, err := l.Summary(ctx, now.Add(-time.Hour), "account")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(rows) != 1 || rows[0].CostUSD == nil || !approx(*rows[0].CostUSD, 5*viewCostB) {
		t.Errorf("summary = %+v, want cost %.2f over 5 rows", rows, 5*viewCostB)
	}
}

// TestPricingViewBasisIsFrozen confirms the view's basis function (not the
// ledger's) supplies cost_basis, alongside the frozen pricing.
func TestPricingViewBasisIsFrozen(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t, viewPricingA())

	v := l.newPricingView(viewPricingB(), viewBasis)
	if err := v.Record(ctx, viewRecord("basis")); err != nil {
		t.Fatalf("view record: %v", err)
	}
	if _, b := viewRow(t, l, "basis"); !b.Valid || b.String != "view:acct" {
		t.Errorf("view basis = %v, want %q", b, "view:acct")
	}

	// The ledger's startup writer keeps its own basis.
	if err := l.Record(ctx, viewRecord("startup")); err != nil {
		t.Fatalf("ledger record: %v", err)
	}
	if _, b := viewRow(t, l, "startup"); !b.Valid || b.String != "metered" {
		t.Errorf("startup basis = %v, want %q", b, "metered")
	}
}

// TestPricingViewConcurrentRace exercises many generations writing through
// their own views at once, plus concurrent reads on the shared ledger. Run
// under -race this proves there is no shared mutation of the view or ledger.
func TestPricingViewConcurrentRace(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t, viewPricingA())
	now := time.Now()

	const gens = 16
	const perGen = 8
	var wg sync.WaitGroup
	for g := range gens {
		p := viewPricingA()
		want := viewCostA
		if g%2 == 1 {
			p = viewPricingB()
			want = viewCostB
		}
		v := l.WithPricing(p)
		wg.Add(1)
		go func(g int, v *PricingView, want float64) {
			defer wg.Done()
			for i := range perGen {
				id := fmt.Sprintf("g%d-r%d", g, i)
				if err := v.Record(ctx, viewRecord(id)); err != nil {
					t.Errorf("gen %d record %s: %v", g, id, err)
					return
				}
				if c, _ := viewRow(t, l, id); !c.Valid || !approx(c.Float64, want) {
					t.Errorf("gen %d row %s cost = %v, want %.2f", g, id, c, want)
				}
			}
		}(g, v, want)
	}
	// Concurrent readers/summarisers on the shared ledger and startup writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = l.Record(ctx, viewRecord("startup-race"))
		if _, err := l.Summary(ctx, now.Add(-24*time.Hour), "account"); err != nil {
			t.Errorf("summary during race: %v", err)
		}
	}()
	wg.Wait()

	rows, err := l.Summary(ctx, now.Add(-24*time.Hour), "account")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(rows) != 1 || rows[0].Requests != gens*perGen+1 {
		t.Fatalf("summary requests = %+v, want %d", rows, gens*perGen+1)
	}
	want := float64(gens*perGen/2)*viewCostA + float64(gens*perGen/2)*viewCostB + viewCostA
	if rows[0].CostUSD == nil || !approx(*rows[0].CostUSD, want) {
		t.Errorf("summary cost = %v, want %.2f", rows[0].CostUSD, want)
	}
}

// TestPricingViewCloseDoesNotCloseDB documents and enforces the no-op Close:
// a view is not the DB owner; closing it must not close the shared ledger.
func TestPricingViewCloseDoesNotCloseDB(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t, viewPricingA())

	v := l.WithPricing(viewPricingB())
	if err := v.Close(); err != nil {
		t.Fatalf("view Close: %v", err)
	}
	if err := v.Close(); err != nil { // idempotent no-op
		t.Fatalf("second view Close: %v", err)
	}

	// The shared ledger is still fully usable.
	if err := l.Ping(ctx); err != nil {
		t.Fatalf("base ping after view.Close: %v", err)
	}
	if err := l.Record(ctx, viewRecord("after-close")); err != nil {
		t.Fatalf("base record after view.Close: %v", err)
	}
	if err := v.Record(ctx, viewRecord("after-close-view")); err != nil {
		t.Fatalf("view record after view.Close: %v", err)
	}
	if c, _ := viewRow(t, l, "after-close-view"); !c.Valid || !approx(c.Float64, viewCostB) {
		t.Errorf("view row after Close cost = %v, want %.2f", c, viewCostB)
	}
}

// TestPricingViewPingInherited: Ping is inherited from the embedded ledger
// and must probe the shared DB without reopening it.
func TestPricingViewPingInherited(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t, viewPricingA())
	v := l.WithPricing(viewPricingB())
	if err := v.Ping(ctx); err != nil {
		t.Fatalf("view ping: %v", err)
	}
}

// TestPricingViewNilPricing: a view pinned to nil/empty pricing writes NULL
// cost, exactly like the nil-safe base ledger.
func TestPricingViewNilPricing(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t, viewPricingA())

	for _, tc := range []struct {
		name string
		p    *Pricing
	}{
		{"nil", nil},
		{"empty", NewPricing(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := l.WithPricing(tc.p)
			id := "unpriced-" + tc.name
			if err := v.Record(ctx, viewRecord(id)); err != nil {
				t.Fatalf("record: %v", err)
			}
			cost, basis := viewRow(t, l, id)
			if cost.Valid || basis.Valid {
				t.Errorf("cost/basis = %v/%v, want both NULL", cost, basis)
			}
		})
	}
}

// TestPricingViewFrozenAgainstSourceMapMutation: a view pinned to a *Pricing
// built from an input map must be immune to later mutation of that source map.
// NewPricing copies every entry, so no clone is needed at view creation; this
// test pins that guarantee for the frozen-view use case.
func TestPricingViewFrozenAgainstSourceMapMutation(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t, nil)

	src := map[string]ModelPrice{"model-a": {Input: 2, Output: 10}}
	v := l.WithPricing(NewPricing(src))
	src["model-a"] = ModelPrice{Input: 800, Output: 4000} // mutate after pinning

	if err := v.Record(ctx, viewRecord("frozen")); err != nil {
		t.Fatalf("record: %v", err)
	}
	if c, _ := viewRow(t, l, "frozen"); !c.Valid || !approx(c.Float64, viewCostA) {
		t.Errorf("frozen cost = %v, want %.2f (source map mutation leaked into view)", c, viewCostA)
	}
}

// TestPricingViewDoesNotRepriceHistory: creating a view (any generation) must
// not retroactively change rows written earlier by the ledger.
func TestPricingViewDoesNotRepriceHistory(t *testing.T) {
	ctx := context.Background()
	l, _ := openTest(t, viewPricingA())
	now := time.Now()

	if err := l.Record(ctx, viewRecord("hist")); err != nil {
		t.Fatalf("history record: %v", err)
	}
	before, err := l.Summary(ctx, now.Add(-time.Hour), "account")
	if err != nil {
		t.Fatalf("summary before: %v", err)
	}

	_ = l.WithPricing(viewPricingB())
	_ = l.WithPricing(viewPricingB()).Record(ctx, viewRecord("new"))

	if c, _ := viewRow(t, l, "hist"); !c.Valid || !approx(c.Float64, viewCostA) {
		t.Errorf("history cost = %v, want %.2f (view creation repriced history)", c, viewCostA)
	}
	after, err := l.Summary(ctx, now.Add(-time.Hour), "account")
	if err != nil {
		t.Fatalf("summary after: %v", err)
	}
	if before[0].CostUSD == nil || after[0].CostUSD == nil ||
		!approx(*after[0].CostUSD-*before[0].CostUSD, viewCostB) {
		t.Errorf("summary delta = %v -> %v, want +%.2f", before[0].CostUSD, after[0].CostUSD, viewCostB)
	}
}
