package hermeslog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var backfillCutoff = secNow.Add(-2 * time.Hour)

// backfillFixture builds a Hermes home with self-routed and direct rows on
// both sides of backfillCutoff, in the default and a "work" profile.
func backfillFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	before, after := secUnix(backfillCutoff.Add(-time.Hour)), secUnix(backfillCutoff.Add(time.Hour))
	db := mkdb(t, filepath.Join(home, "state.db"))
	exec(t, db, `INSERT INTO sessions VALUES ('s1','cli',NULL,1,'/home/u/projects/foo','/home/u/projects/foo'),('s2','cli',NULL,2,NULL,NULL),('s3','cli',NULL,3,NULL,NULL)`)
	q := `INSERT INTO session_model_usage (session_id,model,billing_provider,billing_base_url,billing_mode,task,api_call_count,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,last_seen) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`
	// Self-routed gpt-6.1-sol before the cutoff: selected.
	exec(t, db, q, "s1", "gpt-6.1-sol", "localrouter-codex", "http://127.0.0.1:8787/v1", "codex_responses", "", 5, 100, 50, 1000, 3, 7, before)
	// Self-routed gpt-6.1-sol still active after the cutoff: excluded.
	exec(t, db, q, "s2", "gpt-6.1-sol", "custom", "http://localhost:8787/v1", "", "", 2, 10, 5, 20, 0, 0, after)
	// Self-routed Ollama model the proxy already metered: ignored.
	exec(t, db, q, "s1", "deepseek-v4.1-flash", "localrouter-bg", "http://127.0.0.1:8787/v1", "", "", 9, 900, 90, 0, 0, 0, before)
	// Direct (not through the router): the regular scan's business.
	exec(t, db, q, "s3", "gpt-6.1-sol", "openai-codex", "https://chatgpt.com/backend-api/codex", "", "", 1, 11, 6, 0, 0, 0, before)
	pdb := mkdb(t, filepath.Join(home, "profiles", "work", "state.db"))
	exec(t, pdb, `INSERT INTO sessions VALUES ('s9','cli','s1',1,NULL,NULL)`)
	exec(t, pdb, q, "s9", "gpt-6.1-sol", "localrouter-codex", "http://127.0.0.1:8787/v1", "", "approval", 3, 40, 4, 400, 0, 2, before)
	return home
}

func backfillOpts(apply bool) BackfillOptions {
	return BackfillOptions{
		Models:    []string{"gpt-6.1-sol"},
		Before:    backfillCutoff,
		AccountID: "codex-main",
		Provider:  "codex",
		Apply:     apply,
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
	exists := func(_ context.Context, id string) (bool, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		_, ok := l.rows[id]
		return ok, nil
	}
	o := backfillOpts(true)
	o.Exists = exists
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
	exec(t, db, `INSERT INTO session_model_usage (session_id,model,billing_provider,billing_base_url,api_call_count,input_tokens,last_seen) VALUES ('s1','gpt-6.1-sol','custom','https://example.com/v1',1,10,?)`, secUnix(backfillCutoff.Add(-time.Hour)))
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
		"no models":  {Before: backfillCutoff, AccountID: "a"},
		"no before":  {Models: []string{"m"}, AccountID: "a"},
		"no account": {Models: []string{"m"}, Before: backfillCutoff},
	} {
		if _, err := c.BackfillSelf(context.Background(), o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
