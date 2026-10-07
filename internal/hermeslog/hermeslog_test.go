package hermeslog

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	_ "modernc.org/sqlite"
)

type memLedger struct {
	mu   sync.Mutex
	rows map[string]core.RequestRecord
	fail bool
}

func (m *memLedger) Record(_ context.Context, r core.RequestRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return os.ErrDeadlineExceeded
	}
	if m.rows == nil {
		m.rows = map[string]core.RequestRecord{}
	}
	if _, ok := m.rows[r.ID]; !ok { // idempotent like the real ledger
		m.rows[r.ID] = r
	}
	return nil
}
func (m *memLedger) Summary(context.Context, time.Time, string) ([]core.UsageRow, error) {
	return nil, nil
}
func (m *memLedger) Close() error { return nil }
func (m *memLedger) sum() (n int, in, cr, cw, out int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		n++
		in += r.Usage.InputTokens
		cr += r.Usage.CachedInputTokens
		cw += r.Usage.CacheCreationInputTokens
		out += r.Usage.OutputTokens
	}
	return
}

const schema = `
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, parent_session_id TEXT,
  started_at REAL NOT NULL, cwd TEXT, git_repo_root TEXT);
CREATE TABLE session_model_usage (
  session_id TEXT NOT NULL, model TEXT NOT NULL,
  billing_provider TEXT NOT NULL DEFAULT '', billing_base_url TEXT NOT NULL DEFAULT '',
  billing_mode TEXT NOT NULL DEFAULT '', task TEXT NOT NULL DEFAULT '',
  api_call_count INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0, cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
  estimated_cost_usd REAL NOT NULL DEFAULT 0, actual_cost_usd REAL NOT NULL DEFAULT 0,
  cost_status TEXT, cost_source TEXT, first_seen REAL, last_seen REAL,
  PRIMARY KEY (session_id, model, billing_provider, billing_base_url, billing_mode, task));`

func mkdb(t *testing.T, path string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func exec(t *testing.T, db *sql.DB, q string, a ...any) {
	t.Helper()
	if _, err := db.Exec(q, a...); err != nil {
		t.Fatal(err)
	}
}

func newC(t *testing.T, home string, l core.Ledger) *Collector {
	return New(l, Options{
		Home:      home,
		Accounts:  map[string]string{"anthropic": "claude-max", "openai-codex": "codex-primary"},
		Host:      "pf3llssv",
		SelfHosts: []string{"127.0.0.1:8787", "localhost:8787"},
		StatePath: filepath.Join(t.TempDir(), "state.json"),
	})
}

func TestImportsDeltasSkipsSelfAndMapsAccounts(t *testing.T) {
	home := t.TempDir()
	db := mkdb(t, filepath.Join(home, "state.db"))
	exec(t, db, `INSERT INTO sessions VALUES ('s1','cli',NULL,1,'/home/u/projects/foo','/home/u/projects/foo'),('s2','cli','s1',2,NULL,NULL)`)
	exec(t, db, `INSERT INTO session_model_usage (session_id,model,billing_provider,billing_base_url,task,api_call_count,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,last_seen)
	  VALUES ('s1','claude-fable-5-1','anthropic','https://api.anthropic.com','',8,18,3666,406569,68659,1791406023.4),
	         ('s1','claude-fable-5-1','','','title_generation',1,662,41,0,0,1791405976.7),
	         ('s2','glm-5.3-flash','localrouter-bg','http://127.0.0.1:8787/v1','',3,100,10,0,0,1791405990)`)
	l := &memLedger{}
	c := newC(t, home, l)
	st, err := c.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Recorded != 2 || st.SkippedSelf != 1 {
		t.Fatalf("stats %+v", st)
	}
	n, in, cr, cw, out := l.sum()
	if n != 2 || in != 18+406569+68659+662 || cr != 406569 || cw != 68659 || out != 3666+41 {
		t.Fatalf("sum n=%d in=%d cr=%d cw=%d out=%d", n, in, cr, cw, out)
	}
	for _, r := range l.rows {
		if r.AccountID != "claude-max" {
			t.Errorf("account %q for %+v (aux claude-* rows map via model prefix)", r.AccountID, r.Agent)
		}
		if r.Host != "pf3llssv" || r.Client != "hermes" || r.Route != "hermes" {
			t.Errorf("attribution %+v", r)
		}
		if r.Agent == "main" && r.Task != "foo" {
			t.Errorf("task %q", r.Task)
		}
		if r.Agent != "main" && r.Agent != "aux:title_generation" {
			t.Errorf("agent %q", r.Agent)
		}
	}

	// No change -> nothing new.
	if st, _ := c.ScanOnce(context.Background()); st.Recorded != 0 {
		t.Fatalf("rescan recorded %d", st.Recorded)
	}
	// Session continues: only the increase is recorded.
	exec(t, db, `UPDATE session_model_usage SET api_call_count=10, output_tokens=4000, cache_read_tokens=500000 WHERE session_id='s1' AND task=''`)
	if st, _ := c.ScanOnce(context.Background()); st.Recorded != 1 {
		t.Fatalf("delta recorded %d", st.Recorded)
	}
	_, _, cr2, _, out2 := l.sum()
	if cr2 != 500000 || out2 != 4000+41 {
		t.Fatalf("after delta cr=%d out=%d", cr2, out2)
	}
}

func TestRestartDoesNotDoubleCountAndCrashIsIdempotent(t *testing.T) {
	home := t.TempDir()
	db := mkdb(t, filepath.Join(home, "state.db"))
	exec(t, db, `INSERT INTO sessions VALUES ('s1','cli',NULL,1,NULL,NULL)`)
	exec(t, db, `INSERT INTO session_model_usage (session_id,model,billing_provider,api_call_count,input_tokens,output_tokens,last_seen) VALUES ('s1','claude-opus-5-5','anthropic',1,10,5,1)`)
	l := &memLedger{}
	statePath := filepath.Join(t.TempDir(), "state.json")
	opts := Options{Home: home, StatePath: statePath, Accounts: map[string]string{"anthropic": "claude-max"}}
	if _, err := New(l, opts).ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Restart with saved state: nothing new.
	if st, _ := New(l, opts).ScanOnce(context.Background()); st.Recorded != 0 {
		t.Fatalf("restart recorded %d", st.Recorded)
	}
	// Crash before state save (state lost): re-recording is a no-op by ID.
	os.Remove(statePath)
	if _, err := New(l, opts).ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _, _, _, out := l.sum(); n != 1 || out != 5 {
		t.Fatalf("after lost state n=%d out=%d", n, out)
	}
}

func TestLedgerFailureRetriesAndProfilesAndReset(t *testing.T) {
	home := t.TempDir()
	db := mkdb(t, filepath.Join(home, "profiles", "work", "state.db"))
	exec(t, db, `INSERT INTO sessions VALUES ('s1','cli',NULL,1,NULL,NULL)`)
	exec(t, db, `INSERT INTO session_model_usage (session_id,model,billing_provider,api_call_count,input_tokens,output_tokens) VALUES ('s1','gpt-6.1-sol','openai-codex',2,100,20)`)
	l := &memLedger{fail: true}
	c := newC(t, home, l)
	if _, err := c.ScanOnce(context.Background()); err == nil {
		t.Fatal("expected ledger error")
	}
	l.fail = false
	if st, err := c.ScanOnce(context.Background()); err != nil || st.Recorded != 1 {
		t.Fatalf("retry: %+v %v", st, err)
	}
	for _, r := range l.rows {
		if r.Client != "hermes/work" || r.AccountID != "codex-primary" {
			t.Fatalf("profile attribution %+v", r)
		}
	}
	// Counters reset (session rewritten): rebase, don't record negatives.
	exec(t, db, `UPDATE session_model_usage SET api_call_count=1, input_tokens=5, output_tokens=1`)
	if st, _ := c.ScanOnce(context.Background()); st.Recorded != 0 {
		t.Fatalf("reset recorded %d", st.Recorded)
	}
	exec(t, db, `UPDATE session_model_usage SET api_call_count=2, input_tokens=15, output_tokens=3`)
	if st, _ := c.ScanOnce(context.Background()); st.Recorded != 1 {
		t.Fatalf("post-reset delta %d", st.Recorded)
	}
}

func TestReadOnlyNeverCreatesFiles(t *testing.T) {
	home := t.TempDir()
	db := mkdb(t, filepath.Join(home, "state.db"))
	exec(t, db, `INSERT INTO sessions VALUES ('s1','cli',NULL,1,NULL,NULL)`)
	db.Close()
	before, _ := os.ReadDir(home)
	if _, err := newC(t, home, &memLedger{}).ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadDir(home)
	if len(before) != len(after) {
		t.Fatalf("files changed in Hermes home: %v -> %v", names(before), names(after))
	}
}

func names(es []os.DirEntry) []string {
	var s []string
	for _, e := range es {
		s = append(s, e.Name())
	}
	return s
}
