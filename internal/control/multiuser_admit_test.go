package control

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/policy"
)

// admitPolicyFixture runs admit over the real policy engine. Every account
// carries operator state a user must never learn through admit: an exhausted
// subscription window, a negative OpenRouter balance and a healthy fallback.
func admitPolicyFixture(t *testing.T, mutate func(*Deps)) *muFixture {
	t.Helper()
	accounts := []core.Account{
		{ID: "primary", Provider: core.ProviderCodex},
		{ID: "or-team", Provider: core.ProviderOpenRouter},
		{ID: "backup", Provider: core.ProviderOpenAICompat},
	}
	q := &fakeQuota{snaps: map[string]core.Snapshot{
		"primary": {AccountID: "primary", FetchedAt: t0,
			Windows: []core.Window{{Kind: "5h", UsedFrac: 1, ResetAt: t0.Add(time.Hour), WindowSeconds: 18000}}},
		"or-team": {AccountID: "or-team", FetchedAt: t0, Credits: &core.Credits{FetchedAt: t0, BalanceUSD: -1.25}},
		"backup":  {AccountID: "backup", FetchedAt: t0},
	}}
	return newMUFixture(t, func(d *Deps) {
		d.Accounts = accounts
		d.Quota = q
		d.Policy = policy.New(accounts, q, policy.Options{Clock: fakeClock{t0}})
		d.Routes = []core.Route{
			{Name: "main", Models: []string{"gpt"},
				Interactive: []string{"primary", "or-team", "backup"}, Background: []string{"primary", "or-team", "backup"}},
			{Name: "dead", Models: []string{"dead"},
				Interactive: []string{"primary", "or-team"}, Background: []string{"primary", "or-team"}},
		}
		if mutate != nil {
			mutate(d)
		}
	})
}

// operatorAccountState is what the policy's reason says about the accounts.
var operatorAccountState = []string{"primary", "or-team", "backup", "%", "$", "balance", "exhausted", "skipped"}

func assertNoAccountState(t *testing.T, label, body string) {
	t.Helper()
	for _, s := range operatorAccountState {
		if strings.Contains(body, s) {
			t.Errorf("%s discloses %q: %s", label, s, body)
		}
	}
}

func TestMultiUserAdmitModelIsGenericForUserBearers(t *testing.T) {
	f := admitPolicyFixture(t, nil)
	alice := f.addUser("alice", core.RoleUser)
	f.addStaticUser(aliceStaticToken, alice)
	for _, tok := range []string{alice.Key, aliceStaticToken} {
		for _, c := range []struct{ model, decision, reason string }{
			{"gpt", "allow", admitReasonAdmitted},
			{"dead", "deny", admitReasonNoAccount},
		} {
			rec := f.bearerPost("/control/v1/admit", tok, `{"class":"interactive","model":"`+c.model+`"}`)
			wantStatus(t, rec, http.StatusOK)
			m := decodeMap(t, rec)
			if m["decision"] != c.decision || m["reason"] != c.reason || m["account_id"] != "" {
				t.Errorf("user admit %s = %v, want decision %s reason %s and empty account_id",
					c.model, m, c.decision, c.reason)
			}
			assertNoAccountState(t, "user admit "+c.model, rec.Body.String())
		}
	}

	// The service principal is a global reader and keeps the policy detail.
	rec := f.bearerPost("/control/v1/admit", serviceToken, `{"class":"interactive","model":"gpt"}`)
	wantStatus(t, rec, http.StatusOK)
	m := decodeMap(t, rec)
	if m["account_id"] != "backup" || !strings.Contains(m["reason"].(string), "or-team") {
		t.Fatalf("service admit lost its detail: %v", m)
	}
}

func TestMultiUserAdmitAccountFormIsServiceOnly(t *testing.T) {
	f := admitPolicyFixture(t, nil)
	alice := f.addUser("alice", core.RoleUser)
	f.addStaticUser(aliceStaticToken, alice)
	for _, tok := range []string{alice.Key, aliceStaticToken} {
		var bodies []string
		for _, account := range []string{"or-team", "backup", "no-such-account"} {
			rec := f.bearerPost("/control/v1/admit", tok, `{"class":"interactive","account":"`+account+`"}`)
			wantStatus(t, rec, http.StatusForbidden)
			bodies = append(bodies, rec.Body.String())
		}
		if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
			t.Fatalf("account-form refusals differ by account: %q", bodies)
		}
		assertNoAccountState(t, "user account-form admit", bodies[0])
	}

	// The service principal keeps the single-account dry run and its 404.
	rec := f.bearerPost("/control/v1/admit", serviceToken, `{"class":"interactive","account":"or-team"}`)
	wantStatus(t, rec, http.StatusOK)
	if m := decodeMap(t, rec); m["account_id"] != "or-team" || !strings.Contains(m["reason"].(string), "balance") {
		t.Fatalf("service account-form admit lost its detail: %v", m)
	}
	wantStatus(t, f.bearerPost("/control/v1/admit", serviceToken, `{"class":"interactive","account":"no-such-account"}`),
		http.StatusNotFound)
}

// A user bearer's advisory estimate covers only its own client and user
// buckets: the shared account-scope ceiling is neither read nor named, and a
// denial of the user's own buckets is still visible as budget_exceeded.
func TestMultiUserAdmitBudgetOmitsAccountScopeForUserBearers(t *testing.T) {
	src := &userBudgetSource{admitBudgetSource: &admitBudgetSource{hold: 10, snaps: map[[3]string]budget.Snapshot{},
		limits: []budget.Limit{{Scope: budget.ScopeAccount, Key: "backup", Period: budget.PeriodDay, Micros: 0}}}}
	f := admitPolicyFixture(t, func(d *Deps) { d.Budgets = src })
	alice := f.addUser("alice", core.RoleUser)

	rec := f.bearerPost("/control/v1/admit", alice.Key, `{"class":"interactive","model":"gpt"}`)
	wantStatus(t, rec, http.StatusOK)
	assertNoAccountState(t, "user admit budget", rec.Body.String())
	b := budgetBlock(t, decodeMap(t, rec))
	if _, ok := b["account_id"]; ok || b["client"] != alice.KeyID || b["allow"] != true || b["reason"] != nil {
		t.Fatalf("user budget block = %v, want own client only, allowed, no account", b)
	}
	want := []string{
		"client|" + alice.KeyID + "|day", "client|" + alice.KeyID + "|month",
		"user|" + alice.ID + "|day", "user|" + alice.ID + "|month",
	}
	if got := src.readKeys(); !slices.Equal(got, want) {
		t.Fatalf("user estimate reads = %v, want %v", got, want)
	}

	// The user's own ceiling still shows as a generic budget denial.
	src.perUser = map[string][]budget.Limit{alice.ID: {{Scope: budget.ScopeUser, Key: alice.ID, Period: budget.PeriodMonth, Micros: 5}}}
	rec = f.bearerPost("/control/v1/admit", alice.Key, `{"class":"interactive","model":"gpt"}`)
	if b := budgetBlock(t, decodeMap(t, rec)); b["allow"] != false || b["reason"] != admitBudgetExceeded {
		t.Fatalf("own user ceiling not reported: %v", b)
	}

	// The service principal still sees the account-scope estimate.
	rec = f.bearerPost("/control/v1/admit", serviceToken, `{"class":"interactive","model":"gpt"}`)
	if b := budgetBlock(t, decodeMap(t, rec)); b["account_id"] != "backup" || b["allow"] != false || b["reason"] != admitBudgetExceeded {
		t.Fatalf("service budget block = %v", b)
	}
}
