package budget

// Tests for the per-user budget scope (budgets.db migration 2). A reservation
// that names a User additionally holds the user's UTC day and month instances,
// in the same transaction as the client and account instances, so every API
// key a user owns draws on one shared user budget.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// TestMigration2AddsReservationUserColumn pins the physical schema: a fresh
// store is at version 2 and budget_reservations carries a NOT NULL user_id
// column defaulting to ”.
func TestMigration2AddsReservationUserColumn(t *testing.T) {
	s, _ := openTestStore(t)
	var v int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 2 || len(migrations) != 2 {
		t.Fatalf("version = %d, migrations = %d; want 2/2", v, len(migrations))
	}
	var notNull int
	var dflt *string
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT "notnull", dflt_value FROM pragma_table_info('budget_reservations') WHERE name = 'user_id'`).
		Scan(&notNull, &dflt); err != nil {
		t.Fatalf("user_id column: %v", err)
	}
	if notNull != 1 || dflt == nil || *dflt != "''" {
		t.Fatalf("user_id notnull=%d default=%v; want NOT NULL DEFAULT ''", notNull, dflt)
	}
}

// v1BudgetSchema is the schema-version-1 budgets.db, frozen here so the v1 ->
// v2 upgrade is tested against what an existing deployment really has on disk.
const v1BudgetSchema = `
CREATE TABLE budget_periods (
	scope TEXT NOT NULL, key TEXT NOT NULL, period TEXT NOT NULL, start_utc INTEGER NOT NULL,
	limit_micros INTEGER NOT NULL DEFAULT 0, reserved_micros INTEGER NOT NULL DEFAULT 0,
	spent_reported_micros INTEGER NOT NULL DEFAULT 0, spent_estimated_micros INTEGER NOT NULL DEFAULT 0,
	spent_unknown_micros INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (scope, key, period, start_utc)
);
CREATE TABLE budget_reservations (
	id TEXT PRIMARY KEY, client TEXT NOT NULL, account TEXT NOT NULL, at_unix INTEGER NOT NULL,
	micros INTEGER NOT NULL, state TEXT NOT NULL, basis TEXT NOT NULL DEFAULT '',
	charged_micros INTEGER NOT NULL DEFAULT 0, settle_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX budget_reservations_state ON budget_reservations(state);
CREATE TABLE budget_reservation_periods (
	reservation_id TEXT NOT NULL, scope TEXT NOT NULL, key TEXT NOT NULL, period TEXT NOT NULL,
	start_utc INTEGER NOT NULL, reserved_micros INTEGER NOT NULL,
	PRIMARY KEY (reservation_id, scope, key, period)
);
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version (version) VALUES (1);`

// writeV1Store creates a version-1 budgets.db at path holding two open
// legacy reservations, exactly as the v1 Reserve would have written them:
// each holds the four client/account day/month instances.
func writeV1Store(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(v1BudgetSchema); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy-1", "legacy-2"} {
		if _, err := db.Exec(`INSERT INTO budget_reservations (id, client, account, at_unix, micros, state)
			VALUES (?, 'c', 'a', ?, 400, 'open')`, id, at.Unix()); err != nil {
			t.Fatal(err)
		}
		for _, sk := range [][2]string{{ScopeClient, "c"}, {ScopeAccount, "a"}} {
			for _, p := range []string{PeriodDay, PeriodMonth} {
				start, err := periodStart(p, at)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO budget_periods (scope, key, period, start_utc, reserved_micros)
					VALUES (?,?,?,?,400) ON CONFLICT DO UPDATE SET reserved_micros = reserved_micros + 400`,
					sk[0], sk[1], p, start); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO budget_reservation_periods
					(reservation_id, scope, key, period, start_utc, reserved_micros) VALUES (?,?,?,?,?,400)`,
					id, sk[0], sk[1], p, start); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// TestV1StoreUpgradesAndLegacyReservationsSettleUnchanged: a version-1 store
// opens at version 2 with legacy rows user_id = ”; an open legacy reservation
// settles against exactly its four pinned instances, an orphaned one reconciles
// at its ceiling, a replay of the legacy payload (no User) stays idempotent,
// and the same id replayed with a User conflicts.
func TestV1StoreUpgradesAndLegacyReservationsSettleUnchanged(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "budget.db")
	at := day(2026, time.March, 31, 23, 59)
	writeV1Store(t, path, at)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1: %v", err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != 2 {
		t.Fatalf("version = %d, err %v; want 2", v, err)
	}
	var users []string
	rows, err := s.db.Query(`SELECT user_id FROM budget_reservations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		users = append(users, u)
	}
	rows.Close()
	if !reflect.DeepEqual(users, []string{"", ""}) {
		t.Fatalf("legacy user_id = %q, want two empty", users)
	}

	if err := s.Reserve(ctx, Reservation{ID: "legacy-1", Client: "c", Account: "a", At: at, Micros: 400}, nil); err != nil {
		t.Fatalf("legacy replay: %v, want idempotent nil", err)
	}
	if err := s.Settle(ctx, Settlement{ID: "legacy-1", Micros: 250, Basis: BasisReported}); err != nil {
		t.Fatalf("settle legacy: %v", err)
	}
	if err := s.ReconcileOrphans(ctx); err != nil {
		t.Fatalf("reconcile legacy orphan: %v", err)
	}
	for _, sk := range [][2]string{{ScopeClient, "c"}, {ScopeAccount, "a"}} {
		for _, p := range []string{PeriodDay, PeriodMonth} {
			got, err := s.Snapshot(ctx, sk[0], sk[1], p, at)
			if err != nil {
				t.Fatal(err)
			}
			if want := (Snapshot{Reported: 250, Unknown: 400}); got != want {
				t.Fatalf("%s/%s/%s = %+v, want %+v", sk[0], sk[1], p, got, want)
			}
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM budget_periods`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("budget_periods rows = %d (%v), want the 4 legacy instances only", n, err)
	}
}

// TestNewerBudgetSchemaRefused: a database written by a newer binary is
// refused and left untouched (one-way migration, fail closed on the future).
func TestNewerBudgetSchemaRefused(t *testing.T) {
	s, path := openTestStore(t)
	if _, err := s.db.Exec(`UPDATE schema_version SET version = ?`, len(migrations)+1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s2, err := Open(path); err == nil {
		s2.Close()
		t.Fatal("Open of a newer schema succeeded, want refusal")
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != len(migrations)+1 {
		t.Fatalf("version after refusal = %d (%v), want %d", v, err, len(migrations)+1)
	}
}

// TestReserveTracksUserInstancesOnlyWhenUserSet: a reservation naming a User
// holds the user day and month instances as well as the four client/account
// ones (six in all), even with no limit configured; a reservation without a
// User touches exactly the legacy four.
func TestReserveTracksUserInstancesOnlyWhenUserSet(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	if err := s.Reserve(ctx, Reservation{ID: "u-1", Client: "c", Account: "a", User: "usr_1", At: at, Micros: 300}, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{PeriodDay, PeriodMonth} {
		got, err := s.Snapshot(ctx, ScopeUser, "usr_1", p, at)
		if err != nil {
			t.Fatal(err)
		}
		if got != (Snapshot{Reserved: 300}) {
			t.Fatalf("user/%s = %+v, want Reserved=300", p, got)
		}
	}
	var pinned int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM budget_reservation_periods WHERE reservation_id = 'u-1'`).Scan(&pinned); err != nil || pinned != 6 {
		t.Fatalf("pinned instances = %d (%v), want 6", pinned, err)
	}

	if err := s.Reserve(ctx, Reservation{ID: "legacy", Client: "c", Account: "a", At: at, Micros: 300}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM budget_reservation_periods WHERE reservation_id = 'legacy'`).Scan(&pinned); err != nil || pinned != 4 {
		t.Fatalf("legacy pinned instances = %d (%v), want 4", pinned, err)
	}
	var users int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM budget_periods WHERE scope = 'user'`).Scan(&users); err != nil || users != 2 {
		t.Fatalf("user budget_periods = %d (%v), want 2", users, err)
	}
}

// TestReserveIdempotencyComparesUser: replaying the exact payload (including
// User) is a no-op, while the same id with a different, added or dropped User
// is ErrConflict and moves no money.
func TestReserveIdempotencyComparesUser(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	r := Reservation{ID: "u-1", Client: "c", Account: "a", User: "usr_1", At: at, Micros: 300}
	if err := s.Reserve(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve(ctx, r, nil); err != nil {
		t.Fatalf("identical replay: %v, want nil", err)
	}
	for _, user := range []string{"usr_2", ""} {
		alt := r
		alt.User = user
		if err := s.Reserve(ctx, alt, nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("replay with User %q: %v, want ErrConflict", user, err)
		}
	}
	legacy := Reservation{ID: "legacy", Client: "c", Account: "a", At: at, Micros: 300}
	if err := s.Reserve(ctx, legacy, nil); err != nil {
		t.Fatal(err)
	}
	legacy.User = "usr_1"
	if err := s.Reserve(ctx, legacy, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy id replayed with a User: %v, want ErrConflict", err)
	}
	for _, sk := range [][2]string{{ScopeUser, "usr_1"}, {ScopeUser, "usr_2"}, {ScopeClient, "c"}} {
		got, err := s.Snapshot(ctx, sk[0], sk[1], PeriodDay, at)
		if err != nil {
			t.Fatal(err)
		}
		want := Snapshot{Reserved: 300}
		switch sk[1] {
		case "usr_2":
			want = Snapshot{}
		case "c":
			want = Snapshot{Reserved: 600}
		}
		if got != want {
			t.Fatalf("%s/%s = %+v, want %+v", sk[0], sk[1], got, want)
		}
	}
}

// TestUserLimitAppliesOnlyToItsUser: a user limit restricts only reservations
// naming that user. Limits for other users — even malformed duplicates — are
// skipped after validation exactly like client/account limits for other keys,
// and a reservation without a User is never gated by any user limit.
func TestUserLimitAppliesOnlyToItsUser(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	limits := []Limit{
		limit(ScopeUser, "usr_1", PeriodDay, 0),
		limit(ScopeUser, "usr_other", PeriodDay, 0),
		limit(ScopeUser, "usr_other", PeriodDay, 0),
	}
	if err := s.Reserve(ctx, Reservation{ID: "u2", Client: "c", Account: "a", User: "usr_2", At: at, Micros: 100}, limits); err != nil {
		t.Fatalf("usr_2 under other users' limits: %v, want admitted", err)
	}
	if err := s.Reserve(ctx, Reservation{ID: "legacy", Client: "c", Account: "a", At: at, Micros: 100}, limits); err != nil {
		t.Fatalf("legacy reservation under user limits: %v, want admitted", err)
	}
	if err := s.Reserve(ctx, Reservation{ID: "u1", Client: "c", Account: "a", User: "usr_1", At: at, Micros: 100}, limits); !errors.Is(err, ErrExceeded) {
		t.Fatalf("usr_1 at a zero user limit: %v, want ErrExceeded", err)
	}
	for _, bad := range []Limit{limit(ScopeUser, "", PeriodDay, 1), limit(ScopeUser, "usr_9", PeriodDay, -1), limit(ScopeUser, "usr_9", "week", 1)} {
		if err := s.Reserve(ctx, Reservation{ID: "bad", Client: "c", Account: "a", At: at, Micros: 100}, []Limit{bad}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid user limit %+v: %v, want ErrInvalid", bad, err)
		}
	}
}

// TestConcurrentTwoKeysShareOneUserCap: two keys (distinct client names) of
// one user race 50 reservations against a user day cap of 10 holds. Exactly
// 10 are admitted in total — the per-key dodge is closed because both keys
// contend on the single user instance inside the one write transaction — and
// a denial writes nothing on the client or account instances. Settling the
// admitted holds with overrun, under-run, unknown-floor and unknown-overrun
// charges books exact integer micros on all six pinned instances.
func TestConcurrentTwoKeysShareOneUserCap(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := day(2026, time.March, 15, 9, 30)
	const (
		unit       = 1_000
		capacity   = 10
		contenders = 50
	)
	limits := []Limit{limit(ScopeUser, "usr_1", PeriodDay, unit*capacity)}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		admitted  []Reservation
		denied    int
		otherErrs []error
	)
	start := make(chan struct{})
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := Reservation{ID: fmt.Sprintf("r-%02d", i), Client: []string{"key-a", "key-b"}[i%2],
				Account: "a", User: "usr_1", At: at, Micros: unit}
			err := s.Reserve(ctx, r, limits)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted = append(admitted, r)
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
		t.Fatalf("unexpected errors: %v", otherErrs)
	}
	if len(admitted) != capacity || denied != contenders-capacity {
		t.Fatalf("admitted %d denied %d, want %d/%d", len(admitted), denied, capacity, contenders-capacity)
	}
	perClient := map[string]int64{}
	for _, r := range admitted {
		perClient[r.Client] += unit
	}
	for _, c := range []string{"key-a", "key-b"} {
		got, err := s.Snapshot(ctx, ScopeClient, c, PeriodDay, at)
		if err != nil {
			t.Fatal(err)
		}
		if got != (Snapshot{Reserved: perClient[c]}) {
			t.Fatalf("client %s = %+v, want Reserved=%d (denials write nothing)", c, got, perClient[c])
		}
	}

	// Settle concurrently: charges cycle through overrun, under-run, unknown
	// zero (floored at the hold) and unknown overrun.
	type charge struct {
		micros int64
		basis  string
	}
	charges := []charge{{2_500, BasisReported}, {400, BasisEstimated}, {0, BasisUnknown}, {1_700, BasisUnknown}}
	var want Snapshot
	for i := range admitted {
		c := charges[i%len(charges)]
		switch c.basis {
		case BasisReported:
			want.Reported += c.micros
		case BasisEstimated:
			want.Estimated += c.micros
		default:
			want.Unknown += max(c.micros, unit)
		}
	}
	errs := make(chan error, len(admitted))
	for i, r := range admitted {
		c := charges[i%len(charges)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Settle(ctx, Settlement{ID: r.ID, Micros: c.micros, Basis: c.basis})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
	}
	for _, p := range []string{PeriodDay, PeriodMonth} {
		for _, sk := range [][2]string{{ScopeUser, "usr_1"}, {ScopeAccount, "a"}} {
			got, err := s.Snapshot(ctx, sk[0], sk[1], p, at)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("%s/%s/%s = %+v, want %+v", sk[0], sk[1], p, got, want)
			}
		}
	}
	// The overrun is recorded, not clamped: the user is now over its cap and
	// the next reservation from either key is denied.
	for _, c := range []string{"key-a", "key-b"} {
		err := s.Reserve(ctx, Reservation{ID: "after-" + c, Client: c, Account: "a", User: "usr_1", At: at, Micros: 1}, limits)
		if !errors.Is(err, ErrExceeded) {
			t.Fatalf("reserve after overrun via %s: %v, want ErrExceeded", c, err)
		}
	}
	// Another user of the same keys is unaffected by usr_1's cap.
	if err := s.Reserve(ctx, Reservation{ID: "other-user", Client: "key-a", Account: "a", User: "usr_2", At: at, Micros: unit}, limits); err != nil {
		t.Fatalf("usr_2: %v, want admitted", err)
	}
}

// TestUserHistoryAndUTCBoundary: user spend tracked while no user limit was
// configured counts against a limit supplied later, and a hold admitted at
// 23:59:59Z on the last day of a month settles into that original day and
// month even though settlement runs after midnight.
func TestUserHistoryAndUTCBoundary(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	late := time.Date(2026, time.March, 31, 23, 59, 59, 0, time.UTC)
	if err := s.Reserve(ctx, Reservation{ID: "late", Client: "c", Account: "a", User: "usr_1", At: late, Micros: 600}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, Settlement{ID: "late", Micros: 700, Basis: BasisEstimated}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{PeriodDay, PeriodMonth} {
		if got, _ := s.Snapshot(ctx, ScopeUser, "usr_1", p, late); got != (Snapshot{Estimated: 700}) {
			t.Fatalf("original user %s = %+v, want Estimated=700", p, got)
		}
		if got, _ := s.Snapshot(ctx, ScopeUser, "usr_1", p, late.Add(2*time.Second)); got != (Snapshot{}) {
			t.Fatalf("next user %s = %+v, want untouched", p, got)
		}
	}
	// A month limit configured afterwards sees the 700 already spent.
	limits := []Limit{limit(ScopeUser, "usr_1", PeriodMonth, 1_000)}
	if err := s.Reserve(ctx, Reservation{ID: "next", Client: "c2", Account: "a", User: "usr_1", At: late.Add(-time.Hour), Micros: 301}, limits); !errors.Is(err, ErrExceeded) {
		t.Fatalf("over the later limit: %v, want ErrExceeded", err)
	}
	if err := s.Reserve(ctx, Reservation{ID: "fits", Client: "c2", Account: "a", User: "usr_1", At: late.Add(-time.Hour), Micros: 300}, limits); err != nil {
		t.Fatalf("exactly at the later limit: %v, want admitted", err)
	}
}

// TestMigration2IsTransactional: a failing migration 2 leaves the store at
// version 1 and Open fails closed instead of running half-upgraded.
func TestMigration2IsTransactional(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "budget.db")
	writeV1Store(t, path, day(2026, time.March, 10, 12, 0))
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A conflicting column makes the v2 ALTER fail.
	if _, err := db.Exec(`ALTER TABLE budget_reservations ADD COLUMN user_id INTEGER`); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open succeeded despite a failing migration 2")
	}
	var v int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != 1 {
		t.Fatalf("version after failed migration = %d (%v), want 1", v, err)
	}
}
