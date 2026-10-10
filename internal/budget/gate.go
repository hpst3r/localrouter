package budget

import (
	"context"
	"errors"
	"fmt"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

// ErrNoStore reports that a Gate was used without a store to reserve against.
// It is a wiring error, not a budget denial, so callers must fail closed.
var ErrNoStore = errors.New("budget: gate has no store")

// Gate is the budget runtime adapter: it turns the proxy's per-attempt
// admission and settlement callbacks into store operations, naming the
// reservation from the attempt's own identity and settling it with the cost
// the ledger resolves for that attempt.
//
// A Gate is immutable once built and safe for concurrent use: it holds a
// generation-pinned copy of the limits, the price table used to estimate a
// settlement, and a store that serializes its own writes. Every field is read
// only, so the proxy may share one Gate across all in-flight requests.
//
// The store is the source of truth for money. This adapter adds no policy of
// its own beyond the fixed per-attempt hold and the ledger's cost resolution.
type Gate struct {
	store         *Store
	pricing       *ledger.Pricing
	limits        []Limit
	reserveMicros int64
}

// NewGate builds the adapter the proxy drives. store holds the durable budget;
// pricing resolves an estimated cost at settlement (it may be nil, in which
// case only provider-reported costs settle as known and everything else is
// unknown); limits are the configured ceilings to enforce at admission; and
// reserveMicros is the fixed hold every admitted attempt claims.
//
// limits is copied, so the Gate keeps a generation-immutable view: mutating the
// caller's slice afterwards can neither loosen nor tighten what this Gate
// enforces, which is what makes a config reload safe to race with in-flight
// requests. The copy is nil when limits is nil or empty, meaning no ceiling.
func NewGate(store *Store, pricing *ledger.Pricing, limits []Limit, reserveMicros int64) *Gate {
	return &Gate{
		store:         store,
		pricing:       pricing,
		limits:        append([]Limit(nil), limits...),
		reserveMicros: reserveMicros,
	}
}

// Reserve claims the fixed per-attempt hold for rec against every configured
// ceiling. It names the reservation from the attempt itself: its id, its
// client and account keys, and its started-at time, which fixes the day and
// month instances the hold is written to. The hold is the gate's fixed
// reserveMicros and must be positive.
//
// The returned error is the store's: it wraps ErrExceeded when a ceiling has no
// room (a denial), ErrConflict when the id was already reserved with a
// different payload, and ErrInvalid for a malformed attempt or a non-positive
// hold. The proxy must treat any error as terminal and must not settle, because
// nothing was admitted.
//
// Reserve is deliberately separate from Settle so a failed credential never
// holds budget: the proxy calls it only after a credential is resolved and
// immediately before the send.
func (g *Gate) Reserve(ctx context.Context, rec core.RequestRecord) error {
	if g == nil || g.store == nil {
		return ErrNoStore
	}
	if g.reserveMicros <= 0 {
		return fmt.Errorf("%w: reserve %q: fixed hold must be positive, got %d micros", ErrInvalid, rec.ID, g.reserveMicros)
	}
	return g.store.Reserve(ctx, Reservation{
		ID:      rec.ID,
		Client:  rec.Client,
		Account: rec.AccountID,
		At:      rec.StartedAt,
		Micros:  g.reserveMicros,
	}, g.limits)
}

// Settle closes the reservation Reserve took for rec and books the cost the
// ledger resolves for it. The basis follows ResolveRecordCost: a trusted
// provider-reported cost books as reported, a table-priced estimate books as
// estimated, and anything else books as unknown with zero micros so the store
// charges it at the reservation rather than releasing the hold for free.
//
// A cost the money type cannot represent is never collapsed to zero or a
// partial charge: Settle returns an error and leaves the whole reservation
// held, so the startup reconciler — not a silent discount — disposes of it. A
// charge larger than the hold is booked in full, per the store's contract.
//
// Settle is strict: settling an attempt that was never reserved returns the
// store's ErrUnknownReservation instead of quietly succeeding, so a caller that
// settles a non-reserved attempt is loud about it.
func (g *Gate) Settle(ctx context.Context, rec core.RequestRecord) error {
	return g.settle(ctx, rec, false)
}

// SettleIncomplete closes the reservation for an attempt whose response was
// cut short — a client disconnect, an idle timeout, an upstream read error
// mid-stream, or a stream that reached EOF without its protocol terminal — and
// so never delivered its final usage record. Any cost the
// ledger resolves for such an attempt (typically an OpenRouter usage.cost from
// an intermediate record, which can be a stale zero or a partial amount) is
// only a lower bound on what the provider will bill. It therefore books under
// the unknown basis with that cost as the requested charge, and the store
// floors it at the hold: the charge is max(hold, observed), so an aborted
// stream can never release its hold below the reservation.
//
// rec itself is not modified, so the ledger row may keep the observed cost.
// Errors follow Settle.
func (g *Gate) SettleIncomplete(ctx context.Context, rec core.RequestRecord) error {
	return g.settle(ctx, rec, true)
}

func (g *Gate) settle(ctx context.Context, rec core.RequestRecord, incomplete bool) error {
	if g == nil || g.store == nil {
		return ErrNoStore
	}
	cost, basis := ledger.ResolveRecordCost(rec, g.pricing)
	if incomplete {
		basis = BasisUnknown
	}
	var micros int64
	if cost != nil {
		m, err := ReportedUSDToMicros(*cost)
		if err != nil {
			return fmt.Errorf("%w: settle %q: cannot convert resolved cost: %v", ErrInvalid, rec.ID, err)
		}
		micros = m
	}
	return g.store.Settle(ctx, Settlement{ID: rec.ID, Micros: micros, Basis: basis})
}
