package budget

// TDD tests for the durable spend-control storage primitive (store.go).
//
// Contract under test (all types owned by store.go):
//
//	Limit       {Scope, Key, Period string; Micros int64}
//	Reservation {ID, Client, Account string; At time.Time; Micros int64}
//	Settlement  {ID string; Micros int64; Basis string}
//	Snapshot    {Reported, Estimated, Unknown, Reserved int64}
//	Open(path) (*Store, error)
//	(*Store) Reserve(ctx, Reservation, []Limit) error
//	(*Store) Settle(ctx, Settlement) error
//	(*Store) Snapshot(ctx, scope, key, period string, at time.Time) (Snapshot, error)
//	(*Store) ReconcileOrphans(ctx) error
//	(*Store) Close() error
//
// Everything is integer micro-USD; no comparison in the store uses float64.
// The store is standalone: it opens its own SQLite file at the explicit path
// it is given and is not wired into config, app, core, or the ledger.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "budget.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// day / month helpers keep period-instance arithmetic explicit in tests.
func day(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func monthStart(y int, m time.Month) time.Time {
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

func limit(scope, key, period string, micros int64) Limit {
	return Limit{Scope: scope, Key: key, Period: period, Micros: micros}
}

// TestOpenCreatesSchema pins the physical contract: a WAL SQLite database,
// 0600 file / 0700 dir, the three budget tables, and an empty path rejected.
func TestOpenCreatesSchema(t *testing.T) {
	s, path := openTestStore(t)

	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"budget_periods", "budget_reservation_periods", "budget_reservations", "schema_version"}
	if !reflect.DeepEqual(tables, want) {
		t.Fatalf("tables = %v, want %v", tables, want)
	}

	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("db mode = %v, want 0600", st.Mode().Perm())
	}
	dst, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dst.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", dst.Mode().Perm())
	}

	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") = nil error, want rejection")
	}
}

// TestOpenDoesNotReconcileOrphans is the explicit contract that Open never
// auto-reconciles: an existing database with an open reservation must still
// show that reservation open (Reserved unchanged) after a reopen.
func TestOpenDoesNotReconcileOrphans(t *testing.T) {
	s, path := openTestStore(t)
	at := day(2026, time.March, 10, 12, 0)
	if err := s.Reserve(context.Background(), Reservation{ID: "open-1", Client: "c", Account: "a", At: at, Micros: 500}, []Limit{
		limit(ScopeClient, "c", PeriodDay, 10_000),
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	snap, err := s2.Snapshot(context.Background(), ScopeClient, "c", PeriodDay, at)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Reserved != 500 || snap.Unknown != 0 {
		t.Fatalf("after reopen without ReconcileOrphans: %+v, want Reserved=500 Unknown=0", snap)
	}
}

// TestReserveChargesAllApplicableScopes covers a positive reserve against
// BOTH client and account, day and month: every applicable period row shows
// the reservation in Reserved while actual spend stays zero.
func TestReserveChargesAllApplicableScopes(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	limits := []Limit{
		limit(ScopeClient, "hermes", PeriodDay, 5_000_000),
		limit(ScopeClient, "hermes", PeriodMonth, 50_000_000),
		limit(ScopeAccount, "openrouter-1", PeriodDay, 20_000_000),
		limit(ScopeAccount, "openrouter-1", PeriodMonth, 200_000_000),
	}
	if err := s.Reserve(ctx, Reservation{ID: "r1", Client: "hermes", Account: "openrouter-1", At: at, Micros: 750_000}, limits); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ scope, key, period string }{
		{ScopeClient, "hermes", PeriodDay},
		{ScopeClient, "hermes", PeriodMonth},
		{ScopeAccount, "openrouter-1", PeriodDay},
		{ScopeAccount, "openrouter-1", PeriodMonth},
	} {
		snap, err := s.Snapshot(ctx, tc.scope, tc.key, tc.period, at)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Reserved != 750_000 || snap.Reported != 0 || snap.Estimated != 0 || snap.Unknown != 0 {
			t.Fatalf("%s/%s/%s = %+v, want Reserved=750000 only", tc.scope, tc.key, tc.period, snap)
		}
	}
	// A period instance with no row reports an all-zero snapshot.
	snap, err := s.Snapshot(ctx, ScopeClient, "hermes", PeriodDay, day(2026, time.April, 1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if snap != (Snapshot{}) {
		t.Fatalf("absent period = %+v, want zero", snap)
	}
}

// TestReserveIdempotentAndConflictingRetry pins attempt-ID idempotency: an
// identical retry does not double-reserve, a conflicting payload is an error.
func TestReserveIdempotentAndConflictingRetry(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	limits := []Limit{limit(ScopeClient, "c", PeriodDay, 1_000)}
	res := Reservation{ID: "attempt-9", Client: "c", Account: "a", At: at, Micros: 250}

	if err := s.Reserve(ctx, res, limits); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve(ctx, res, limits); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	snap, err := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Reserved != 250 {
		t.Fatalf("after identical retry Reserved = %d, want 250 (no double reserve)", snap.Reserved)
	}

	// Idempotent even when the retry carries tighter limits: the first
	// admission already happened, a retry is not a new admission.
	if err := s.Reserve(ctx, res, []Limit{limit(ScopeClient, "c", PeriodDay, 1)}); err != nil {
		t.Fatalf("identical retry under tighter limits: %v", err)
	}

	// A conflicting payload for the same attempt id is rejected.
	conflicting := res
	conflicting.Micros = 999
	if err := s.Reserve(ctx, conflicting, limits); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting reserve error = %v, want ErrConflict", err)
	}
	conflicting = res
	conflicting.At = at.Add(time.Hour)
	if err := s.Reserve(ctx, conflicting, limits); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting at error = %v, want ErrConflict", err)
	}

	// The rejected retry left state untouched.
	snap, _ = s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reserved != 250 {
		t.Fatalf("after conflict Reserved = %d, want 250", snap.Reserved)
	}
}

// TestZeroLimitDeniesExplicitly pins the "configured zero budget means DENY"
// rule: a limit row with Micros == 0 admits nothing, even a 1-micro reserve.
func TestZeroLimitDeniesExplicitly(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	err := s.Reserve(ctx, Reservation{ID: "z1", Client: "c", Account: "a", At: at, Micros: 1}, []Limit{
		limit(ScopeClient, "c", PeriodDay, 0),
	})
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("zero-limit reserve error = %v, want ErrExceeded", err)
	}
	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap != (Snapshot{}) {
		t.Fatalf("denied reserve left state %+v, want zero", snap)
	}
}

// TestAbsentLimitIsUnlimited pins the "absence means unlimited" rule: limits
// that name a different key, or are not supplied at all, do not gate.
func TestAbsentLimitIsUnlimited(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)

	// No limits at all.
	if err := s.Reserve(ctx, Reservation{ID: "u1", Client: "c", Account: "a", At: at, Micros: 1 << 40}, nil); err != nil {
		t.Fatalf("no limits: %v", err)
	}
	// A limit for another key does not apply.
	if err := s.Reserve(ctx, Reservation{ID: "u2", Client: "c", Account: "a", At: at, Micros: 1 << 40}, []Limit{
		limit(ScopeClient, "someone-else", PeriodDay, 0),
		limit(ScopeAccount, "other-account", PeriodDay, 0),
	}); err != nil {
		t.Fatalf("non-applicable limits: %v", err)
	}
}

// TestClientDenyDoesNotChargeAccount pins all-or-nothing across scopes: when
// the client ceiling fails, the account budget is not touched at all.
func TestClientDenyDoesNotChargeAccount(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	err := s.Reserve(ctx, Reservation{ID: "d1", Client: "c", Account: "a", At: at, Micros: 100}, []Limit{
		limit(ScopeClient, "c", PeriodDay, 50),     // too small
		limit(ScopeAccount, "a", PeriodDay, 1<<40), // plenty
	})
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("client-denied reserve error = %v, want ErrExceeded", err)
	}
	for _, tc := range []struct{ scope, key string }{{ScopeClient, "c"}, {ScopeAccount, "a"}} {
		snap, err := s.Snapshot(ctx, tc.scope, tc.key, PeriodDay, at)
		if err != nil {
			t.Fatal(err)
		}
		if snap != (Snapshot{}) {
			t.Fatalf("%s/%s = %+v after deny, want untouched zero", tc.scope, tc.key, snap)
		}
	}
}

// TestAccountDenyDoesNotPartiallyWriteClient is the mirror: when the account
// ceiling (checked after the client one) fails, the client row is rolled back.
func TestAccountDenyDoesNotPartiallyWriteClient(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	err := s.Reserve(ctx, Reservation{ID: "d2", Client: "c", Account: "a", At: at, Micros: 100}, []Limit{
		limit(ScopeClient, "c", PeriodDay, 1<<40), // plenty
		limit(ScopeAccount, "a", PeriodDay, 50),   // too small
	})
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("account-denied reserve error = %v, want ErrExceeded", err)
	}
	for _, tc := range []struct{ scope, key string }{{ScopeClient, "c"}, {ScopeAccount, "a"}} {
		snap, err := s.Snapshot(ctx, tc.scope, tc.key, PeriodDay, at)
		if err != nil {
			t.Fatal(err)
		}
		if snap != (Snapshot{}) {
			t.Fatalf("%s/%s = %+v after deny, want untouched zero", tc.scope, tc.key, snap)
		}
	}
}

// TestSettleThreeBases covers the reported / estimated / unknown basis split,
// the release of reserved as spend is booked, and the zero-actual case.
func TestSettleThreeBases(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	lim := []Limit{limit(ScopeClient, "c", PeriodDay, 10_000)}

	cases := []struct {
		id     string
		basis  string
		charge int64
	}{
		{"s-reported", BasisReported, 40},
		{"s-estimated", BasisEstimated, 25},
		// Unknown is floored at the held reservation, so the charge equals the
		// 100 hold rather than a smaller observation (see the dedicated floor
		// regression test).
		{"s-unknown", BasisUnknown, 100},
		{"s-zero", BasisReported, 0}, // zero actual is valid
	}
	for _, tc := range cases {
		if err := s.Reserve(ctx, Reservation{ID: tc.id, Client: "c", Account: "a", At: at, Micros: 100}, lim); err != nil {
			t.Fatalf("reserve %s: %v", tc.id, err)
		}
		if err := s.Settle(ctx, Settlement{ID: tc.id, Micros: tc.charge, Basis: tc.basis}); err != nil {
			t.Fatalf("settle %s: %v", tc.id, err)
		}
	}
	snap, err := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if err != nil {
		t.Fatal(err)
	}
	want := Snapshot{Reported: 40, Estimated: 25, Unknown: 100, Reserved: 0}
	if snap != want {
		t.Fatalf("snapshot = %+v, want %+v", snap, want)
	}
}

// TestSettleIdempotentAndConflicting pins settlement idempotency: an identical
// second settle is a no-op, a different payload is rejected, and neither
// double-charges.
func TestSettleIdempotentAndConflicting(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	if err := s.Reserve(ctx, Reservation{ID: "x", Client: "c", Account: "a", At: at, Micros: 100},
		[]Limit{limit(ScopeClient, "c", PeriodDay, 10_000)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, Settlement{ID: "x", Micros: 60, Basis: BasisReported}); err != nil {
		t.Fatal(err)
	}
	// Identical retry: no double charge.
	if err := s.Settle(ctx, Settlement{ID: "x", Micros: 60, Basis: BasisReported}); err != nil {
		t.Fatalf("identical settle retry: %v", err)
	}
	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reported != 60 || snap.Reserved != 0 {
		t.Fatalf("after idempotent settle = %+v, want Reported=60 Reserved=0", snap)
	}
	// Conflicting payload: rejected, no change.
	if err := s.Settle(ctx, Settlement{ID: "x", Micros: 61, Basis: BasisReported}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting settle error = %v, want ErrConflict", err)
	}
	if err := s.Settle(ctx, Settlement{ID: "x", Micros: 60, Basis: BasisEstimated}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting basis error = %v, want ErrConflict", err)
	}
	snap, _ = s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reported != 60 || snap.Estimated != 0 {
		t.Fatalf("after conflicting settles = %+v, want unchanged", snap)
	}
	// Unknown reservation id.
	if err := s.Settle(ctx, Settlement{ID: "nope", Micros: 1, Basis: BasisReported}); !errors.Is(err, ErrUnknownReservation) {
		t.Fatalf("unknown settle error = %v, want ErrUnknownReservation", err)
	}
}

// TestOverrunChargesFullObservedCost pins the no-clamp rule: when the observed
// settlement exceeds the reservation, the full observed cost is booked and the
// snapshot may exceed the limit.
func TestOverrunChargesFullObservedCost(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	limits := []Limit{
		limit(ScopeClient, "c", PeriodDay, 1_000),
		limit(ScopeAccount, "a", PeriodDay, 1_000),
	}
	if err := s.Reserve(ctx, Reservation{ID: "o1", Client: "c", Account: "a", At: at, Micros: 300}, limits); err != nil {
		t.Fatal(err)
	}
	// Observed cost 900 exceeds the 300 reservation but not the 1000 limit here.
	if err := s.Settle(ctx, Settlement{ID: "o1", Micros: 900, Basis: BasisReported}); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reported != 900 || snap.Reserved != 0 {
		t.Fatalf("overrun snapshot = %+v, want Reported=900 Reserved=0", snap)
	}

	// A second reservation is admitted against the now-900 spent; its
	// settlement can push the total past the limit, and it is NOT clamped.
	if err := s.Reserve(ctx, Reservation{ID: "o2", Client: "c", Account: "a", At: at, Micros: 100}, limits); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, Settlement{ID: "o2", Micros: 5_000, Basis: BasisEstimated}); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reported != 900 || snap.Estimated != 5_000 {
		t.Fatalf("clamped snapshot = %+v, want full charges 900 + 5000", snap)
	}
	if snap.Reported+snap.Estimated <= 1_000 {
		t.Fatalf("total spend %d did not exceed the 1000 limit; overrun must not clamp", snap.Reported+snap.Estimated)
	}
}

// TestUnknownSettlementIsNeverFree pins that an open reservation settled with
// basis unknown is charged at whatever ceiling the caller passes (the store
// never invents a zero for an unknown charge it was asked to record).
func TestUnknownSettlementIsNeverFree(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	if err := s.Reserve(ctx, Reservation{ID: "u", Client: "c", Account: "a", At: at, Micros: 400},
		[]Limit{limit(ScopeClient, "c", PeriodDay, 10_000)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, Settlement{ID: "u", Micros: 400, Basis: BasisUnknown}); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Unknown != 400 || snap.Reserved != 0 {
		t.Fatalf("unknown-ceiling snapshot = %+v, want Unknown=400 Reserved=0", snap)
	}
}

// TestSettleUsesOriginalPeriodBucket pins attribution: a reservation opened in
// one day (and month) instance always settles into that same instance, even
// though settlement time is unrelated to the attempt timestamp.
func TestSettleUsesOriginalPeriodBucket(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 31, 23, 30)
	limits := []Limit{
		limit(ScopeClient, "c", PeriodDay, 10_000),
		limit(ScopeClient, "c", PeriodMonth, 10_000),
	}
	if err := s.Reserve(ctx, Reservation{ID: "late", Client: "c", Account: "a", At: at, Micros: 500}, limits); err != nil {
		t.Fatal(err)
	}
	// Settle happens "in April" from the store's perspective, but no time is
	// passed: the pinned March 31 day bucket and March month bucket are used.
	if err := s.Settle(ctx, Settlement{ID: "late", Micros: 300, Basis: BasisReported}); err != nil {
		t.Fatal(err)
	}
	daySnap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if daySnap.Reported != 300 || daySnap.Reserved != 0 {
		t.Fatalf("original day bucket = %+v, want Reported=300 Reserved=0", daySnap)
	}
	monthSnap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodMonth, at)
	if monthSnap.Reported != 300 || monthSnap.Reserved != 0 {
		t.Fatalf("original month bucket = %+v, want Reported=300 Reserved=0", monthSnap)
	}
	// The next day's bucket is untouched.
	nextDay, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, day(2026, time.April, 1, 0, 30))
	if nextDay != (Snapshot{}) {
		t.Fatalf("rolled-forward bucket = %+v, want zero", nextDay)
	}
}

// TestReopenPreservesReservations pins durability: reserved state survives a
// close/reopen, and the re-opened store can still settle the reservation.
func TestReopenPreservesReservations(t *testing.T) {
	s, path := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	if err := s.Reserve(ctx, Reservation{ID: "persist", Client: "c", Account: "a", At: at, Micros: 700},
		[]Limit{limit(ScopeClient, "c", PeriodDay, 10_000)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	snap, _ := s2.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reserved != 700 {
		t.Fatalf("reopened Reserved = %d, want 700", snap.Reserved)
	}
	if err := s2.Settle(ctx, Settlement{ID: "persist", Micros: 700, Basis: BasisUnknown}); err != nil {
		t.Fatalf("settle after reopen: %v", err)
	}
	snap, _ = s2.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Unknown != 700 || snap.Reserved != 0 {
		t.Fatalf("after reopen settle = %+v, want Unknown=700 Reserved=0", snap)
	}
}

// TestReconcileOrphansChargesUnknownCeiling pins the explicit-startup orphan
// path: open reservations settle at their reserved ceiling with basis unknown,
// the call is idempotent, and it is never run implicitly by Open.
func TestReconcileOrphansChargesUnknownCeiling(t *testing.T) {
	s, path := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	limits := []Limit{
		limit(ScopeClient, "c", PeriodDay, 10_000),
		limit(ScopeAccount, "a", PeriodDay, 10_000),
	}
	for _, id := range []string{"orphan-1", "orphan-2"} {
		if err := s.Reserve(ctx, Reservation{ID: id, Client: "c", Account: "a", At: at, Micros: 250}, limits); err != nil {
			t.Fatal(err)
		}
	}
	// A settled reservation must be left alone by reconciliation.
	if err := s.Reserve(ctx, Reservation{ID: "done", Client: "c", Account: "a", At: at, Micros: 100}, limits); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, Settlement{ID: "done", Micros: 100, Basis: BasisReported}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.ReconcileOrphans(ctx); err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	daySnap, _ := s2.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	// Two orphans charged 250 each at the ceiling; the settled row keeps its
	// reported charge and is not double counted.
	if daySnap.Unknown != 500 || daySnap.Reported != 100 || daySnap.Reserved != 0 {
		t.Fatalf("after reconcile = %+v, want Unknown=500 Reported=100 Reserved=0", daySnap)
	}
	accSnap, _ := s2.Snapshot(ctx, ScopeAccount, "a", PeriodDay, at)
	if accSnap.Unknown != 500 || accSnap.Reserved != 0 {
		t.Fatalf("account after reconcile = %+v, want Unknown=500 Reserved=0", accSnap)
	}

	// Idempotent: a second call changes nothing.
	if err := s2.ReconcileOrphans(ctx); err != nil {
		t.Fatalf("second ReconcileOrphans: %v", err)
	}
	daySnap2, _ := s2.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if daySnap2 != daySnap {
		t.Fatalf("second reconcile changed state: %+v -> %+v", daySnap, daySnap2)
	}
}

// TestReconcileOrphansNoOrphans is a cheap guard that an empty store is fine.
func TestReconcileOrphansNoOrphans(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.ReconcileOrphans(context.Background()); err != nil {
		t.Fatalf("ReconcileOrphans on empty store: %v", err)
	}
}

// TestLimitLoweringNoGrandfather pins §4.9: an already-created reservation
// keeps counting against its period, and a later admission is evaluated
// against the CURRENT (lowered) limit and denied once outstanding state
// reaches it.
func TestLimitLoweringNoGrandfather(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)

	high := []Limit{limit(ScopeClient, "c", PeriodDay, 1_000)}
	if err := s.Reserve(ctx, Reservation{ID: "first", Client: "c", Account: "a", At: at, Micros: 600}, high); err != nil {
		t.Fatal(err)
	}
	// Lower the limit below the outstanding reservation.
	low := []Limit{limit(ScopeClient, "c", PeriodDay, 500)}
	if err := s.Reserve(ctx, Reservation{ID: "second", Client: "c", Account: "a", At: at, Micros: 10}, low); !errors.Is(err, ErrExceeded) {
		t.Fatalf("post-lowering reserve error = %v, want ErrExceeded", err)
	}
	// The outstanding reservation still counts (not retroactively voided).
	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reserved != 600 {
		t.Fatalf("after lowering Reserved = %d, want 600", snap.Reserved)
	}
	// No grandfather loophole: outstanding (600) already exceeds the lowered
	// limit (500), so every further attempt is denied regardless of its size.
	if err := s.Reserve(ctx, Reservation{ID: "third", Client: "c", Account: "a", At: at, Micros: 400}, low); !errors.Is(err, ErrExceeded) {
		t.Fatalf("second post-lowering reserve error = %v, want ErrExceeded", err)
	}
	// The lowered limit is the operative ceiling on a fresh instance of the
	// period: 400 fits under 500, 600 does not.
	next := day(2026, time.March, 16, 9, 30)
	if err := s.Reserve(ctx, Reservation{ID: "fresh-fits", Client: "c", Account: "a", At: next, Micros: 400}, low); err != nil {
		t.Fatalf("fresh-period reserve under lowered limit: %v", err)
	}
	if err := s.Reserve(ctx, Reservation{ID: "fresh-over", Client: "c", Account: "a", At: next, Micros: 600}, low); !errors.Is(err, ErrExceeded) {
		t.Fatalf("fresh-period over-limit reserve error = %v, want ErrExceeded", err)
	}
}

// TestIntegerOverflowChecked pins fail-closed checked arithmetic: pushing the
// reserved total past int64 is an error, never a wrap, and never a float
// comparison.
func TestIntegerOverflowChecked(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	maxLim := []Limit{limit(ScopeClient, "c", PeriodDay, math.MaxInt64)}

	if err := s.Reserve(ctx, Reservation{ID: "big", Client: "c", Account: "a", At: at, Micros: math.MaxInt64}, maxLim); err != nil {
		t.Fatalf("reserve at exact ceiling: %v", err)
	}
	// One more micro must overflow the checked sum, not wrap to negative.
	err := s.Reserve(ctx, Reservation{ID: "over", Client: "c", Account: "a", At: at, Micros: 1}, maxLim)
	if err == nil {
		t.Fatal("overflowing reserve = nil error, want checked overflow error")
	}
	if errors.Is(err, ErrExceeded) {
		t.Fatalf("overflow reported as ErrExceeded (%v); want a distinct arithmetic error", err)
	}
	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reserved != math.MaxInt64 {
		t.Fatalf("after overflow Reserved = %d, want MaxInt64 (unchanged)", snap.Reserved)
	}

	// Settlement overflow is checked too, and a rolled-back settlement leaves
	// its reservation open. Work in a fresh period instance so the counters
	// start clean.
	at2 := day(2026, time.April, 5, 12, 0)
	lim2 := []Limit{limit(ScopeClient, "c", PeriodDay, math.MaxInt64)}
	if err := s.Reserve(ctx, Reservation{ID: "s1", Client: "c", Account: "a", At: at2, Micros: 10}, lim2); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve(ctx, Reservation{ID: "s2", Client: "c", Account: "a", At: at2, Micros: 10}, lim2); err != nil {
		t.Fatal(err)
	}
	// Push spent_reported to the exact ceiling through s2.
	if err := s.Settle(ctx, Settlement{ID: "s2", Micros: math.MaxInt64, Basis: BasisReported}); err != nil {
		t.Fatalf("settle at exact ceiling: %v", err)
	}
	// Charging s1 on top of the ceiling overflows and must be rejected.
	err = s.Settle(ctx, Settlement{ID: "s1", Micros: 10, Basis: BasisReported})
	if err == nil {
		t.Fatal("overflowing settlement = nil error, want checked overflow error")
	}
	if errors.Is(err, ErrExceeded) {
		t.Fatalf("settle overflow reported as ErrExceeded (%v); want a distinct arithmetic error", err)
	}
	// s1 stays open because the settle transaction rolled back.
	snap, _ = s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at2)
	if snap.Reported != math.MaxInt64 || snap.Reserved != 10 {
		t.Fatalf("after settle overflow = %+v, want Reported=MaxInt64 Reserved=10 (s1 still open)", snap)
	}
}

// TestValidation pins the string and argument validation: scope, period, and
// basis domains, plus structural argument checks.
func TestValidation(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)

	bad := []struct {
		name string
		fn   func() error
	}{
		{"empty-id", func() error {
			return s.Reserve(ctx, Reservation{Client: "c", Account: "a", At: at, Micros: 1}, nil)
		}},
		{"empty-client", func() error {
			return s.Reserve(ctx, Reservation{ID: "v", Account: "a", At: at, Micros: 1}, nil)
		}},
		{"empty-account", func() error {
			return s.Reserve(ctx, Reservation{ID: "v", Client: "c", At: at, Micros: 1}, nil)
		}},
		{"zero-at", func() error {
			return s.Reserve(ctx, Reservation{ID: "v", Client: "c", Account: "a", Micros: 1}, nil)
		}},
		{"negative-micros", func() error {
			return s.Reserve(ctx, Reservation{ID: "v", Client: "c", Account: "a", At: at, Micros: -1}, nil)
		}},
		{"zero-micros", func() error {
			return s.Reserve(ctx, Reservation{ID: "v", Client: "c", Account: "a", At: at, Micros: 0}, nil)
		}},
		{"bad-scope", func() error {
			return s.Reserve(ctx, Reservation{ID: "v", Client: "c", Account: "a", At: at, Micros: 1},
				[]Limit{limit("team", "c", PeriodDay, 100)})
		}},
		{"bad-period", func() error {
			return s.Reserve(ctx, Reservation{ID: "v", Client: "c", Account: "a", At: at, Micros: 1},
				[]Limit{limit(ScopeClient, "c", "week", 100)})
		}},
		{"negative-limit", func() error {
			return s.Reserve(ctx, Reservation{ID: "v", Client: "c", Account: "a", At: at, Micros: 1},
				[]Limit{limit(ScopeClient, "c", PeriodDay, -1)})
		}},
		{"empty-limit-key", func() error {
			return s.Reserve(ctx, Reservation{ID: "v", Client: "c", Account: "a", At: at, Micros: 1},
				[]Limit{limit(ScopeClient, "", PeriodDay, 100)})
		}},
		{"duplicate-limit", func() error {
			return s.Reserve(ctx, Reservation{ID: "v", Client: "c", Account: "a", At: at, Micros: 1},
				[]Limit{limit(ScopeClient, "c", PeriodDay, 100), limit(ScopeClient, "c", PeriodDay, 200)})
		}},
		{"settle-empty-id", func() error {
			return s.Settle(ctx, Settlement{Micros: 1, Basis: BasisReported})
		}},
		{"settle-negative", func() error {
			return s.Settle(ctx, Settlement{ID: "v", Micros: -1, Basis: BasisReported})
		}},
		{"settle-bad-basis", func() error {
			return s.Settle(ctx, Settlement{ID: "v", Micros: 1, Basis: "guess"})
		}},
		{"snapshot-bad-scope", func() error {
			_, err := s.Snapshot(ctx, "team", "c", PeriodDay, at)
			return err
		}},
		{"snapshot-bad-period", func() error {
			_, err := s.Snapshot(ctx, ScopeClient, "c", "week", at)
			return err
		}},
		{"snapshot-empty-key", func() error {
			_, err := s.Snapshot(ctx, ScopeClient, "", PeriodDay, at)
			return err
		}},
		{"snapshot-zero-at", func() error {
			_, err := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, time.Time{})
			return err
		}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err == nil {
				t.Fatalf("%s: nil error, want validation failure", tc.name)
			}
		})
	}
}

// TestConcurrentSaturationTenReservations is the load-bearing concurrency
// test: a period limit admits exactly N reservations under contention.
// Serialized Reserve transactions mean exactly ten 100-micro reservations fit
// a 1000-micro ceiling; the rest are denied with ErrExceeded, and no other
// error (including SQLITE_BUSY) may surface.
func TestConcurrentSaturationTenReservations(t *testing.T) {
	s, _ := openTestStore(t)
	at := day(2026, time.March, 15, 9, 30)
	const (
		unit       = 100
		capacity   = 10
		contenders = 60
	)
	limits := []Limit{limit(ScopeClient, "c", PeriodDay, unit*capacity)}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		admitted  int
		denied    int
		otherErrs []error
	)
	start := make(chan struct{})
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := s.Reserve(context.Background(), Reservation{
				ID:      fmt.Sprintf("c-%02d", i),
				Client:  "c",
				Account: "a",
				At:      at,
				Micros:  unit,
			}, limits)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted++
			case errors.Is(err, ErrExceeded):
				denied++
			default:
				otherErrs = append(otherErrs, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(otherErrs) != 0 {
		t.Fatalf("unexpected errors under contention: %v", otherErrs)
	}
	if admitted != capacity {
		t.Fatalf("admitted = %d, want exactly %d", admitted, capacity)
	}
	if denied != contenders-capacity {
		t.Fatalf("denied = %d, want %d", denied, contenders-capacity)
	}
	snap, err := s.Snapshot(context.Background(), ScopeClient, "c", PeriodDay, at)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Reserved != unit*capacity {
		t.Fatalf("Reserved = %d, want %d", snap.Reserved, unit*capacity)
	}
}

// TestConcurrentDistinctKeysReserveIndependently checks that serialization does
// not couple unrelated budgets.
func TestConcurrentDistinctKeysReserveIndependently(t *testing.T) {
	s, _ := openTestStore(t)
	at := day(2026, time.March, 15, 9, 30)
	const keys = 12
	var wg sync.WaitGroup
	errs := make([]error, keys)
	for i := 0; i < keys; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("client-%02d", i)
			errs[i] = s.Reserve(context.Background(), Reservation{
				ID:      fmt.Sprintf("id-%02d", i),
				Client:  key,
				Account: "a",
				At:      at,
				Micros:  100,
			}, []Limit{limit(ScopeClient, key, PeriodDay, 100)})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	var ids []string
	rows, err := s.db.Query(`SELECT key FROM budget_periods WHERE scope=? AND period=? ORDER BY key`, ScopeClient, PeriodDay)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, k)
	}
	if len(ids) != keys {
		t.Fatalf("period rows = %v, want %d independent keys", ids, keys)
	}
	want := make([]string, keys)
	for i := range want {
		want[i] = fmt.Sprintf("client-%02d", i)
	}
	sort.Strings(want)
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("keys = %v, want %v", ids, want)
	}
}

// TestConcurrencyDoesNotLoseReservedUnderSettlement mixes reserves and settles
// concurrently to shake out transaction interleaving.
func TestConcurrencyDoesNotLoseReservedUnderSettlement(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	limits := []Limit{limit(ScopeClient, "c", PeriodDay, 1<<40)}

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("mix-%02d", i)
			if err := s.Reserve(ctx, Reservation{ID: id, Client: "c", Account: "a", At: at, Micros: 10}, limits); err != nil {
				t.Errorf("reserve %s: %v", id, err)
				return
			}
			if err := s.Settle(ctx, Settlement{ID: id, Micros: 10, Basis: BasisEstimated}); err != nil {
				t.Errorf("settle %s: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
	snap, err := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Estimated != n*10 || snap.Reserved != 0 {
		t.Fatalf("mixed snapshot = %+v, want Estimated=%d Reserved=0", snap, n*10)
	}
}

// TestReserveRequiresPositiveMicros pins the admission floor: a reservation of
// zero or negative micro-USD is ErrInvalid, whether the budget is bounded or
// unlimited, and it writes nothing (a zero reservation could otherwise release
// a hold for free or create a no-op row).
func TestReserveRequiresPositiveMicros(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	for _, micros := range []int64{0, -1} {
		if err := s.Reserve(ctx, Reservation{
			ID: fmt.Sprintf("bad-%d", micros), Client: "c", Account: "a", At: at, Micros: micros,
		}, []Limit{limit(ScopeClient, "c", PeriodDay, 10_000)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bounded reserve micros=%d error = %v, want ErrInvalid", micros, err)
		}
		if err := s.Reserve(ctx, Reservation{
			ID: fmt.Sprintf("bad-unlim-%d", micros), Client: "c", Account: "a", At: at, Micros: micros,
		}, nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unlimited reserve micros=%d error = %v, want ErrInvalid", micros, err)
		}
	}
	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap != (Snapshot{}) {
		t.Fatalf("rejected zero/negative reserves left state %+v, want zero", snap)
	}
	// The smallest admissible reservation is still accepted.
	if err := s.Reserve(ctx, Reservation{ID: "one", Client: "c", Account: "a", At: at, Micros: 1}, nil); err != nil {
		t.Fatalf("reserve micros=1: %v", err)
	}
}

// TestUnknownZeroSettlementNeverReleasesTheHold pins the store-held floor for
// an unknown outcome reported as zero: the reservation's hold is charged with
// basis unknown, never released for free, and the retry stays idempotent on the
// normalized payload while any different normalized charge conflicts.
func TestUnknownZeroSettlementNeverReleasesTheHold(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	if err := s.Reserve(ctx, Reservation{ID: "uz", Client: "c", Account: "a", At: at, Micros: 400},
		[]Limit{limit(ScopeClient, "c", PeriodDay, 10_000)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, Settlement{ID: "uz", Micros: 0, Basis: BasisUnknown}); err != nil {
		t.Fatalf("unknown-zero settle: %v", err)
	}
	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Unknown != 400 || snap.Reserved != 0 {
		t.Fatalf("unknown-zero snapshot = %+v, want Unknown=400 Reserved=0", snap)
	}
	// Identical (normalized) retry: no second charge.
	if err := s.Settle(ctx, Settlement{ID: "uz", Micros: 0, Basis: BasisUnknown}); err != nil {
		t.Fatalf("unknown-zero retry: %v", err)
	}
	// A retry whose normalized charge differs conflicts.
	if err := s.Settle(ctx, Settlement{ID: "uz", Micros: 500, Basis: BasisUnknown}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting unknown retry = %v, want ErrConflict", err)
	}
	snap, _ = s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Unknown != 400 || snap.Reserved != 0 {
		t.Fatalf("after conflicting retry = %+v, want unchanged Unknown=400 Reserved=0", snap)
	}
}

// TestUnknownSettlementFloorsAtHeldReservation pins that an unknown observation
// below the hold is floored at the hold, while reported and estimated bookings
// keep their full (possibly smaller) observation.
func TestUnknownSettlementFloorsAtHeldReservation(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	lim := []Limit{limit(ScopeClient, "c", PeriodDay, 10_000)}

	if err := s.Reserve(ctx, Reservation{ID: "fl", Client: "c", Account: "a", At: at, Micros: 400}, lim); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, Settlement{ID: "fl", Micros: 150, Basis: BasisUnknown}); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Unknown != 400 || snap.Reserved != 0 {
		t.Fatalf("floored unknown snapshot = %+v, want Unknown=400 Reserved=0", snap)
	}

	// The same observation under a reported basis is not floored.
	if err := s.Reserve(ctx, Reservation{ID: "rep", Client: "c", Account: "a", At: at, Micros: 400}, lim); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, Settlement{ID: "rep", Micros: 150, Basis: BasisReported}); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reported != 150 || snap.Unknown != 400 || snap.Reserved != 0 {
		t.Fatalf("reported snapshot = %+v, want Reported=150 Unknown=400 Reserved=0", snap)
	}
}

// TestReserveTracksAllFourInstancesWithoutLimits pins that every admitted
// attempt tracks the client AND account budget for the day AND month instance
// even when no limit is configured for some of them, so a limit added later is
// evaluated against the real history of the period.
func TestReserveTracksAllFourInstancesWithoutLimits(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	// No limits at all: unlimited admission, but still fully tracked.
	if err := s.Reserve(ctx, Reservation{ID: "track", Client: "c", Account: "a", At: at, Micros: 1_234}, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ scope, key, period string }{
		{ScopeClient, "c", PeriodDay},
		{ScopeClient, "c", PeriodMonth},
		{ScopeAccount, "a", PeriodDay},
		{ScopeAccount, "a", PeriodMonth},
	} {
		snap, err := s.Snapshot(ctx, tc.scope, tc.key, tc.period, at)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Reserved != 1_234 {
			t.Fatalf("%s/%s/%s Reserved = %d, want 1234 (tracked even without a limit)",
				tc.scope, tc.key, tc.period, snap.Reserved)
		}
	}
}

// TestUnlimitedHistoryThenNewLimitDenies pins the honest-history rule: spend
// admitted while a budget was unlimited still counts, so a limit configured
// afterwards is evaluated against it and denies the next attempt.
func TestUnlimitedHistoryThenNewLimitDenies(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)

	// Admitted while the budget is unlimited; the hold is still tracked.
	if err := s.Reserve(ctx, Reservation{ID: "hist", Client: "c", Account: "a", At: at, Micros: 1_000_000}, nil); err != nil {
		t.Fatal(err)
	}
	// A day limit configured afterwards: the outstanding million exceeds it.
	err := s.Reserve(ctx, Reservation{ID: "after", Client: "c", Account: "a", At: at, Micros: 1},
		[]Limit{limit(ScopeClient, "c", PeriodDay, 500_000)})
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("reserve under a later day limit = %v, want ErrExceeded", err)
	}
	// The month instance was tracked too, so a later month limit is equally real.
	err = s.Reserve(ctx, Reservation{ID: "after-month", Client: "c", Account: "a", At: at, Micros: 1},
		[]Limit{limit(ScopeClient, "c", PeriodMonth, 500_000)})
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("reserve under a later month limit = %v, want ErrExceeded", err)
	}
	// The history is intact and the denied attempts wrote nothing.
	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reserved != 1_000_000 || snap.Reported != 0 || snap.Unknown != 0 {
		t.Fatalf("after later-limit denials = %+v, want Reserved=1000000 only", snap)
	}
	// A limit that names no tracked key still restricts nothing.
	if err := s.Reserve(ctx, Reservation{ID: "unmatched", Client: "c", Account: "a", At: at, Micros: 1},
		[]Limit{limit(ScopeClient, "someone-else", PeriodDay, 0)}); err != nil {
		t.Fatalf("unmatched-key limit restricted admission: %v", err)
	}
}

// TestStaleReconcileListAfterOtherSettleBooksNothing reproduces the double-charge
// blocker deterministically. A reconciler snapshots the open set, ANOTHER writer
// (a second *Store over the same file, standing in for another process owner)
// claims the reservation first, and the reconciler then walks its now-STALE
// list. The claim is rechecked transactionally before any counter moves, so the
// stale entry books nothing, charges nothing a second time, and never drives
// Reserved negative.
func TestStaleReconcileListAfterOtherSettleBooksNothing(t *testing.T) {
	s, path := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)

	if err := s.Reserve(ctx, Reservation{ID: "r", Client: "c", Account: "a", At: at, Micros: 500}, nil); err != nil {
		t.Fatal(err)
	}
	// A second store on the SAME file: the "other process owner".
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	// The reconciler reads the open set...
	stale, err := s.openReservationIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "r" {
		t.Fatalf("open set = %v, want [r]", stale)
	}

	// ...and then the other writer wins the claim before the reconciler runs.
	if err := s2.Settle(ctx, Settlement{ID: "r", Micros: 500, Basis: BasisReported}); err != nil {
		t.Fatalf("other settle: %v", err)
	}

	// Walking the STALE list must be a no-op: the claim is lost.
	for _, id := range stale {
		claimed, err := s.reconcileOrphan(ctx, id)
		if err != nil {
			t.Fatalf("reconcileOrphan(%q) on a settled reservation: %v", id, err)
		}
		if claimed {
			t.Fatalf("reconcileOrphan(%q) claimed a reservation already settled by another writer", id)
		}
	}

	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Reported != 500 || snap.Unknown != 0 || snap.Reserved != 0 {
		t.Fatalf("after stale reconcile = %+v, want Reported=500 Unknown=0 Reserved=0", snap)
	}
	if snap.Reserved < 0 {
		t.Fatalf("Reserved went negative: %d", snap.Reserved)
	}
	// The whole ReconcileOrphans pass is likewise a no-op now.
	if err := s.ReconcileOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at); again != snap {
		t.Fatalf("ReconcileOrphans re-charged: %+v -> %+v", snap, again)
	}
}

// TestStaleReconcileUnknownZeroStillFloorsWhenReconcilerWins is the mirror
// ordering of the case above: the reconciler wins the claim on an open
// reservation, and the "other settle" it races books nothing and cannot release
// the hold for free. The reconciler's own unknown charge is floored at the held
// reservation, so the total booked is the ceiling exactly once.
func TestStaleReconcileUnknownZeroStillFloorsWhenReconcilerWins(t *testing.T) {
	s, path := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)

	if err := s.Reserve(ctx, Reservation{ID: "r", Client: "c", Account: "a", At: at, Micros: 500}, nil); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	// Reconciler claims first, at the ceiling, with a zero requested charge.
	claimed, err := s.reconcileOrphan(ctx, "r")
	if err != nil || !claimed {
		t.Fatalf("reconcileOrphan = (%v, %v), want (true, nil)", claimed, err)
	}
	// The racing other writer's settle is a no-op (or an idempotent match).
	if err := s2.Settle(ctx, Settlement{ID: "r", Micros: 0, Basis: BasisUnknown}); err != nil && !errors.Is(err, ErrConflict) {
		t.Fatalf("racing other settle: %v", err)
	}

	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Unknown != 500 || snap.Reserved != 0 {
		t.Fatalf("after reconciler wins = %+v, want Unknown=500 Reserved=0", snap)
	}
}

// TestConcurrentReconcileAndSettleChargesOnce races reconciliation across two
// stores on one file against settles of the same reservation. The transactional
// claim admits exactly one winner, so the reservation is booked once — never
// twice, never with a negative hold. Run under -race -count=3 it exercises the
// same/other-store settlement race the exclusive-owner contract relies on.
func TestConcurrentReconcileAndSettleChargesOnce(t *testing.T) {
	s, path := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	if err := s.Reserve(ctx, Reservation{ID: "r", Client: "c", Account: "a", At: at, Micros: 500}, nil); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	work := []func() error{
		func() error { return s.ReconcileOrphans(ctx) },
		func() error { return s2.ReconcileOrphans(ctx) },
		func() error { return s.Settle(ctx, Settlement{ID: "r", Micros: 500, Basis: BasisUnknown}) },
		func() error { return s2.Settle(ctx, Settlement{ID: "r", Micros: 500, Basis: BasisUnknown}) },
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, fn := range work {
		wg.Add(1)
		go func(fn func() error) {
			defer wg.Done()
			<-start
			if err := fn(); err != nil && !errors.Is(err, ErrConflict) {
				t.Errorf("concurrent reconcile/settle: %v", err)
			}
		}(fn)
	}
	close(start)
	wg.Wait()

	snap, _ := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if snap.Unknown != 500 || snap.Reserved != 0 {
		t.Fatalf("after concurrent reconcile+settle = %+v, want Unknown=500 Reserved=0", snap)
	}
}

// TestNormalizeChargeUnknownFloorsAtReserved pins the store-held floor rule
// directly: an unknown settlement charges max(requested, reserved), so it can
// never release the hold for a discount or for nothing, while reported and
// estimated book the observed value unchanged (including an honest zero).
func TestNormalizeChargeUnknownFloorsAtReserved(t *testing.T) {
	cases := []struct {
		name                string
		requested, reserved int64
		basis               string
		want                int64
	}{
		{"unknown-zero-never-releases-hold", 0, 500, BasisUnknown, 500},
		{"unknown-below-floor-is-raised", 75, 100, BasisUnknown, 100},
		{"unknown-above-floor-is-not-clamped", 200, 100, BasisUnknown, 200},
		{"unknown-equal-floor", 100, 100, BasisUnknown, 100},
		{"reported-zero-is-honest", 0, 500, BasisReported, 0},
		{"estimated-zero-is-honest", 0, 500, BasisEstimated, 0},
		{"reported-below-floor-is-honest", 250, 500, BasisReported, 250},
	}
	for _, c := range cases {
		if got := normalizeCharge(c.requested, c.reserved, c.basis); got != c.want {
			t.Errorf("%s: normalizeCharge(%d, %d, %s) = %d, want %d",
				c.name, c.requested, c.reserved, c.basis, got, c.want)
		}
	}
}

// --- Close-vs-writer ownership contract -------------------------------------
//
// Note on scope. Several tests in this file open the store's *sql.DB directly
// to inspect physical state (schema, rows). Those are RAW-db checks: they assert
// SQLite-level facts and are explicitly NOT the public Store contract. The
// tests below are the contract tests: every assertion about fail-closed writes
// and charge stability goes through the public API (Reserve/Settle/
// ReconcileOrphans/Snapshot/Open/Close). They drive the store's real writer
// critical section by taking the store's own mu and opening a real write
// transaction on the store's own db, so no second connection is used to bypass
// mu the way an out-of-band probe would.

// TestCloseSerializesWithInFlightWriterAndFailsClosed is the deterministic
// ownership test. It holds the writer critical section (mu plus a real write
// transaction) while Close runs and pins:
//   - Close does not return while a writer is in flight (it blocks on mu);
//   - once the writer commits and leaves, Close returns;
//   - after Close returns, public writers fail closed with ErrClosed and book
//     nothing, so the seeded claim's charge is unchanged;
//   - Close is idempotent.
func TestCloseSerializesWithInFlightWriterAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	s, path := openTestStore(t)
	at := day(2026, time.March, 15, 9, 30)

	if err := s.Reserve(ctx, Reservation{ID: "seed", Client: "c", Account: "a", At: at, Micros: 100}, nil); err != nil {
		t.Fatalf("seed reserve: %v", err)
	}

	// Enter the writer critical section exactly as Reserve/Settle do: take mu
	// and open a real write transaction on the store's own db.
	s.mu.Lock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.mu.Unlock()
		t.Fatalf("begin in-flight writer tx: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()

	// Close must not return while the writer holds the critical section.
	select {
	case err := <-closeDone:
		_ = tx.Rollback()
		s.mu.Unlock()
		t.Fatalf("Close returned (%v) while a writer held the critical section", err)
	case <-time.After(200 * time.Millisecond):
	}

	// The writer commits and leaves; only now may Close run.
	if err := tx.Commit(); err != nil {
		s.mu.Unlock()
		t.Fatalf("commit in-flight writer tx: %v", err)
	}
	s.mu.Unlock()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close after writer released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the writer released the critical section")
	}

	// Post-Close: public writers must fail closed and mutate nothing.
	if err := s.Reserve(ctx, Reservation{ID: "late", Client: "c", Account: "a", At: at, Micros: 999}, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reserve after Close: got %v, want ErrClosed", err)
	}
	if err := s.Settle(ctx, Settlement{ID: "seed", Micros: 500, Basis: BasisReported}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Settle after Close: got %v, want ErrClosed", err)
	}
	if err := s.ReconcileOrphans(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReconcileOrphans after Close: got %v, want ErrClosed", err)
	}
	if _, err := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at); err == nil {
		t.Fatal("Snapshot after Close succeeded; want a closed-database error")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Durable state is unchanged by the post-Close attempts: "seed" still holds
	// 100 with no spend booked, and "late" was never created.
	re, err := Open(path)
	if err != nil {
		t.Fatalf("reopen for verification: %v", err)
	}
	defer re.Close()
	snap, err := re.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if err != nil {
		t.Fatalf("post-close snapshot: %v", err)
	}
	if snap != (Snapshot{Reserved: 100}) {
		t.Fatalf("post-close state = %+v, want Reserved=100 only (charges unchanged)", snap)
	}
	var lateCount int
	if err := re.db.QueryRow(`SELECT COUNT(*) FROM budget_reservations WHERE id = ?`, "late").Scan(&lateCount); err != nil {
		t.Fatalf("count late: %v", err)
	}
	if lateCount != 0 {
		t.Fatalf("post-close Reserve created %d 'late' rows, want 0", lateCount)
	}
}

// TestCloseWaitsForRollbackAndClosedHandleNeverReopens pins the rollback half
// of the writer contract and the terminal nature of a closed handle: Close also
// waits for a writer that rolls back, and even after a fresh Open of the same
// file the old handle's writers stay ErrClosed rather than reopening.
func TestCloseWaitsForRollbackAndClosedHandleNeverReopens(t *testing.T) {
	ctx := context.Background()
	s, path := openTestStore(t)
	at := day(2026, time.March, 15, 9, 30)

	s.mu.Lock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.mu.Unlock()
		t.Fatalf("begin in-flight writer tx: %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	select {
	case err := <-closeDone:
		_ = tx.Rollback()
		s.mu.Unlock()
		t.Fatalf("Close returned (%v) while a writer held the critical section", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		s.mu.Unlock()
		t.Fatalf("rollback in-flight writer tx: %v", err)
	}
	s.mu.Unlock()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close after rollback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the writer rolled back")
	}

	// A fresh store on the same file works...
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("fresh Open: %v", err)
	}
	defer s2.Close()
	if err := s2.Reserve(ctx, Reservation{ID: "fresh", Client: "c", Account: "a", At: at, Micros: 10}, nil); err != nil {
		t.Fatalf("fresh store reserve: %v", err)
	}

	// ...but the closed handle never reopens: its writers stay ErrClosed.
	if err := s.Reserve(ctx, Reservation{ID: "ghost", Client: "c", Account: "a", At: at, Micros: 10}, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed handle Reserve after a fresh Open: got %v, want ErrClosed", err)
	}
	if err := s.Settle(ctx, Settlement{ID: "fresh", Micros: 10, Basis: BasisReported}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed handle Settle after a fresh Open: got %v, want ErrClosed", err)
	}
	if _, err := s.Snapshot(ctx, ScopeClient, "c", PeriodDay, at); err == nil {
		t.Fatal("closed handle Snapshot succeeded after a fresh Open")
	}

	// The fresh store's claim is intact; nothing the closed handle attempted landed.
	snap, err := s2.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
	if err != nil {
		t.Fatalf("fresh snapshot: %v", err)
	}
	if snap != (Snapshot{Reserved: 10}) {
		t.Fatalf("fresh store state = %+v, want Reserved=10", snap)
	}
}

// TestCloseRacesFiveWritersFailsClosedNoDoubleCharge starts five public writers
// (Reserve then Settle, distinct ids) racing Close, five times, and checks that
// every writer either committed before Close returned or failed closed with
// ErrClosed, with no panic and an exact durable ledger: each reservation is
// absent, open holding its reservation, or settled for exactly its reservation,
// so reserved and reported totals are the counts times the unit — no double
// charge and no lost hold.
func TestCloseRacesFiveWritersFailsClosedNoDoubleCharge(t *testing.T) {
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	const (
		writers = 5
		micros  = 100
	)
	for iter := 0; iter < 5; iter++ {
		s, path := openTestStore(t)
		start := make(chan struct{})
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			errs []error
		)
		record := func(err error) {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				id := fmt.Sprintf("w%d-%d", iter, i)
				if err := s.Reserve(ctx, Reservation{ID: id, Client: "c", Account: "a", At: at, Micros: micros}, nil); err != nil {
					record(err)
					return
				}
				record(s.Settle(ctx, Settlement{ID: id, Micros: micros, Basis: BasisReported}))
			}(i)
		}
		closeErr := make(chan error, 1)
		go func() {
			close(start)
			closeErr <- s.Close()
		}()
		wg.Wait()
		if err := <-closeErr; err != nil {
			t.Fatalf("iter %d: Close: %v", iter, err)
		}
		for _, err := range errs {
			if err != nil && !errors.Is(err, ErrClosed) {
				t.Fatalf("iter %d: writer error = %v, want nil or ErrClosed", iter, err)
			}
		}

		re, err := Open(path)
		if err != nil {
			t.Fatalf("iter %d: reopen: %v", iter, err)
		}
		snap, err := re.Snapshot(ctx, ScopeClient, "c", PeriodDay, at)
		if err != nil {
			re.Close()
			t.Fatalf("iter %d: snapshot: %v", iter, err)
		}
		var open, settled, other int
		rows, err := re.db.Query(`SELECT state FROM budget_reservations`)
		if err != nil {
			re.Close()
			t.Fatalf("iter %d: query states: %v", iter, err)
		}
		for rows.Next() {
			var st string
			if err := rows.Scan(&st); err != nil {
				rows.Close()
				re.Close()
				t.Fatalf("iter %d: scan state: %v", iter, err)
			}
			switch st {
			case stateOpen:
				open++
			case stateSettled:
				settled++
			default:
				other++
			}
		}
		rows.Close()
		re.Close()
		if other != 0 {
			t.Fatalf("iter %d: %d reservations in an unexpected state", iter, other)
		}
		if open+settled > writers {
			t.Fatalf("iter %d: open+settled = %d, want <= %d", iter, open+settled, writers)
		}
		if got := snap.Reserved; got != int64(open)*micros {
			t.Fatalf("iter %d: Reserved = %d, want %d (open=%d)", iter, got, int64(open)*micros, open)
		}
		if got := snap.Reported; got != int64(settled)*micros {
			t.Fatalf("iter %d: Reported = %d, want %d (settled=%d)", iter, got, int64(settled)*micros, settled)
		}
	}
}
