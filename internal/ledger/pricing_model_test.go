package ledger

// Backend attribution / pricing tests (schema v3 -> v4).
//
// The contract: a capability-constrained route (per-candidate Upstreams) records
// PricingModel = the backend it actually resolved, and the ledger prices such a
// row by PricingModel when it is set, else by Model. A legacy/unconstrained route
// records no PricingModel and keeps the exact legacy behaviour. A constrained
// route never falls back from an unpriced backend to the client alias: the row
// stays honestly unpriced (NULL cost). Schema v4 adds nullable upstream_model and
// pricing_model columns; every pre-v4 row keeps NULL. Reprice and a
// generation-pinned PricingView must use the same key as the original insert.

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// v3Schema is schema version 3 (v2 + host), frozen here so the v3 -> v4 upgrade
// path stays tested independently of migrations[0..2]. The row leaves
// upstream_model/pricing_model absent (the columns do not exist yet).
const v3Schema = `CREATE TABLE requests (
	id TEXT PRIMARY KEY, started_at INTEGER NOT NULL, finished_at INTEGER,
	client TEXT NOT NULL, class TEXT NOT NULL, route TEXT NOT NULL,
	model TEXT NOT NULL, provider TEXT NOT NULL, account_id TEXT NOT NULL,
	upstream_identity TEXT NOT NULL, status INTEGER NOT NULL, failover_of TEXT,
	input_tokens INTEGER NOT NULL, cached_input_tokens INTEGER NOT NULL,
	output_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL,
	usage_known INTEGER NOT NULL, cost_usd REAL, cost_basis TEXT,
	latency_ms INTEGER NOT NULL, bytes_out INTEGER NOT NULL, session TEXT NOT NULL,
	task TEXT NOT NULL, agent TEXT NOT NULL, error TEXT NOT NULL,
	cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
	host TEXT NOT NULL DEFAULT ''
);
CREATE INDEX requests_started_at ON requests(started_at);
CREATE INDEX requests_account_id ON requests(account_id);
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version (version) VALUES (3);
INSERT INTO requests VALUES ('old', 1, NULL, 'c', 'background', 'r', 'gpt-x', 'p',
	'a', '', 200, NULL, 10, 2, 3, 0, 1, NULL, NULL, 5, 0, '', '', '', '', 7, 'vm1');`

// attrPricing keys the client alias and the backends at deliberately distinct
// prices so a mis-keyed row is impossible to miss (USD per 1M tokens, so a 1M
// input usage costs exactly ModelPrice.Input).
//
//	alias gpt-x :  1
//	backend-a   : 10
//	backend-b   : 100
func attrPricing() *Pricing {
	return NewPricing(map[string]ModelPrice{
		"gpt-x":     {Input: 1, Output: 2},
		"backend-a": {Input: 10, Output: 20},
		"backend-b": {Input: 100, Output: 200},
	})
}

// attrRow is a 1M-input record. PricingModel empty = a legacy/unconstrained row.
func attrRow(id, model, pricingModel string) core.RequestRecord {
	return core.RequestRecord{
		ID:           id,
		StartedAt:    time.Now(),
		Model:        model,
		PricingModel: pricingModel,
		AccountID:    "acc",
		UsageKnown:   true,
		Usage:        core.Usage{InputTokens: 1_000_000},
	}
}

// TestMigrateV3ToV4PreservesLegacyRows: opening a version-3 database upgrades it
// in place to version 4, adds BOTH nullable attribution columns, and leaves the
// pre-existing row's attribution NULL (no backfilled guess) while its data is
// untouched.
func TestMigrateV3ToV4PreservesLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "localrouter.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(v3Schema); err != nil {
		t.Fatal(err)
	}
	db.Close()

	l, err := Open(path, attrPricing(), func(string) string { return "api_equivalent" })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	var v int
	l.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != 5 || len(migrations) != 5 {
		t.Fatalf("version = %d, migrations = %d; want 5/5", v, len(migrations))
	}
	var in, creation int64
	var host string
	var upstream, pricing sql.NullString
	if err := l.db.QueryRow(`SELECT input_tokens, cache_creation_input_tokens, host,
		upstream_model, pricing_model FROM requests WHERE id='old'`).
		Scan(&in, &creation, &host, &upstream, &pricing); err != nil {
		t.Fatal(err)
	}
	if in != 10 || creation != 7 || host != "vm1" {
		t.Fatalf("legacy row damaged: in=%d creation=%d host=%q", in, creation, host)
	}
	if upstream.Valid || pricing.Valid {
		t.Fatalf("legacy attribution = %v/%v; want NULL/NULL (no backfill)", upstream, pricing)
	}

	// Both columns are writable after the upgrade and store the resolved backend.
	if err := l.Record(context.Background(), core.RequestRecord{
		ID: "new", StartedAt: time.UnixMilli(2), Model: "gpt-x", AccountID: "acc",
		UpstreamModel: "backend-a", PricingModel: "backend-a", UsageKnown: true,
		Usage: core.Usage{InputTokens: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	var nu, npr sql.NullString
	if err := l.db.QueryRow(`SELECT upstream_model, pricing_model FROM requests WHERE id='new'`).
		Scan(&nu, &npr); err != nil {
		t.Fatal(err)
	}
	if nu.String != "backend-a" || !nu.Valid || npr.String != "backend-a" || !npr.Valid {
		t.Fatalf("new row attribution = %v/%v", nu, npr)
	}
	// The legacy row is still NULL after a reopen (idempotent migration).
	l.Close()
	l2, err := Open(path, attrPricing(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	l2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != 5 {
		t.Fatalf("reopen version = %d; want 5", v)
	}
}

// TestPricingModelKeysCostToBackend: two constrained attempts serving the SAME
// client alias but resolving to different backends are costed at their own
// backend's price, not the alias's.
func TestPricingModelKeysCostToBackend(t *testing.T) {
	l, _ := openTest(t, attrPricing())
	ctx := context.Background()

	recs := []core.RequestRecord{
		attrRow("b-a", "gpt-x", "backend-a"),
		attrRow("b-b", "gpt-x", "backend-b"),
	}
	if err := l.RecordBatch(ctx, recs); err != nil {
		t.Fatal(err)
	}
	wantStoredCost(t, readStored(t, l, "b-a"), fp(10), "metered")
	wantStoredCost(t, readStored(t, l, "b-b"), fp(100), "metered")
	// The alias price itself (1) must not have been used for either.
	for _, id := range []string{"b-a", "b-b"} {
		if s := readStored(t, l, id); s.cost.Valid && s.cost.Float64 == 1 {
			t.Fatalf("%s costed at the client alias price", id)
		}
	}
}

// TestUnpricedBackendStaysUnpricedOnConstrainedRoute: on a constrained route an
// unpriced backend never falls back to the priced client alias — the row stays
// honestly unpriced (NULL cost, NULL basis).
func TestUnpricedBackendStaysUnpricedOnConstrainedRoute(t *testing.T) {
	l, _ := openTest(t, attrPricing())
	ctx := context.Background()

	if err := l.Record(ctx, attrRow("b-new", "gpt-x", "backend-new")); err != nil {
		t.Fatal(err)
	}
	wantStoredCost(t, readStored(t, l, "b-new"), nil, "")
}

// TestLegacyRowIgnoresUpstreamModelForPricing: a legacy/unconstrained row has no
// PricingModel even when UpstreamModel names a priced backend; pricing keys on
// Model exactly as before, so the legacy alias price is unchanged.
func TestLegacyRowIgnoresUpstreamModelForPricing(t *testing.T) {
	l, _ := openTest(t, attrPricing())
	ctx := context.Background()

	r := attrRow("legacy", "gpt-x", "")
	r.UpstreamModel = "backend-a" // priced at 10, but must be ignored for pricing
	if err := l.Record(ctx, r); err != nil {
		t.Fatal(err)
	}
	wantStoredCost(t, readStored(t, l, "legacy"), fp(1), "metered")

	// UpstreamModel persisted, PricingModel NULL.
	var up, pr sql.NullString
	if err := l.db.QueryRow(`SELECT upstream_model, pricing_model FROM requests WHERE id='legacy'`).Scan(&up, &pr); err != nil {
		t.Fatal(err)
	}
	if !up.Valid || up.String != "backend-a" {
		t.Fatalf("upstream_model = %v; want backend-a", up)
	}
	if pr.Valid {
		t.Fatalf("legacy pricing_model = %v; want NULL", pr)
	}
}

// TestRepriceUsesPricingModelKey: Reprice must recompute under the same key the
// row recorded — pricing_model when set, else model — so a constrained row
// reprices at its backend and a legacy row at its model, and an unpriced backend
// still stays unpriced.
func TestRepriceUsesPricingModelKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "l.db")
	ctx := context.Background()
	now := time.Now()

	l, err := Open(path, nil, func(string) string { return "api_equivalent" })
	if err != nil {
		t.Fatal(err)
	}
	recs := []core.RequestRecord{
		{ID: "k-backend", StartedAt: now, Model: "gpt-x", PricingModel: "backend-a",
			AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 1_000_000}},
		{ID: "k-legacy", StartedAt: now, Model: "gpt-x",
			AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 1_000_000}},
		{ID: "k-unpriced", StartedAt: now, Model: "gpt-x", PricingModel: "backend-z",
			AccountID: "acc", UsageKnown: true, Usage: core.Usage{InputTokens: 1_000_000}},
		// Reported provenance must survive Reprice untouched, key or no key.
		{ID: "k-reported", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "gpt-x",
			PricingModel: "backend-a", AccountID: "acc", ReportedCostUSD: fp(0.42)},
	}
	if err := l.RecordBatch(ctx, recs); err != nil {
		t.Fatal(err)
	}
	l.Close()

	// Import prices after the fact; backend-z stays unpriced.
	l, err = Open(path, NewPricing(map[string]ModelPrice{
		"gpt-x":     {Input: 1, Output: 2},
		"backend-a": {Input: 10, Output: 20},
	}), func(string) string { return "api_equivalent" })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	n, err := l.Reprice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 { // k-backend + k-legacy; k-unpriced has no price, k-reported is protected
		t.Fatalf("Reprice priced %d rows; want 2", n)
	}
	wantStoredCost(t, readStored(t, l, "k-backend"), fp(10), "api_equivalent")
	wantStoredCost(t, readStored(t, l, "k-legacy"), fp(1), "api_equivalent")
	wantStoredCost(t, readStored(t, l, "k-unpriced"), nil, "")
	wantStoredCost(t, readStored(t, l, "k-reported"), fp(0.42), wantBasisReported)
}

// TestReportedCostPrecedenceWithPricingModel: an OpenRouter reported cost still
// wins over PricingModel-derived pricing regardless of UsageKnown, and the
// PricingModel key is used for the table fallback when no reported cost exists.
func TestReportedCostPrecedenceWithPricingModel(t *testing.T) {
	l, _ := openTest(t, attrPricing())
	ctx := context.Background()
	now := time.Now()

	recs := []core.RequestRecord{
		{ID: "r-win", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "gpt-x",
			PricingModel: "backend-a", AccountID: "acc", ReportedCostUSD: fp(0.42)},
		{ID: "r-win-zero", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "gpt-x",
			PricingModel: "backend-a", AccountID: "acc", ReportedCostUSD: fp(0)},
		{ID: "r-fallback", StartedAt: now, Provider: core.ProviderOpenRouter, Model: "gpt-x",
			PricingModel: "backend-a", AccountID: "acc", UsageKnown: true,
			Usage: core.Usage{InputTokens: 1_000_000}},
	}
	if err := l.RecordBatch(ctx, recs); err != nil {
		t.Fatal(err)
	}
	wantStoredCost(t, readStored(t, l, "r-win"), fp(0.42), wantBasisReported)
	wantStoredCost(t, readStored(t, l, "r-win-zero"), fp(0), wantBasisReported)
	// No reported cost: priced by PricingModel (10), not the alias (1).
	wantStoredCost(t, readStored(t, l, "r-fallback"), fp(10), "metered")
}

// TestPricingViewUsesPricingModelKey: a generation-pinned PricingView must
// attribute cost with the same key, using its own frozen table — the backend
// price for a constrained row, the model price for a legacy row, never the
// client alias for an unpriced backend.
func TestPricingViewUsesPricingModelKey(t *testing.T) {
	l, _ := openTest(t, attrPricing()) // startup generation
	// Pin a distinct generation: model-a 8/40 (1M/1M -> 48), backend-a 1/1 (1M -> 1).
	v := l.newPricingView(NewPricing(map[string]ModelPrice{
		"model-a":   {Input: 8, Output: 40},
		"backend-a": {Input: 1, Output: 1},
	}), viewBasis)
	ctx := context.Background()

	legacy := viewRecord("pv-legacy") // Model model-a, no PricingModel
	if err := v.Record(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	// Constrained row: the client model (model-a) is priced in this generation,
	// but PricingModel backend-a must win.
	cons := viewRecord("pv-cons")
	cons.PricingModel = "backend-a"
	if err := v.Record(ctx, cons); err != nil {
		t.Fatal(err)
	}
	// Unpriced backend on a constrained row: stays unpriced, no alias fallback.
	unpriced := viewRecord("pv-unpriced")
	unpriced.PricingModel = "backend-new"
	if err := v.Record(ctx, unpriced); err != nil {
		t.Fatal(err)
	}

	wantStoredCost(t, readStored(t, v.Ledger, "pv-legacy"), fp(viewCostB), "view:acct")
	wantStoredCost(t, readStored(t, v.Ledger, "pv-cons"), fp(2), "view:acct")
	wantStoredCost(t, readStored(t, v.Ledger, "pv-unpriced"), nil, "")
}

// TestPricingModelJSONOmitEmpty: the wire form omits both attribution keys when
// empty (so legacy ingest is byte-identical) and carries them when set.
func TestPricingModelJSONOmitEmpty(t *testing.T) {
	b, err := json.Marshal(core.RequestRecord{ID: "x", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "pricing_model") || strings.Contains(string(b), "upstream_model") {
		t.Fatalf("empty attribution leaked into wire form: %s", b)
	}
	b, err = json.Marshal(core.RequestRecord{ID: "x", Model: "m",
		UpstreamModel: "backend-a", PricingModel: "backend-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"upstream_model":"backend-a"`) ||
		!strings.Contains(string(b), `"pricing_model":"backend-a"`) {
		t.Fatalf("attribution missing from wire form: %s", b)
	}
}
