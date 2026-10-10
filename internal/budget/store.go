// Package budget implements the spend-control storage primitive on SQLite
// (modernc.org/sqlite): durable, atomic, integer-micro-USD reservations against
// client and account budgets, plus their settlement.
//
// Scope. This file owns a separate budget database, not the request ledger.
// A caller constructs it with Open at an explicit path. The app holds exclusive
// ownership and shares the store across generation-scoped proxy budget gates.
//
// Money. Every amount is integer micro-USD (1 USD = 1_000_000 micros) held in
// an int64. No comparison or accumulation in this file uses float64; sums are
// checked for int64 overflow and an overflow is an error, never a wrap.
//
// Model. A budget is a (scope, key, period) ceiling. scope is "client" or
// "account"; key is a client name or account id; period is "day" or "month" in
// UTC. A period instance is a concrete [start, end) UTC window identified by
// (period, start_utc) and chosen once, at admission, from the attempt
// timestamp. It is never re-derived at read time, so a late settlement always
// books into the instance the reservation was admitted under (no cross-period
// drift).
//
// A reservation is durable and covers an attempt. Every admitted reservation
// holds all FOUR instances reachable from the attempt — the client budget and
// the account budget, each for the UTC day and the UTC month containing the
// attempt timestamp — whether or not a limit happens to be configured for
// them. Only the supplied limits whose scope/key name the attempt's client or
// account restrict admission; a limit whose key names something else restricts
// nothing. Tracking the full set is what makes a limit configured later honest:
// it is evaluated against the real history of the period, not merely the subset
// it used to gate.
//
// Reserve tests every applicable limit inside ONE write transaction and either
// admits (writing the reservation plus the reserved amount on all four tracked
// instances) or denies, writing nothing. A limit that is not supplied for a
// (scope, key, period) is unlimited; a supplied limit of zero micros is a budget
// of zero and denies every reservation; a negative limit is invalid. A
// reservation's own micros must be strictly positive. Because each budget
// tracks total spend independently, a settlement books the full charge against
// every tracked instance.
//
// Settlement books exactly one of three bases — reported, estimated, unknown —
// and is idempotent: a retry whose normalized payload matches the recorded one
// is a no-op, while a retry that conflicts is rejected. A settlement charge is
// never clamped to the reservation: if the observed cost exceeds it, the full
// observed cost is booked and the period total may exceed its limit. An UNKNOWN
// settlement, however, is never free: it is charged at
// max(observed, reserved), so an unknown outcome can never release the
// store-held reservation for a discount or for nothing.
//
// Ownership and crash recovery. Orphan reservations (still open) are charged at
// their reserved ceiling, with basis unknown, by an EXPLICIT call to
// ReconcileOrphans. Open never reconciles on its own; this does not waive the
// application's single-writer ownership requirement. ReconcileOrphans
// must only be called at startup, before this process serves traffic, by a
// caller that can prove it is the exclusive process owner of the database; the
// store cannot verify single-process ownership for the caller. Settlement and
// reconciliation are nonetheless safe against a racing settle — the same store
// or another *Store on the same file — because a reservation is claimed
// transactionally, exactly once, before any counter moves: a stale or losing
// writer books nothing. There is no TTL sweep and no other exit from the open
// state.
package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers driver "sqlite"
)

// Scope and period domains. A Limit carries these as strings and they are
// validated on every entry point.
const (
	ScopeClient  = "client"
	ScopeAccount = "account"

	PeriodDay   = "day"
	PeriodMonth = "month"
)

// Settlement bases. Each contributes to a distinct per-period column so the
// provenance of every charged micro is durable and auditable.
const (
	BasisReported  = "reported"
	BasisEstimated = "estimated"
	BasisUnknown   = "unknown"
)

// Reservation states and settle reasons.
const (
	stateOpen    = "open"
	stateSettled = "settled"

	reasonOrphanStartup = "orphan_startup"
)

// ErrExceeded is the fail-closed denial reported when a reservation
// would not fit an applicable budget; it wraps the offending scope/key/period
// so errors.Is(err, ErrExceeded) still holds. ErrInvalid reports a malformed
// argument (empty identifiers, a zero timestamp, a non-positive reservation, a
// negative or duplicate limit, an unknown scope/period/basis). ErrClosed reports
// a write attempted on a store whose Close has already returned. The rest
// distinguish retry and lookup failures from store IO failures.
var (
	ErrExceeded           = errors.New("budget: limit exceeded")
	ErrUnknownReservation = errors.New("budget: unknown reservation")
	ErrConflict           = errors.New("budget: conflicting retry")
	ErrInvalid            = errors.New("budget: invalid argument")
	ErrClosed             = errors.New("budget: store closed")
)

// Limit is one configured ceiling: at most Micros micro-USD may be either
// already spent or reserved for (Scope, Key, Period) within a period instance.
// Micros must be non-negative; zero is a real budget of zero and denies every
// reservation, while the absence of a Limit for a (scope, key, period) means
// that budget is unlimited.
type Limit struct {
	Scope  string
	Key    string
	Period string
	Micros int64
}

// Reservation is one attempt's durable claim on the budgets named by its
// Client and Account keys. At is the attempt timestamp: it fixes the UTC day
// and month period instances the claim is made against, and it is recorded so
// a settlement always books into the original instances. Micros must be
// strictly positive.
type Reservation struct {
	ID      string
	Client  string
	Account string
	At      time.Time
	Micros  int64
}

// Settlement closes a reservation with the micro-USD actually charged and the
// provenance of that number. Basis must be one of BasisReported,
// BasisEstimated or BasisUnknown. Micros is the full observed charge and is not
// clamped to the reservation; for BasisUnknown it is floored at the
// reservation's held micros, so an unknown outcome never releases the hold for
// free.
type Settlement struct {
	ID     string
	Micros int64
	Basis  string
}

// Snapshot is the accumulated state of one period instance. Reported,
// Estimated and Unknown are the settled spend split by basis; Reserved is what
// open reservations currently hold. Actual spend of zero is valid and normal.
type Snapshot struct {
	Reported  int64
	Estimated int64
	Unknown   int64
	Reserved  int64
}

// Store is a SQLite-backed budget store. Writes are serialized by mu so a
// single writer transaction is in flight at a time; the connection pool is
// bounded at two connections so read-only snapshots can proceed on their own
// connection while a write transaction is open. Transactions begin IMMEDIATE
// and carry a busy timeout, and the store never retries internally: if the
// database is busy beyond the timeout, or any step fails, the operation fails
// closed.
//
// mu serializes only the writers of THIS Store instance. A deployment that opens
// the same database file from more than one process must enforce single-writer
// ownership at the application level; the store cannot verify it (see
// ReconcileOrphans).
//
// Close shares mu with the writers, so the lifecycle of the handle is itself
// serialized: Close never returns while a write transaction is in flight, and
// once it has returned the handle is terminally closed and every later write
// fails closed. closed and closeErr are guarded by mu.
type Store struct {
	db       *sql.DB
	mu       sync.Mutex // serializes write transactions and Close
	closed   bool       // set by the first Close, under mu
	closeErr error      // the first Close's error, replayed by later calls
}

// migrations are applied in order; index+1 is the schema version.
//
// budget_periods holds one row per (scope, key, period, start_utc) instance:
// the durable counters. limit_micros is informational (the limit supplied at
// the most recent admission) and is never consulted for a decision — every
// decision uses the limits passed to Reserve.
//
// budget_reservations holds one row per attempt, keyed by the attempt id.
// budget_reservation_periods pins, for each tracked (scope, key, period)
// instance — always the client and account budgets for the day and month — how
// much that reservation holds there, so settlement decrements exactly the right
// rows regardless of when it runs.
var migrations = []string{
	`CREATE TABLE budget_periods (
		scope TEXT NOT NULL,
		key TEXT NOT NULL,
		period TEXT NOT NULL,
		start_utc INTEGER NOT NULL,
		limit_micros INTEGER NOT NULL DEFAULT 0,
		reserved_micros INTEGER NOT NULL DEFAULT 0,
		spent_reported_micros INTEGER NOT NULL DEFAULT 0,
		spent_estimated_micros INTEGER NOT NULL DEFAULT 0,
		spent_unknown_micros INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (scope, key, period, start_utc)
	);
	CREATE TABLE budget_reservations (
		id TEXT PRIMARY KEY,
		client TEXT NOT NULL,
		account TEXT NOT NULL,
		at_unix INTEGER NOT NULL,
		micros INTEGER NOT NULL,
		state TEXT NOT NULL,
		basis TEXT NOT NULL DEFAULT '',
		charged_micros INTEGER NOT NULL DEFAULT 0,
		settle_reason TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX budget_reservations_state ON budget_reservations(state);
	CREATE TABLE budget_reservation_periods (
		reservation_id TEXT NOT NULL,
		scope TEXT NOT NULL,
		key TEXT NOT NULL,
		period TEXT NOT NULL,
		start_utc INTEGER NOT NULL,
		reserved_micros INTEGER NOT NULL,
		PRIMARY KEY (reservation_id, scope, key, period)
	);`,
}

// Open opens (creating if needed) the budget database at path. The path is
// explicit and caller-chosen; typically DataDir/budget.db, but the store does
// not know or consult configuration. It never reconciles orphan reservations:
// see ReconcileOrphans, which the caller must invoke explicitly.
//
// Transactions are opened IMMEDIATE (_txlock=immediate): a writer takes the
// write lock up front instead of upgrading a read snapshot, which is what makes
// the "claim, then book" settlement safe even if another *Store or process
// shares the file.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("budget: open: empty database path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("budget: open: %w", err)
	}
	// Pre-create with 0600; SQLite gives WAL/SHM files the same mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("budget: open: %w", err)
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("budget: open: %w", err)
	}

	q := url.Values{}
	q.Add("_txlock", "immediate")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("budget: open: %w", err)
	}
	// Two bounded connections: the write mutex keeps at most one writer
	// transaction in flight, leaving the other connection free for reads.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("budget: migrate: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("budget: migrate: %w", err)
	}
	var v int
	err = tx.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return fmt.Errorf("budget: migrate: %w", err)
		}
	case err != nil:
		return fmt.Errorf("budget: migrate: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("budget: database schema version %d is newer than supported %d", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if _, err := tx.Exec(migrations[i]); err != nil {
			return fmt.Errorf("budget: migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, len(migrations)); err != nil {
		return fmt.Errorf("budget: migrate: %w", err)
	}
	return tx.Commit()
}

// Close closes the database. It serializes with the writers on the same mu that
// Reserve, Settle and ReconcileOrphans hold for the whole of their write
// transaction, so it never returns while a public write is in flight: Close
// waits until the in-flight writer has committed or rolled back and released
// the mutex. The database is therefore quiescent when db.Close runs, and no
// write transaction can still be running (and none can commit) after Close
// returns.
//
// Once Close has returned, the handle is terminally closed and is never
// reopened: every later Reserve, Settle or ReconcileOrphans fails closed with
// ErrClosed before it begins a transaction, and reads fail with the driver's
// closed-database error. A caller that wants the database again must Open a
// fresh store; there is no reopen path through a closed *Store.
//
// Close is idempotent and safe to call concurrently: the first call performs the
// close and every later call is a no-op that replays the first call's result
// (nil unless the underlying db.Close failed).
//
// Bound. Close blocks for as long as the in-flight writer holds mu, and that is
// bounded only by the writer's own context (plus the connection's 5s
// busy_timeout): a caller that passes an unbounded-lived context while its
// transaction stalls can delay Close without limit. Close does not impose its
// own deadline on the writer, because aborting an in-flight commit is not safe;
// the single-writer callers are expected to bound their own contexts.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.closeErr = s.db.Close()
	return s.closeErr
}

// Reserve atomically admits one attempt against every applicable budget.
//
// Every admitted attempt holds the four instances named by r.Client and
// r.Account for the UTC day and month containing r.At. The supplied limits whose
// scope/key name r.Client ("client") or r.Account ("account") restrict
// admission: for each, spent + reserved + r.Micros must not exceed Micros. All
// of them are checked, and the hold is written on all four tracked instances,
// inside a single write transaction; if any limit has no room, or the
// arithmetic overflows int64, nothing is written and the call returns
// ErrExceeded (or the arithmetic error), so callers fail closed. A limit that
// names a different key restricts nothing but is still tracked against.
//
// Reserve is idempotent on r.ID: replaying an identical reservation returns nil
// without reserving again; replaying the same ID with a different payload
// returns ErrConflict. The reservation records the exact instances it touched,
// so a later settlement cannot be moved to a different period. A non-positive
// r.Micros, an empty id/client/account or a zero r.At is ErrInvalid.
func (s *Store) Reserve(ctx context.Context, r Reservation, limits []Limit) error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("%w: reserve: empty attempt id", ErrInvalid)
	}
	if r.Client == "" || r.Account == "" {
		return fmt.Errorf("%w: reserve: attempt %q: client and account are required", ErrInvalid, r.ID)
	}
	if r.At.IsZero() {
		return fmt.Errorf("%w: reserve: attempt %q: zero attempt timestamp", ErrInvalid, r.ID)
	}
	if r.Micros <= 0 {
		return fmt.Errorf("%w: reserve: attempt %q: reservation must be greater than zero, got %d", ErrInvalid, r.ID, r.Micros)
	}

	restrictions, err := applicableChecks(r, limits)
	if err != nil {
		return err
	}
	tracked, err := trackedPeriods(r)
	if err != nil {
		return err
	}
	restricted := make(map[[3]string]int64, len(restrictions))
	for _, c := range restrictions {
		restricted[[3]string{c.scope, c.key, c.period}] = c.limit
	}
	atUnix := r.At.Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("%w: reserve", ErrClosed)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("budget: reserve: %w", err)
	}
	defer tx.Rollback()

	// Idempotency: an identical retry is a no-op, a conflicting one an error.
	var (
		existClient, existAccount string
		existAt, existMicros      int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT client, account, at_unix, micros FROM budget_reservations WHERE id = ?`, r.ID).
		Scan(&existClient, &existAccount, &existAt, &existMicros)
	switch {
	case err == sql.ErrNoRows:
		// First admission for this attempt id; proceed.
	case err != nil:
		return fmt.Errorf("budget: reserve: %w", err)
	case existClient == r.Client && existAccount == r.Account && existAt == atUnix && existMicros == r.Micros:
		return nil // identical retry: the reservation already exists
	default:
		return fmt.Errorf("%w: attempt %q already reserved with a different payload", ErrConflict, r.ID)
	}

	// One row per tracked instance, created if absent and then read back so the
	// restriction check and the reserved delta both see the durable counters.
	type instance struct {
		periodLink
		limit       int64 // restriction limit supplied at this admission (0 if none)
		hasLimit    bool
		limitMicros int64 // existing informational value
		reserved    int64
	}
	instances := make([]instance, 0, len(tracked))
	for _, tp := range tracked {
		lim, has := restricted[[3]string{tp.scope, tp.key, tp.period}]
		if !has {
			lim = 0
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO budget_periods (scope, key, period, start_utc, limit_micros)
			 VALUES (?,?,?,?,?)
			 ON CONFLICT(scope, key, period, start_utc) DO NOTHING`,
			tp.scope, tp.key, tp.period, tp.start, lim); err != nil {
			return fmt.Errorf("budget: reserve: %w", err)
		}
		var reported, estimated, unknown, reserved, limitMicros int64
		if err := tx.QueryRowContext(ctx,
			`SELECT spent_reported_micros, spent_estimated_micros, spent_unknown_micros,
			        reserved_micros, limit_micros
			 FROM budget_periods WHERE scope=? AND key=? AND period=? AND start_utc=?`,
			tp.scope, tp.key, tp.period, tp.start).
			Scan(&reported, &estimated, &unknown, &reserved, &limitMicros); err != nil {
			return fmt.Errorf("budget: reserve: %w", err)
		}
		if has {
			spent, ok := addMicros(reported, estimated)
			if !ok {
				return fmt.Errorf("budget: reserve: %s/%s/%s: spent overflow", tp.scope, tp.key, tp.period)
			}
			if spent, ok = addMicros(spent, unknown); !ok {
				return fmt.Errorf("budget: reserve: %s/%s/%s: spent overflow", tp.scope, tp.key, tp.period)
			}
			outstanding, ok := addMicros(spent, reserved)
			if !ok {
				return fmt.Errorf("budget: reserve: %s/%s/%s: outstanding overflow", tp.scope, tp.key, tp.period)
			}
			total, ok := addMicros(outstanding, r.Micros)
			if !ok {
				return fmt.Errorf("budget: reserve: %s/%s/%s: reservation %d overflows int64", tp.scope, tp.key, tp.period, r.Micros)
			}
			if total > c0(lim) {
				return fmt.Errorf("%w: %s/%s/%s: %d outstanding + %d reserved > %d limit",
					ErrExceeded, tp.scope, tp.key, tp.period, outstanding, r.Micros, lim)
			}
		}
		instances = append(instances, instance{
			periodLink:  tp,
			limit:       lim,
			hasLimit:    has,
			limitMicros: limitMicros,
			reserved:    reserved,
		})
	}

	// Every restriction passed: record this attempt's hold on all four
	// instances (all-or-nothing; any failure above rolls the whole tx back).
	for _, inst := range instances {
		newReserved, ok := addMicros(inst.reserved, r.Micros)
		if !ok {
			return fmt.Errorf("budget: reserve: %s/%s/%s: reserved overflow", inst.scope, inst.key, inst.period)
		}
		limVal := inst.limitMicros
		if inst.hasLimit {
			limVal = inst.limit
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE budget_periods SET limit_micros = ?, reserved_micros = ?
			 WHERE scope=? AND key=? AND period=? AND start_utc=?`,
			limVal, newReserved, inst.scope, inst.key, inst.period, inst.start); err != nil {
			return fmt.Errorf("budget: reserve: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO budget_reservations (id, client, account, at_unix, micros, state)
		 VALUES (?,?,?,?,?,?)`,
		r.ID, r.Client, r.Account, atUnix, r.Micros, stateOpen); err != nil {
		return fmt.Errorf("budget: reserve: %w", err)
	}
	for _, inst := range instances {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO budget_reservation_periods (reservation_id, scope, key, period, start_utc, reserved_micros)
			 VALUES (?,?,?,?,?,?)`,
			r.ID, inst.scope, inst.key, inst.period, inst.start, r.Micros); err != nil {
			return fmt.Errorf("budget: reserve: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("budget: reserve: %w", err)
	}
	return nil
}

// c0 returns its argument unchanged; it exists only to keep the deny branch's
// comparison spelled with the limit variable rather than a constant.
func c0(limit int64) int64 { return limit }

// Settle closes the reservation st.ID with the charge st.Micros booked under
// basis st.Basis against every period instance the reservation pinned when it
// was admitted — never the instance a read-time clock would pick. The reserved
// amount is released on each of those instances and the full charge is booked,
// even when it exceeds the reservation (overrun is recorded, not clamped). An
// unknown basis is floored at the reservation's held micros.
//
// Settle is idempotent: replaying the recorded (normalized) payload returns nil
// without charging twice, while a second settle with a different charge or
// basis returns ErrConflict, and settling an unknown id returns
// ErrUnknownReservation. If the reservation was already settled — including by
// a racing settle in this or another Store — the recorded basis/charge is
// authoritative and is never revised, and this call books nothing.
func (s *Store) Settle(ctx context.Context, st Settlement) error {
	if strings.TrimSpace(st.ID) == "" {
		return fmt.Errorf("%w: settle: empty attempt id", ErrInvalid)
	}
	if st.Micros < 0 {
		return fmt.Errorf("%w: settle: attempt %q: negative charge %d", ErrInvalid, st.ID, st.Micros)
	}
	if !validBasis(st.Basis) {
		return fmt.Errorf("%w: settle: attempt %q: invalid basis %q", ErrInvalid, st.ID, st.Basis)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("%w: settle", ErrClosed)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("budget: settle: %w", err)
	}
	defer tx.Rollback()

	// Claim the reservation before touching any period counter. A lost claim
	// (already settled, here or elsewhere) books nothing.
	claimed, effective, rec, err := claimReservationTx(ctx, tx, st.ID, st.Micros, st.Basis, "")
	if err != nil {
		return err
	}
	if !claimed {
		switch {
		case rec == nil:
			return fmt.Errorf("%w: %q", ErrUnknownReservation, st.ID)
		case rec.basis == st.Basis && rec.charged == effective:
			return nil // identical retry: already settled, do not charge twice
		default:
			return fmt.Errorf("%w: reservation %q already settled (charge %d basis %s)",
				ErrConflict, st.ID, rec.charged, rec.basis)
		}
	}

	if err := applySettlementTx(ctx, tx, st.ID, effective, st.Basis); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("budget: settle: %w", err)
	}
	return nil
}

// recordedReservation is the durable row state a lost claim needs to answer.
type recordedReservation struct {
	state    string
	basis    string
	charged  int64
	reserved int64 // the micros the reservation held (its admission ceiling)
}

// normalizeCharge is the single definition of the settled charge: an unknown
// basis is floored at the reservation's held micros, so an unknown outcome can
// never release the store-held hold for free; every other basis books the full
// observed charge unchanged (overrun is recorded, not clamped).
func normalizeCharge(requested, reserved int64, basis string) int64 {
	if basis == BasisUnknown && reserved > requested {
		return reserved
	}
	return requested
}

// claimReservationTx atomically moves reservation id from open to settled,
// recording the normalized charge, and reports whether THIS call won the claim.
//
// The guarded UPDATE is the first thing it does and it insists that exactly one
// row changed. It performs NO period-counter deltas — the caller applies those
// only when claimed is true. That ordering is what makes settlement safe under
// concurrent settlement (the same store, or another *Store/process sharing the
// file): a stale or racing writer either wins the single claim and only then
// books, or loses it and books nothing, so reserved money can never be released
// twice and no charge can be double-counted.
//
// When claimed is false, rec is the recorded row (nil for an unknown id) so the
// caller can apply idempotent/conflict semantics; effective is the normalized
// charge that a retry of this payload would have recorded.
func claimReservationTx(ctx context.Context, tx *sql.Tx, id string, requested int64, basis, reason string) (claimed bool, effective int64, rec *recordedReservation, err error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE budget_reservations
		 SET state = ?, basis = ?,
		     charged_micros = CASE WHEN ? = ? THEN MAX(micros, ?) ELSE ? END,
		     settle_reason = ?
		 WHERE id = ? AND state = ?`,
		stateSettled, basis, basis, BasisUnknown, requested, requested, reason, id, stateOpen)
	if err != nil {
		return false, 0, nil, fmt.Errorf("budget: settle: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, 0, nil, fmt.Errorf("budget: settle: %w", err)
	}
	switch {
	case n == 1:
		var charged int64
		if err := tx.QueryRowContext(ctx,
			`SELECT charged_micros FROM budget_reservations WHERE id = ?`, id).Scan(&charged); err != nil {
			return false, 0, nil, fmt.Errorf("budget: settle: %w", err)
		}
		return true, charged, nil, nil
	case n > 1:
		return false, 0, nil, fmt.Errorf("budget: settle: reservation %q: claim affected %d rows, want exactly 1", id, n)
	}

	// n == 0: the id is unknown or already settled. Read it back to say which.
	var r recordedReservation
	err = tx.QueryRowContext(ctx,
		`SELECT state, basis, charged_micros, micros FROM budget_reservations WHERE id = ?`, id).
		Scan(&r.state, &r.basis, &r.charged, &r.reserved)
	if err == sql.ErrNoRows {
		return false, 0, nil, nil // unknown id
	}
	if err != nil {
		return false, 0, nil, fmt.Errorf("budget: settle: %w", err)
	}
	if r.state != stateSettled {
		// The row exists and is open, yet the guarded claim matched no row:
		// fail closed rather than guess.
		return false, 0, nil, fmt.Errorf("budget: settle: reservation %q is %q but was not claimable", id, r.state)
	}
	return false, normalizeCharge(requested, r.reserved, basis), &r, nil
}

// applySettlementTx books charge against every period instance pinned to the
// reservation id and releases each pinned hold as the spend lands. The caller
// must have already WON the transaction claim for id (see claimReservationTx);
// it must never be called on a reservation it did not claim, so a hold is
// released exactly once. Sums are checked and a release is refused if it would
// drive reserved negative: either failure aborts the whole settlement, leaving
// the reservation open (the transaction rolls back).
func applySettlementTx(ctx context.Context, tx *sql.Tx, id string, charge int64, basis string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT scope, key, period, start_utc, reserved_micros
		 FROM budget_reservation_periods WHERE reservation_id = ?`, id)
	if err != nil {
		return fmt.Errorf("budget: settle: %w", err)
	}
	type pinnedPeriod struct {
		scope, key, period string
		start, reserved    int64
	}
	var pinned []pinnedPeriod
	for rows.Next() {
		var p pinnedPeriod
		if err := rows.Scan(&p.scope, &p.key, &p.period, &p.start, &p.reserved); err != nil {
			rows.Close()
			return fmt.Errorf("budget: settle: %w", err)
		}
		pinned = append(pinned, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("budget: settle: %w", err)
	}
	rows.Close()

	col := spentColumn(basis)
	for _, p := range pinned {
		var current, reservedNow int64
		if err := tx.QueryRowContext(ctx,
			`SELECT `+col+`, reserved_micros FROM budget_periods
			 WHERE scope=? AND key=? AND period=? AND start_utc=?`,
			p.scope, p.key, p.period, p.start).Scan(&current, &reservedNow); err != nil {
			return fmt.Errorf("budget: settle: %w", err)
		}
		next, ok := addMicros(current, charge)
		if !ok {
			return fmt.Errorf("budget: settle: %s/%s/%s: charging %d overflows int64",
				p.scope, p.key, p.period, charge)
		}
		if reservedNow < p.reserved {
			return fmt.Errorf("budget: settle: %s/%s/%s: releasing %d but only %d is held",
				p.scope, p.key, p.period, p.reserved, reservedNow)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE budget_periods SET `+col+` = ?, reserved_micros = reserved_micros - ?
			 WHERE scope=? AND key=? AND period=? AND start_utc=?`,
			next, p.reserved, p.scope, p.key, p.period, p.start); err != nil {
			return fmt.Errorf("budget: settle: %w", err)
		}
	}
	return nil
}

// Snapshot returns the accumulated state of the period instance containing at
// for (scope, key, period). A period instance that has never been touched
// reports an all-zero Snapshot with no error.
func (s *Store) Snapshot(ctx context.Context, scope, key, period string, at time.Time) (Snapshot, error) {
	if err := validateScope(scope); err != nil {
		return Snapshot{}, err
	}
	if err := validatePeriod(period); err != nil {
		return Snapshot{}, err
	}
	if key == "" {
		return Snapshot{}, fmt.Errorf("%w: snapshot: empty key", ErrInvalid)
	}
	if at.IsZero() {
		return Snapshot{}, fmt.Errorf("%w: snapshot: zero timestamp", ErrInvalid)
	}
	start, err := periodStart(period, at)
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	err = s.db.QueryRowContext(ctx,
		`SELECT spent_reported_micros, spent_estimated_micros, spent_unknown_micros, reserved_micros
		 FROM budget_periods WHERE scope=? AND key=? AND period=? AND start_utc=?`,
		scope, key, period, start).
		Scan(&snap.Reported, &snap.Estimated, &snap.Unknown, &snap.Reserved)
	if err == sql.ErrNoRows {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("budget: snapshot: %w", err)
	}
	return snap, nil
}

// ReconcileOrphans settles every still-open reservation at its reserved
// ceiling, with basis unknown and settle_reason "orphan_startup". It is
// idempotent: an already-settled reservation is never touched, and a second
// call finds nothing open. It never releases reserved money — the ceiling is
// charged, not refunded.
//
// This is an EXPLICIT startup operation. Open does NOT call it and never
// reconciles on its own, because settling an open reservation while its attempt
// is still running would charge that attempt twice. The caller must therefore
// invoke ReconcileOrphans only at startup, before serving traffic, and only
// when it can prove it is the exclusive process owner of this database (a
// single-writer / single-router deployment). The store cannot verify exclusive
// ownership for the caller; there is no TTL sweep and no other exit from the
// open state.
//
// Even so, the per-orphan pass claims each reservation transactionally before
// booking, so a stale list entry — one already settled by a racing Settle in
// this or another store, or by another process — is a harmless no-op rather
// than a second charge.
func (s *Store) ReconcileOrphans(ctx context.Context) error {
	ids, err := s.openReservationIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.reconcileOrphan(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// openReservationIDs returns the ids of the reservations still open at the
// moment it runs. The list is a hint only: reconcileOrphan re-establishes, and
// atomically claims, the open state inside its own transaction, so an id that
// another writer settles after this snapshot becomes a no-op rather than a
// second charge.
func (s *Store) openReservationIDs(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("%w: reconcile orphans", ErrClosed)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM budget_reservations WHERE state = ? ORDER BY id`, stateOpen)
	if err != nil {
		return nil, fmt.Errorf("budget: reconcile orphans: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("budget: reconcile orphans: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("budget: reconcile orphans: %w", err)
	}
	return ids, nil
}

// reconcileOrphan settles ONE still-open reservation at its held ceiling with
// basis unknown and settle_reason orphan_startup. It claims the reservation
// transactionally before touching any period counter, so if the id was already
// settled — by another goroutine, another *Store, or another process sharing
// the file — the claim is lost and the call is a no-op: no second charge and no
// negative reserved hold. The bool reports whether this call did the work.
func (s *Store) reconcileOrphan(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, fmt.Errorf("%w: reconcile orphans", ErrClosed)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("budget: reconcile orphans: %w", err)
	}
	defer tx.Rollback()

	// requested 0 with basis unknown: normalization floors the charge at the
	// reservation's held micros, so the ceiling is charged and the hold is
	// never released for free.
	claimed, effective, _, err := claimReservationTx(ctx, tx, id, 0, BasisUnknown, reasonOrphanStartup)
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil // already settled elsewhere: idempotent skip
	}
	if err := applySettlementTx(ctx, tx, id, effective, BasisUnknown); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("budget: reconcile orphans: %w", err)
	}
	return true, nil
}

// periodLink names one concrete period instance a reservation touches.
type periodLink struct {
	scope, key, period string
	start              int64
}

// trackedPeriods returns the four instances every admitted reservation holds:
// the client and account budgets for both the UTC day and the UTC month
// containing r.At. They are tracked on every attempt, whether or not a limit is
// configured for them, so a limit configured later is evaluated against the
// true history of the period instead of only what it happened to gate.
func trackedPeriods(r Reservation) ([]periodLink, error) {
	out := make([]periodLink, 0, 4)
	for _, scope := range [...]string{ScopeClient, ScopeAccount} {
		key := r.Client
		if scope == ScopeAccount {
			key = r.Account
		}
		for _, period := range [...]string{PeriodDay, PeriodMonth} {
			start, err := periodStart(period, r.At)
			if err != nil {
				return nil, err
			}
			out = append(out, periodLink{scope: scope, key: key, period: period, start: start})
		}
	}
	return out, nil
}

// check is one supplied, applicable limit resolved to the period instance it
// gates. It restricts admission only; the reservation is still tracked on all
// four instances regardless.
type check struct {
	scope, key, period string
	start              int64
	limit              int64
}

// applicableChecks validates the supplied limits and resolves the ones that
// restrict r into concrete period instances. A limit that names a different key
// or scope than r gates nothing and is skipped after validation. Duplicate
// limits for the same (scope, key, period) are rejected rather than silently
// merged.
func applicableChecks(r Reservation, limits []Limit) ([]check, error) {
	seen := make(map[[3]string]bool, len(limits))
	checks := make([]check, 0, len(limits))
	for _, l := range limits {
		if err := validateScope(l.Scope); err != nil {
			return nil, fmt.Errorf("budget: reserve: %w", err)
		}
		if err := validatePeriod(l.Period); err != nil {
			return nil, fmt.Errorf("budget: reserve: %w", err)
		}
		if l.Key == "" {
			return nil, fmt.Errorf("%w: reserve: limit for %s has an empty key", ErrInvalid, l.Scope)
		}
		if l.Micros < 0 {
			return nil, fmt.Errorf("%w: reserve: limit for %s/%s/%s is negative (%d)", ErrInvalid, l.Scope, l.Key, l.Period, l.Micros)
		}
		switch l.Scope {
		case ScopeClient:
			if l.Key != r.Client {
				continue
			}
		case ScopeAccount:
			if l.Key != r.Account {
				continue
			}
		}
		key := [3]string{l.Scope, l.Key, l.Period}
		if seen[key] {
			return nil, fmt.Errorf("%w: reserve: duplicate limit for %s/%s/%s", ErrInvalid, l.Scope, l.Key, l.Period)
		}
		seen[key] = true
		start, err := periodStart(l.Period, r.At)
		if err != nil {
			return nil, err
		}
		checks = append(checks, check{scope: l.Scope, key: l.Key, period: l.Period, start: start, limit: l.Micros})
	}
	return checks, nil
}

func validateScope(scope string) error {
	switch scope {
	case ScopeClient, ScopeAccount:
		return nil
	}
	return fmt.Errorf("%w: invalid scope %q (want %q or %q)", ErrInvalid, scope, ScopeClient, ScopeAccount)
}

func validatePeriod(period string) error {
	switch period {
	case PeriodDay, PeriodMonth:
		return nil
	}
	return fmt.Errorf("%w: invalid period %q (want %q or %q)", ErrInvalid, period, PeriodDay, PeriodMonth)
}

func validBasis(basis string) bool {
	switch basis {
	case BasisReported, BasisEstimated, BasisUnknown:
		return true
	}
	return false
}

// spentColumn maps a validated basis to its per-period accumulator column.
func spentColumn(basis string) string {
	switch basis {
	case BasisReported:
		return "spent_reported_micros"
	case BasisEstimated:
		return "spent_estimated_micros"
	default:
		return "spent_unknown_micros"
	}
}

// periodStart returns the Unix time (UTC seconds) at which the period instance
// containing at begins. It is a pure function of (period, at) and never depends
// on the local time zone: a period instance is fixed at admission and is never
// re-derived at read time.
func periodStart(period string, at time.Time) (int64, error) {
	t := at.UTC()
	switch period {
	case PeriodDay:
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case PeriodMonth:
		t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return 0, fmt.Errorf("%w: invalid period %q (want %q or %q)", ErrInvalid, period, PeriodDay, PeriodMonth)
	}
	return t.Unix(), nil
}

// addMicros returns a+b and reports whether the sum fits in an int64. It is
// integer-only by construction; no budget arithmetic here ever goes through
// float64.
func addMicros(a, b int64) (int64, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}
