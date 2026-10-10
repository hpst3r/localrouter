package budget

// Global per-user default ceilings (budgets.users): Gate.WithUserLimits(day,
// month *int64) (*Gate, error) returns a generation-immutable copy of the gate
// that, per Reserve, adds exactly one Limit{ScopeUser, rec.UserID, period} for
// each configured default period, unless the gate's explicit limits already
// name that user and period (explicit wins).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func i64(v int64) *int64 { return &v }

// Two API keys of one user draw on one default user budget.
func TestGateUserDefaultCapsAllKeysOfAUser(t *testing.T) {
	s, _ := openTestStore(t)
	const hold = int64(250_000)
	g, err := NewGate(s, nil, nil, hold).WithUserLimits(i64(2*hold), nil)
	if err != nil {
		t.Fatal(err)
	}
	reserveOK(t, g, userGateRec("a1", "k_a", "u_1"))
	reserveOK(t, g, userGateRec("b1", "k_b", "u_1"))
	if err := g.Reserve(context.Background(), userGateRec("b2", "k_b", "u_1")); !errors.Is(err, ErrExceeded) {
		t.Fatalf("third hold across two keys: %v, want ErrExceeded", err)
	}
	// A different user has their own default budget.
	reserveOK(t, g, userGateRec("c1", "k_c", "u_2"))
	if got := snapAt(t, s, ScopeUser, "u_1", PeriodDay); got != (Snapshot{Reserved: 2 * hold}) {
		t.Fatalf("u_1 day = %+v", got)
	}
}

// An explicit user limit overrides the default for that user and period only:
// it is not duplicated (which the store would reject) and the default still
// applies to the user's other period and to every other user.
func TestGateExplicitUserLimitOverridesDefault(t *testing.T) {
	s, _ := openTestStore(t)
	const hold = int64(250_000)
	explicit := []Limit{limit(ScopeUser, "u_vip", PeriodDay, 3*hold)}
	g, err := NewGate(s, nil, explicit, hold).WithUserLimits(i64(hold), i64(3*hold))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"v1", "v2", "v3"} {
		reserveOK(t, g, userGateRec(id, "k_v", "u_vip"))
	}
	if err := g.Reserve(context.Background(), userGateRec("v4", "k_v", "u_vip")); !errors.Is(err, ErrExceeded) {
		t.Fatalf("vip fourth hold: %v, want ErrExceeded (explicit day 3x / default month 3x)", err)
	}
	reserveOK(t, g, userGateRec("o1", "k_o", "u_other"))
	if err := g.Reserve(context.Background(), userGateRec("o2", "k_o", "u_other")); !errors.Is(err, ErrExceeded) {
		t.Fatalf("default day cap for another user: %v, want ErrExceeded", err)
	}
}

// Invalid defaults are rejected when the generation is built, not per request.
// Applying defaults to a gate that already has them is ambiguous and denied.
func TestGateWithUserLimitsValidation(t *testing.T) {
	s, _ := openTestStore(t)
	g := NewGate(s, nil, nil, 1)
	if _, err := g.WithUserLimits(i64(-1), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative day: %v, want ErrInvalid", err)
	}
	if _, err := g.WithUserLimits(nil, i64(-1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative month: %v, want ErrInvalid", err)
	}
	var nilGate *Gate
	if _, err := nilGate.WithUserLimits(i64(1), nil); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil gate: %v, want ErrNoStore", err)
	}
	if _, err := NewGate(nil, nil, nil, 1).WithUserLimits(i64(1), nil); !errors.Is(err, ErrNoStore) {
		t.Fatalf("storeless gate: %v, want ErrNoStore", err)
	}
	once, err := g.WithUserLimits(i64(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := once.WithUserLimits(nil, i64(5)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second defaults: %v, want ErrInvalid", err)
	}
}

// The source gate and the caller's values are never changed: a generation's
// gate keeps exactly the defaults it was built with.
func TestGateWithUserLimitsIsImmutable(t *testing.T) {
	s, _ := openTestStore(t)
	const hold = int64(250_000)
	base := NewGate(s, nil, nil, hold)
	day := i64(hold)
	g, err := base.WithUserLimits(day, nil)
	if err != nil {
		t.Fatal(err)
	}
	*day = 100 * hold // caller mutation after the generation was built
	reserveOK(t, g, userGateRec("x1", "k_x", "u_x"))
	if err := g.Reserve(context.Background(), userGateRec("x2", "k_x", "u_x")); !errors.Is(err, ErrExceeded) {
		t.Fatalf("default changed by caller mutation: %v", err)
	}
	// The base gate never acquired the default.
	reserveOK(t, base, userGateRec("y1", "k_y", "u_y"))
	reserveOK(t, base, userGateRec("y2", "k_y", "u_y"))
	if _, err := base.WithUserLimits(nil, i64(1)); err != nil {
		t.Fatalf("base gate marked as having defaults: %v", err)
	}
}

// Attempts without a user (legacy and service static clients) get no user
// limit and keep the four-instance behaviour.
func TestGateUserDefaultSkipsUnownedAttempts(t *testing.T) {
	s, _ := openTestStore(t)
	g, err := NewGate(s, nil, nil, 10).WithUserLimits(i64(0), i64(0))
	if err != nil {
		t.Fatal(err)
	}
	reserveOK(t, g, gateRec("svc-1", "svc", "acc"))
	reserveOK(t, g, gateRec("svc-2", "svc", "acc"))
	if err := g.Reserve(context.Background(), userGateRec("u-1", "k_z", "u_z")); !errors.Is(err, ErrExceeded) {
		t.Fatalf("zero default for a user: %v, want ErrExceeded", err)
	}
}

// A month-only default caps across days; the day is unlimited.
func TestGateUserDefaultMonthOnly(t *testing.T) {
	s, _ := openTestStore(t)
	const hold = int64(100)
	g, err := NewGate(s, nil, nil, hold).WithUserLimits(nil, i64(2*hold))
	if err != nil {
		t.Fatal(err)
	}
	r1 := userGateRec("m1", "k_a", "u_m")
	r2 := userGateRec("m2", "k_b", "u_m")
	r2.StartedAt = gateAt.AddDate(0, 0, 1)
	r3 := userGateRec("m3", "k_c", "u_m")
	r3.StartedAt = gateAt.AddDate(0, 0, 2)
	reserveOK(t, g, r1)
	reserveOK(t, g, r2)
	if err := g.Reserve(context.Background(), r3); !errors.Is(err, ErrExceeded) {
		t.Fatalf("month default across days: %v, want ErrExceeded", err)
	}
}

// Many concurrent attempts across several keys of one user never hold more
// than the user's default allows, and every key's client budget stays separate.
func TestGateUserDefaultConcurrentAcrossKeys(t *testing.T) {
	s, _ := openTestStore(t)
	const hold = int64(1_000)
	const capHolds = 5
	g, err := NewGate(s, nil, nil, hold).WithUserLimits(i64(capHolds*hold), nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"k_1", "k_2", "k_3", "k_4"}
	var wg sync.WaitGroup
	var admitted, denied atomic.Int64
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := g.Reserve(context.Background(), userGateRec(fmt.Sprintf("c-%d", i), keys[i%len(keys)], "u_c"))
			switch {
			case err == nil:
				admitted.Add(1)
			case errors.Is(err, ErrExceeded):
				denied.Add(1)
			default:
				t.Errorf("reserve %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if admitted.Load() != capHolds || denied.Load() != 24-capHolds {
		t.Fatalf("admitted=%d denied=%d, want %d/%d", admitted.Load(), denied.Load(), capHolds, 24-capHolds)
	}
	if got := snapAt(t, s, ScopeUser, "u_c", PeriodDay); got.Reserved != capHolds*hold {
		t.Fatalf("user day reserved = %d", got.Reserved)
	}
}
