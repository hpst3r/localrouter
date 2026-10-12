package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
)

// budgetReadTimeout bounds the store reads a single budget request performs, so
// a slow or hung budget database cannot make the read itself hang. It mirrors
// storagePingTimeout. This is a latency bound on the read only: it does not
// reserve a store connection or otherwise isolate these reads from concurrent
// writes, so a saturated or hung pool can still stall a read until this
// deadline fires. The read is bounded at 2s; it is not guaranteed never to block.
const budgetReadTimeout = 2 * time.Second

// BudgetSource is the generation-pinned, read-only view of spend controls that
// the control server consults per request (a live view, like ReloadStatus).
//
// Its limits are the copied slice the generation's Gate enforces, not the
// store's informational limit column: a reload publishes a fresh source, so a
// read can never disagree with enforcement.
//
// A nil BudgetSource in Deps means spend controls are not configured for this
// generation, and GET /control/v1/budgets then reports enabled:false rather than
// claiming zero money.
type BudgetSource interface {
	// Snapshot reads the period instance containing at (interpreted in UTC) for
	// (scope,key,period). A never-touched instance is an all-zero Snapshot with
	// a nil error. It is bounded and read-only.
	Snapshot(ctx context.Context, scope, key, period string, at time.Time) (budget.Snapshot, error)
	// Limits returns this generation's configured ceilings, exactly the slice
	// the generation's Gate was built with.
	Limits() []budget.Limit
	// ReservationMicros is this generation's fixed per-attempt hold.
	ReservationMicros() int64
}

// UserBudgetSource is optionally implemented by the value in Deps.Budgets in
// multi-user mode. It supplies the per-user ceilings, which are dynamic (one
// per user) and therefore cannot be listed in Limits.
type UserBudgetSource interface {
	// UserLimits returns this generation's effective ceilings for userID,
	// instantiated as Limit{Scope: budget.ScopeUser, Key: userID, Period,
	// Micros} — exactly the user limits the generation's Gate enforces for
	// that user (configured default and any explicit entry already resolved
	// by the implementation). Nil or empty means the user is unlimited.
	// Entries naming another scope or user are ignored. User usage itself is
	// read through Snapshot(ctx, budget.ScopeUser, userID, period, at).
	UserLimits(userID string) []budget.Limit
}

// budgetDoc is the GET /control/v1/budgets response. When spend controls are
// disabled (a nil BudgetSource) it is exactly {schema_version, enabled:false}
// and carries no money at all.
type budgetDoc struct {
	SchemaVersion int         `json:"schema_version"`
	Enabled       bool        `json:"enabled"`
	ReserveMicros int64       `json:"reserve_micros,omitempty"`
	Rows          []budgetRow `json:"rows,omitempty"`
}

// budgetRow is one (scope,key,period) instance. Every amount is exact integer
// micro-USD paired with its exact six-digit USD rendering. The limit and
// available fields are pointers omitted when the budget is unlimited, so
// presence distinguishes an explicit zero budget from "no ceiling"; available
// is signed and never clamped, so a settled overrun stays visible.
type budgetRow struct {
	Scope  string    `json:"scope"`
	Key    string    `json:"key"`
	Period string    `json:"period"`
	Start  time.Time `json:"start"`
	Reset  time.Time `json:"reset"`

	ReportedMicros  int64  `json:"reported_micros"`
	ReportedUSD     string `json:"reported_usd"`
	EstimatedMicros int64  `json:"estimated_micros"`
	EstimatedUSD    string `json:"estimated_usd"`
	UnknownMicros   int64  `json:"unknown_micros"`
	UnknownUSD      string `json:"unknown_usd"`
	ReservedMicros  int64  `json:"reserved_micros"`
	ReservedUSD     string `json:"reserved_usd"`

	LimitMicros     *int64  `json:"limit_micros,omitempty"`
	LimitUSD        *string `json:"limit_usd,omitempty"`
	AvailableMicros *int64  `json:"available_micros,omitempty"`
	AvailableUSD    *string `json:"available_usd,omitempty"`
}

// Generic, non-leaking messages. Raw store errors can embed filesystem paths,
// DSNs or credentials, so they are never echoed to the client.
const (
	budgetUnavailableMsg = "budget store unavailable"
	budgetTimeoutMsg     = "budget read timed out"
)

// budgets serves GET /control/v1/budgets, the operator read of one configured
// client's or account's spend-control position. It is registered on the same
// s.auth gate as status/usage/diagnostics, so any valid client key may read any
// configured identity under RequireAuth (the same access model; no new policy).
//
// It requires ?scope=client|account and ?key=<configured identity>, and accepts
// an optional ?period=day|month (default: both). There is deliberately no
// all-identities query: N is bounded by the immutable per-generation config and
// the caller can never name an arbitrary key to probe the store.
func (s *Server) budgets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	src := s.deps.Budgets
	if src == nil {
		// Spend controls are not configured for this generation: report the
		// block disabled, with no money and no store access.
		writeJSON(w, http.StatusOK, budgetDoc{SchemaVersion: SchemaVersion, Enabled: false})
		return
	}

	q := r.URL.Query()
	scope := q.Get("scope")
	key := q.Get("key")
	// Multi-user global readers (admin session, service) may also read one
	// user's instances; the user must exist in the identity store.
	_, multi := authFrom(r.Context())
	userScope := multi && scope == budget.ScopeUser
	if scope != budget.ScopeClient && scope != budget.ScopeAccount && !userScope {
		msg := "scope must be client or account"
		if multi {
			msg = "scope must be client, account or user"
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	periods := []string{budget.PeriodDay, budget.PeriodMonth}
	if p := q.Get("period"); p != "" {
		if p != budget.PeriodDay && p != budget.PeriodMonth {
			writeError(w, http.StatusBadRequest, "period must be day or month")
			return
		}
		periods = []string{p}
	}
	if userScope {
		s.userBudgets(w, r, src, key, periods)
		return
	}
	// The readable identities come only from this generation's configuration;
	// a non-member never reaches the store.
	if !s.knownIdentity(scope, key) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown %s %q", scope, key))
		return
	}

	s.writeBudgetDoc(w, r, src, scope, key, periods, src.Limits())
}

// userBudgets renders one user's instances for a global reader. The user id
// is checked against the identity store first, so an unknown id never reaches
// the budget store and is never echoed.
func (s *Server) userBudgets(w http.ResponseWriter, r *http.Request, src BudgetSource, user string, periods []string) {
	if s.deps.Identity == nil {
		writeError(w, http.StatusServiceUnavailable, "identity unavailable")
		return
	}
	if !idParamRE.MatchString(user) {
		writeError(w, http.StatusNotFound, "unknown user")
		return
	}
	if _, err := s.deps.Identity.User(r.Context(), user); err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown user")
			return
		}
		slog.Warn("control: identity store unavailable")
		writeError(w, http.StatusServiceUnavailable, "identity unavailable")
		return
	}
	limits, ok := effectiveLimits(src, user)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, budgetUnavailableMsg)
		return
	}
	s.writeBudgetDoc(w, r, src, budget.ScopeUser, user, periods, limits)
}

// writeBudgetDoc reads and renders the (scope,key) instances for periods
// against limits. The caller has already authorized and validated the
// identity; this never takes it from the request.
func (s *Server) writeBudgetDoc(w http.ResponseWriter, r *http.Request, src BudgetSource, scope, key string, periods []string, limits []budget.Limit) {
	// One clock read governs the whole document: every period boundary and every
	// Snapshot call below uses this same UTC instant.
	now := s.deps.Clock.Now().UTC()
	ctx, cancel := context.WithTimeout(r.Context(), budgetReadTimeout)
	defer cancel()

	rows := make([]budgetRow, 0, len(periods))
	for _, period := range periods {
		snap, err := src.Snapshot(ctx, scope, key, period, now)
		if err != nil {
			// Log the generic event with scope/period only; the err attribute
			// is the stable sanitized local code, never the raw error chain
			// (which can embed paths, DSNs or credentials).
			slog.Warn("control: budget snapshot failed", "scope", scope, "period", period, "err", budgetReadError(err))
			writeError(w, http.StatusServiceUnavailable, budgetReadError(err))
			return
		}
		row, ok := budgetRowFor(scope, key, period, now, snap, limits)
		if !ok {
			// An amount that cannot be represented as int64 micro-USD is never
			// wrapped or clamped: fail the whole read closed.
			writeError(w, http.StatusServiceUnavailable, budgetUnavailableMsg)
			return
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, budgetDoc{
		SchemaVersion: SchemaVersion,
		Enabled:       true,
		ReserveMicros: src.ReservationMicros(),
		Rows:          rows,
	})
}

// knownIdentity reports whether key names a configured identity in scope. The
// configured set is the generation's own Deps.Clients / Deps.Accounts, never the
// query string.
func (s *Server) knownIdentity(scope, key string) bool {
	switch scope {
	case budget.ScopeClient:
		return slices.ContainsFunc(s.deps.Clients, func(c ClientInfo) bool { return c.Name == key })
	case budget.ScopeAccount:
		return slices.ContainsFunc(s.deps.Accounts, func(a core.Account) bool { return a.ID == key })
	}
	return false
}

// budgetRowFor builds one row. limitMicros comes from the captured (generation
// -pinned) limits, which are authoritative; the store's own limit column is
// informational and is never consulted. ok is false only on an int64 overflow
// in the available calculation, which the caller reports as unavailable.
func budgetRowFor(scope, key, period string, now time.Time, snap budget.Snapshot, limits []budget.Limit) (budgetRow, bool) {
	row := budgetRow{
		Scope: scope, Key: key, Period: period,
		Start: periodStartUTC(period, now), Reset: periodResetUTC(period, now),

		ReportedMicros:  snap.Reported,
		ReportedUSD:     formatMicrosUSD(snap.Reported),
		EstimatedMicros: snap.Estimated,
		EstimatedUSD:    formatMicrosUSD(snap.Estimated),
		UnknownMicros:   snap.Unknown,
		UnknownUSD:      formatMicrosUSD(snap.Unknown),
		ReservedMicros:  snap.Reserved,
		ReservedUSD:     formatMicrosUSD(snap.Reserved),
	}
	limit, ok := limitFor(limits, scope, key, period)
	if !ok {
		// No configured ceiling: unlimited. Omit limit and available entirely.
		return row, true
	}
	avail, ok := budgetAvailable(limit, snap)
	if !ok {
		return budgetRow{}, false
	}
	l, a := limit, avail
	lu, au := formatMicrosUSD(l), formatMicrosUSD(a)
	row.LimitMicros, row.LimitUSD = &l, &lu
	row.AvailableMicros, row.AvailableUSD = &a, &au
	return row, true
}

// limitFor finds the configured ceiling for one (scope,key,period). The captured
// limits are authoritative; a missing entry means the budget is unlimited.
func limitFor(limits []budget.Limit, scope, key, period string) (int64, bool) {
	for _, l := range limits {
		if l.Scope == scope && l.Key == key && l.Period == period {
			return l.Micros, true
		}
	}
	return 0, false
}

// budgetAvailable is limit - (reported+estimated+unknown+reserved), signed and
// never clamped so a settled overrun stays visible. ok is false when any step
// overflows int64; no wrapped number is ever reported.
func budgetAvailable(limit int64, s budget.Snapshot) (int64, bool) {
	spent, ok := addMicros(s.Reported, s.Estimated)
	if !ok {
		return 0, false
	}
	if spent, ok = addMicros(spent, s.Unknown); !ok {
		return 0, false
	}
	if spent, ok = addMicros(spent, s.Reserved); !ok {
		return 0, false
	}
	return subMicros(limit, spent)
}

// addMicros returns a+b and reports whether the sum fits in an int64.
func addMicros(a, b int64) (int64, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// subMicros returns a-b and reports whether the difference fits in an int64.
func subMicros(a, b int64) (int64, bool) {
	diff := a - b
	if (b < 0 && diff < a) || (b > 0 && diff > a) {
		return 0, false
	}
	return diff, true
}

// periodStartUTC is the UTC boundary at which the period instance containing at
// begins: midnight for day, the 1st at midnight for month. It matches
// budget.periodStart exactly, so the reported start always names the instance
// the Snapshot read.
func periodStartUTC(period string, at time.Time) time.Time {
	t := at.UTC()
	switch period {
	case budget.PeriodMonth:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
}

// periodResetUTC is the UTC boundary after now at which the instance rolls over:
// next midnight for day, first-of-next-month midnight for month. The reset is
// informational only; spend controls never restore credit at a reset.
func periodResetUTC(period string, at time.Time) time.Time {
	start := periodStartUTC(period, at)
	if period == budget.PeriodMonth {
		return start.AddDate(0, 1, 0)
	}
	return start.AddDate(0, 0, 1)
}

// formatMicrosUSD renders exact integer micro-USD as a fixed six-fractional-digit
// USD string without ever going through float64 (whole = m/1e6, frac = m%1e6).
// A negative value keeps its sign on the whole part; math.MinInt64 is handled by
// formatting the uint64 magnitude, so no value wraps.
func formatMicrosUSD(micros int64) string {
	neg := micros < 0
	var mag uint64
	if neg {
		mag = uint64(-(micros + 1)) + 1 // two's-complement-safe magnitude
	} else {
		mag = uint64(micros)
	}
	whole, frac := mag/1_000_000, mag%1_000_000
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(strconv.FormatUint(whole, 10))
	b.WriteByte('.')
	f := strconv.FormatUint(frac, 10)
	b.WriteString(strings.Repeat("0", 6-len(f)))
	b.WriteString(f)
	return b.String()
}

// budgetReadError sanitizes a source error into one of two generic reasons. The
// raw error text (which may carry paths, DSNs or credentials) is never included.
func budgetReadError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return budgetTimeoutMsg
	}
	return budgetUnavailableMsg
}
