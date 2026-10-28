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

var (
	backfillCutoff = secNow.Add(-2 * time.Hour)
	// backfillAfter is the window's lower bound in backfillOpts.
	backfillAfter = backfillCutoff.Add(-3 * time.Hour)
)

const backfillInsert = `INSERT INTO session_model_usage (session_id,model,billing_provider,billing_base_url,billing_mode,task,api_call_count,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,first_seen,last_seen) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// backfillFixture builds a Hermes home with self-routed and direct rows on
// both sides of backfillCutoff, in the default and a "work" profile. Every
// row's first_seen is inside the window.
func backfillFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	first := secUnix(backfillCutoff.Add(-90 * time.Minute))
	before, after := secUnix(backfillCutoff.Add(-time.Hour)), secUnix(backfillCutoff.Add(time.Hour))
	db := mkdb(t, filepath.Join(home, "state.db"))
	exec(t, db, `INSERT INTO sessions VALUES ('s1','cli',NULL,1,'/home/u/projects/foo','/home/u/projects/foo'),('s2','cli',NULL,2,NULL,NULL),('s3','cli',NULL,3,NULL,NULL)`)
	q := backfillInsert
	// Self-routed gpt-6.1-sol before the cutoff: selected.
	exec(t, db, q, "s1", "gpt-6.1-sol", "localrouter-codex", "http://127.0.0.1:8787/v1", "codex_responses", "", 5, 100, 50, 1000, 3, 7, first, before)
	// Self-routed gpt-6.1-sol still active after the cutoff: excluded.
	exec(t, db, q, "s2", "gpt-6.1-sol", "custom", "http://localhost:8787/v1", "", "", 2, 10, 5, 20, 0, 0, first, after)
	// Self-routed Ollama model the proxy already metered: ignored.
	exec(t, db, q, "s1", "deepseek-v4.1-flash", "localrouter-bg", "http://127.0.0.1:8787/v1", "", "", 9, 900, 90, 0, 0, 0, first, before)
	// Direct (not through the router): the regular scan's business.
	exec(t, db, q, "s3", "gpt-6.1-sol", "openai-codex", "https://chatgpt.com/backend-api/codex", "", "", 1, 11, 6, 0, 0, 0, first, before)
	pdb := mkdb(t, filepath.Join(home, "profiles", "work", "state.db"))
	exec(t, pdb, `INSERT INTO sessions VALUES ('s9','cli','s1',1,NULL,NULL)`)
	exec(t, pdb, q, "s9", "gpt-6.1-sol", "localrouter-codex", "http://127.0.0.1:8787/v1", "", "approval", 3, 40, 4, 400, 0, 2, first, before)
	return home
}

func backfillOpts(apply bool) BackfillOptions {
	return BackfillOptions{
		Models:    []string{"gpt-6.1-sol"},
		After:     backfillAfter,
		Before:    backfillCutoff,
		AccountID: "codex-main",
		Provider:  "codex",
		Apply:     apply,
	}
}

// ledgerExists fakes the ledger wiring of BackfillOptions.Exists: the ID,
// or a first-backfill row (no key prefix) with the same labels.
func ledgerExists(l *memLedger) func(context.Context, core.RequestRecord) (bool, error) {
	return func(_ context.Context, rec core.RequestRecord) (bool, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if _, ok := l.rows[rec.ID]; ok {
			return true, nil
		}
		for id, r := range l.rows {
			if strings.HasPrefix(id, BackfillIDPrefix) && !strings.HasPrefix(id, BackfillKeyIDPrefix) &&
				r.Client == rec.Client && r.Session == rec.Session && r.Model == rec.Model && r.Agent == rec.Agent && r.Task == rec.Task {
				return true, nil
			}
		}
		return false, nil
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

// CX-1: a session backfilled once that then resumes (its later calls
// metered by the fixed proxy) is not recorded again by a rerun with a later
// --before: the ID depends on the key only, so the grown row is "present".
func TestCXFixCX1BackfillRerunLaterCutoffRecordsKeyOnce(t *testing.T) {
	home := backfillFixture(t)
	l := &memLedger{}
	c := newC(t, home, l)
	ctx := context.Background()

	o := backfillOpts(true)
	o.Exists = ledgerExists(l)
	if _, err := c.BackfillSelf(ctx, o); err != nil {
		t.Fatal(err)
	}
	first := sessionInput(l, "s1", BackfillIDPrefix)
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
	if got := sessionInput(l, "s1", BackfillIDPrefix); got != first || res.Recorded != 0 {
		t.Fatalf("s1 backfilled input=%d (want %d) recorded=%d: grown key re-recorded", got, first, res.Recorded)
	}
	var s1 *BackfillRow
	for i := range res.Selected {
		if res.Selected[i].Session == "s1" {
			s1 = &res.Selected[i]
		}
	}
	if s1 == nil || !s1.Present || res.AlreadyPresent != 2 {
		t.Fatalf("rerun: s1=%+v present=%d", s1, res.AlreadyPresent)
	}
}

// CX-1: rows written by the first backfill (IDs hashing the counters) count
// as present, so switching to key-only IDs does not re-record them.
func TestCXFixCX1FirstBackfillRowsArePresent(t *testing.T) {
	home := backfillFixture(t)
	l := &memLedger{}
	c := newC(t, home, l)
	res, err := c.BackfillSelf(context.Background(), backfillOpts(false))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Selected {
		r.Record.ID = BackfillIDPrefix + "legacy-" + r.Session
		if err := l.Record(context.Background(), r.Record); err != nil {
			t.Fatal(err)
		}
	}
	o := backfillOpts(true)
	o.Exists = ledgerExists(l)
	if res, err = c.BackfillSelf(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if res.Recorded != 0 || res.AlreadyPresent != 2 {
		t.Fatalf("recorded=%d present=%d", res.Recorded, res.AlreadyPresent)
	}
}

// CX-2 (rewritten for Amendment R1.2): only rows whose whole activity lies
// in [After, Before) are selected. A session that began before After (e.g.
// while the proxy still metered Codex usage) is excluded. A row without
// first_seen whose last_seen is in the window goes to Review and is recorded
// only with AcceptPartialSessions. (R1.3's ledger cross-check is not
// implemented: proxied rows' session label is not Hermes' session id.)
func TestCXFixCX2BackfillWindowExcludesEarlierAndPartialSessions(t *testing.T) {
	home := backfillFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	last := secUnix(backfillCutoff.Add(-time.Hour))
	// s4 began before After: the proxy metered its early calls.
	exec(t, db, backfillInsert, "s4", "gpt-6.1-sol", "localrouter-codex", "http://127.0.0.1:8787/v1", "", "", 4, 40, 4, 0, 0, 0,
		secUnix(backfillAfter.Add(-time.Minute)), last)
	// s5 has no first_seen: Review.
	exec(t, db, backfillInsert, "s5", "gpt-6.1-sol", "localrouter-codex", "http://127.0.0.1:8787/v1", "", "", 2, 20, 2, 0, 0, 0, nil, last)
	// s6 has no first_seen and was last seen before After: excluded.
	exec(t, db, backfillInsert, "s6", "gpt-6.1-sol", "localrouter-codex", "http://127.0.0.1:8787/v1", "", "", 1, 10, 1, 0, 0, 0,
		nil, secUnix(backfillAfter.Add(-time.Minute)))

	l := &memLedger{}
	c := newC(t, home, l)
	ctx := context.Background()
	o := backfillOpts(true)
	o.Exists = ledgerExists(l)
	res, err := c.BackfillSelf(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, r := range res.Excluded {
		reasons[r.Session] = r.Reason
	}
	if reasons["s4"] != ExcludedBeforeAfter || reasons["s6"] != ExcludedBeforeAfter || reasons["s2"] != ExcludedAfterBefore {
		t.Fatalf("excluded %v", reasons)
	}
	if len(res.Review) != 1 || res.Review[0].Session != "s5" || !res.Review[0].FirstSeen.IsZero() {
		t.Fatalf("review %+v", res.Review)
	}
	if res.Recorded != 2 || sessionInput(l, "s4", "") != 0 || sessionInput(l, "s5", "") != 0 {
		t.Fatalf("apply without accept: recorded=%d s4=%d s5=%d", res.Recorded, sessionInput(l, "s4", ""), sessionInput(l, "s5", ""))
	}

	o.AcceptPartialSessions = true
	if res, err = c.BackfillSelf(ctx, o); err != nil {
		t.Fatal(err)
	}
	if res.Recorded != 1 || res.AlreadyPresent != 2 || sessionInput(l, "s5", BackfillKeyIDPrefix) != 20 || sessionInput(l, "s4", "") != 0 {
		t.Fatalf("accept partial: recorded=%d present=%d s5=%d", res.Recorded, res.AlreadyPresent, sessionInput(l, "s5", ""))
	}
}

// An older Hermes schema without first_seen sends every in-window row to
// Review.
func TestBackfillNoFirstSeenColumnReviews(t *testing.T) {
	home := t.TempDir()
	db := mkdb(t, filepath.Join(home, "state.db"))
	exec(t, db, `ALTER TABLE session_model_usage DROP COLUMN first_seen`)
	exec(t, db, `INSERT INTO session_model_usage (session_id,model,billing_provider,billing_base_url,api_call_count,input_tokens,last_seen) VALUES ('s1','gpt-6.1-sol','custom','http://127.0.0.1:8787/v1',1,10,?)`,
		secUnix(backfillCutoff.Add(-time.Hour)))
	res, err := newC(t, home, &memLedger{}).BackfillSelf(context.Background(), backfillOpts(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Review) != 1 || len(res.Selected) != 0 {
		t.Fatalf("review=%d selected=%d", len(res.Review), len(res.Selected))
	}
}

func TestBackfillSelfDryRunApplyIdempotentAndStateUntouched(t *testing.T) {
	home := backfillFixture(t)
	l := &memLedger{}
	c := newC(t, home, l)
	ctx := context.Background()

	// Dry run: two selectable rows, one excluded, nothing recorded.
	res, err := c.BackfillSelf(ctx, backfillOpts(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selected) != 2 || len(res.Excluded) != 1 || res.Recorded != 0 {
		t.Fatalf("dry run: selected=%d excluded=%d recorded=%d", len(res.Selected), len(res.Excluded), res.Recorded)
	}
	if ex := res.Excluded[0]; ex.Session != "s2" || ex.Profile != "default" || ex.Model != "gpt-6.1-sol" || !ex.LastSeen.Equal(backfillCutoff.Add(time.Hour)) {
		t.Fatalf("excluded %+v", ex)
	}
	if n, _, _, _, _ := l.sum(); n != 0 {
		t.Fatalf("dry run recorded %d rows", n)
	}

	// Apply: exactly the two selected rows, full cumulative counters.
	res, err = c.BackfillSelf(ctx, backfillOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	if res.Recorded != 2 {
		t.Fatalf("apply recorded %d", res.Recorded)
	}
	if n, in, cr, cw, out := l.sum(); n != 2 || in != 100+1000+3+40+400 || cr != 1000+400 || cw != 3 || out != 50+4 {
		t.Fatalf("sum n=%d in=%d cr=%d cw=%d out=%d", n, in, cr, cw, out)
	}
	for _, r := range l.rows {
		if !strings.HasPrefix(r.ID, "hermes-backfill:") || r.AccountID != "codex-main" || r.Provider != "codex" || !r.UsageKnown {
			t.Errorf("record %+v", r)
		}
		if !r.StartedAt.Equal(backfillCutoff.Add(-time.Hour)) || !r.FinishedAt.Equal(r.StartedAt) {
			t.Errorf("timestamp %v", r.StartedAt)
		}
		switch r.Session {
		case "s1":
			if r.Client != "hermes" || r.Agent != "main" || r.Task != "foo" || r.Usage.ReasoningTokens != 7 || r.Model != "gpt-6.1-sol" {
				t.Errorf("s1 labels %+v", r)
			}
		case "s9":
			if r.Client != "hermes/work" || r.Agent != "aux:approval" || r.Usage.ReasoningTokens != 2 {
				t.Errorf("s9 labels %+v", r)
			}
		default:
			t.Errorf("unexpected session %q", r.Session)
		}
	}

	// Second apply: nothing new, and Exists reports the rows as present.
	o := backfillOpts(true)
	o.Exists = ledgerExists(l)
	res, err = c.BackfillSelf(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Recorded != 0 || res.AlreadyPresent != 2 {
		t.Fatalf("second apply recorded=%d present=%d", res.Recorded, res.AlreadyPresent)
	}
	if n, _, _, _, _ := l.sum(); n != 2 {
		t.Fatalf("second apply left %d rows", n)
	}

	// The collector's state file is untouched, and a regular scan still skips
	// every self-routed row and imports only the direct one.
	if _, err := os.Stat(c.opts.StatePath); !os.IsNotExist(err) {
		t.Fatalf("backfill wrote state: %v", err)
	}
	st, err := c.ScanOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Recorded != 1 || st.SkippedSelf != 4 {
		t.Fatalf("scan stats %+v", st)
	}
	var regular int
	for id, r := range l.rows {
		if strings.HasPrefix(id, "hermes:") {
			regular++
			if r.Session != "s3" || r.AccountID != "codex-primary" {
				t.Errorf("regular row %+v", r)
			}
		}
	}
	if regular != 1 {
		t.Fatalf("regular rows %d", regular)
	}
}

// Backfill IDs never collide with the regular collector's IDs for the same
// key and counters.
func TestBackfillIDsDistinctFromRegular(t *testing.T) {
	home := t.TempDir()
	db := mkdb(t, filepath.Join(home, "state.db"))
	exec(t, db, `INSERT INTO session_model_usage (session_id,model,billing_provider,billing_base_url,api_call_count,input_tokens,first_seen,last_seen) VALUES ('s1','gpt-6.1-sol','custom','https://example.com/v1',1,10,?,?)`,
		secUnix(backfillCutoff.Add(-time.Hour)), secUnix(backfillCutoff.Add(-time.Hour)))
	l := &memLedger{}
	c := newC(t, home, l)
	// Make the row "self" for the backfill only.
	c.opts.SelfHosts = []string{"example.com"}
	if _, err := c.BackfillSelf(context.Background(), backfillOpts(true)); err != nil {
		t.Fatal(err)
	}
	c.opts.SelfHosts = nil
	if _, err := c.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, in, _, _, _ := l.sum(); n != 2 || in != 20 {
		t.Fatalf("rows=%d in=%d (backfill and regular must not dedupe)", n, in)
	}
}

func TestSelfHosts(t *testing.T) {
	got := strings.Join(SelfHosts("127.0.0.1:8787"), ",")
	if got != "127.0.0.1:8787,127.0.0.1:8787,localhost:8787,[::1]:8787" {
		t.Errorf("loopback: %s", got)
	}
	if got := strings.Join(SelfHosts("10.0.0.5:9000"), ","); got != "10.0.0.5:9000" {
		t.Errorf("lan: %s", got)
	}
	if SelfHosts("bogus") != nil {
		t.Error("bad listen")
	}
}

func TestBackfillOptionErrors(t *testing.T) {
	c := newC(t, t.TempDir(), &memLedger{})
	for name, o := range map[string]BackfillOptions{
		"no models":       {After: backfillAfter, Before: backfillCutoff, AccountID: "a"},
		"no before":       {Models: []string{"m"}, After: backfillAfter, AccountID: "a"},
		"no after":        {Models: []string{"m"}, Before: backfillCutoff, AccountID: "a"},
		"after >= before": {Models: []string{"m"}, After: backfillCutoff, Before: backfillCutoff, AccountID: "a"},
		"no account":      {Models: []string{"m"}, After: backfillAfter, Before: backfillCutoff},
	} {
		if _, err := c.BackfillSelf(context.Background(), o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
