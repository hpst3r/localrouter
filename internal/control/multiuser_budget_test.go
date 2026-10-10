package control

import (
	"net/http"
	"slices"
	"testing"

	"github.com/hpst3r/localrouter/internal/budget"
)

// userBudgetSource adds the optional per-user limits seam to the recording
// admit source.
type userBudgetSource struct {
	*admitBudgetSource
	perUser map[string][]budget.Limit
	asked   []string
}

func (s *userBudgetSource) UserLimits(userID string) []budget.Limit {
	s.mu.Lock()
	s.asked = append(s.asked, userID)
	s.mu.Unlock()
	return s.perUser[userID]
}

var _ UserBudgetSource = (*userBudgetSource)(nil)

func TestMultiUserAdmitEstimateChargesUserBuckets(t *testing.T) {
	src := &userBudgetSource{admitBudgetSource: &admitBudgetSource{hold: 10, snaps: map[[3]string]budget.Snapshot{}}}
	f := newMUFixture(t, func(d *Deps) { d.Budgets = src })
	alice := f.addUser("alice", "user")
	src.perUser = map[string][]budget.Limit{alice.ID: {
		{Scope: budget.ScopeUser, Key: alice.ID, Period: budget.PeriodDay, Micros: 5},
	}}

	rec := f.bearerPost("/control/v1/admit", alice.Key, `{"class":"interactive","model":"gpt"}`)
	wantStatus(t, rec, http.StatusOK)
	m := decodeMap(t, rec)
	b := budgetBlock(t, m)
	if b["client"] != alice.KeyID || b["allow"] != false || b["reason"] != admitBudgetExceeded {
		t.Fatalf("budget block = %v", b)
	}
	// A user bearer's estimate never reads the shared account scope.
	want := []string{
		"client|" + alice.KeyID + "|day", "client|" + alice.KeyID + "|month",
		"user|" + alice.ID + "|day", "user|" + alice.ID + "|month",
	}
	if got := src.readKeys(); !slices.Equal(got, want) {
		t.Fatalf("reads = %v, want %v", got, want)
	}
	if !slices.Equal(src.asked, []string{alice.ID}) {
		t.Fatalf("UserLimits asked for %v", src.asked)
	}
	if m["decision"] != "allow" {
		t.Fatalf("advisory budget changed the decision: %v", m)
	}
}

func TestMultiUserGlobalBudgetsUserScope(t *testing.T) {
	src := &userBudgetSource{admitBudgetSource: &admitBudgetSource{hold: 10, snaps: map[[3]string]budget.Snapshot{}}}
	f := newMUFixture(t, func(d *Deps) { d.Budgets = src })
	root := f.addUser("root", "admin")
	alice := f.addUser("alice", "user")
	src.perUser = map[string][]budget.Limit{alice.ID: {{Scope: budget.ScopeUser, Key: alice.ID, Period: budget.PeriodDay, Micros: 7}}}

	for _, get := range []func(string) int{
		func(q string) int { return f.bearerGet("/control/v1/budgets?"+q, serviceToken).Code },
		func(q string) int { return f.session(http.MethodGet, "/ui/v1/admin/budgets?"+q, root, "").Code },
	} {
		if c := get("scope=user&key=" + alice.ID + "&period=day"); c != http.StatusOK {
			t.Fatalf("user budget status %d", c)
		}
		// Unknown users never reach the store.
		before := src.callCount()
		if c := get("scope=user&key=u_nobody"); c != http.StatusNotFound {
			t.Fatalf("unknown user status %d", c)
		}
		if src.callCount() != before {
			t.Fatal("unknown user read the store")
		}
	}
	rec := f.session(http.MethodGet, "/ui/v1/admin/budgets?scope=user&key="+alice.ID+"&period=day", root, "")
	doc := decodeBudgetDoc(t, rec)
	if len(doc.Rows) != 1 || doc.Rows[0].LimitMicros == nil || *doc.Rows[0].LimitMicros != 7 {
		t.Fatalf("user budget doc = %+v", doc)
	}
	// A user bearer still cannot read budgets at all.
	wantStatus(t, f.bearerGet("/control/v1/budgets?scope=user&key="+alice.ID, alice.Key), http.StatusForbidden)
}

func TestMultiUserGlobalBudgetsUserScopeIdentityDown(t *testing.T) {
	src := &userBudgetSource{admitBudgetSource: &admitBudgetSource{hold: 10, snaps: map[[3]string]budget.Snapshot{}}}
	f := newMUFixture(t, func(d *Deps) { d.Budgets = src })
	alice := f.addUser("alice", "user")
	_ = f.ids.Close()
	rec := f.bearerGet("/control/v1/budgets?scope=user&key="+alice.ID, serviceToken)
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if src.callCount() != 0 {
		t.Fatal("store read without verifying the user")
	}
}
