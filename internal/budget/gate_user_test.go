package budget

import (
	"context"
	"errors"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// userGateRec is an attempt made with a user-owned key: its client and key id
// identify the credential, its UserID the owner whose user budget it draws on.
func userGateRec(id, client, userID string) core.RequestRecord {
	return core.RequestRecord{ID: id, Client: client, AccountID: "acc", UserID: userID, KeyID: "key_" + client, StartedAt: gateAt}
}

// TestGateReservePinsTheUserFromTheRecord: Gate.Reserve names the user budget
// from rec.UserID, so a user limit supplied as an immutable Limit entry caps
// all of that user's keys together, while a record without a UserID keeps the
// legacy four-instance behaviour.
func TestGateReservePinsTheUserFromTheRecord(t *testing.T) {
	s, _ := openTestStore(t)
	const hold = int64(250_000)
	g := NewGate(s, nil, []Limit{limit(ScopeUser, "usr_1", PeriodDay, 2*hold)}, hold)

	reserveOK(t, g, userGateRec("k1-1", "key-a", "usr_1"))
	reserveOK(t, g, userGateRec("k2-1", "key-b", "usr_1"))
	if err := g.Reserve(context.Background(), userGateRec("k1-2", "key-a", "usr_1")); !errors.Is(err, ErrExceeded) {
		t.Fatalf("third hold across two keys: %v, want ErrExceeded", err)
	}
	if got := snapAt(t, s, ScopeUser, "usr_1", PeriodDay); got != (Snapshot{Reserved: 2 * hold}) {
		t.Fatalf("user day = %+v, want Reserved=%d", got, 2*hold)
	}
	reserveOK(t, g, gateRec("legacy", "key-a", "acc"))
	if got := snapAt(t, s, ScopeUser, "usr_1", PeriodMonth); got != (Snapshot{Reserved: 2 * hold}) {
		t.Fatalf("user month after a legacy attempt = %+v, want Reserved=%d", got, 2*hold)
	}
}

// TestGateUserSettlementPaths: completed (reported overrun), failed (nothing
// resolvable, floored at the hold) and cancelled/incomplete (reported partial
// cost is a lower bound) attempts each book their exact micros on the user's
// day and month instances as well as the client and account ones.
func TestGateUserSettlementPaths(t *testing.T) {
	s, _ := openTestStore(t)
	const hold = int64(250_000)
	g := NewGate(s, nil, nil, hold)
	ctx := context.Background()

	reserveOK(t, g, userGateRec("done", "key-a", "usr_1"))
	settleOK(t, g, core.RequestRecord{ID: "done", Provider: core.ProviderOpenRouter, ReportedCostUSD: usd(0.4)})
	reserveOK(t, g, userGateRec("failed", "key-b", "usr_1"))
	settleOK(t, g, core.RequestRecord{ID: "failed", Status: 502, Error: "upstream failed"})
	reserveOK(t, g, userGateRec("cancel", "key-a", "usr_1"))
	if err := g.SettleIncomplete(ctx, core.RequestRecord{ID: "cancel", Provider: core.ProviderOpenRouter,
		ReportedCostUSD: usd(0.00002), Error: "client disconnected"}); err != nil {
		t.Fatal(err)
	}

	want := Snapshot{Reported: 400_000, Unknown: 2 * hold}
	for _, inst := range [][3]string{
		{ScopeUser, "usr_1", PeriodDay}, {ScopeUser, "usr_1", PeriodMonth},
		{ScopeAccount, "acc", PeriodDay}, {ScopeAccount, "acc", PeriodMonth},
	} {
		if got := snapAt(t, s, inst[0], inst[1], inst[2]); got != want {
			t.Fatalf("%s/%s/%s = %+v, want %+v", inst[0], inst[1], inst[2], got, want)
		}
	}
	if got := snapAt(t, s, ScopeClient, "key-a", PeriodDay); got != (Snapshot{Reported: 400_000, Unknown: hold}) {
		t.Fatalf("client key-a = %+v", got)
	}
	if got := snapAt(t, s, ScopeClient, "key-b", PeriodDay); got != (Snapshot{Unknown: hold}) {
		t.Fatalf("client key-b = %+v", got)
	}
}
