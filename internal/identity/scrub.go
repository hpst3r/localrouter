package identity

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

// Deferred physical scrub.
//
// DeleteUser removes profile data logically in its transaction, but older
// page images holding it stay in the WAL (and the main file) until a
// TRUNCATE checkpoint completes. That checkpoint cannot complete while any
// connection, in this or another process, holds a read snapshot taken before
// the delete. So the delete transaction also records a pending-scrub mark in
// meta. The mark is durable and visible to every *Store on the file. After
// every committed write, on Ping and on Close, a *Store that sees the mark
// makes one bounded TRUNCATE attempt. It clears the mark only when that
// checkpoint actually completed. There is no background goroutine: the scrub
// happens on the next housekeeping point after the last old reader is gone.

const (
	metaScrubPending = "scrub_pending"

	// scrubBusyTimeout bounds how long one scrub attempt waits on other
	// connections' locks, so a long reader elsewhere defers the scrub
	// instead of stalling the operation that triggered it.
	scrubBusyTimeout = 250 * time.Millisecond
)

// markScrubPending records inside tx that a scrub is owed. Each mark bumps a
// generation, so a scrub that raced a later delete does not clear that
// delete's mark.
func markScrubPending(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, '1')
		 ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + 1`, metaScrubPending); err != nil {
		return fmt.Errorf("identity: mark scrub: %w", err)
	}
	return nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scrubPending(ctx context.Context, q queryRower) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key = ?`, metaScrubPending).Scan(&n)
	return n > 0, err
}

// scrub makes one bounded attempt to complete a pending scrub and reports
// whether nothing is pending any more. The caller holds s.life exclusively,
// or shared together with s.wmu.
func (s *Store) scrub(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, s.opts.OpTimeout)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false
	}
	defer conn.Close()

	var gen string
	err = conn.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaScrubPending).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	if err != nil {
		return false
	}

	// Both the checkpoint and the clearing write invoke the busy handler,
	// which does not observe ctx; shorten it on this connection for the
	// attempt. If it cannot be restored, the connection is discarded.
	short := min(scrubBusyTimeout, s.opts.OpTimeout)
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, short.Milliseconds())); err != nil {
		discardConn(conn)
		return false
	}
	defer func() {
		if _, err := conn.ExecContext(context.Background(),
			fmt.Sprintf(`PRAGMA busy_timeout = %d`, s.opts.OpTimeout.Milliseconds())); err != nil {
			discardConn(conn)
		}
	}()

	// TRUNCATE reports busy=1 (not an error) when a reader or writer kept
	// it from checkpointing every frame and resetting the WAL.
	var busy, logFrames, done int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil ||
		busy != 0 || logFrames != done {
		return false
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM meta WHERE key = ? AND value = ?`, metaScrubPending, gen); err != nil {
		return false
	}
	if err := tx.Commit(); err != nil {
		return false
	}
	pending, err := scrubPending(ctx, conn)
	return err == nil && !pending
}

// discardConn makes database/sql drop conn instead of returning it to the
// pool.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

// ScrubPending reports whether a deleted user's superseded profile data may
// still be on disk because the post-delete checkpoint has not completed yet
// (see DeleteUser). The deletion itself is already effective either way.
func (s *Store) ScrubPending(ctx context.Context) (bool, error) {
	var pending bool
	err := s.read(ctx, func(ctx context.Context) error {
		var err error
		pending, err = scrubPending(ctx, s.db)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("identity: scrub pending: %w", err)
	}
	return pending, nil
}
