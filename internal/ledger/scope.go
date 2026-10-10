package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

var _ core.ScopedLedger = (*Ledger)(nil)

// SummaryScoped is Summary restricted to the rows scope may see. A UserID
// scope sees only rows whose user_id equals it — never an unowned (NULL) row,
// never a row guessed from its client text; an AllUsers scope sees every row,
// unowned ones included. An invalid scope (neither or both set) is an error
// wrapping core.ErrInvalidScope, never "all".
func (l *Ledger) SummaryScoped(ctx context.Context, since time.Time, group string, scope core.DataScope) ([]core.UsageRow, error) {
	return l.summary(ctx, since, group, &scope)
}

// ownerFilter returns the SQL predicate (with a leading " AND ") and its bind
// arguments restricting rows to scope. It is appended to the WHERE clause of
// every aggregate query, so ranking, top-N, __other__, totals and breakdowns
// are all computed over the scoped rows only. A nil scope is the legacy
// unscoped read and yields no predicate; callers decide whether nil is allowed.
// The user id is always a bound argument, never interpolated.
func ownerFilter(scope *core.DataScope) (string, []any, error) {
	if scope == nil {
		return "", nil, nil
	}
	if err := scope.Validate(); err != nil {
		return "", nil, fmt.Errorf("ledger: %w", err)
	}
	if scope.AllUsers {
		return "", nil, nil
	}
	return " AND user_id = ?", []any{scope.UserID}, nil
}

// RequireScope switches this ledger into multi-user mode: from then on the
// legacy Summary, and Analytics with a nil AnalyticsQuery.Scope, fail with an
// error wrapping core.ErrInvalidScope instead of reading every row, so a
// caller that forgot to scope a read fails closed. Writes (Record,
// RecordBatch), RelabelHost, Reprice, Ping and explicitly scoped reads
// (SummaryScoped, Analytics with a valid Scope) are unaffected; an explicit
// all-users read is DataScope{AllUsers: true}.
//
// The mode is one-way and per *Ledger — never a process-wide flag: it applies
// to this ledger and to every PricingView over it (which share its reads), and
// to no other ledger. The app calls it once, before serving traffic, when
// identity is enabled. It is safe for concurrent use.
func (l *Ledger) RequireScope() { l.requireScope.Store(true) }

// errUnscopedRead is returned for a nil-scope read on a ledger in RequireScope
// mode. It deliberately does not start with "analytics: " so the control layer
// classifies it as a server-side wiring failure, not a client validation error.
var errUnscopedRead = fmt.Errorf("ledger: unscoped read: %w", core.ErrInvalidScope)

// readFilter is ownerFilter for a read entry point: in RequireScope mode a nil
// scope is denied rather than read unscoped.
func (l *Ledger) readFilter(scope *core.DataScope) (string, []any, error) {
	if scope == nil && l.requireScope.Load() {
		return "", nil, errUnscopedRead
	}
	return ownerFilter(scope)
}

// AnalyticsOwnerDimensions are the owner dimensions an explicitly scoped read
// (SummaryScoped, or Analytics with a non-nil Scope) may group or filter by,
// in addition to core.AnalyticsDimensions, which is deliberately left
// unchanged so legacy documents keep their exact key set. An unscoped read
// treats them as unknown. Which principals may use them (e.g. "user" for
// admins only) is decided by the caller, not the ledger.
var AnalyticsOwnerDimensions = []string{"user", "key"}

// ownerColumns maps AnalyticsOwnerDimensions to SQL expressions. An unowned
// (NULL) row groups and filters as "" — and only an AllUsers scope can see one.
var ownerColumns = map[string]string{
	"user": "COALESCE(user_id, '')",
	"key":  "COALESCE(key_id, '')",
}

// scopedColumn resolves a group/filter name against legacy, then — only for an
// explicitly scoped read — owner columns.
func scopedColumn(legacy map[string]string, name string, scope *core.DataScope) (string, bool) {
	if col, ok := legacy[name]; ok {
		return col, true
	}
	if scope == nil {
		return "", false
	}
	col, ok := ownerColumns[name]
	return col, ok
}
