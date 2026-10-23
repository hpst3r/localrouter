package hermeslog

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func reviewExists(l *memLedger) func(context.Context, string) (bool, error) {
	return func(_ context.Context, id string) (bool, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		_, ok := l.rows[id]
		return ok, nil
	}
}

// sessionInput sums InputTokens of rows for session s whose ID has prefix.
func sessionInput(l *memLedger, s, prefix string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int64
	for _, r := range l.rows {
		if r.Session == s && strings.HasPrefix(r.ID, prefix) {
			n += r.Usage.InputTokens
		}
	}
	return n
}

// A session backfilled once, that then resumes (its later calls metered by
// the fixed proxy), is recorded AGAIN for its full cumulative counters by a
// rerun with a later --before: the ID hashes the cumulative counters, so the
// grown row gets a fresh ID and Exists does not catch it.
func TestReviewCX1BackfillRerunLaterCutoffDoubleCounts(t *testing.T) {
	if os.Getenv("LR_REVIEW") == "" {
		t.Skip("BUG CX-1: backfill-self rerun with a later --before re-records a grown session's full cumulative counters")
	}
	home := backfillFixture(t)
	l := &memLedger{}
	c := newC(t, home, l)
	ctx := context.Background()

	o := backfillOpts(true)
	o.Exists = reviewExists(l)
	if _, err := c.BackfillSelf(ctx, o); err != nil {
		t.Fatal(err)
	}
	first := sessionInput(l, "s1", backfillIDPrefix)
	if first != 100+1000+3 {
		t.Fatalf("first backfill s1 input=%d", first)
	}

	// s1 resumes after the cutoff: one more call (+10 input), metered by the
	// now-fixed proxy.
	db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, `UPDATE session_model_usage SET api_call_count=6, input_tokens=110, last_seen=? WHERE session_id='s1' AND model='gpt-6.1-sol'`,
		secUnix(backfillCutoff.Add(30*time.Minute)))

	// The operator reruns with a later cutoff (e.g. to pick up stragglers).
	o.Before = backfillCutoff.Add(45 * time.Minute)
	res, err := c.BackfillSelf(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	got := sessionInput(l, "s1", backfillIDPrefix)
	// At most the Hermes cumulative total may ever be backfilled for s1
	// (strictly, only the pre-cutoff 1103, since the proxy metered the rest).
	if got > 110+1000+3 {
		t.Fatalf("s1 backfilled input=%d across reruns (recorded=%d this run); Hermes cumulative is only %d: double counted",
			got, res.Recorded, 110+1000+3)
	}
}

// Self-routed usage that the proxy DID meter (e.g. while the original hotfix
// was live, or a call failed over/metered with usage known) is recorded again:
// BackfillSelf has no lower bound and never consults proxy rows for the same
// session/model, only its own deterministic IDs.
func TestReviewCX2BackfillIgnoresProxyMeteredUsage(t *testing.T) {
	if os.Getenv("LR_REVIEW") == "" {
		t.Skip("BUG CX-2: backfill-self records full counters even when the proxy already metered the session")
	}
	home := backfillFixture(t)
	l := &memLedger{}
	// The proxy metered s1's gpt-6.1-sol calls with usage known.
	at := backfillCutoff.Add(-90 * time.Minute)
	if err := l.Record(context.Background(), core.RequestRecord{
		ID: "proxy-1", StartedAt: at, FinishedAt: at, Client: "hermes", Model: "gpt-6.1-sol",
		Provider: "codex", AccountID: "codex-main", Status: 200, UsageKnown: true, Session: "s1",
		Usage: core.Usage{InputTokens: 1103, CachedInputTokens: 1000, CacheCreationInputTokens: 3, OutputTokens: 50},
	}); err != nil {
		t.Fatal(err)
	}
	c := newC(t, home, l)
	o := backfillOpts(true)
	o.Exists = reviewExists(l)
	if _, err := c.BackfillSelf(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if got := sessionInput(l, "s1", ""); got > 1103 {
		t.Fatalf("s1 total input in ledger=%d, Hermes cumulative=1103: proxy-metered usage double counted", got)
	}
}
