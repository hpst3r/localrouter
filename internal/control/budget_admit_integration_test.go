package control

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
)

// gateParitySource is a read-only control.BudgetSource backed by a real,
// SQLite-durable budget.Store. Every Snapshot delegates straight to the store,
// so the advisory estimate and budget.Gate.Reserve (the proxy's authoritative
// admission) read and write the very same money. It hands out a copy of the
// generation's limits and its fixed hold, exactly the slice and number the
// generation's Gate was built with, and it records each (scope,key,period) read
// in call order so a test can prove which instances were touched.
type gateParitySource struct {
	store  *budget.Store
	limits []budget.Limit
	hold   int64

	mu    sync.Mutex
	reads [][3]string
}

func (s *gateParitySource) Snapshot(ctx context.Context, scope, key, period string, at time.Time) (budget.Snapshot, error) {
	s.mu.Lock()
	s.reads = append(s.reads, [3]string{scope, key, period})
	s.mu.Unlock()
	return s.store.Snapshot(ctx, scope, key, period, at)
}

// Limits returns a copy: the estimate sees the generation-pinned ceilings and
// can never mutate the slice the Gate enforces.
func (s *gateParitySource) Limits() []budget.Limit { return append([]budget.Limit(nil), s.limits...) }

func (s *gateParitySource) ReservationMicros() int64 { return s.hold }

// readKeys renders the recorded reads as "scope|key|period" in call order.
func (s *gateParitySource) readKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.reads))
	for _, r := range s.reads {
		out = append(out, strings.Join(r[:], "|"))
	}
	return out
}

func (s *gateParitySource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reads)
}

// openBudgetStore opens a fresh durable budget database under the test's own
// temp dir (never the service's real key file) and closes it on cleanup.
func openBudgetStore(t *testing.T) *budget.Store {
	t.Helper()
	st, err := budget.Open(filepath.Join(t.TempDir(), "budget.db"))
	if err != nil {
		t.Fatalf("open budget store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// snapshotAll reads the four period instances an admitted attempt always pins
// for (client, account) at at. Comparing the map before and after an admit call
// is the no-write proof: every field of every instance must be identical.
func snapshotAll(t *testing.T, st *budget.Store, client, account string, at time.Time) map[[3]string]budget.Snapshot {
	t.Helper()
	ctx := context.Background()
	out := make(map[[3]string]budget.Snapshot, 4)
	for _, in := range admitInstances(client, account) {
		snap, err := st.Snapshot(ctx, in[0], in[1], in[2], at)
		if err != nil {
			t.Fatalf("snapshot %v: %v", in, err)
		}
		out[in] = snap
	}
	return out
}

func assertNoWrites(t *testing.T, before, after map[[3]string]budget.Snapshot) {
	t.Helper()
	for in, b := range before {
		if a := after[in]; a != b {
			t.Errorf("%v changed across the admit call: %+v -> %+v (admit wrote to the store)", in, b, a)
		}
	}
}

// TestAdmitBudgetGateParityRealStore runs the advisory estimate against a real
// SQLite store and compares its verdict with budget.Gate.Reserve — the
// authoritative admission for the same attempt, identity and instant — across a
// matrix of ceilings: no ceiling at all, an explicit zero client/account
// day/month ceiling, the exact hold boundary and one micro short of it, a
// settled overrun, and active holds that do and do not leave room.
//
// Each case proves three things at once: the advisory block never overrides the
// policy decision, the estimate reads all four pinned instances and writes
// nothing (before/after snapshots are identical), and its allow verdict is
// exactly whether the generation's own Gate would have admitted the attempt.
func TestAdmitBudgetGateParityRealStore(t *testing.T) {
	const (
		client  = "tester"
		account = "secondary"
		hold    = int64(250_000)
	)
	const boundary = int64(100_000) + int64(50_000) + hold // reported+reserved+hold

	type parityCase struct {
		name     string
		reported int64
		reserved int64
		limits   []budget.Limit
	}
	cases := []parityCase{
		{name: "unlimited", reported: 100_000, reserved: 50_000},
		{name: "client-day-zero", reported: 100_000, reserved: 50_000,
			limits: []budget.Limit{{Scope: budget.ScopeClient, Key: client, Period: budget.PeriodDay}}},
		{name: "client-month-zero", reported: 100_000, reserved: 50_000,
			limits: []budget.Limit{{Scope: budget.ScopeClient, Key: client, Period: budget.PeriodMonth}}},
		{name: "account-day-zero", reported: 100_000, reserved: 50_000,
			limits: []budget.Limit{{Scope: budget.ScopeAccount, Key: account, Period: budget.PeriodDay}}},
		{name: "account-month-zero", reported: 100_000, reserved: 50_000,
			limits: []budget.Limit{{Scope: budget.ScopeAccount, Key: account, Period: budget.PeriodMonth}}},
		{name: "hold-exact-boundary-client-day", reported: 100_000, reserved: 50_000,
			limits: []budget.Limit{{Scope: budget.ScopeClient, Key: client, Period: budget.PeriodDay, Micros: boundary}}},
		{name: "hold-one-micro-short-client-day", reported: 100_000, reserved: 50_000,
			limits: []budget.Limit{{Scope: budget.ScopeClient, Key: client, Period: budget.PeriodDay, Micros: boundary - 1}}},
		{name: "hold-exact-boundary-account-month", reported: 100_000, reserved: 50_000,
			limits: []budget.Limit{{Scope: budget.ScopeAccount, Key: account, Period: budget.PeriodMonth, Micros: boundary}}},
		{name: "settled-overrun-client-month", reported: 600_000, reserved: 0,
			limits: []budget.Limit{{Scope: budget.ScopeClient, Key: client, Period: budget.PeriodMonth, Micros: 500_000}}},
		{name: "active-holds-leave-no-room-account-day", reported: 0, reserved: 450_000,
			limits: []budget.Limit{{Scope: budget.ScopeAccount, Key: account, Period: budget.PeriodDay, Micros: 600_000}}},
		{name: "active-holds-within-room-account-day", reported: 0, reserved: 100_000,
			limits: []budget.Limit{{Scope: budget.ScopeAccount, Key: account, Period: budget.PeriodDay, Micros: 500_000}}},
		{name: "second-ceiling-exhausted", reported: 100_000, reserved: 50_000,
			limits: []budget.Limit{
				{Scope: budget.ScopeClient, Key: client, Period: budget.PeriodDay, Micros: 1_000_000},
				{Scope: budget.ScopeAccount, Key: account, Period: budget.PeriodMonth, Micros: 1},
			}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openBudgetStore(t)
			ctx := context.Background()
			seedPeriodState(t, st, client, account, t0, tc.reported, tc.reserved)

			limits := append([]budget.Limit(nil), tc.limits...)
			src := &gateParitySource{store: st, limits: limits, hold: hold}
			f, _ := admitBudgetFixture(t, src, false)

			before := snapshotAll(t, st, client, account, t0)
			m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))

			// The policy decision is the response's own; the estimate is additive.
			if m["decision"] != "allow" || m["account_id"] != account || m["reason"] != "admitted" {
				t.Fatalf("policy decision changed by the estimate: %v", m)
			}
			b := budgetBlock(t, m)
			advisoryAllow, ok := b["allow"].(bool)
			if !ok {
				t.Fatalf("allow = %v, want a bool", b["allow"])
			}
			if b["advisory"] != true || b["reservation_created"] != false {
				t.Errorf("block = %v, want advisory true / reservation_created false", b)
			}
			if b["hold_micros"] != float64(hold) {
				t.Errorf("hold_micros = %v, want %d", b["hold_micros"], hold)
			}
			if got := src.callCount(); got != 4 {
				t.Errorf("store reads = %d, want the four pinned instances", got)
			}
			// No writes: every one of the four instances is untouched by admit.
			assertNoWrites(t, before, snapshotAll(t, st, client, account, t0))

			// The reference admission is the generation's own Gate, with the same
			// copied limits and fixed hold, for the same attempt id/identity/instant.
			gate := budget.NewGate(st, nil, limits, hold)
			gateErr := gate.Reserve(ctx, core.RequestRecord{
				ID:        "gate-parity-" + strings.ReplaceAll(tc.name, " ", "-"),
				Client:    client,
				AccountID: account,
				StartedAt: t0,
			})
			gateAllow := gateErr == nil
			if gateAllow != advisoryAllow {
				t.Fatalf("advisory allow = %v, Gate.Reserve err = %v; the estimate must agree with admission", advisoryAllow, gateErr)
			}
			if !gateAllow && !errors.Is(gateErr, budget.ErrExceeded) {
				t.Errorf("Gate.Reserve denied for a non-ceiling reason: %v", gateErr)
			}
		})
	}
}

// TestAdmitBudgetGateParityReadsChosenIdentityOnly proves the estimate consults
// the durable store for exactly the policy-chosen identity's four instances —
// never a candidate the policy declined — for both a model-routed request and
// an account-only request, and that it invents no failover attempt of its own
// (the policy's Acquire is never called and only one DryRun happens).
func TestAdmitBudgetGateParityReadsChosenIdentityOnly(t *testing.T) {
	const hold = int64(250_000)

	t.Run("model-route-chosen-account", func(t *testing.T) {
		st := openBudgetStore(t)
		ctx := context.Background()
		seedPeriodState(t, st, "tester", "secondary", t0, 100_000, 50_000)
		limits := []budget.Limit{{Scope: budget.ScopeAccount, Key: "secondary", Period: budget.PeriodDay, Micros: 1_000_000}}
		src := &gateParitySource{store: st, limits: limits, hold: hold}
		f, _ := admitBudgetFixture(t, src, false)

		m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))
		b := budgetBlock(t, m)
		if b["client"] != "tester" || b["account_id"] != "secondary" {
			t.Fatalf("block = %v, want tester/secondary", b)
		}
		want := wantAdmitInstances("tester", "secondary")
		if got := src.readKeys(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("reads = %v, want exactly the chosen identity's instances %v", got, want)
		}
		for _, k := range src.readKeys() {
			if strings.Contains(k, "primary") {
				t.Errorf("estimate read the non-chosen candidate %q", k)
			}
		}
		if len(f.policy.dryRuns) != 1 || f.policy.dryRuns[0].class != core.ClassInteractive ||
			strings.Join(f.policy.dryRuns[0].candidates, ",") != "primary,secondary" {
			t.Errorf("dry runs = %+v, want one interactive call over the route's candidates", f.policy.dryRuns)
		}
		if f.policy.acquires != 0 {
			t.Errorf("policy Acquire called %d times; the estimate must not fail over", f.policy.acquires)
		}

		gate := budget.NewGate(st, nil, limits, hold)
		gateErr := gate.Reserve(ctx, core.RequestRecord{ID: "chosen-model", Client: "tester", AccountID: "secondary", StartedAt: t0})
		if allow, _ := b["allow"].(bool); (gateErr == nil) != allow {
			t.Errorf("advisory allow = %v but Gate.Reserve err = %v", b["allow"], gateErr)
		}
	})

	t.Run("account-request-chosen-account", func(t *testing.T) {
		st := openBudgetStore(t)
		ctx := context.Background()
		limits := []budget.Limit{{Scope: budget.ScopeAccount, Key: "expired", Period: budget.PeriodMonth}}
		src := &gateParitySource{store: st, limits: limits, hold: hold}
		f, _ := admitBudgetFixture(t, src, false)
		f.policy.decision = core.Decision{Allow: true, AccountID: "expired", Reason: "admitted"}

		m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"background","account":"expired"}`, secretKey))
		if m["decision"] != "allow" || m["account_id"] != "expired" {
			t.Fatalf("decision = %v, want the policy's allow for expired", m)
		}
		b := budgetBlock(t, m)
		if b["allow"] != false || b["reason"] != "budget_exceeded" {
			t.Errorf("block = %v, want allow false / budget_exceeded (zero account-month ceiling)", b)
		}
		want := wantAdmitInstances("tester", "expired")
		if got := src.readKeys(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("reads = %v, want exactly %v", got, want)
		}

		gate := budget.NewGate(st, nil, limits, hold)
		gateErr := gate.Reserve(ctx, core.RequestRecord{ID: "chosen-account", Client: "tester", AccountID: "expired", StartedAt: t0})
		if !errors.Is(gateErr, budget.ErrExceeded) {
			t.Errorf("Gate.Reserve err = %v, want ErrExceeded for the zero account-month ceiling", gateErr)
		}
	})
}

// TestAdmitBudgetGateParityMissingBearerRealStore drives the estimate with a
// real, reachable store and a fully valid, known generation config — client
// "tester" is registered and account "secondary" is configured — but with no
// caller bearer at all. The estimate must name the failure as an identity
// problem (client_identity_unavailable), never a configured-ceiling denial,
// must leave the policy's allow untouched, must not touch the store, and must
// write nothing.
func TestAdmitBudgetGateParityMissingBearerRealStore(t *testing.T) {
	const hold = int64(250_000)
	st := openBudgetStore(t)
	seedPeriodState(t, st, "tester", "secondary", t0, 100_000, 50_000)
	limits := []budget.Limit{{Scope: budget.ScopeClient, Key: "tester", Period: budget.PeriodDay, Micros: 1_000_000}}
	src := &gateParitySource{store: st, limits: limits, hold: hold}
	f, auth := admitBudgetFixture(t, src, false)

	before := snapshotAll(t, st, "tester", "secondary", t0)
	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, ""))

	if m["decision"] != "allow" || m["account_id"] != "secondary" || m["reason"] != "admitted" {
		t.Fatalf("missing bearer changed the policy decision: %v", m)
	}
	b := budgetBlock(t, m)
	if b["advisory"] != true || b["allow"] != false {
		t.Errorf("block = %v, want advisory true / allow false", b)
	}
	if b["reason"] != "client_identity_unavailable" {
		t.Errorf("reason = %v, want client_identity_unavailable", b["reason"])
	}
	if _, ok := b["client"]; ok {
		t.Errorf("missing bearer must not name a client: %v", b["client"])
	}
	if _, ok := b["account_id"]; ok {
		t.Errorf("missing bearer must not name an account: %v", b["account_id"])
	}
	if *auth != 0 {
		t.Errorf("Authenticate called %d times without a bearer, want 0", *auth)
	}
	if src.callCount() != 0 {
		t.Errorf("store read %d times for an unidentified caller, want 0", src.callCount())
	}
	assertNoWrites(t, before, snapshotAll(t, st, "tester", "secondary", t0))
}

// TestAdmitBudgetGateParityOverflowRealStore covers the arithmetic fail-closed
// path, which is deliberately separate from the ceiling-denial parity above: a
// charge and an open hold whose sum cannot be represented as int64 micro-USD.
// The estimate must refuse to compute (budget_store_error) and the Gate must
// refuse to admit, and neither may report the overflow as an ordinary
// ErrExceeded ceiling denial — it is a broken-money failure, not a budget one.
func TestAdmitBudgetGateParityOverflowRealStore(t *testing.T) {
	const hold = int64(250_000)
	st := openBudgetStore(t)
	ctx := context.Background()
	// reported = 1 and an open hold of MaxInt64: reported+reserved overflows.
	seedPeriodState(t, st, "tester", "secondary", t0, 1, math.MaxInt64)

	limits := []budget.Limit{{Scope: budget.ScopeClient, Key: "tester", Period: budget.PeriodDay, Micros: math.MaxInt64}}
	src := &gateParitySource{store: st, limits: limits, hold: hold}
	f, _ := admitBudgetFixture(t, src, false)

	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, secretKey))
	if m["decision"] != "allow" {
		t.Fatalf("decision = %v, want the policy decision untouched", m["decision"])
	}
	b := budgetBlock(t, m)
	if b["allow"] != false || b["reason"] != "budget_store_error" {
		t.Errorf("block = %v, want allow false / budget_store_error on an unrepresentable amount", b)
	}

	gate := budget.NewGate(st, nil, limits, hold)
	gateErr := gate.Reserve(ctx, core.RequestRecord{ID: "gate-overflow", Client: "tester", AccountID: "secondary", StartedAt: t0})
	if gateErr == nil {
		t.Fatalf("Gate.Reserve admitted an overflowing reservation; parity broken")
	}
	if errors.Is(gateErr, budget.ErrExceeded) {
		t.Errorf("overflow surfaced as a ceiling denial (%v); it is an arithmetic fail-closed path", gateErr)
	}
}
