package ledger

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Test prices are arbitrary fixtures, not real model prices.
func testPricing() *Pricing {
	half := 0.5
	return NewPricing(map[string]ModelPrice{
		"model-a": {Input: 2, CachedInput: &half, Output: 10},
		"Model-B": {Input: 1, Output: 4},
	})
}

func basisFn(id string) string {
	if id == "sub" {
		return "api_equivalent"
	}
	return "metered"
}

func openTest(t *testing.T, p *Pricing) (*Ledger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "localrouter.db")
	l, err := Open(path, p, basisFn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestSchemaHasNoContentColumns(t *testing.T) {
	l, path := openTest(t, nil)
	rows, err := l.db.Query(`SELECT name FROM pragma_table_info('requests') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, n)
	}
	want := []string{"id", "started_at", "finished_at", "client", "class", "route", "model",
		"provider", "account_id", "upstream_identity", "status", "failover_of", "input_tokens",
		"cached_input_tokens", "output_tokens", "reasoning_tokens", "usage_known", "cost_usd",
		"cost_basis", "latency_ms", "bytes_out", "session", "task", "agent", "error"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("columns = %v\nwant %v", cols, want)
	}

	var tables []string
	trows, err := l.db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer trows.Close()
	for trows.Next() {
		var n string
		trows.Scan(&n)
		tables = append(tables, n)
	}
	if !reflect.DeepEqual(tables, []string{"requests", "schema_version"}) {
		t.Fatalf("tables = %v", tables)
	}

	var mode string
	l.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %q", mode)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("db mode = %v", st.Mode().Perm())
	}
	dst, _ := os.Stat(filepath.Dir(path))
	if dst.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v", dst.Mode().Perm())
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	l, path := openTest(t, nil)
	if err := l.Record(context.Background(), core.RequestRecord{ID: "x", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	l2, err := Open(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	var n, v int
	l2.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
	l2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if n != 1 || v != len(migrations) {
		t.Fatalf("rows=%d version=%d", n, v)
	}
}

func TestCostMath(t *testing.T) {
	p := testPricing()
	u := core.Usage{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 200_000, ReasoningTokens: 50_000}
	// (1e6-4e5)*2 + 4e5*0.5 + 2e5*10, /1e6 = 1.2 + 0.2 + 2.0
	c, ok := p.Cost("model-a", u)
	if !ok || !approx(c, 3.4) {
		t.Fatalf("cost = %v %v", c, ok)
	}
	// cached_input missing => input rate; lowercase match.
	c, ok = p.Cost("model-b", u)
	if !ok || !approx(c, 1.0+0.8) {
		t.Fatalf("cost model-b = %v %v", c, ok)
	}
	if _, ok := p.Cost("model-c", u); ok {
		t.Fatal("unknown model priced")
	}
	if _, ok := p.Cost("model", u); ok {
		t.Fatal("prefix matched")
	}
	var nilP *Pricing
	if _, ok := nilP.Cost("model-a", u); ok {
		t.Fatal("nil pricing priced")
	}
}

func TestRecordCostAndNull(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	now := time.Now()
	u := core.Usage{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 200_000}
	recs := []core.RequestRecord{
		{ID: "priced", StartedAt: now, Model: "model-a", AccountID: "sub", Usage: u, UsageKnown: true},
		{ID: "metered", StartedAt: now, Model: "model-a", AccountID: "key", Usage: u, UsageKnown: true},
		{ID: "unpriced", StartedAt: now, Model: "mystery", AccountID: "sub", Usage: u, UsageKnown: true},
		{ID: "unknown", StartedAt: now, Model: "model-a", AccountID: "sub"},
	}
	for _, r := range recs {
		if err := l.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	check := func(id string, wantCost *float64, wantBasis string) {
		t.Helper()
		var cost *float64
		var basis *string
		if err := l.db.QueryRow(`SELECT cost_usd, cost_basis FROM requests WHERE id=?`, id).Scan(&cost, &basis); err != nil {
			t.Fatal(err)
		}
		if (cost == nil) != (wantCost == nil) || (cost != nil && !approx(*cost, *wantCost)) {
			t.Errorf("%s cost = %v want %v", id, cost, wantCost)
		}
		gotBasis := ""
		if basis != nil {
			gotBasis = *basis
		}
		if gotBasis != wantBasis {
			t.Errorf("%s basis = %q want %q", id, gotBasis, wantBasis)
		}
	}
	c := 3.4
	check("priced", &c, "api_equivalent")
	check("metered", &c, "metered")
	check("unpriced", nil, "")
	check("unknown", nil, "")
}

func TestSummary(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	now := time.Now()
	day1 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)
	day2 := day1.Add(24 * time.Hour)
	u := core.Usage{InputTokens: 1000, CachedInputTokens: 100, OutputTokens: 50, ReasoningTokens: 10}
	recs := []core.RequestRecord{
		{ID: "1", StartedAt: day1, AccountID: "b", Model: "model-a", Class: core.ClassBackground, Client: "c1", Route: "r", Usage: u, UsageKnown: true},
		{ID: "2", StartedAt: day1, AccountID: "b", Model: "mystery", Class: core.ClassBackground, Client: "c1", Route: "r", Usage: u, UsageKnown: true},
		{ID: "3", StartedAt: day2, AccountID: "a", Model: "model-a", Class: core.ClassInteractive, Client: "c2", Route: "r", UsageKnown: false},
		{ID: "4", StartedAt: day2, AccountID: "a", Model: "mystery", Class: core.ClassInteractive, Client: "c2", Route: "r", Usage: u, UsageKnown: true},
		{ID: "old", StartedAt: day1.Add(-48 * time.Hour), AccountID: "a", Model: "model-a", Usage: u, UsageKnown: true},
	}
	for _, r := range recs {
		if err := l.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	since := day1.Add(-time.Hour)
	costA, _ := testPricing().Cost("model-a", u)

	rows, err := l.Summary(ctx, since, "account")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Key != "a" || rows[1].Key != "b" {
		t.Fatalf("rows = %+v", rows)
	}
	a, b := rows[0], rows[1]
	if a.Requests != 2 || a.UnknownUsageRequests != 1 || a.UnpricedRequests != 1 || a.CostUSD != nil {
		t.Errorf("a = %+v", a)
	}
	if a.InputTokens != 1000 || a.OutputTokens != 50 || a.ReasoningTokens != 10 || a.CachedInputTokens != 100 {
		t.Errorf("a tokens = %+v", a)
	}
	if b.Requests != 2 || b.UnknownUsageRequests != 0 || b.UnpricedRequests != 1 || b.CostUSD == nil || !approx(*b.CostUSD, costA) {
		t.Errorf("b = %+v", b)
	}
	if b.InputTokens != 2000 {
		t.Errorf("b input = %d", b.InputTokens)
	}

	for group, keys := range map[string][]string{
		"model":  {"model-a", "mystery"},
		"class":  {"background", "interactive"},
		"client": {"c1", "c2"},
		"route":  {"r"},
		"day":    {day1.Format(time.DateOnly), day2.Format(time.DateOnly)},
	} {
		rows, err := l.Summary(ctx, since, group)
		if err != nil {
			t.Fatal(group, err)
		}
		var got []string
		var total int64
		for _, r := range rows {
			got = append(got, r.Key)
			total += r.Requests
		}
		if !reflect.DeepEqual(got, keys) || total != 4 {
			t.Errorf("%s keys=%v total=%d", group, got, total)
		}
	}

	// Day and account aggregation agree on counts and cost.
	days, _ := l.Summary(ctx, since, "day")
	if d := days[0]; d.Requests != 2 || d.UnpricedRequests != 1 || d.CostUSD == nil || !approx(*d.CostUSD, costA) {
		t.Errorf("day1 = %+v", d)
	}
	if d := days[1]; d.UnknownUsageRequests != 1 || d.UnpricedRequests != 1 || d.CostUSD != nil {
		t.Errorf("day2 = %+v", d)
	}

	if _, err := l.Summary(ctx, since, "prompt; DROP TABLE requests"); err == nil {
		t.Fatal("unknown group accepted")
	}
	rows, err = l.Summary(ctx, now.Add(time.Hour), "account")
	if err != nil || len(rows) != 0 {
		t.Fatalf("future since: %v %v", rows, err)
	}
}

func TestConcurrentRecord(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- l.Record(ctx, core.RequestRecord{
				ID: fmt.Sprintf("r%d", i), StartedAt: time.Now(), Model: "model-a", AccountID: "sub",
				Usage: core.Usage{InputTokens: 10, OutputTokens: 1}, UsageKnown: true,
			})
			if _, err := l.Summary(ctx, time.Time{}, "account"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := l.Summary(ctx, time.Time{}, "account")
	if err != nil || len(rows) != 1 || rows[0].Requests != 50 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestRecordGeneratesID(t *testing.T) {
	l, _ := openTest(t, nil)
	for range 2 {
		if err := l.Record(context.Background(), core.RequestRecord{StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	l.db.QueryRow(`SELECT COUNT(DISTINCT id) FROM requests WHERE id != ''`).Scan(&n)
	if n != 2 {
		t.Fatalf("distinct ids = %d", n)
	}
}

const litellmFixture = `{
  "sample_spec": {"input_cost_per_token": "price per token", "max_tokens": "n"},
  "model-x": {"input_cost_per_token": 0.000002, "output_cost_per_token": 0.00001, "cache_read_input_token_cost": 0.0000005},
  "prov/model-y": {"input_cost_per_token": 0.000001, "output_cost_per_token": 0.000004},
  "other/model-x": {"input_cost_per_token": 0.000009, "output_cost_per_token": 0.000009},
  "p1/model-z": {"input_cost_per_token": 0.000001, "output_cost_per_token": 0.000002},
  "p2/model-z": {"input_cost_per_token": 0.000003, "output_cost_per_token": 0.000002},
  "embed-only": {"input_cost_per_token": 0.0000001},
  "image-gen": {"output_cost_per_pixel": 0.01}
}`

func TestImportLiteLLM(t *testing.T) {
	m, err := ImportLiteLLM(strings.NewReader(litellmFixture))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	for _, k := range []string{"model-x", "prov/model-y", "model-y", "other/model-x", "p1/model-z", "p2/model-z"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q (have %v)", k, keys)
		}
	}
	for _, k := range []string{"sample_spec", "embed-only", "image-gen", "model-z"} {
		if _, ok := m[k]; ok {
			t.Errorf("unexpected %q", k)
		}
	}
	x := m["model-x"]
	if !approx(x.Input, 2) || !approx(x.Output, 10) || x.CachedInput == nil || !approx(*x.CachedInput, 0.5) {
		t.Errorf("model-x = %+v", x)
	}
	if y := m["model-y"]; !approx(y.Input, 1) || !approx(y.Output, 4) || y.CachedInput != nil {
		t.Errorf("model-y = %+v", y)
	}
	if _, err := ImportLiteLLM(strings.NewReader("not json")); err == nil {
		t.Error("bad json accepted")
	}
}

func TestPricingRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "pricing.yaml")

	p, err := LoadPricing(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Cost("anything", core.Usage{InputTokens: 1}); ok {
		t.Fatal("missing file should be empty pricing")
	}

	m, err := ImportLiteLLM(strings.NewReader(litellmFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePricing(path, m); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "models:\n") || strings.Index(string(b), "model-x:") > strings.Index(string(b), "model-y:") {
		t.Errorf("unsorted or malformed yaml:\n%s", b)
	}
	if strings.Contains(string(b), "model-y:\n        input: 1\n        cached_input") {
		t.Error("absent cached_input written")
	}
	p, err = LoadPricing(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range m {
		got, ok := p.Lookup(name)
		if !ok || !samePrice(got, want) {
			t.Errorf("%s: got %+v want %+v", name, got, want)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("leftover temp files: %v", entries)
	}

	if err := os.WriteFile(path, []byte("models:\n  m: {input: -1, output: 1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPricing(path); err == nil {
		t.Error("negative price accepted")
	}
	if err := os.WriteFile(path, []byte("models: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPricing(path); err == nil {
		t.Error("bad yaml accepted")
	}
}
