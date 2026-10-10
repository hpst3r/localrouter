package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// TestSecAnalyticsBreakdownBounded: Breakdown is capped at
// core.AnalyticsMaxBreakdown ranked rows with the rest counted in
// BreakdownOmitted; non-top keys fold into one __other__ series.
func TestSecAnalyticsBreakdownBounded(t *testing.T) {
	l, _ := openTest(t, testPricing())
	h := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	const n = core.AnalyticsMaxBreakdown + 57
	rs := make([]core.RequestRecord, 0, n)
	for i := range n {
		r := at(h.Add(time.Duration(i)*time.Minute), "model-a", int64(1000+i), 1)
		r.ID = fmt.Sprint(i)
		r.Task = fmt.Sprintf("t-%04d", i)
		rs = append(rs, r)
		if len(rs) == MaxBatch {
			record(t, l, rs...)
			rs = rs[:0]
		}
	}
	record(t, l, rs...)
	res := mustAnalytics(t, l, core.AnalyticsQuery{From: h, To: h.Add(6 * time.Hour), Bucket: "hour", Group: "task", TopN: 2}, time.UTC)
	if len(res.Breakdown) != core.AnalyticsMaxBreakdown || res.BreakdownOmitted != n-core.AnalyticsMaxBreakdown {
		t.Fatalf("breakdown len=%d omitted=%d", len(res.Breakdown), res.BreakdownOmitted)
	}
	if res.Breakdown[0].Key != fmt.Sprintf("t-%04d", n-1) || res.Breakdown[1].Key != fmt.Sprintf("t-%04d", n-2) {
		t.Fatalf("ranking: %q %q", res.Breakdown[0].Key, res.Breakdown[1].Key)
	}
	if len(res.Series) != 3 || res.Series[2].Key != core.AnalyticsOtherKey {
		t.Fatalf("series = %d", len(res.Series))
	}
	if res.Series[0].Total != res.Breakdown[0] && !rowsEqual(res.Series[0].Total, res.Breakdown[0]) {
		t.Fatalf("top series %+v != breakdown %+v", res.Series[0].Total, res.Breakdown[0])
	}
	other := res.Series[2].Total
	if other.Requests != n-2 || res.Totals.Requests != n ||
		other.InputTokens != res.Totals.InputTokens-res.Series[0].Total.InputTokens-res.Series[1].Total.InputTokens {
		t.Fatalf("other = %+v totals = %+v", other, res.Totals)
	}
}

// TestSecAnalyticsRankAcrossChunks: a key's rank is its total over every
// concurrently queried chunk, and a top-ranked empty key stays distinct from
// the __other__ fold.
func TestSecAnalyticsRankAcrossChunks(t *testing.T) {
	l, _ := openTest(t, testPricing())
	h := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	var rs []core.RequestRecord
	for i, r := range []core.RequestRecord{
		at(h, "", 10, 0),
		at(h.Add(7*time.Hour), "", 10, 0), // "" = 20 over two chunks
		at(h.Add(3*time.Hour), "b", 15, 0),
		at(h.Add(5*time.Hour), "c", 1, 0),
	} {
		r.ID = fmt.Sprint(i)
		rs = append(rs, r)
	}
	record(t, l, rs...)
	res := mustAnalytics(t, l, core.AnalyticsQuery{From: h, To: h.Add(8 * time.Hour), Bucket: "hour", Group: "model", TopN: 1}, time.UTC)
	if len(res.Breakdown) != 3 || res.Breakdown[0].Key != "" || res.Breakdown[0].InputTokens != 20 ||
		res.Breakdown[1].Key != "b" || res.BreakdownOmitted != 0 {
		t.Fatalf("breakdown = %+v", res.Breakdown)
	}
	if len(res.Series) != 2 || res.Series[0].Key != "" || res.Series[0].Total.Requests != 2 ||
		res.Series[1].Key != core.AnalyticsOtherKey || res.Series[1].Total.InputTokens != 16 {
		t.Fatalf("series = %+v", res.Series)
	}
}

// TestSecReportedCostCapped: provider-reported costs above
// core.MaxReportedCostUSD are unusable (stored NULL, not summed to +Inf).
func TestSecReportedCostCapped(t *testing.T) {
	for _, c := range []float64{core.MaxReportedCostUSD + 1, 1.5e308} {
		if _, ok := validReportedCost(&c); ok {
			t.Fatalf("validReportedCost(%v) accepted", c)
		}
	}
	if c := core.MaxReportedCostUSD; func() bool { _, ok := validReportedCost(&c); return !ok }() {
		t.Fatal("max cost rejected")
	}
	l, _ := openTest(t, nil)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"a", "b"} {
		c := 1.5e308
		if err := l.Record(ctx, core.RequestRecord{ID: id, StartedAt: now, Provider: core.ProviderOpenRouter,
			AccountID: "or", Model: "m", Client: "c", Class: core.ClassInteractive, Route: "r",
			Status: 200, ReportedCostUSD: &c}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := l.Summary(ctx, now.Add(-time.Hour), "account")
	if err != nil || len(rows) != 1 || rows[0].CostUSD != nil {
		t.Fatalf("rows = %+v err = %v", rows, err)
	}
}

// TestSecCostSumsSaturate: even if stored costs overflow when summed, Summary
// (every group) and Analytics stay finite and JSON-encodable.
func TestSecCostSumsSaturate(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	now := time.Now().Truncate(time.Hour)
	record(t, l, at(now, "model-a", 1, 0), at(now.Add(time.Minute), "model-a", 1, 0))
	if _, err := l.db.Exec(`UPDATE requests SET cost_usd = 1.5e308`); err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"account", "day"} {
		rows, err := l.Summary(ctx, now.Add(-time.Hour), g)
		if err != nil || len(rows) != 1 || rows[0].CostUSD == nil || *rows[0].CostUSD != math.MaxFloat64 {
			t.Fatalf("group %s: rows = %+v err = %v", g, rows, err)
		}
		if _, err := json.Marshal(rows); err != nil {
			t.Fatalf("group %s: %v", g, err)
		}
	}
	res, err := l.Analytics(ctx, core.AnalyticsQuery{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: "hour", Group: "account"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(res); err != nil {
		t.Fatal(err)
	}
	if res.Totals.CostUSD == nil || *res.Totals.CostUSD != math.MaxFloat64 {
		t.Fatalf("totals = %+v", res.Totals)
	}
}

// TestSecNonFiniteComputedCostStoredNull: a price table that bypassed load
// validation cannot store an infinite cost via Record or Reprice.
func TestSecNonFiniteComputedCostStoredNull(t *testing.T) {
	p := NewPricing(map[string]ModelPrice{"huge": {Input: 1e306, Output: 1e306}})
	l, _ := openTest(t, p)
	ctx := context.Background()
	now := time.Now()
	record(t, l, at(now, "huge", 1e12, 1e12))
	var n int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE cost_usd IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("null costs = %d err = %v", n, err)
	}
	if _, err := l.db.Exec(`UPDATE requests SET cost_usd = 1`); err != nil {
		t.Fatal(err)
	}
	if priced, err := l.Reprice(ctx); err != nil || priced != 0 {
		t.Fatalf("Reprice = %d, %v", priced, err)
	}
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE cost_usd IS NULL AND cost_basis IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("null costs after reprice = %d err = %v", n, err)
	}
}

// TestSecLiteLLMImportRejectsBadPrice: an imported per-1M price above
// core.MaxPricePerMTokUSD or negative fails the import, naming the model.
func TestSecLiteLLMImportRejectsBadPrice(t *testing.T) {
	for _, js := range []string{
		`{"ok":{"input_cost_per_token":1e-6,"output_cost_per_token":1e-6},"evil-model":{"input_cost_per_token":1e301,"output_cost_per_token":1e-6}}`,
		`{"evil-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2}}`,
		`{"evil-model":{"input_cost_per_token":1e-6,"output_cost_per_token":1e-6,"cache_read_input_token_cost":-1e-6}}`,
		`{"evil-model":{"input_cost_per_token":1e-6,"output_cost_per_token":1e-6,"cache_creation_input_token_cost":1e300}}`,
	} {
		_, err := ImportLiteLLM(strings.NewReader(js))
		if err == nil || !strings.Contains(err.Error(), `"evil-model"`) {
			t.Fatalf("%s: err = %v", js, err)
		}
	}
	// Exactly the ceiling is accepted.
	prices, err := ImportLiteLLM(strings.NewReader(`{"m":{"input_cost_per_token":1,"output_cost_per_token":0}}`))
	if err != nil || prices["m"].Input != core.MaxPricePerMTokUSD {
		t.Fatalf("prices = %+v err = %v", prices, err)
	}
}

// TestSecLoadPricingRejectsNonFinite: YAML .nan/.inf and over-ceiling prices
// are rejected in both the imported and the local overrides file.
func TestSecLoadPricingRejectsNonFinite(t *testing.T) {
	for _, body := range []string{
		"models:\n  m:\n    input: .nan\n    output: 1\n",
		"models:\n  m:\n    input: 1\n    output: .inf\n",
		"models:\n  m:\n    input: 1\n    output: -.inf\n",
		"models:\n  m:\n    input: 1000001\n    output: 1\n",
		"models:\n  m:\n    input: 1\n    output: 1\n    cached_input: .nan\n",
		"models:\n  m:\n    input: 1\n    output: 1\n    cache_creation_input: 1e300\n",
	} {
		for _, local := range []bool{false, true} {
			path := filepath.Join(t.TempDir(), "pricing.yaml")
			target := path
			if local {
				target = LocalPricingPath(path)
			}
			if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPricing(path); err == nil || !strings.Contains(err.Error(), `"m"`) {
				t.Fatalf("local=%v %q: err = %v", local, body, err)
			}
		}
	}
}
