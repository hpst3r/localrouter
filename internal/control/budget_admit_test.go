package control

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
)

// admitBudgetSource is the BudgetSource the advisory-estimate tests drive. It
// records every read in order (scope, key, period, instant, context deadline)
// and can fail with a fixed error, so a test can prove exactly which instances
// the estimate touches, that all of them share one UTC instant and one bounded
// context, and that a store error is never echoed.
type admitBudgetSource struct {
	mu     sync.Mutex
	limits []budget.Limit
	hold   int64
	snaps  map[[3]string]budget.Snapshot
	err    error

	reads [][3]string
	ats   []time.Time
	dls   []time.Duration
}

func (s *admitBudgetSource) Snapshot(ctx context.Context, scope, key, period string, at time.Time) (budget.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, [3]string{scope, key, period})
	s.ats = append(s.ats, at)
	if dl, ok := ctx.Deadline(); ok {
		s.dls = append(s.dls, time.Until(dl))
	} else {
		s.dls = append(s.dls, -1)
	}
	if s.err != nil {
		return budget.Snapshot{}, s.err
	}
	return s.snaps[[3]string{scope, key, period}], nil
}

func (s *admitBudgetSource) Limits() []budget.Limit { return s.limits }

func (s *admitBudgetSource) ReservationMicros() int64 { return s.hold }

// readKeys renders the recorded reads as "scope|key|period" in call order.
func (s *admitBudgetSource) readKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.reads))
	for _, r := range s.reads {
		out = append(out, strings.Join(r[:], "|"))
	}
	return out
}

func (s *admitBudgetSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reads)
}

// storeBudgetSource adapts a real durable Store to the control BudgetSource so
// the parity test runs the advisory estimate against the same money the gate
// writes.
type storeBudgetSource struct {
	store  *budget.Store
	limits []budget.Limit
	hold   int64
}

func (s storeBudgetSource) Snapshot(ctx context.Context, scope, key, period string, at time.Time) (budget.Snapshot, error) {
	return s.store.Snapshot(ctx, scope, key, period, at)
}

func (s storeBudgetSource) Limits() []budget.Limit { return s.limits }

func (s storeBudgetSource) ReservationMicros() int64 { return s.hold }

// admitBudgetFixture wires spend controls onto the standard admit fixture. The
// client "tester" authenticates for the fixture's secret key and is registered
// in Deps so the estimate can name the client-scope budget; its class is
// deliberately background while the fixture's policy is asked for interactive,
// proving the estimate keeps the caller's request class for policy but the
// authenticated identity for the cost scope. authCalls counts Authenticate
// invocations so a test can prove no revalidation happens where it must not.
func admitBudgetFixture(t *testing.T, src BudgetSource, requireAuth bool) (*fixture, *int) {
	t.Helper()
	f := newFixture(requireAuth)
	f.srv.deps.Budgets = src
	f.srv.deps.Clients = []ClientInfo{{Name: "tester", Class: "interactive"}}
	calls := 0
	f.srv.deps.Authenticate = func(b string) (core.Client, bool) {
		calls++
		if b == secretKey {
			return core.Client{Name: "tester", Class: core.ClassBackground}, true
		}
		return core.Client{}, false
	}
	return f, &calls
}

func budgetBlock(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	raw, ok := m["budget"]
	if !ok {
		t.Fatalf("response has no budget block: %v", m)
	}
	b, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("budget is %T, want an object", raw)
	}
	return b
}

func noBudgetBlock(t *testing.T, m map[string]any) {
	t.Helper()
	if raw, ok := m["budget"]; ok {
		t.Fatalf("unexpected budget block: %v", raw)
	}
}

// The four instances an admitted attempt always pins, in the order the estimate
// reads them.
func wantAdmitInstances(client, account string) []string {
	return []string{
		budget.ScopeClient + "|" + client + "|" + budget.PeriodDay,
		budget.ScopeClient + "|" + client + "|" + budget.PeriodMonth,
		budget.ScopeAccount + "|" + account + "|" + budget.PeriodDay,
		budget.ScopeAccount + "|" + account + "|" + budget.PeriodMonth,
	}
}

func TestAdmitBudgetUnconfiguredIsByteCompatible(t *testing.T) {
	f := newFixture(false) // no Budgets wired: legacy admit
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	want := "{\"decision\":\"allow\",\"account_id\":\"secondary\",\"reason\":\"admitted\"}\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want the legacy %q (no budget field)", got, want)
	}
}

func TestAdmitBudgetAdvisoryEstimate(t *testing.T) {
	src := &admitBudgetSource{
		hold: 250_000,
		limits: []budget.Limit{
			{Scope: budget.ScopeAccount, Key: "secondary", Period: budget.PeriodDay, Micros: 1_000_000},
		},
		snaps: map[[3]string]budget.Snapshot{
			[3]string{budget.ScopeAccount, "secondary", budget.PeriodDay}: {Reported: 100_000},
		},
	}
	f, _ := admitBudgetFixture(t, src, false)
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	m := decode(t, rec)
	if m["decision"] != "allow" || m["account_id"] != "secondary" || m["reason"] != "admitted" {
		t.Fatalf("policy decision changed by the estimate: %v", m)
	}
	b := budgetBlock(t, m)
	if b["advisory"] != true {
		t.Errorf("advisory = %v, want true", b["advisory"])
	}
	if b["reservation_created"] != false {
		t.Errorf("reservation_created = %v, want false", b["reservation_created"])
	}
	if b["client"] != "tester" {
		t.Errorf("client = %v, want the authenticated tester", b["client"])
	}
	if b["account_id"] != "secondary" {
		t.Errorf("account_id = %v, want the policy-chosen secondary", b["account_id"])
	}
	if b["hold_micros"] != float64(250_000) || b["hold_usd"] != "0.250000" {
		t.Errorf("hold = %v/%v, want 250000/0.250000", b["hold_micros"], b["hold_usd"])
	}
	if b["allow"] != true {
		t.Errorf("allow = %v, want true (100k+50k headroom over 1.0)", b["allow"])
	}
	if _, ok := b["reason"]; ok {
		t.Errorf("reason should be omitted on a clean allow, got %v", b["reason"])
	}

	want := wantAdmitInstances("tester", "secondary")
	if got := src.readKeys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("reads = %v, want exactly the four pinned instances %v", got, want)
	}
	if len(src.ats) != 4 {
		t.Fatalf("at count = %d, want 4", len(src.ats))
	}
	for i, at := range src.ats {
		if !at.Equal(t0) || at.Location() != time.UTC {
			t.Errorf("read %d instant = %v (%v), want %v UTC", i, at, at.Location(), t0)
		}
	}
	for i, dl := range src.dls {
		if dl <= 0 || dl > 2*time.Second {
			t.Errorf("read %d deadline = %v, want a bounded 2s context", i, dl)
		}
	}
}

func TestAdmitBudgetUnknownCallerNoEstimate(t *testing.T) {
	src := &admitBudgetSource{hold: 250_000}
	f, auth := admitBudgetFixture(t, src, false) // RequireAuth false: no bearer at all
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, "")
	m := decode(t, rec)
	if m["decision"] != "allow" || m["reason"] != "admitted" {
		t.Fatalf("unknown caller must keep the policy decision, got %v", m)
	}
	b := budgetBlock(t, m)
	if b["advisory"] != true || b["allow"] != false {
		t.Errorf("block = %v, want advisory true / allow false", b)
	}
	if b["reason"] != "client_identity_unavailable" {
		t.Errorf("reason = %v, want client_identity_unavailable", b["reason"])
	}
	if _, ok := b["client"]; ok {
		t.Errorf("unknown caller must not name a client: %v", b["client"])
	}
	if _, ok := b["account_id"]; ok {
		t.Errorf("unknown caller must not name an account: %v", b["account_id"])
	}
	if src.callCount() != 0 {
		t.Errorf("store read %d times for an unknown caller, want 0", src.callCount())
	}
	if *auth != 0 {
		t.Errorf("Authenticate called %d times without a bearer, want 0", *auth)
	}
}

func TestAdmitBudgetInvalidBearerNoEstimate(t *testing.T) {
	src := &admitBudgetSource{hold: 250_000}
	f, auth := admitBudgetFixture(t, src, false)
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, "wrong-key")
	m := decode(t, rec)
	if m["decision"] != "allow" {
		t.Fatalf("invalid bearer must not change the policy decision: %v", m)
	}
	b := budgetBlock(t, m)
	if b["allow"] != false || b["reason"] != "client_identity_unavailable" {
		t.Errorf("block = %v, want allow false / client_identity_unavailable", b)
	}
	if src.callCount() != 0 {
		t.Errorf("store read %d times for an invalid bearer, want 0", src.callCount())
	}
	if *auth != 1 {
		t.Errorf("Authenticate called %d times, want 1", *auth)
	}
}

func TestAdmitBudgetUnderAuthGate(t *testing.T) {
	src := &admitBudgetSource{hold: 1_000}
	f, _ := admitBudgetFixture(t, src, true) // RequireAuth true: a valid key is required
	if rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated admit = %d, want 401", rec.Code)
	}
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey)
	m := decode(t, rec)
	b := budgetBlock(t, m)
	if b["allow"] != true || b["client"] != "tester" {
		t.Errorf("block = %v, want an allowed estimate for tester", b)
	}
}

func TestAdmitBudgetStoreErrorIsGeneric(t *testing.T) {
	const raw = "open /var/lib/localrouter/secret/budget.db: permission denied"
	src := &admitBudgetSource{hold: 250_000, err: errors.New(raw)}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	f, _ := admitBudgetFixture(t, src, false)
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey)
	m := decode(t, rec)
	if m["decision"] != "allow" || m["reason"] != "admitted" {
		t.Fatalf("store error must not change the policy decision: %v", m)
	}
	b := budgetBlock(t, m)
	if b["allow"] != false || b["reason"] != "budget_store_error" {
		t.Errorf("block = %v, want allow false / budget_store_error", b)
	}
	if strings.Contains(rec.Body.String(), raw) || strings.Contains(rec.Body.String(), "/var/lib/localrouter") {
		t.Errorf("response leaked the store error: %s", rec.Body)
	}
	if buf.Len() != 0 {
		t.Errorf("admit logged a budget error: %s", buf.String())
	}
}

func TestAdmitBudgetDeniesWhenLimitExhausted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  budget.Limit
		snap   budget.Snapshot
		reason string
	}{
		{"zero account day", budget.Limit{Scope: budget.ScopeAccount, Key: "secondary", Period: budget.PeriodDay}, budget.Snapshot{}, "budget_exceeded"},
		{"spent over client month", budget.Limit{Scope: budget.ScopeClient, Key: "tester", Period: budget.PeriodMonth, Micros: 10}, budget.Snapshot{Reported: 11}, "budget_exceeded"},
		{"reserved leaves no room", budget.Limit{Scope: budget.ScopeClient, Key: "tester", Period: budget.PeriodDay, Micros: 100}, budget.Snapshot{Reserved: 100}, "budget_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &admitBudgetSource{
				hold:   250_000,
				limits: []budget.Limit{tc.limit},
				snaps:  map[[3]string]budget.Snapshot{[3]string{tc.limit.Scope, tc.limit.Key, tc.limit.Period}: tc.snap},
			}
			f, _ := admitBudgetFixture(t, src, false)
			rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey)
			m := decode(t, rec)
			if m["decision"] != "allow" || m["reason"] != "admitted" {
				t.Fatalf("estimate overrode the policy decision: %v", m)
			}
			b := budgetBlock(t, m)
			if b["allow"] != false || b["reason"] != tc.reason {
				t.Errorf("block = %v, want allow false / %s", b, tc.reason)
			}
		})
	}
}

func TestAdmitBudgetNonPositiveHoldFailsClosed(t *testing.T) {
	for _, hold := range []int64{0, -1} {
		src := &admitBudgetSource{hold: hold}
		f, _ := admitBudgetFixture(t, src, false)
		m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))
		b := budgetBlock(t, m)
		if b["allow"] != false || b["reason"] != "budget_store_error" {
			t.Errorf("hold %d: block = %v, want allow false / budget_store_error", hold, b)
		}
		if src.callCount() != 0 {
			t.Errorf("hold %d: store read %d times, want 0", hold, src.callCount())
		}
	}
}

func TestAdmitBudgetUnlimitedWhenNoLimits(t *testing.T) {
	src := &admitBudgetSource{hold: 250_000}
	f, _ := admitBudgetFixture(t, src, false)
	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))
	b := budgetBlock(t, m)
	if b["allow"] != true {
		t.Errorf("block = %v, want allow true with no configured ceiling", b)
	}
	if src.callCount() != 4 {
		t.Errorf("reads = %d, want the four pinned instances even when unlimited", src.callCount())
	}
}

func TestAdmitBudgetPolicyDenySkipsEstimate(t *testing.T) {
	src := &admitBudgetSource{hold: 250_000}
	f, auth := admitBudgetFixture(t, src, false)
	f.policy.decision = core.Decision{Allow: false, Reason: "reserve"}
	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))
	if m["decision"] != "deny" || m["reason"] != "reserve" {
		t.Fatalf("decision = %v, want the policy denial verbatim", m)
	}
	noBudgetBlock(t, m)
	if src.callCount() != 0 {
		t.Errorf("store read %d times on a policy deny, want 0", src.callCount())
	}
	if *auth != 0 {
		t.Errorf("Authenticate called %d times on a policy deny, want 0", *auth)
	}
}

func TestAdmitBudgetUsesChosenAccountOnly(t *testing.T) {
	src := &admitBudgetSource{hold: 250_000}
	f, _ := admitBudgetFixture(t, src, false)
	// The gpt route offers primary,secondary; the policy chooses secondary. The
	// estimate must touch only the chosen account and never iterate candidates.
	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))
	b := budgetBlock(t, m)
	if b["account_id"] != "secondary" {
		t.Fatalf("account_id = %v, want secondary", b["account_id"])
	}
	got := src.readKeys()
	if strings.Join(got, ",") != strings.Join(wantAdmitInstances("tester", "secondary"), ",") {
		t.Errorf("reads = %v, want only the chosen account's instances", got)
	}
	for _, k := range got {
		if strings.Contains(k, "primary") {
			t.Errorf("estimate read a non-chosen candidate: %s", k)
		}
	}
	if len(f.policy.dryRuns) != 1 || f.policy.dryRuns[0].class != core.ClassInteractive {
		t.Errorf("policy dry runs = %+v, want one interactive call", f.policy.dryRuns)
	}
}

func TestAdmitBudgetAccountOnlyRequest(t *testing.T) {
	src := &admitBudgetSource{hold: 250_000}
	f, _ := admitBudgetFixture(t, src, false)
	f.policy.decision = core.Decision{Allow: true, AccountID: "expired", Reason: "admitted"}
	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"background","account":"expired"}`, secretKey))
	b := budgetBlock(t, m)
	if b["account_id"] != "expired" {
		t.Errorf("account_id = %v, want expired", b["account_id"])
	}
	if len(f.policy.dryRuns) != 1 || strings.Join(f.policy.dryRuns[0].candidates, ",") != "expired" {
		t.Fatalf("dry runs = %+v, want the single account", f.policy.dryRuns)
	}
	if got := src.readKeys(); strings.Join(got, ",") != strings.Join(wantAdmitInstances("tester", "expired"), ",") {
		t.Errorf("reads = %v, want the account-only instances", got)
	}
}

func TestAdmitBudgetIgnoresUnknownRequestFields(t *testing.T) {
	src := &admitBudgetSource{hold: 250_000}
	f, _ := admitBudgetFixture(t, src, false)
	body := `{"class":"interactive","model":"gpt-5","budget":{"hold_micros":1},"client":"attacker","account_id":"x"}`
	m := decode(t, f.do(t, "POST", "/control/v1/admit", body, secretKey))
	b := budgetBlock(t, m)
	if b["client"] != "tester" || b["account_id"] != "secondary" {
		t.Errorf("request fields leaked into the estimate: %v", b)
	}
	if b["hold_micros"] != float64(250_000) {
		t.Errorf("hold_micros = %v, want the generation's 250000", b["hold_micros"])
	}
}

func TestAdmitBudgetForeignAccountNoEstimate(t *testing.T) {
	src := &admitBudgetSource{hold: 250_000}
	f, _ := admitBudgetFixture(t, src, false)
	// The policy named an account outside the eligible set it was given: the
	// estimate must not read some other account's budget.
	f.policy.decision = core.Decision{Allow: true, AccountID: "rogue", Reason: "admitted"}
	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))
	if m["decision"] != "allow" {
		t.Fatalf("decision = %v, want the policy allow untouched", m)
	}
	b := budgetBlock(t, m)
	if b["allow"] != false || b["reason"] != "account_identity_unavailable" {
		t.Errorf("block = %v, want allow false / account_identity_unavailable", b)
	}
	if _, ok := b["account_id"]; ok {
		t.Errorf("estimate named an ineligible account: %v", b["account_id"])
	}
	if src.callCount() != 0 {
		t.Errorf("store read %d times for a foreign account, want 0", src.callCount())
	}
}

// seedPeriodState books a durable reported spend and leaves an open hold for
// one (client, account) pair at at, using the store's own public reserve/settle
// path so the seeded money is indistinguishable from real spend.
func seedPeriodState(t *testing.T, st *budget.Store, client, account string, at time.Time, reported, reserved int64) {
	t.Helper()
	ctx := context.Background()
	if reported > 0 {
		const id = "seed-reported"
		if err := st.Reserve(ctx, budget.Reservation{ID: id, Client: client, Account: account, At: at, Micros: 1}, nil); err != nil {
			t.Fatalf("seed reserve: %v", err)
		}
		if err := st.Settle(ctx, budget.Settlement{ID: id, Micros: reported, Basis: budget.BasisReported}); err != nil {
			t.Fatalf("seed settle: %v", err)
		}
	}
	if reserved > 0 {
		if err := st.Reserve(ctx, budget.Reservation{ID: "seed-hold", Client: client, Account: account, At: at, Micros: reserved}, nil); err != nil {
			t.Fatalf("seed hold: %v", err)
		}
	}
}

func admitInstances(client, account string) [][3]string {
	return [][3]string{
		{budget.ScopeClient, client, budget.PeriodDay},
		{budget.ScopeClient, client, budget.PeriodMonth},
		{budget.ScopeAccount, account, budget.PeriodDay},
		{budget.ScopeAccount, account, budget.PeriodMonth},
	}
}

func TestAdmitBudgetWritesNothing(t *testing.T) {
	st, err := budget.Open(filepath.Join(t.TempDir(), "budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	seedPeriodState(t, st, "tester", "secondary", t0, 100_000, 50_000)

	instances := admitInstances("tester", "secondary")
	before := make(map[[3]string]budget.Snapshot, len(instances))
	for _, in := range instances {
		snap, err := st.Snapshot(ctx, in[0], in[1], in[2], t0)
		if err != nil {
			t.Fatal(err)
		}
		before[in] = snap
	}

	limits := []budget.Limit{{Scope: budget.ScopeAccount, Key: "secondary", Period: budget.PeriodDay, Micros: 1_000_000}}
	f, _ := admitBudgetFixture(t, storeBudgetSource{store: st, limits: limits, hold: 250_000}, false)
	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))
	if b := budgetBlock(t, m); b["allow"] != true {
		t.Fatalf("block = %v, want allow true", b)
	}

	for _, in := range instances {
		after, err := st.Snapshot(ctx, in[0], in[1], in[2], t0)
		if err != nil {
			t.Fatal(err)
		}
		if before[in] != after {
			t.Errorf("%v changed: %+v -> %+v", in, before[in], after)
		}
	}
	// The estimate must not have taken a reservation: settling the attempt id
	// the admit probe would have used reports an unknown reservation.
	if err := st.Settle(ctx, budget.Settlement{ID: "admit-1", Basis: budget.BasisReported}); !errors.Is(err, budget.ErrUnknownReservation) {
		t.Errorf("admit created a reservation: settle err = %v, want ErrUnknownReservation", err)
	}
}

func TestAdmitBudgetRealStoreParityWithReserve(t *testing.T) {
	const (
		client       = "tester"
		account      = "secondary"
		hold         = int64(250_000)
		reported     = int64(100_000)
		openReserved = int64(50_000)
	)
	type limitSpec struct {
		scope, key, period string
		micros             int64
	}
	cases := []struct {
		name   string
		limits []limitSpec
	}{
		{"unlimited", nil},
		{"client day zero", []limitSpec{{budget.ScopeClient, client, budget.PeriodDay, 0}}},
		{"client month zero", []limitSpec{{budget.ScopeClient, client, budget.PeriodMonth, 0}}},
		{"account day zero", []limitSpec{{budget.ScopeAccount, account, budget.PeriodDay, 0}}},
		{"account month zero", []limitSpec{{budget.ScopeAccount, account, budget.PeriodMonth, 0}}},
		{"client day above usage", []limitSpec{{budget.ScopeClient, client, budget.PeriodDay, 1_000_000}}},
		{"account month above usage", []limitSpec{{budget.ScopeAccount, account, budget.PeriodMonth, 9_000_000}}},
		{"client day exactly to the micro", []limitSpec{{budget.ScopeClient, client, budget.PeriodDay, reported + openReserved + hold}}},
		{"client day one micro short", []limitSpec{{budget.ScopeClient, client, budget.PeriodDay, reported + openReserved + hold - 1}}},
		{"two ceilings, second exhausted", []limitSpec{
			{budget.ScopeClient, client, budget.PeriodDay, 1_000_000},
			{budget.ScopeAccount, account, budget.PeriodMonth, 1},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := budget.Open(filepath.Join(t.TempDir(), "budget.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()
			seedPeriodState(t, st, client, account, t0, reported, openReserved)

			limits := make([]budget.Limit, 0, len(tc.limits))
			for _, l := range tc.limits {
				limits = append(limits, budget.Limit{Scope: l.scope, Key: l.key, Period: l.period, Micros: l.micros})
			}

			f, _ := admitBudgetFixture(t, storeBudgetSource{store: st, limits: limits, hold: hold}, false)
			m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))
			if m["decision"] != "allow" {
				t.Fatalf("policy decision = %v, want allow (never overridden)", m["decision"])
			}
			b := budgetBlock(t, m)
			advisoryAllow, _ := b["allow"].(bool)

			// The reference is the real admission path for the same attempt,
			// identity and instant: any error (a denial or an arithmetic
			// failure) means the gate would not have admitted it.
			refErr := st.Reserve(ctx, budget.Reservation{
				ID: "parity-probe", Client: client, Account: account, At: t0, Micros: hold,
			}, limits)
			if refAllow := refErr == nil; advisoryAllow != refAllow {
				t.Fatalf("advisory allow = %v, Reserve err = %v; the estimate must agree with admission", advisoryAllow, refErr)
			}
		})
	}
}

func TestAdmitBudgetOverflowFailsClosed(t *testing.T) {
	st, err := budget.Open(filepath.Join(t.TempDir(), "budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	// An open hold of MaxInt64 cannot be added to the generation's positive hold
	// without overflowing: both the estimate and the real admission fail closed.
	if err := st.Reserve(ctx, budget.Reservation{ID: "huge", Client: "tester", Account: "secondary", At: t0, Micros: math.MaxInt64}, nil); err != nil {
		t.Fatal(err)
	}
	limits := []budget.Limit{{Scope: budget.ScopeClient, Key: "tester", Period: budget.PeriodDay, Micros: math.MaxInt64}}
	f, _ := admitBudgetFixture(t, storeBudgetSource{store: st, limits: limits, hold: 250_000}, false)
	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))
	b := budgetBlock(t, m)
	if b["allow"] != false {
		t.Errorf("block = %v, want allow false on overflow", b)
	}
	if m["decision"] != "allow" {
		t.Errorf("decision = %v, want the policy decision untouched", m["decision"])
	}
	if refErr := st.Reserve(ctx, budget.Reservation{ID: "overflow-probe", Client: "tester", Account: "secondary", At: t0, Micros: 250_000}, limits); refErr == nil {
		t.Errorf("store admitted an overflowing reservation; parity broken")
	}
}
