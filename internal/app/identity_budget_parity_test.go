package app

// Budget report/enforcement parity in multi-user mode, through the real
// App.Handler: one owned static user key charges all six buckets (client,
// account and user, day and month) in the generation's Gate, the control
// report of that same generation shows exactly those six instances with the
// ceilings the Gate enforces, and a reload that lowers the per-user default is
// enforced and reported by the one new generation.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/hpst3r/localrouter/internal/weblogin"
)

type parityRow struct {
	Scope, Key, Period string
	Reported           int64  `json:"reported_micros"`
	Estimated          int64  `json:"estimated_micros"`
	Unknown            int64  `json:"unknown_micros"`
	Reserved           int64  `json:"reserved_micros"`
	Limit              *int64 `json:"limit_micros"`
}

func (r parityRow) spent() int64 { return r.Reported + r.Estimated + r.Unknown }

type parityDoc struct {
	Enabled bool        `json:"enabled"`
	Reserve int64       `json:"reserve_micros"`
	Rows    []parityRow `json:"rows"`
}

// budgetRows reads one (scope,key) through the service key, as an operator
// would, and returns its day and month rows.
func budgetRows(t *testing.T, a *App, scope, key string) map[string]parityRow {
	t.Helper()
	q := url.Values{"scope": {scope}, "key": {key}}
	w := serve(a, "GET", "/control/v1/budgets?"+q.Encode(), "", withBearer(svcStaticKey))
	if w.Code != http.StatusOK {
		t.Fatalf("budgets %s/%s: %d %s", scope, key, w.Code, w.Body)
	}
	var d parityDoc
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if !d.Enabled || d.Reserve != 10_000 {
		t.Fatalf("budgets %s/%s: enabled=%v reserve=%d", scope, key, d.Enabled, d.Reserve)
	}
	out := map[string]parityRow{}
	for _, r := range d.Rows {
		if r.Scope != scope || r.Key != key {
			t.Fatalf("budgets %s/%s returned row %+v", scope, key, r)
		}
		out[r.Period] = r
	}
	if len(out) != 2 {
		t.Fatalf("budgets %s/%s: rows %+v, want day and month", scope, key, d.Rows)
	}
	return out
}

func micros(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func TestMultiUserBudgetSixBucketParityAndReloadedUserDefault(t *testing.T) {
	clock := newIDClock()
	up := newFakeUpstream(t, false)
	f := newIDFixture(t)
	f.upstream = up.srv.URL
	f.budgets = "budgets:\n  reserve_usd: \"0.01\"\n  users: {daily_usd: \"3\", monthly_usd: \"30\"}\n"
	a := f.build(Overrides{IdentityClock: clock})
	uid, userKey, _ := seedUser(t, a, clock, "uma", weblogin.RoleUser)

	// Live reload: an owned static key for uma with its own client ceilings,
	// account ceilings, and the same user default.
	bcWrite(t, filepath.Join(f.dir, "uma.key"), "uma-static-key-000000000000")
	f.clients = "  - {name: uma-cli, class: interactive, key_file: " + filepath.Join(f.dir, "uma.key") + ", role: user, owner: " + uid + "}\n"
	userBudget := func(daily string) string {
		return "budgets:\n  reserve_usd: \"0.01\"\n" +
			"  clients:\n    uma-cli: {daily_usd: \"1\", monthly_usd: \"10\"}\n" +
			"  accounts:\n    acct: {daily_usd: \"2\", monthly_usd: \"20\"}\n" +
			"  users: {daily_usd: \"" + daily + "\", monthly_usd: \"30\"}\n"
	}
	f.budgets = userBudget("3")
	if _, err := a.ReloadConfig(f.load()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if w := serve(a, "POST", "/v1/chat/completions", chatBody, withBearer("uma-static-key-000000000000")); w.Code != http.StatusOK {
		t.Fatalf("owned static key: %d %s", w.Code, w.Body)
	}
	want := map[string][2]int64{ // scope -> {day, month} ceilings
		"client":  {1_000_000, 10_000_000},
		"account": {2_000_000, 20_000_000},
		"user":    {3_000_000, 30_000_000},
	}
	keys := map[string]string{"client": "uma-cli", "account": "acct", "user": uid}
	var spent int64 = -1
	for _, scope := range []string{"client", "account", "user"} {
		rows := budgetRows(t, a, scope, keys[scope])
		for i, period := range []string{"day", "month"} {
			r := rows[period]
			if micros(r.Limit) != want[scope][i] {
				t.Fatalf("%s/%s limit = %d, want %d", scope, period, micros(r.Limit), want[scope][i])
			}
			if r.Reserved != 0 || r.spent() <= 0 {
				t.Fatalf("%s/%s after one settled request: %+v", scope, period, r)
			}
			if spent == -1 {
				spent = r.spent()
			}
			if r.spent() != spent {
				t.Fatalf("%s/%s spent %d, want the same %d in all six buckets", scope, period, r.spent(), spent)
			}
		}
	}
	hits := up.count()

	// Lower the user default below the fixed hold: the next generation both
	// reports and enforces it, for every key of the user.
	f.budgets = userBudget("0.005")
	if _, err := a.ReloadConfig(f.load()); err != nil {
		t.Fatalf("reload lowering the user default: %v", err)
	}
	for _, key := range []string{"uma-static-key-000000000000", userKey} {
		if w := serve(a, "POST", "/v1/chat/completions", chatBody, withBearer(key)); w.Code != http.StatusTooManyRequests {
			t.Fatalf("over the lowered user default: %d %s", w.Code, w.Body)
		}
	}
	if up.count() != hits {
		t.Fatal("a budget-denied attempt reached the upstream")
	}
	day := budgetRows(t, a, "user", uid)["day"]
	if micros(day.Limit) != 5_000 || day.spent() != spent || day.Reserved != 0 {
		t.Fatalf("user/day after the lowering reload: %+v, want limit 5000 spent %d", day, spent)
	}
	src := a.current.Load().budgets.(*budgetReport)
	if got := src.UserLimits(uid); len(got) != 2 || got[0].Micros != 5_000 || got[1].Micros != 30_000_000 {
		t.Fatalf("generation UserLimits = %+v", got)
	}
	for _, scope := range []string{"client", "account"} {
		if r := budgetRows(t, a, scope, keys[scope])["day"]; r.spent() != spent || r.Reserved != 0 {
			t.Fatalf("%s/day changed by a denied attempt: %+v", scope, r)
		}
	}
	snap, err := src.Snapshot(context.Background(), "user", uid, "day", a.reloadClock.Now())
	if err != nil || snap.Reserved != 0 {
		t.Fatalf("denied attempts left a hold: %+v %v", snap, err)
	}

	// Raising it again admits the user in the next generation.
	f.budgets = userBudget("3")
	if _, err := a.ReloadConfig(f.load()); err != nil {
		t.Fatal(err)
	}
	if w := serve(a, "POST", "/v1/chat/completions", chatBody, withBearer(userKey)); w.Code != http.StatusOK {
		t.Fatalf("after raising the user default: %d %s", w.Code, w.Body)
	}
}
