package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/budget"
)

// A config with no budgets block leaves spend controls disabled: the pointer is
// nil and the conversion helpers are inert, so existing configs are unchanged.
func TestBudgetsAbsentDisablesSpendControls(t *testing.T) {
	c, err := Load(write(t, `
clients: [{name: a, class: interactive, key_file: k}]
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Budgets != nil {
		t.Fatalf("budgets should be nil when omitted, got %+v", c.Budgets)
	}
	var b *BudgetConfig
	if ls, err := b.Limits(); err != nil || ls != nil {
		t.Fatalf("nil budgets Limits() = %v, %v; want nil, nil", ls, err)
	}
	if _, err := b.ReservationMicros(); err == nil {
		t.Fatal("nil budgets ReservationMicros() should error, got nil")
	}
}

// Quoted decimal strings load verbatim and convert to integer micro-USD by
// budget.ParseUSD; an explicit "0" is a real zero budget, an omitted limit is
// no entry at all (unlimited).
func TestBudgetsValidLoadsAndConverts(t *testing.T) {
	c, err := Load(write(t, `
clients: [{name: a, class: interactive, key_file: k}]
accounts: [{id: acc, provider: codex}]
budgets:
  reserve_usd: "0.25"
  clients:
    a: {daily_usd: "5.25", monthly_usd: "0"}
  accounts:
    acc: {daily_usd: "10"}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Budgets == nil || c.Budgets.ReserveUSD != "0.25" {
		t.Fatalf("budgets not loaded: %+v", c.Budgets)
	}
	if got, err := c.Budgets.ReservationMicros(); err != nil || got != 250_000 {
		t.Fatalf("ReservationMicros = %d, %v; want 250000", got, err)
	}

	cl := c.Budgets.Clients["a"]
	if cl.DailyUSD == nil || *cl.DailyUSD != "5.25" {
		t.Fatalf("client daily_usd not preserved as string: %+v", cl.DailyUSD)
	}
	if cl.MonthlyUSD == nil || *cl.MonthlyUSD != "0" {
		t.Fatalf("client monthly_usd not preserved as string: %+v", cl.MonthlyUSD)
	}
	want := []budget.Limit{
		{Scope: budget.ScopeClient, Key: "a", Period: budget.PeriodDay, Micros: 5_250_000},
		{Scope: budget.ScopeClient, Key: "a", Period: budget.PeriodMonth, Micros: 0},
	}
	if got, err := cl.Limits(budget.ScopeClient, "a"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("client Limits = %+v, %v; want %+v", got, err, want)
	}

	// Only the daily limit is present: the omitted monthly limit yields no
	// entry, i.e. that budget is unlimited.
	wantAcct := []budget.Limit{
		{Scope: budget.ScopeAccount, Key: "acc", Period: budget.PeriodDay, Micros: 10_000_000},
	}
	if got, err := c.Budgets.Accounts["acc"].Limits(budget.ScopeAccount, "acc"); err != nil || !reflect.DeepEqual(got, wantAcct) {
		t.Fatalf("account Limits = %+v, %v; want %+v", got, err, wantAcct)
	}

	// The block-wide conversion covers clients and accounts, and every
	// (scope, key, period) triple is unique.
	all, err := c.Budgets.Limits()
	if err != nil {
		t.Fatalf("Limits: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("Limits returned %d entries: %+v", len(all), all)
	}
	seen := map[[3]string]bool{}
	for _, l := range all {
		k := [3]string{l.Scope, l.Key, l.Period}
		if seen[k] {
			t.Fatalf("duplicate budget limit %v in %+v", k, all)
		}
		seen[k] = true
	}
}

// The amount fields are strings, so YAML never rounds a value through float64:
// an unquoted exact decimal is carried textually and parsed exactly.
func TestBudgetsAmountIsNotFloatRounded(t *testing.T) {
	c, err := Load(write(t, `
clients: [{name: a, class: interactive, key_file: k}]
budgets:
  reserve_usd: "0.1"
  clients: {a: {daily_usd: 0.1}}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.Budgets.Clients["a"].DailyUSD; got == nil || *got != "0.1" {
		t.Fatalf("daily_usd = %v; want exact string \"0.1\"", got)
	}
	ls, err := c.Budgets.Clients["a"].Limits(budget.ScopeClient, "a")
	if err != nil || len(ls) != 1 || ls[0].Micros != 100_000 {
		t.Fatalf("Limits = %+v, %v; want one 100000-micros limit", ls, err)
	}
}

// The budgets block is strict about unknown fields, exactly like the rest of
// the config: a misspelled key anywhere in the block is rejected.
func TestBudgetsUnknownFieldsRejected(t *testing.T) {
	for _, body := range []string{
		`
clients: [{name: a, class: interactive, key_file: k}]
budgets: {reserve_usd: "1", bogus: 2}
`,
		`
clients: [{name: a, class: interactive, key_file: k}]
budgets:
  reserve_usd: "1"
  clients: {a: {weekly_usd: "1"}}
`,
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Fatalf("unknown budgets field accepted:\n%s", body)
		} else if !strings.Contains(err.Error(), "not found") {
			t.Fatalf("unexpected error %v for:\n%s", err, body)
		}
	}
}

// When the block is present it is validated: reserve_usd is required and must
// be an explicit positive exact decimal (never defaulted or estimated), at
// least one ceiling must be configured, every key must name a configured
// client or account, and every amount must parse exactly as nonnegative USD.
func TestBudgetsValidation(t *testing.T) {
	const validTail = `
clients: [{name: a, class: interactive, key_file: k}]
accounts: [{id: acc, provider: codex}]
`
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty reserve", `budgets: {reserve_usd: "", clients: {a: {daily_usd: "1"}}}`, "reserve_usd"},
		{"zero reserve", `budgets: {reserve_usd: "0", clients: {a: {daily_usd: "1"}}}`, "reserve_usd"},
		{"negative reserve", `budgets: {reserve_usd: "-1", clients: {a: {daily_usd: "1"}}}`, "reserve_usd"},
		{"malformed reserve", `budgets: {reserve_usd: "1,5", clients: {a: {daily_usd: "1"}}}`, "reserve_usd"},
		{"exponent reserve", `budgets: {reserve_usd: "1e3", clients: {a: {daily_usd: "1"}}}`, "reserve_usd"},
		{"overflow reserve", `budgets: {reserve_usd: "999999999999999999999", clients: {a: {daily_usd: "1"}}}`, "reserve_usd"},
		{"no ceilings", `budgets: {reserve_usd: "1"}`, "budgets"},
		{"unknown client", `budgets: {reserve_usd: "1", clients: {nope: {daily_usd: "1"}}}`, "nope"},
		{"unknown account", `budgets: {reserve_usd: "1", accounts: {nope: {daily_usd: "1"}}}`, "nope"},
		{"negative daily", `budgets: {reserve_usd: "1", clients: {a: {daily_usd: "-1"}}}`, "daily_usd"},
		{"exponent monthly", `budgets: {reserve_usd: "1", clients: {a: {monthly_usd: "1e3"}}}`, "monthly_usd"},
		{"overflow daily", `budgets: {reserve_usd: "1", accounts: {acc: {daily_usd: "999999999999999999999"}}}`, "daily_usd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(write(t, validTail+"\n"+tc.body+"\n")); err == nil {
				t.Fatalf("want error, got nil for %s", tc.body)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v does not mention %q", err, tc.want)
			}
		})
	}
}

// A budgets block whose entries resolve to no ceiling at all — an empty client
// or account entry, or an explicit null or blank amount — is rejected rather
// than enabling spend controls with nothing to enforce. An empty entry next to
// a real ceiling is still fine: only the block as a whole needs one.
func TestBudgetsRequireAtLeastOneResolvedCeiling(t *testing.T) {
	const head = `
clients: [{name: a, class: interactive, key_file: k}]
accounts: [{id: acc, provider: codex}]
routes: [{name: r, models: [m], interactive: [acc]}]
`
	for _, tc := range []struct{ name, body string }{
		{"empty client entry", `budgets: {reserve_usd: "1", clients: {a: {}}}`},
		{"empty account entry", `budgets: {reserve_usd: "1", accounts: {acc: {}}}`},
		{"explicit null daily", `budgets: {reserve_usd: "1", clients: {a: {daily_usd: null}}}`},
		{"blank daily", "budgets:\n  reserve_usd: \"1\"\n  clients:\n    a:\n      daily_usd:\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, head+tc.body+"\n"))
			if err == nil {
				t.Fatalf("budgets block with no resolved ceiling accepted: %s", tc.body)
			}
			if !strings.Contains(err.Error(), "at least one") {
				t.Fatalf("error %v does not name the missing ceiling", err)
			}
		})
	}

	c, err := Load(write(t, head+`budgets: {reserve_usd: "1", clients: {a: {}}, accounts: {acc: {monthly_usd: "0"}}}`+"\n"))
	if err != nil {
		t.Fatalf("empty entry beside a real ceiling rejected: %v", err)
	}
	if ls, err := c.Budgets.Limits(); err != nil || len(ls) != 1 {
		t.Fatalf("Limits = %+v, %v; want the one account ceiling", ls, err)
	}
}

// A valid block passes Load/Validate, and a quota-only claude account (which
// cannot serve inference but is a configured account) is an acceptable budget
// key. A nil budgets block is the unchanged default and validates as before.
func TestBudgetsValidAndClaudeAccountAccepted(t *testing.T) {
	c, err := Load(write(t, `
clients: [{name: a, class: interactive, key_file: k}]
accounts:
  - {id: acc, provider: codex}
  - {id: cl, provider: claude}
routes: [{name: r, models: [m], interactive: [acc]}]
budgets:
  reserve_usd: "0.5"
  clients: {a: {daily_usd: "5"}}
  accounts: {cl: {monthly_usd: "100"}}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c.Budgets.Accounts["cl"].MonthlyUSD; got == nil || *got != "100" {
		t.Fatalf("claude account budget not preserved")
	}
}

// The spend block survives a JSON round-trip — the mechanism the reload clone
// (internal/app.cloneConfig) relies on — with pointer limits preserved exactly:
// a present "0" stays present, an omitted limit stays nil, and a nil block
// round-trips back to nil.
func TestBudgetsJSONRoundTripPreservesPointers(t *testing.T) {
	c, err := Load(write(t, `
clients: [{name: a, class: interactive, key_file: k}]
budgets:
  reserve_usd: "0.25"
  clients: {a: {daily_usd: "0"}}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Budgets == nil {
		t.Fatal("budgets lost in JSON round-trip")
	}
	if got := out.Budgets.Clients["a"].DailyUSD; got == nil || *got != "0" {
		t.Fatalf("present zero limit not preserved: %v", got)
	}
	if got := out.Budgets.Clients["a"].MonthlyUSD; got != nil {
		t.Fatalf("omitted limit should stay nil, got %q", *got)
	}
	if out.Budgets.ReserveUSD != "0.25" {
		t.Fatalf("reserve not preserved: %q", out.Budgets.ReserveUSD)
	}

	var empty Config
	eb, err := json.Marshal(&empty)
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	var emptyOut Config
	if err := json.Unmarshal(eb, &emptyOut); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if emptyOut.Budgets != nil {
		t.Fatalf("nil budgets should round-trip nil, got %+v", emptyOut.Budgets)
	}
}
