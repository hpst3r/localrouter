package ledger

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLocalPricingOverridesAndAliases(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "pricing.yaml")
	writeFile(t, base, "models:\n  zai/glm-5.3: {input: 1.4, cached_input: 0.26, output: 4.4}\n  x: {input: 1, output: 2}\n")
	if got := LocalPricingPath(base); got != filepath.Join(dir, "pricing.local.yaml") {
		t.Fatalf("LocalPricingPath = %s", got)
	}
	writeFile(t, LocalPricingPath(base), "models:\n  x: {input: 10, output: 20}\naliases:\n  glm-5.3: ZAI/GLM-5.3\n")
	p, err := LoadPricing(base)
	if err != nil {
		t.Fatal(err)
	}
	if mp, ok := p.Lookup("x"); !ok || mp.Input != 10 {
		t.Fatalf("override not applied: %+v", mp)
	}
	mp, ok := p.Lookup("glm-5.3")
	if !ok || mp.Input != 1.4 || mp.Output != 4.4 {
		t.Fatalf("alias not resolved: %+v %v", mp, ok)
	}
}

func TestLocalPricingAliasToUnpricedFails(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "pricing.yaml")
	writeFile(t, LocalPricingPath(base), "aliases:\n  a: nope\n")
	if _, err := LoadPricing(base); err == nil || !strings.Contains(err.Error(), "not priced") {
		t.Fatalf("want not priced error, got %v", err)
	}
}

func TestAliasesRejectedInImportedFile(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "pricing.yaml")
	writeFile(t, base, "models:\n  a: {input: 1, output: 1}\naliases:\n  b: a\n")
	if _, err := LoadPricing(base); err == nil {
		t.Fatal("aliases in the imported file must be rejected")
	}
}

func TestReprice(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "l.db")
	l, err := Open(db, nil, func(string) string { return "api_equivalent" })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	for _, r := range []core.RequestRecord{
		{ID: "a", StartedAt: now, Model: "m", AccountID: "acc", UsageKnown: true,
			Usage: core.Usage{InputTokens: 1000, CachedInputTokens: 200, OutputTokens: 100}},
		{ID: "b", StartedAt: now, Model: "unpriced", UsageKnown: true, Usage: core.Usage{InputTokens: 5}},
		{ID: "c", StartedAt: now, Model: "m", UsageKnown: false},
	} {
		if err := l.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	cached := 0.5
	l, err = Open(db, NewPricing(map[string]ModelPrice{"m": {Input: 2, CachedInput: &cached, Output: 10}}),
		func(string) string { return "api_equivalent" })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	n, err := l.Reprice(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Reprice = %d, %v", n, err)
	}
	rows, err := l.Summary(ctx, now.Add(-time.Hour), "model")
	if err != nil {
		t.Fatal(err)
	}
	want := (800*2 + 200*0.5 + 100*10) / 1e6
	for _, r := range rows {
		switch r.Key {
		case "m":
			if r.CostUSD == nil || math.Abs(*r.CostUSD-want) > 1e-12 {
				t.Fatalf("m cost = %v want %v", r.CostUSD, want)
			}
		case "unpriced":
			if r.CostUSD != nil {
				t.Fatalf("unpriced got cost %v", *r.CostUSD)
			}
		}
	}
}
