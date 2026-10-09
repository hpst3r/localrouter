package ledger

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// wantBasisReported is the reserved cost_basis provenance for a cost the
// upstream provider reported itself (OpenRouter usage.cost).
const wantBasisReported = "provider_reported"

func fp(v float64) *float64 { return &v }

type storedCost struct {
	cost  sql.NullFloat64
	basis sql.NullString
	known int
	in    int64
	out   int64
}

func readStored(t *testing.T, l *Ledger, id string) storedCost {
	t.Helper()
	var s storedCost
	if err := l.db.QueryRow(`SELECT cost_usd, cost_basis, usage_known, input_tokens, output_tokens
		FROM requests WHERE id = ?`, id).Scan(&s.cost, &s.basis, &s.known, &s.in, &s.out); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return s
}

func wantStoredCost(t *testing.T, s storedCost, cost *float64, basis string) {
	t.Helper()
	if cost == nil {
		if s.cost.Valid {
			t.Fatalf("cost = %v, want NULL", s.cost.Float64)
		}
	} else if !s.cost.Valid || *cost != s.cost.Float64 {
		t.Fatalf("cost = %+v, want %v", s.cost, *cost)
	}
	if basis == "" {
		if s.basis.Valid {
			t.Fatalf("basis = %q, want NULL", s.basis.String)
		}
	} else if !s.basis.Valid || s.basis.String != basis {
		t.Fatalf("basis = %+v, want %q", s.basis, basis)
	}
}

// validReportedCost is the ledger's own defense: only a finite, non-negative
// value from an explicit pointer is usable, whatever an agent may push.
func TestValidReportedCost(t *testing.T) {
	cases := []struct {
		name string
		in   *float64
		want float64
		ok   bool
	}{
		{"nil", nil, 0, false},
		{"zero", fp(0), 0, true},
		{"paid", fp(0.123), 0.123, true},
		{"tiny", fp(0.000123), 0.000123, true},
		{"negative", fp(-0.01), 0, false},
		{"nan", fp(math.NaN()), 0, false},
		{"posinf", fp(math.Inf(1)), 0, false},
		{"neginf", fp(math.Inf(-1)), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := validReportedCost(tc.in)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Fatalf("validReportedCost(%v) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// A provider-reported cost is persisted verbatim with cost_basis
// "provider_reported", taking precedence over the local pricing table
// (including an explicit zero, and regardless of UsageKnown). Missing,
// invalid, or non-OpenRouter costs fall back to existing logic.
func TestReportedCostPrecedence(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	now := time.Now()

	recs := []core.RequestRecord{
		// Explicit zero from OpenRouter wins over the table's price.
		{ID: "or-zero", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "acc", ReportedCostUSD: fp(0)},
		// A paid reported cost wins even though the table would price it too.
		{ID: "or-paid", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 1000},
			ReportedCostUSD: fp(0.123)},
		// No reported cost: the table prices it as before.
		{ID: "or-nil-priced", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 1000}},
		// No reported cost and no known usage: unpriced.
		{ID: "or-nil-unknown", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "acc"},
		// Provider-reported basis is provenance, not the account's basis.
		{ID: "or-sub", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "sub", UsageKnown: true, Usage: core.Usage{InputTokens: 1000},
			ReportedCostUSD: fp(0.9)},
		// A negative cost is unusable: fall back to the table.
		{ID: "or-negative", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "model-a",
			AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 1000},
			ReportedCostUSD: fp(-1)},
		// Non-OpenRouter cost fields are ignored entirely.
		{ID: "other-cost", StartedAt: now, Provider: core.ProviderOpenAICompat, Model: "model-a",
			AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 1000},
			ReportedCostUSD: fp(0.5)},
	}
	for _, r := range recs {
		if err := l.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	tablePrice := 1000 * 2 / 1e6 // model-a input 2 USD/Mtok
	wantStoredCost(t, readStored(t, l, "or-zero"), fp(0), wantBasisReported)
	wantStoredCost(t, readStored(t, l, "or-paid"), fp(0.123), wantBasisReported)
	wantStoredCost(t, readStored(t, l, "or-nil-priced"), fp(tablePrice), "metered")
	wantStoredCost(t, readStored(t, l, "or-nil-unknown"), nil, "")
	wantStoredCost(t, readStored(t, l, "or-sub"), fp(0.9), wantBasisReported)
	wantStoredCost(t, readStored(t, l, "or-negative"), fp(tablePrice), "metered")
	wantStoredCost(t, readStored(t, l, "other-cost"), fp(tablePrice), "metered")
}

// An unusable reported cost never drops the valid token usage recorded
// alongside it.
func TestReportedCostInvalidKeepsTokens(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	r := core.RequestRecord{ID: "t", StartedAt: time.Now(), Provider: core.ProviderOpenRouter,
		Model: "unpriced", AccountID: "acc", UsageKnown: true,
		Usage:           core.Usage{InputTokens: 21, OutputTokens: 128, CachedInputTokens: 6, ReasoningTokens: 2},
		ReportedCostUSD: fp(math.NaN())}
	if err := l.Record(ctx, r); err != nil {
		t.Fatal(err)
	}
	s := readStored(t, l, "t")
	if s.in != 21 || s.out != 128 || s.known != 1 {
		t.Fatalf("tokens lost: %+v", s)
	}
	wantStoredCost(t, s, nil, "")
}

// Aggregates treat an explicit zero cost as priced, not unpriced.
func TestReportedCostZeroIsPricedInSummary(t *testing.T) {
	l, _ := openTest(t, nil) // no pricing table at all
	ctx := context.Background()
	now := time.Now()
	recs := []core.RequestRecord{
		{ID: "z", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "m", AccountID: "zero",
			UsageKnown: true, Usage: core.Usage{InputTokens: 21, OutputTokens: 128}, ReportedCostUSD: fp(0)},
		{ID: "p", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "m", AccountID: "paid",
			UsageKnown: true, Usage: core.Usage{InputTokens: 21, OutputTokens: 128}, ReportedCostUSD: fp(0.25)},
		{ID: "u", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "m", AccountID: "unpriced",
			UsageKnown: true, Usage: core.Usage{InputTokens: 21, OutputTokens: 128}},
	}
	for _, r := range recs {
		if err := l.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	since := now.Add(-time.Hour)

	got := map[string]core.UsageRow{}
	rows, err := l.Summary(ctx, since, "account")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		got[r.Key] = r
	}
	if r := got["zero"]; r.CostUSD == nil || *r.CostUSD != 0 || r.UnpricedRequests != 0 {
		t.Fatalf("zero-cost group %+v", r)
	}
	if r := got["paid"]; r.CostUSD == nil || *r.CostUSD != 0.25 || r.UnpricedRequests != 0 {
		t.Fatalf("paid group %+v", r)
	}
	if r := got["unpriced"]; r.CostUSD != nil || r.UnpricedRequests != 1 {
		t.Fatalf("unpriced group %+v", r)
	}

	// The local-day grouping must also reflect the zero as priced.
	day, err := l.Summary(ctx, since, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(day) != 1 {
		t.Fatalf("day rows %+v", day)
	}
	if day[0].CostUSD == nil || *day[0].CostUSD != 0.25 || day[0].UnpricedRequests != 1 {
		t.Fatalf("day aggregate %+v", day[0])
	}

	// Analytics must likewise count the reported zero as priced.
	res, err := l.Analytics(ctx, core.AnalyticsQuery{
		From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: "day", Group: "account",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Totals.CostUSD == nil || *res.Totals.CostUSD != 0.25 || res.Totals.UnpricedRequests != 1 {
		t.Fatalf("analytics totals %+v", res.Totals)
	}
}

// RecordBatch and idempotent re-records preserve provider-reported costs, and
// reopening an existing database keeps them.
func TestReportedCostBatchIdempotentReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "l.db")
	ctx := context.Background()
	now := time.Now()

	l, err := Open(path, nil, basisFn)
	if err != nil {
		t.Fatal(err)
	}
	batch := []core.RequestRecord{
		{ID: "b0", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "m", AccountID: "acc",
			UsageKnown: true, Usage: core.Usage{InputTokens: 5}, ReportedCostUSD: fp(0)},
		{ID: "b1", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "m", AccountID: "acc",
			UsageKnown: true, Usage: core.Usage{InputTokens: 5}, ReportedCostUSD: fp(0.000123)},
	}
	if err := l.RecordBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	// Re-recording the same IDs is a no-op.
	if err := l.RecordBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(ctx, batch[1]); err != nil {
		t.Fatal(err)
	}
	wantStoredCost(t, readStored(t, l, "b0"), fp(0), wantBasisReported)
	wantStoredCost(t, readStored(t, l, "b1"), fp(0.000123), wantBasisReported)
	l.Close()

	l2, err := Open(path, nil, basisFn)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	wantStoredCost(t, readStored(t, l2, "b0"), fp(0), wantBasisReported)
	wantStoredCost(t, readStored(t, l2, "b1"), fp(0.000123), wantBasisReported)
	var n int
	l2.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
	if n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
}

// Reprice recomputes estimated rows but must never overwrite or delete a
// provider-reported cost.
func TestRepriceProtectsProviderReportedCosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "l.db")
	ctx := context.Background()
	now := time.Now()

	l, err := Open(path, nil, func(string) string { return "api_equivalent" })
	if err != nil {
		t.Fatal(err)
	}
	recs := []core.RequestRecord{
		// A reported paid cost (actual) — must survive untouched.
		{ID: "p1", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "m", AccountID: "or",
			UsageKnown: true, Usage: core.Usage{InputTokens: 1000, CachedInputTokens: 200, OutputTokens: 100},
			ReportedCostUSD: fp(0.05)},
		// A reported cost on a cost-only row (usage unknown) — also protected.
		{ID: "p2", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "m", AccountID: "or",
			ReportedCostUSD: fp(0.07)},
		// An estimated row that reprice must update.
		{ID: "e1", StartedAt: now, Provider: core.ProviderOpenAICompat, Model: "m", AccountID: "acc",
			UsageKnown: true, Usage: core.Usage{InputTokens: 1000, CachedInputTokens: 200, OutputTokens: 100}},
		// Still unpriced after reprice.
		{ID: "u1", StartedAt: now, Provider: core.ProviderOpenAICompat, Model: "unpriced",
			AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 5}},
	}
	for _, r := range recs {
		if err := l.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	l, err = Open(path, NewPricing(map[string]ModelPrice{"m": {Input: 2, CachedInput: fp(0.5), Output: 10}}),
		func(string) string { return "api_equivalent" })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	n, err := l.Reprice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Reprice = %d, want 1 (only the estimated row)", n)
	}
	estimate := (800*2 + 200*0.5 + 100*10) / 1e6
	wantStoredCost(t, readStored(t, l, "p1"), fp(0.05), wantBasisReported)
	wantStoredCost(t, readStored(t, l, "p2"), fp(0.07), wantBasisReported)
	wantStoredCost(t, readStored(t, l, "e1"), fp(estimate), "api_equivalent")
	wantStoredCost(t, readStored(t, l, "u1"), nil, "")

	// A second reprice is stable.
	if n, err = l.Reprice(ctx); err != nil || n != 1 {
		t.Fatalf("second Reprice = %d, %v", n, err)
	}
	wantStoredCost(t, readStored(t, l, "p1"), fp(0.05), wantBasisReported)
	wantStoredCost(t, readStored(t, l, "p2"), fp(0.07), wantBasisReported)
}

// The reprice guards are NULL-safe: a provider_reported row whose cost is NULL
// (never expected, but possible) is left alone rather than being priced, and
// its basis is never cleared.
func TestRepriceNullSafeOnReportedBasis(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	// Hand-insert an inconsistent row: provider_reported provenance, no cost.
	if _, err := l.db.Exec(`INSERT INTO requests (id, started_at, client, class, route, model,
		provider, account_id, upstream_identity, status, input_tokens, cached_input_tokens,
		output_tokens, reasoning_tokens, usage_known, cost_usd, cost_basis, latency_ms, bytes_out,
		session, task, agent, error, cache_creation_input_tokens, host)
		VALUES ('weird', 1, 'c', 'interactive', 'r', 'model-a', 'openrouter', 'acc', '', 200,
		1000, 0, 0, 0, 1, NULL, ?, 0, 0, '', '', '', '', 0, '')`, wantBasisReported); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reprice(ctx); err != nil {
		t.Fatal(err)
	}
	s := readStored(t, l, "weird")
	if s.cost.Valid {
		t.Fatalf("provider_reported NULL-cost row was priced: %+v", s)
	}
	if !s.basis.Valid || s.basis.String != wantBasisReported {
		t.Fatalf("provider_reported basis lost: %+v", s)
	}
}

// Regression coverage: a cost-only row (UsageKnown=false) is still a real
// observation of price even though token usage is unknown. Such rows must
// accumulate their provider-reported cost in Summary and analytics, be counted
// as unknown-usage (usage_known=0 counts unknown TOKENS, not unknown cost), and
// must never be counted as unpriced while a usable reported cost is present.
func TestReportedCostCostOnlyRowsAggregate(t *testing.T) {
	l, _ := openTest(t, nil) // no pricing table: only the provider cost is available
	ctx := context.Background()
	now := time.Now()

	recs := []core.RequestRecord{
		{ID: "cz", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "m",
			AccountID: "or", ReportedCostUSD: fp(0)},
		{ID: "cp", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "m",
			AccountID: "or", ReportedCostUSD: fp(0.25)},
	}
	for _, r := range recs {
		if err := l.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// Persistence: the cost-only rows store the reported cost verbatim.
	wantStoredCost(t, readStored(t, l, "cz"), fp(0), wantBasisReported)
	wantStoredCost(t, readStored(t, l, "cp"), fp(0.25), wantBasisReported)

	since := now.Add(-time.Hour)
	rows, err := l.Summary(ctx, since, "account")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("summary rows %+v", rows)
	}
	if s := rows[0]; s.Requests != 2 || s.CostUSD == nil || *s.CostUSD != 0.25 ||
		s.UnknownUsageRequests != 2 || s.UnpricedRequests != 0 {
		t.Fatalf("summary %+v, want 2 requests, cost 0.25, unknown 2, unpriced 0", rows[0])
	}

	dayRows, err := l.Summary(ctx, since, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(dayRows) != 1 || dayRows[0].CostUSD == nil || *dayRows[0].CostUSD != 0.25 ||
		dayRows[0].UnknownUsageRequests != 2 || dayRows[0].UnpricedRequests != 0 {
		t.Fatalf("day summary %+v, want cost 0.25, unknown 2, unpriced 0", dayRows)
	}

	res, err := l.Analytics(ctx, core.AnalyticsQuery{
		From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: "day", Group: "account",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Totals.Requests != 2 || res.Totals.CostUSD == nil || *res.Totals.CostUSD != 0.25 ||
		res.Totals.UnknownUsageRequests != 2 || res.Totals.UnpricedRequests != 0 {
		t.Fatalf("analytics totals %+v, want 2 requests, cost 0.25, unknown 2, unpriced 0", res.Totals)
	}
}
