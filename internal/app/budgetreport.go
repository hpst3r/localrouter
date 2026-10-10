package app

import (
	"context"
	"errors"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/control"
)

// budgetReport adapts the process-wide budget.Store to control.BudgetSource for
// one runtime generation. limits and hold are copied out of the generation's
// *config.BudgetConfig at construction — never read from the store, whose limit
// column is informational — so the report cannot disagree with the Gate the
// same generation enforces, and a reload publishes a fresh adapter carrying the
// new limits.
//
// The store lives in an unexported field and never escapes: a caller receives
// only the control.BudgetSource interface.
type budgetReport struct {
	store  *budget.Store
	limits []budget.Limit
	hold   int64
	// users are the generation's global per-user default ceilings
	// (budgets.users) with an empty Key; UserLimits instantiates them.
	users []budget.Limit
}

// budgetReport implements control.BudgetSource and, for multi-user
// generations, control.UserBudgetSource.
var (
	_ control.BudgetSource     = (*budgetReport)(nil)
	_ control.UserBudgetSource = (*budgetReport)(nil)
)

// budgetSource builds this generation's read-only budget view over the shared
// store, with the ceilings and fixed reservation copied from the generation's
// config. It returns nil when spend controls are disabled for the generation
// (no config block, or no store), which the control server reports as
// enabled:false. It is called on the makeGeneration goroutine while reloadMu is
// held, exactly like budgetGate, so it observes the same store as the
// generation's Gate.
func (a *App) budgetSource(b *config.BudgetConfig) control.BudgetSource {
	if b == nil || a.budgetStore == nil {
		return nil
	}
	limits, err := b.Limits()
	if err != nil {
		return nil
	}
	hold, err := b.ReservationMicros()
	if err != nil {
		return nil
	}
	r := &budgetReport{store: a.budgetStore, limits: limits, hold: hold}
	if b.Users != nil {
		if r.users, err = b.Users.Limits(budget.ScopeUser, ""); err != nil {
			return nil
		}
	}
	return r
}

// UserLimits returns the per-user ceilings this generation's Gate enforces
// for userID: the configured default for each period, keyed by the user id
// (explicit per-user overrides are not configurable in this version). Nil for
// an empty id or when no user default is configured.
func (r *budgetReport) UserLimits(userID string) []budget.Limit {
	if r == nil || userID == "" || len(r.users) == 0 {
		return nil
	}
	out := make([]budget.Limit, len(r.users))
	for i, l := range r.users {
		l.Key = userID
		out[i] = l
	}
	return out
}

// Snapshot reads one period instance (at interpreted in UTC). A nil adapter or
// nil store reports "no store" rather than panicking.
func (r *budgetReport) Snapshot(ctx context.Context, scope, key, period string, at time.Time) (budget.Snapshot, error) {
	if r == nil || r.store == nil {
		return budget.Snapshot{}, errors.New("budget report: no store")
	}
	return r.store.Snapshot(ctx, scope, key, period, at.UTC())
}

// Limits returns a copy of the generation's configured ceilings, so a caller
// cannot mutate the adapter's captured set.
func (r *budgetReport) Limits() []budget.Limit {
	if r == nil {
		return nil
	}
	return append([]budget.Limit(nil), r.limits...)
}

// ReservationMicros returns the generation's fixed per-attempt hold.
func (r *budgetReport) ReservationMicros() int64 {
	if r == nil {
		return 0
	}
	return r.hold
}
