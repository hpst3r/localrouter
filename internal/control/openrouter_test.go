package control

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func f64(v float64) *float64 { return &v }

func orStatus(t *testing.T, snaps map[string]core.Snapshot, ids ...string) map[string]map[string]any {
	t.Helper()
	var accts []core.Account
	for _, id := range ids {
		accts = append(accts, core.Account{ID: id, Provider: core.ProviderOpenRouter})
	}
	srv := New(Deps{Accounts: accts, Quota: &fakeQuota{snaps: snaps}, Policy: &fakePolicy{}, Clock: fakeClock{t0}}, Options{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/control/v1/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, a := range doc.Accounts {
		out[a["id"].(string)] = a
	}
	return out
}

func num(t *testing.T, v any) float64 {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("not a number: %#v", v)
	}
	f, _ := n.Float64()
	return f
}

func TestStatusOpenRouterSignedBalance(t *testing.T) {
	fetched := t0.Add(-90 * time.Second)
	st := orStatus(t, map[string]core.Snapshot{"or": {
		AccountID: "or", FetchedAt: fetched, Source: "usage_api",
		Credits: &core.Credits{TotalCreditsUSD: 770.8176, TotalUsageUSD: 770.893717902, BalanceUSD: 770.8176 - 770.893717902, FetchedAt: fetched},
		Key: &core.KeyUsage{UsageUSD: 770.89, UsageDailyUSD: f64(1.25), UsageWeeklyUSD: f64(7.5), UsageMonthlyUSD: f64(30.25),
			BYOKUsageUSD: f64(2), FetchedAt: fetched},
	}}, "or")["or"]
	c, ok := st["credits"].(map[string]any)
	if !ok {
		t.Fatalf("no credits object: %v", st)
	}
	if b := num(t, c["balance_usd"]); math.Abs(b-(-0.076117902)) > 1e-9 {
		t.Errorf("balance_usd = %v, want signed -0.076117902", b)
	}
	if num(t, c["total_credits_usd"]) != 770.8176 || num(t, c["total_usage_usd"]) != 770.893717902 {
		t.Errorf("totals = %v", c)
	}
	if c["available"] != true || c["exhausted"] != true || num(t, c["age_s"]) != 90 || c["error"] != nil {
		t.Errorf("credits = %v", c)
	}
	k := st["key"].(map[string]any)
	if k["available"] != true || k["unlimited"] != true || k["limit_usd"] != nil || k["limit_remaining_usd"] != nil || k["exhausted"] != false {
		t.Errorf("unlimited key = %v", k)
	}
	if num(t, k["usage_usd"]) != 770.89 || num(t, k["usage_daily_usd"]) != 1.25 || num(t, k["byok_usage_usd"]) != 2 || k["byok_usage_daily_usd"] != nil {
		t.Errorf("key usage = %v", k)
	}
	if ws := st["windows"].([]any); len(ws) != 0 {
		t.Errorf("no fake windows: %v", ws)
	}
}

func TestStatusOpenRouterUnavailableAndCaps(t *testing.T) {
	st := orStatus(t, map[string]core.Snapshot{
		"noperm": {
			AccountID: "noperm", FetchedAt: t0, Err: "credits: management key required (http 403); balance unavailable",
			Credits: &core.Credits{Err: "credits: management key required (http 403); balance unavailable"},
			Key:     &core.KeyUsage{LimitUSD: f64(0), LimitRemainingUSD: f64(0), UsageUSD: 3, FetchedAt: t0},
		},
		"daily": {
			AccountID: "daily", FetchedAt: t0,
			Credits: &core.Credits{TotalCreditsUSD: 20, TotalUsageUSD: 5, BalanceUSD: 15, FetchedAt: t0},
			Key: &core.KeyUsage{LimitUSD: f64(10), LimitRemainingUSD: f64(9), LimitReset: "daily",
				LimitResetAt: t0.Add(12 * time.Hour), UsageUSD: 500, FetchedAt: t0},
		},
	}, "noperm", "daily", "never")

	c := st["noperm"]["credits"].(map[string]any)
	if c["available"] != false || c["balance_usd"] != nil || c["total_credits_usd"] != nil || c["exhausted"] != false || c["age_s"] != nil {
		t.Errorf("unavailable balance must be null, not zero: %v", c)
	}
	if e, _ := c["error"].(string); !strings.Contains(e, "management key required") {
		t.Errorf("error = %v", c["error"])
	}
	if st["noperm"]["healthy"] != false {
		t.Error("unavailable balance must not claim full health")
	}
	k := st["noperm"]["key"].(map[string]any)
	if k["unlimited"] != false || num(t, k["limit_usd"]) != 0 || num(t, k["limit_remaining_usd"]) != 0 || k["exhausted"] != true {
		t.Errorf("zero cap must be explicit and exhausted: %v", k)
	}

	k = st["daily"]["key"].(map[string]any)
	if num(t, k["limit_remaining_usd"]) != 9 || k["limit_reset"] != "daily" || k["exhausted"] != false || k["limit_reset_at"] != "2026-10-02T00:00:00Z" {
		t.Errorf("daily key = %v", k)
	}
	if c := st["daily"]["credits"].(map[string]any); num(t, c["balance_usd"]) != 15 || c["exhausted"] != false {
		t.Errorf("daily credits = %v", c)
	}

	// Never fetched: both parts present and explicitly unavailable.
	for _, part := range []string{"credits", "key"} {
		p, ok := st["never"][part].(map[string]any)
		if !ok || p["available"] != false {
			t.Errorf("never-fetched %s = %v", part, st["never"][part])
		}
	}
	if c := st["never"]["credits"].(map[string]any); c["balance_usd"] != nil {
		t.Errorf("never-fetched balance = %v", c["balance_usd"])
	}
}

// Other providers do not grow credits/key objects.
func TestStatusNonOpenRouterUnchanged(t *testing.T) {
	f := newFixture(false)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/control/v1/status", nil))
	if strings.Contains(rec.Body.String(), `"credits"`) || strings.Contains(rec.Body.String(), `"key"`) {
		t.Errorf("status grew openrouter fields for other providers: %s", rec.Body.String())
	}
}
