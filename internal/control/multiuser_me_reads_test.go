package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/budget"
)

func TestSessionMeReadsAreOwnerScoped(t *testing.T) {
	f := newMUFixture(t, nil)
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")
	root := f.addUser("root", "admin")
	seedTwoOwners(f, alice, bob)

	rec := f.session(http.MethodGet, "/ui/v1/me/usage?group=model", alice, "")
	wantStatus(t, rec, http.StatusOK)
	if got := usageKeys(t, rec); len(got) != 1 || got["gpt-alice"] != 2 {
		t.Fatalf("alice me/usage = %v", got)
	}
	wantStatus(t, f.session(http.MethodGet, "/ui/v1/me/usage?group=user", alice, ""), http.StatusBadRequest)

	rec = f.session(http.MethodGet, "/ui/v1/me/analytics?range=24h&group=model", alice, "")
	wantStatus(t, rec, http.StatusOK)
	if res := decodeAnalytics(t, rec.Body.Bytes()); res.Totals.Requests != 2 {
		t.Fatalf("alice me/analytics totals = %+v", res.Totals)
	}
	rec = f.session(http.MethodGet, "/ui/v1/me/analytics/dimensions?range=24h", alice, "")
	wantStatus(t, rec, http.StatusOK)
	for _, needle := range []string{bob.ID, bob.KeyID, "gpt-bob", "legacy-client"} {
		if strings.Contains(rec.Body.String(), needle) {
			t.Fatalf("me dimensions leaked %q", needle)
		}
	}
	// An admin's /me view is their own (empty) data, not global.
	rec = f.session(http.MethodGet, "/ui/v1/me/usage?group=model", root, "")
	wantStatus(t, rec, http.StatusOK)
	if got := usageKeys(t, rec); len(got) != 0 {
		t.Fatalf("admin me/usage = %v, want own (none)", got)
	}
	// Query parameters cannot select another owner.
	rec = f.session(http.MethodGet, "/ui/v1/me/usage?group=model&user="+bob.ID+"&scope=all", alice, "")
	wantStatus(t, rec, http.StatusOK)
	if got := usageKeys(t, rec); got["gpt-bob"] != 0 {
		t.Fatalf("query override widened scope: %v", got)
	}
}

func TestSessionMeBudget(t *testing.T) {
	src := &userBudgetSource{admitBudgetSource: &admitBudgetSource{hold: 10, snaps: map[[3]string]budget.Snapshot{}}}
	f := newMUFixture(t, func(d *Deps) { d.Budgets = src })
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")
	src.perUser = map[string][]budget.Limit{
		alice.ID: {{Scope: budget.ScopeUser, Key: alice.ID, Period: budget.PeriodMonth, Micros: 5_000_000}},
		bob.ID:   {{Scope: budget.ScopeUser, Key: bob.ID, Period: budget.PeriodMonth, Micros: 1}},
	}
	src.snaps[[3]string{budget.ScopeUser, alice.ID, budget.PeriodMonth}] = budget.Snapshot{Reported: 1_000_000, Reserved: 10}

	rec := f.session(http.MethodGet, "/ui/v1/me/budget", alice, "")
	wantStatus(t, rec, http.StatusOK)
	doc := decodeBudgetDoc(t, rec)
	if !doc.Enabled || len(doc.Rows) != 2 {
		t.Fatalf("me/budget = %+v", doc)
	}
	for _, row := range doc.Rows {
		if row.Scope != budget.ScopeUser || row.Key != alice.ID {
			t.Fatalf("row names another identity: %+v", row)
		}
		switch row.Period {
		case budget.PeriodDay:
			if row.LimitMicros != nil {
				t.Fatalf("day row has a limit: %+v", row)
			}
		case budget.PeriodMonth:
			if row.LimitMicros == nil || *row.LimitMicros != 5_000_000 || *row.AvailableMicros != 3_999_990 {
				t.Fatalf("month row = %+v", row)
			}
		}
	}
	if strings.Contains(rec.Body.String(), bob.ID) {
		t.Fatal("bob's budget leaked")
	}
	// Query parameters are ignored: the key is always the session user.
	rec = f.session(http.MethodGet, "/ui/v1/me/budget?scope=user&key="+bob.ID, alice, "")
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), bob.ID) {
		t.Fatal("query selected bob's budget")
	}
	for _, k := range src.readKeys() {
		if strings.Contains(k, bob.ID) {
			t.Fatalf("store read for bob: %v", src.readKeys())
		}
	}

	// Without spend controls the doc is the disabled shape.
	g := newMUFixture(t, nil)
	carol := g.addUser("carol", "user")
	rec = g.session(http.MethodGet, "/ui/v1/me/budget", carol, "")
	wantStatus(t, rec, http.StatusOK)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["enabled"] != false || len(m) != 2 {
		t.Fatalf("disabled me/budget = %v", m)
	}
}
