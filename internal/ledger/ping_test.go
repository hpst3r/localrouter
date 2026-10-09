package ledger

import (
	"context"
	"testing"
)

// Ping must be a bounded, read-only storage probe: it succeeds on a healthy
// open ledger, mutates nothing, and surfaces context errors.
func TestPingReadOnly(t *testing.T) {
	l, _ := openTest(t, nil)

	var before int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := l.Ping(context.Background()); err != nil {
		t.Fatalf("Ping healthy ledger: %v", err)
	}
	var after int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("Ping mutated rows: before=%d after=%d", before, after)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Ping(ctx); err == nil {
		t.Fatal("Ping with canceled context should error")
	}
}

// A closed database must fail Ping so readiness flips not-ready.
func TestPingAfterClose(t *testing.T) {
	l, _ := openTest(t, nil)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Ping(context.Background()); err == nil {
		t.Fatal("Ping on closed ledger should error")
	}
}
