package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hpst3r/localrouter/internal/core"

	_ "modernc.org/sqlite" // registers driver "sqlite"
)

// Store is the SQLite-backed identity store. See the package comment.
//
// life guards the handle's lifecycle: every operation holds it shared for
// its whole duration and Close takes it exclusively, so Close waits for
// in-flight operations (each bounded by Options.OpTimeout) and, once it has
// returned, every operation fails with ErrClosed. wmu serializes this
// instance's write transactions; transactions are IMMEDIATE, so writers in
// other *Store instances or processes serialize on SQLite's write lock.
type Store struct {
	db     *sql.DB
	opts   Options
	clock  core.Clock
	life   sync.RWMutex
	closed bool // guarded by life
	wmu    sync.Mutex
}

// migrations are applied in order; index+1 is the schema version. Times are
// unix milliseconds UTC.
var migrations = []string{
	`CREATE TABLE meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE users (
		id TEXT PRIMARY KEY,
		status TEXT NOT NULL CHECK (status IN ('active','disabled','deleted')),
		role TEXT NOT NULL CHECK (role IN ('user','admin')),
		display_name TEXT,
		email TEXT,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		last_login_at INTEGER,
		disabled_at INTEGER,
		deleted_at INTEGER
	);
	CREATE TABLE identities (
		issuer TEXT NOT NULL,
		subject TEXT NOT NULL,
		user_id TEXT NOT NULL UNIQUE REFERENCES users(id),
		created_at INTEGER NOT NULL,
		PRIMARY KEY (issuer, subject)
	);
	CREATE TABLE subject_tombstones (
		digest BLOB PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES users(id),
		deleted_at INTEGER NOT NULL
	);
	CREATE TABLE api_keys (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES users(id),
		name TEXT NOT NULL,
		token_hash BLOB NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		revoked_at INTEGER,
		revoke_reason TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX api_keys_user ON api_keys(user_id);
	CREATE TABLE sessions (
		token_hash BLOB PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES users(id),
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		idle_expires_at INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL
	);
	CREATE INDEX sessions_user ON sessions(user_id);
	CREATE TABLE audit_events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		at INTEGER NOT NULL,
		action TEXT NOT NULL,
		actor_kind TEXT NOT NULL,
		actor_user_id TEXT NOT NULL DEFAULT '',
		target_user_id TEXT NOT NULL DEFAULT '',
		target_key_id TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT ''
	);`,
	// v2: owned static key registry (statickeys.go). user_id has no
	// foreign key: a configured owner may not be provisioned yet.
	`CREATE TABLE static_keys (
		digest BLOB PRIMARY KEY CHECK (length(digest) = 32),
		user_id TEXT NOT NULL,
		registered_at INTEGER NOT NULL,
		revoked_at INTEGER,
		revoke_reason TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX static_keys_user ON static_keys(user_id);`,
}

// Open opens (creating if needed) the identity database at path. The parent
// directory is created 0700 if missing, the database file is created or
// tightened to 0600 (SQLite gives the WAL/SHM files the same mode) and a
// symlink at path is refused. A schema newer than this binary is refused.
// See Options for the pinned issuer/client binding. On platforms without
// symlink-safe file creation (non-unix, e.g. windows) Open always fails with
// an error wrapping errors.ErrUnsupported.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	opts, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: open: empty database path", ErrInvalid)
	}
	if err := createDBFile(path); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Add("_txlock", "immediate")
	// The busy handler does not observe the context, so it is bounded by the
	// same per-operation limit.
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", opts.OpTimeout.Milliseconds()))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	// Overwrite deleted content so scrubbed profile data and digests do not
	// linger in free pages.
	q.Add("_pragma", "secure_delete(1)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("identity: open: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)

	s := &Store{db: db, opts: opts, clock: opts.Clock}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// maxBindingBytes bounds the pinned issuer and client id.
const maxBindingBytes = 2048

func normalizeOptions(o Options) (Options, error) {
	if !validText(o.Issuer, maxBindingBytes) {
		return o, fmt.Errorf("%w: issuer must be non-empty printable UTF-8", ErrInvalid)
	}
	if !validText(o.ClientID, maxBindingBytes) {
		return o, fmt.Errorf("%w: client id must be non-empty printable UTF-8", ErrInvalid)
	}
	idleExplicit := o.SessionIdleTTL != 0
	durs := []struct {
		name     string
		v        *time.Duration
		def, max time.Duration
	}{
		{"key max ttl", &o.KeyMaxTTL, MaxKeyTTL, MaxKeyTTL},
		{"login max age", &o.LoginMaxAge, MaxLoginAge, MaxLoginAge},
		{"session absolute ttl", &o.SessionAbsoluteTTL, MaxSessionAbsolute, MaxSessionAbsolute},
		{"session idle ttl", &o.SessionIdleTTL, MaxSessionIdle, MaxSessionIdle},
		{"max auth age", &o.MaxAuthAge, DefaultMaxAuthAge, MaxAuthAgeCeiling},
		{"op timeout", &o.OpTimeout, DefaultOpTimeout, time.Minute},
	}
	for _, d := range durs {
		if *d.v == 0 {
			*d.v = d.def
		}
		if *d.v < 0 || *d.v > d.max {
			return o, fmt.Errorf("%w: %s must be in (0, %s]", ErrInvalid, d.name, d.max)
		}
	}
	if o.SessionIdleTTL > o.SessionAbsoluteTTL {
		// An explicit idle above the absolute lifetime is a configuration
		// error; a defaulted idle is clamped to a tighter absolute.
		if idleExplicit {
			return o, fmt.Errorf("%w: session idle ttl exceeds absolute ttl", ErrInvalid)
		}
		o.SessionIdleTTL = o.SessionAbsoluteTTL
	}
	if o.MaxKeysPerUser == 0 {
		o.MaxKeysPerUser = DefaultMaxKeysPerUser
	}
	if o.MaxKeysPerUser < 1 || o.MaxKeysPerUser > MaxKeysPerUserCeiling {
		return o, fmt.Errorf("%w: max keys per user must be in [1, %d]", ErrInvalid, MaxKeysPerUserCeiling)
	}
	if o.Clock == nil {
		o.Clock = core.SystemClock{}
	}
	if o.Rand == nil {
		o.Rand = rand.Reader
	}
	return o, nil
}

// validText reports whether s is non-empty, at most max bytes, valid UTF-8
// and free of control characters.
func validText(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)
	}) < 0
}

func (s *Store) migrate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.OpTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("identity: migrate: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("identity: migrate: %w", err)
	}
	var v int
	err = tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return fmt.Errorf("identity: migrate: %w", err)
		}
	case err != nil:
		return fmt.Errorf("identity: migrate: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("identity: database schema version %d is newer than supported %d", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("identity: migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, len(migrations)); err != nil {
		return fmt.Errorf("identity: migrate: %w", err)
	}
	if err := s.pinBinding(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Binding meta keys. Errors name the configuration key, never its value.
const (
	metaIssuer   = "oidc_issuer"
	metaClientID = "oidc_client_id"
)

// pinBinding records the issuer and client id on first initialization and
// afterwards requires a byte-exact match: users are keyed by (issuer,
// subject), and subjects may be pairwise per client, so a silent change
// would orphan or cross-match every identity.
func (s *Store) pinBinding(ctx context.Context, tx *sql.Tx) error {
	want := []struct{ key, value, label string }{
		{metaIssuer, s.opts.Issuer, "issuer"},
		{metaClientID, s.opts.ClientID, "client_id"},
	}
	var pinned int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM meta WHERE key IN (?, ?)`, metaIssuer, metaClientID).Scan(&pinned); err != nil {
		return fmt.Errorf("identity: binding: %w", err)
	}
	if pinned == 0 {
		for _, w := range want {
			if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)`, w.key, w.value); err != nil {
				return fmt.Errorf("identity: binding: %w", err)
			}
		}
		return nil
	}
	for _, w := range want {
		var got string
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, w.key).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && got != w.value) {
			return fmt.Errorf("%w (identity.oidc.%s)", ErrBindingMismatch, w.label)
		}
		if err != nil {
			return fmt.Errorf("identity: binding: %w", err)
		}
	}
	return nil
}

// Close closes the database. It waits for in-flight operations of this
// *Store, is terminal (every later call fails with ErrClosed) and is
// idempotent. It first retries a pending scrub (see DeleteUser), bounded.
func (s *Store) Close() error {
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.OpTimeout)
	pending, err := scrubPending(ctx, s.db)
	cancel()
	if err == nil && pending {
		s.scrub(context.Background())
	}
	return s.db.Close()
}

// Ping is the readiness probe: one bounded read of the schema version. It
// also retries a pending scrub (see DeleteUser), bounded; the scrub's
// outcome does not affect readiness.
func (s *Store) Ping(ctx context.Context) error {
	var pending bool
	err := s.read(ctx, func(ctx context.Context) error {
		var v int
		if err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v); err != nil {
			return err
		}
		pending, _ = scrubPending(ctx, s.db)
		return nil
	})
	if err != nil || !pending {
		return err
	}
	s.life.RLock()
	defer s.life.RUnlock()
	if !s.closed {
		s.wmu.Lock()
		defer s.wmu.Unlock()
		s.scrub(ctx)
	}
	return nil
}

// read runs fn under the shared lifecycle lock with a bounded context.
func (s *Store) read(ctx context.Context, fn func(ctx context.Context) error) error {
	s.life.RLock()
	defer s.life.RUnlock()
	if s.closed {
		return ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, s.opts.OpTimeout)
	defer cancel()
	return fn(ctx)
}

// write runs fn in one IMMEDIATE transaction under the shared lifecycle lock
// and this instance's writer mutex, with a bounded context. fn's error rolls
// the transaction back, except that a *commitErr commits and then returns
// its wrapped error (used to persist a denial audit row). After a commit it
// retries a pending scrub (see scrub.go); that never changes the result.
func (s *Store) write(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	s.life.RLock()
	defer s.life.RUnlock()
	if s.closed {
		return ErrClosed
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	opCtx, cancel := context.WithTimeout(ctx, s.opts.OpTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(opCtx, nil)
	if err != nil {
		return fmt.Errorf("identity: begin: %w", err)
	}
	defer tx.Rollback()
	ferr := fn(opCtx, tx)
	var ce *commitErr
	if ferr != nil && !errors.As(ferr, &ce) {
		return ferr
	}
	pending, _ := scrubPending(opCtx, tx)
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("identity: commit: %w", err)
	}
	if pending {
		s.scrub(ctx)
	}
	if ce != nil {
		return ce.err
	}
	return nil
}

// commitErr marks an error returned after the transaction is committed.
type commitErr struct{ err error }

func (e *commitErr) Error() string { return e.err.Error() }

// now returns the clock's time truncated to the stored millisecond precision.
func (s *Store) now() time.Time { return s.clock.Now().UTC().Truncate(time.Millisecond) }

func toMS(t time.Time) int64 { return t.UnixMilli() }

func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func fromNullMS(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return fromMS(ms.Int64)
}
