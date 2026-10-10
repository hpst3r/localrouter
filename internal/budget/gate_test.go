package budget

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

// The Gate adapter is the only place the proxy's per-attempt budget calls meet
// the durable store. These tests pin the adapter's contract: what it holds,
// what it books and — just as importantly — what it leaves held when it cannot
// decide.

// gateAt is the attempt timestamp every case fixes its day and month instances
// against.
var gateAt = day(2026, time.October, 1, 12, 0)

func usd(v float64) *float64 { return &v }

func testGatePricing() *ledger.Pricing {
	half := 0.5
	return ledger.NewPricing(map[string]ledger.ModelPrice{
		"model-a": {Input: 2, CachedInput: &half, Output: 10},
	})
}

func gateRec(id, client, account string) core.RequestRecord {
	return core.RequestRecord{ID: id, Client: client, AccountID: account, StartedAt: gateAt}
}

func snapAt(t *testing.T, s *Store, scope, key, period string) Snapshot {
	t.Helper()
	got, err := s.Snapshot(context.Background(), scope, key, period, gateAt)
	if err != nil {
		t.Fatalf("Snapshot(%s, %s, %s): %v", scope, key, period, err)
	}
	return got
}

func reserveOK(t *testing.T, g *Gate, rec core.RequestRecord) {
	t.Helper()
	if err := g.Reserve(context.Background(), rec); err != nil {
		t.Fatalf("Reserve(%s): %v", rec.ID, err)
	}
}

func settleOK(t *testing.T, g *Gate, rec core.RequestRecord) {
	t.Helper()
	if err := g.Settle(context.Background(), rec); err != nil {
		t.Fatalf("Settle(%s): %v", rec.ID, err)
	}
}

func TestGateReserveHoldsTheFixedAmountOnEveryTrackedInstance(t *testing.T) {
	s, _ := openTestStore(t)
	g := NewGate(s, nil, []Limit{
		limit(ScopeClient, "alice", PeriodDay, 10_000_000),
		limit(ScopeAccount, "acc", PeriodDay, 10_000_000),
	}, 250_000)

	reserveOK(t, g, gateRec("a1", "alice", "acc"))
	if got := snapAt(t, s, ScopeClient, "alice", PeriodDay); got.Reserved != 250_000 {
		t.Fatalf("client reserved = %d, want 250000", got.Reserved)
	}
	if got := snapAt(t, s, ScopeAccount, "acc", PeriodDay); got.Reserved != 250_000 {
		t.Fatalf("account reserved = %d, want 250000", got.Reserved)
	}
	// Each attempt holds again, so two attempts in flight hold twice.
	reserveOK(t, g, gateRec("a2", "alice", "acc"))
	if got := snapAt(t, s, ScopeClient, "alice", PeriodDay); got.Reserved != 500_000 {
		t.Fatalf("client reserved after two attempts = %d, want 500000", got.Reserved)
	}
}

func TestGateReserveDeniesWhenALimitIsExhausted(t *testing.T) {
	s, _ := openTestStore(t)
	g := NewGate(s, nil, []Limit{limit(ScopeClient, "alice", PeriodDay, 100_000)}, 250_000)

	err := g.Reserve(context.Background(), gateRec("d1", "alice", "acc"))
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("err = %v, want ErrExceeded", err)
	}
	// A denied reservation writes nothing at all.
	if got := snapAt(t, s, ScopeClient, "alice", PeriodDay); got != (Snapshot{}) {
		t.Fatalf("denied reservation mutated the store: %+v", got)
	}
}

func TestGateReserveHonoursAZeroBudget(t *testing.T) {
	s, _ := openTestStore(t)
	g := NewGate(s, nil, []Limit{limit(ScopeClient, "alice", PeriodDay, 0)}, 1)

	if err := g.Reserve(context.Background(), gateRec("z1", "alice", "acc")); !errors.Is(err, ErrExceeded) {
		t.Fatalf("err = %v, want ErrExceeded for a zero budget", err)
	}
	if got := snapAt(t, s, ScopeClient, "alice", PeriodDay); got != (Snapshot{}) {
		t.Fatalf("zero-budget denial mutated the store: %+v", got)
	}
}

func TestGateReserveRequiresAPositiveHoldAndAStore(t *testing.T) {
	s, _ := openTestStore(t)
	// A non-positive fixed hold is a misconfiguration, never a free attempt.
	if err := NewGate(s, nil, nil, 0).Reserve(context.Background(), gateRec("n1", "alice", "acc")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero hold: err = %v, want ErrInvalid", err)
	}
	if err := NewGate(s, nil, nil, -1).Reserve(context.Background(), gateRec("n2", "alice", "acc")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative hold: err = %v, want ErrInvalid", err)
	}
	if got := snapAt(t, s, ScopeClient, "alice", PeriodDay); got != (Snapshot{}) {
		t.Fatalf("invalid hold mutated the store: %+v", got)
	}

	// A nil gate and a gate without a store refuse rather than panic.
	var nilGate *Gate
	if err := nilGate.Reserve(context.Background(), gateRec("n3", "alice", "acc")); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil gate Reserve: err = %v, want ErrNoStore", err)
	}
	if err := nilGate.Settle(context.Background(), gateRec("n3", "alice", "acc")); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil gate Settle: err = %v, want ErrNoStore", err)
	}
	if err := NewGate(nil, nil, nil, 250_000).Reserve(context.Background(), gateRec("n4", "alice", "acc")); !errors.Is(err, ErrNoStore) {
		t.Fatalf("storeless Reserve: err = %v, want ErrNoStore", err)
	}
}

func TestGateSettleBooksEachBasis(t *testing.T) {
	s, _ := openTestStore(t)
	g := NewGate(s, testGatePricing(), nil, 250_000)

	// reported: 0.0001 USD = 100 micros, settled exactly as observed.
	reserveOK(t, g, gateRec("rep", "c", "a"))
	settleOK(t, g, core.RequestRecord{ID: "rep", Provider: core.ProviderOpenRouter, ReportedCostUSD: usd(0.0001)})

	// estimated: model-a at 1M input tokens = 2.00 USD = 2_000_000 micros.
	reserveOK(t, g, gateRec("est", "c", "a"))
	settleOK(t, g, core.RequestRecord{ID: "est", Provider: core.ProviderOpenAICompat, Model: "model-a", UsageKnown: true, Usage: core.Usage{InputTokens: 1_000_000}})

	// unknown: nothing to price, so the store floors it at the hold.
	reserveOK(t, g, gateRec("unk", "c", "a"))
	settleOK(t, g, core.RequestRecord{ID: "unk"})

	want := Snapshot{Reported: 100, Estimated: 2_000_000, Unknown: 250_000}
	if got := snapAt(t, s, ScopeClient, "c", PeriodDay); got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

func TestGateSettleUnknownOutcomeIsNeverFree(t *testing.T) {
	s, _ := openTestStore(t)
	g := NewGate(s, nil, nil, 250_000)

	// An attempt with no usage, no reported cost and no price table entry must
	// not silently release its hold.
	reserveOK(t, g, gateRec("unk", "c", "a"))
	settleOK(t, g, core.RequestRecord{ID: "unk"})
	want := Snapshot{Unknown: 250_000}
	if got := snapAt(t, s, ScopeClient, "c", PeriodDay); got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

func TestGateSettleReportedZeroAndOverrun(t *testing.T) {
	s, _ := openTestStore(t)
	g := NewGate(s, nil, nil, 250_000)

	// An explicit reported zero releases the hold and books a reported zero.
	reserveOK(t, g, gateRec("zero", "c", "a"))
	settleOK(t, g, core.RequestRecord{ID: "zero", Provider: core.ProviderOpenRouter, ReportedCostUSD: usd(0)})
	if got := snapAt(t, s, ScopeClient, "c", PeriodDay); got != (Snapshot{}) {
		t.Fatalf("reported zero: snapshot = %+v, want all zero", got)
	}

	// A reported overrun books the full observed cost, not the hold.
	reserveOK(t, g, gateRec("over", "c", "a"))
	settleOK(t, g, core.RequestRecord{ID: "over", Provider: core.ProviderOpenRouter, ReportedCostUSD: usd(0.9)})
	if got := snapAt(t, s, ScopeClient, "c", PeriodDay); got != (Snapshot{Reported: 900_000}) {
		t.Fatalf("reported overrun: snapshot = %+v, want {Reported:900000}", got)
	}
}

// An attempt whose stream was cut short (client disconnect, idle timeout,
// upstream read error) may carry a provider-reported cost from an intermediate
// usage record. That value is only a lower bound on the real charge, so
// SettleIncomplete books it as unknown and the store floors it at the hold:
// the charge is max(hold, observed), never a stale zero or partial release.
func TestGateSettleIncompleteTreatsReportedCostAsLowerBound(t *testing.T) {
	const hold = int64(250_000)
	for _, tc := range []struct {
		name string
		cost *float64
		want Snapshot
	}{
		{"no cost", nil, Snapshot{Unknown: hold}},
		{"stale zero", usd(0), Snapshot{Unknown: hold}},
		{"partial below hold", usd(0.00001), Snapshot{Unknown: hold}},
		{"above hold", usd(0.9), Snapshot{Unknown: 900_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			g := NewGate(s, nil, nil, hold)
			reserveOK(t, g, gateRec("inc", "c", "a"))
			rec := core.RequestRecord{ID: "inc", Provider: core.ProviderOpenRouter, ReportedCostUSD: tc.cost, Error: "client disconnected"}
			if err := g.SettleIncomplete(context.Background(), rec); err != nil {
				t.Fatalf("SettleIncomplete: %v", err)
			}
			for _, inst := range []struct{ scope, key, period string }{
				{ScopeClient, "c", PeriodDay}, {ScopeClient, "c", PeriodMonth},
				{ScopeAccount, "a", PeriodDay}, {ScopeAccount, "a", PeriodMonth},
			} {
				if got := snapAt(t, s, inst.scope, inst.key, inst.period); got != tc.want {
					t.Fatalf("%s/%s/%s = %+v, want %+v", inst.scope, inst.key, inst.period, got, tc.want)
				}
			}
		})
	}

	// The same reported zero on a COMPLETED attempt is a real, final cost and
	// still releases the hold through Settle.
	s, _ := openTestStore(t)
	g := NewGate(s, nil, nil, hold)
	reserveOK(t, g, gateRec("done", "c", "a"))
	settleOK(t, g, core.RequestRecord{ID: "done", Provider: core.ProviderOpenRouter, ReportedCostUSD: usd(0)})
	if got := snapAt(t, s, ScopeClient, "c", PeriodDay); got != (Snapshot{}) {
		t.Fatalf("completed reported zero: snapshot = %+v, want all zero", got)
	}
}

// hugeCostRec is an attempt whose resolved cost cannot be represented in
// micro-USD. A provider-reported cost that large is rejected by the ledger as
// unusable (core.MaxReportedCostUSD), so the overflow is reached through an
// estimated cost from an unvalidated price table instead.
func hugeCostRec(id string) (*ledger.Pricing, core.RequestRecord) {
	p := ledger.NewPricing(map[string]ledger.ModelPrice{"model-huge": {Input: 1e300}})
	rec := core.RequestRecord{ID: id, Model: "model-huge", UsageKnown: true,
		Usage: core.Usage{InputTokens: 1_000_000}}
	return p, rec
}

func TestGateSettleIncompleteConversionFailureKeepsTheHold(t *testing.T) {
	s, _ := openTestStore(t)
	p, rec := hugeCostRec("huge")
	g := NewGate(s, p, nil, 250_000)
	reserveOK(t, g, gateRec("huge", "c", "a"))
	err := g.SettleIncomplete(context.Background(), rec)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if got := snapAt(t, s, ScopeClient, "c", PeriodDay); got != (Snapshot{Reserved: 250_000}) {
		t.Fatalf("conversion failure moved money: %+v", got)
	}
	var nilGate *Gate
	if err := nilGate.SettleIncomplete(context.Background(), gateRec("n", "c", "a")); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil gate SettleIncomplete: err = %v, want ErrNoStore", err)
	}
}

func TestGateSettleConversionFailureKeepsTheHold(t *testing.T) {
	s, _ := openTestStore(t)
	p, rec := hugeCostRec("huge")
	g := NewGate(s, p, nil, 250_000)
	reserveOK(t, g, gateRec("huge", "c", "a"))

	// A resolved cost too large for micro-USD must not settle as false or
	// zero: the gate fails and the reservation stays held.
	err := g.Settle(context.Background(), rec)
	if err == nil {
		t.Fatal("overflowing resolved cost settled without error")
	}
	got := snapAt(t, s, ScopeClient, "c", PeriodDay)
	if got.Reserved != 250_000 || got.Reported != 0 {
		t.Fatalf("conversion failure moved money: %+v", got)
	}

	// The hold survived, so a correct settle still succeeds afterwards.
	settleOK(t, g, core.RequestRecord{ID: "huge", Provider: core.ProviderOpenRouter, ReportedCostUSD: usd(0.5)})
	if got := snapAt(t, s, ScopeClient, "c", PeriodDay); got != (Snapshot{Reported: 500_000}) {
		t.Fatalf("retry after conversion failure: snapshot = %+v, want {Reported:500000}", got)
	}
}

// A provider-reported cost above core.MaxReportedCostUSD is unusable, so it is
// not trusted as final: the attempt books as unknown at its hold.
func TestGateSettleOverCapReportedCostBooksTheHoldAsUnknown(t *testing.T) {
	s, _ := openTestStore(t)
	g := NewGate(s, nil, nil, 250_000)
	reserveOK(t, g, gateRec("huge", "c", "a"))
	settleOK(t, g, core.RequestRecord{ID: "huge", Provider: core.ProviderOpenRouter, ReportedCostUSD: usd(1e300)})
	if got := snapAt(t, s, ScopeClient, "c", PeriodDay); got != (Snapshot{Unknown: 250_000}) {
		t.Fatalf("over-cap reported cost: snapshot = %+v, want {Unknown:250000}", got)
	}
}

func TestGateLimitsAreCopiedAndImmutable(t *testing.T) {
	s, _ := openTestStore(t)
	limits := []Limit{limit(ScopeClient, "alice", PeriodDay, 10_000_000)}
	g := NewGate(s, nil, limits, 250_000)

	// Mutating the caller's slice after construction must not reach the gate:
	// its limits are a generation-pinned, immutable copy.
	limits[0] = limit(ScopeClient, "alice", PeriodDay, 0)
	reserveOK(t, g, gateRec("m1", "alice", "acc"))

	// ...and loosening a caller's slice must not loosen an existing gate.
	tight := []Limit{limit(ScopeClient, "alice", PeriodDay, 0)}
	g2 := NewGate(s, nil, tight, 250_000)
	tight[0].Micros = 1 << 40
	if err := g2.Reserve(context.Background(), gateRec("m2", "alice", "acc")); !errors.Is(err, ErrExceeded) {
		t.Fatalf("gate observed a mutation of the caller's limits: err = %v", err)
	}
}

func TestGateSettleReportsAnUnknownReservation(t *testing.T) {
	// Settling a reservation the gate never took is an error, so a proxy bug
	// that settles an unreserved attempt is loud rather than silent.
	s, _ := openTestStore(t)
	g := NewGate(s, nil, nil, 250_000)
	if err := g.Settle(context.Background(), core.RequestRecord{ID: "never-reserved"}); !errors.Is(err, ErrUnknownReservation) {
		t.Fatalf("err = %v, want ErrUnknownReservation", err)
	}
}
