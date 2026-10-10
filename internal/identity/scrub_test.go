package identity

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	scrubSubject = "sub-pii"
	scrubMarker  = "SCRUBPIIMARKER@example.test"
)

// holdSnapshot opens a read transaction on another *Store over the same file
// (the CLI next to the app) and returns its release func.
func (f *fixture) holdSnapshot() func() {
	f.t.Helper()
	ctx := context.Background()
	reader := f.second()
	conn, err := reader.db.Conn(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil { // deferred read snapshot
		f.t.Fatal(err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			f.t.Fatal(err)
		}
		conn.Close()
	}
	f.t.Cleanup(release)
	return release
}

// markedUser provisions scrubSubject with the on-disk marker as its email.
func (f *fixture) markedUser() User {
	f.t.Helper()
	l := f.login(scrubSubject, core.RoleUser)
	l.Email = scrubMarker
	u, err := f.s.ResolveLogin(context.Background(), l)
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

// deleteWhileReaderHolds deletes u (a markedUser) through a CLI store while
// another connection holds a read snapshot, checks the deletion is logically
// immediate and bounded in time, and returns the CLI store and the release
// func of the snapshot.
func (f *fixture) deleteWhileReaderHolds(u User) (*Store, func()) {
	f.t.Helper()
	ctx := context.Background()
	release := f.holdSnapshot()
	cli := f.second()
	start := time.Now()
	if err := cli.DeleteUser(ctx, Actor{Kind: ActorCLI}, u.ID); err != nil {
		f.t.Fatalf("DeleteUser = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		f.t.Fatalf("DeleteUser blocked %s behind a reader, want a short bounded wait", d)
	}
	got, err := f.s.User(ctx, u.ID)
	if err != nil || got.Status != StatusDeleted || got.Email != "" {
		f.t.Fatalf("deleted user = %+v, %v; want deleted and scrubbed immediately", got, err)
	}
	if _, err := f.s.ResolveLogin(ctx, f.login(scrubSubject, core.RoleUser)); !errors.Is(err, ErrUserDeleted) {
		f.t.Fatalf("re-login while scrub pending = %v, want ErrUserDeleted", err)
	}
	return cli, release
}

func (f *fixture) assertMarkerGone(when string) {
	f.t.Helper()
	if bytes.Contains(fileBytes(f.t, f.path), []byte(scrubMarker)) {
		f.t.Fatalf("deleted user's email still in identity.db/-wal after %s", when)
	}
}

// The original foundation-audit reproduction: a reader holding a snapshot
// defeats the post-delete checkpoint; the next write after it is released
// must finish the scrub.
func TestDeleteScrubCompletesOnNextWriteAfterReader(t *testing.T) {
	f := newFixture(t)
	u := f.markedUser()
	_, release := f.deleteWhileReaderHolds(u)
	release()
	f.user("sub-other", core.RoleUser) // ordinary traffic on the app's store
	f.assertMarkerGone("the reader released and a later write")
}

// The mark is durable and shared: the app's store finishes a scrub left
// pending by the CLI's store on its next readiness Ping, without any write.
func TestDeleteScrubCompletesOnPingAfterReader(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.markedUser()
	_, release := f.deleteWhileReaderHolds(u)

	// While the reader still holds its snapshot Ping stays healthy and bounded.
	start := time.Now()
	if err := f.s.Ping(ctx); err != nil {
		t.Fatalf("Ping while scrub blocked = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Ping blocked %s behind a reader", d)
	}
	release()
	if err := f.s.Ping(ctx); err != nil {
		t.Fatalf("Ping = %v", err)
	}
	f.assertMarkerGone("the reader released and a Ping")
}

// Close of the deleting store finishes the scrub once the reader is gone.
func TestDeleteScrubCompletesOnClose(t *testing.T) {
	f := newFixture(t)
	u := f.markedUser()
	cli, release := f.deleteWhileReaderHolds(u)
	release()
	if err := cli.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	f.assertMarkerGone("the reader released and the CLI store closed")
}

// ScrubPending exposes the pending cleanup separately from the (already
// effective) deletion. Two deletes behind one reader are both scrubbed.
func TestScrubPendingReportsDeferredCleanup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if p, err := f.s.ScrubPending(ctx); err != nil || p {
		t.Fatalf("ScrubPending before any delete = %v, %v; want false", p, err)
	}
	u := f.markedUser()
	cli, release := f.deleteWhileReaderHolds(u)
	other := f.user("sub-pii-2", core.RoleUser)
	if err := cli.DeleteUser(ctx, Actor{Kind: ActorCLI}, other.ID); err != nil {
		t.Fatalf("second DeleteUser = %v", err)
	}
	for name, s := range map[string]*Store{"app": f.s, "cli": cli} {
		if p, err := s.ScrubPending(ctx); err != nil || !p {
			t.Fatalf("%s ScrubPending behind a reader = %v, %v; want true", name, p, err)
		}
	}
	release()
	if err := f.s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*Store{"app": f.s, "cli": cli} {
		if p, err := s.ScrubPending(ctx); err != nil || p {
			t.Fatalf("%s ScrubPending after the scrub = %v, %v; want false", name, p, err)
		}
	}
	f.assertMarkerGone("both deletes and a Ping")
	if bytes.Contains(fileBytes(t, f.path), []byte("sub-pii-2@example.test")) {
		t.Fatal("second deleted user's email still on disk")
	}
	cli.Close()
	if _, err := cli.ScrubPending(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("ScrubPending after Close = %v, want ErrClosed", err)
	}
}

// A blocked scrub attempt shortens busy_timeout only for itself: pooled
// connections keep the OpTimeout bound afterwards.
func TestScrubRestoresBusyTimeout(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.markedUser()
	cli, _ := f.deleteWhileReaderHolds(u)
	want := cli.opts.OpTimeout.Milliseconds()
	conns := make([]*sql.Conn, 0, 2)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 2; i++ { // MaxOpenConns: hold both so each is checked
		conn, err := cli.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		var got int64
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("pooled conn %d busy_timeout = %d after a scrub attempt, want %d", i, got, want)
		}
	}
}
