package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const hermesSchema = `
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

// backfillEnv writes a Hermes home with one self-routed gpt-6.1-sol row
// inside the window, one after the cutoff, one without first_seen (review)
// and a self-routed Ollama row, plus a config pointing hermes_logs at it. It
// returns the config path, the data dir and the cutoff; the window starts 3h
// before the cutoff.
func backfillEnv(t *testing.T, enabled bool) (string, string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "hermes")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cutoff := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	before := float64(cutoff.Add(-time.Hour).Unix())
	after := float64(cutoff.Add(time.Minute).Unix())
	for _, q := range []string{hermesSchema,
		`INSERT INTO sessions VALUES ('s1','cli',NULL,1,'/p/foo','/p/foo')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	first := float64(cutoff.Add(-2 * time.Hour).Unix())
	ins := `INSERT INTO session_model_usage (session_id,model,billing_provider,billing_base_url,task,api_call_count,input_tokens,output_tokens,cache_read_tokens,first_seen,last_seen) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	for _, a := range [][]any{
		{"s1", "gpt-6.1-sol", "localrouter-codex", "http://127.0.0.1:8787/v1", "", 4, 100, 10, 1000, first, before},
		{"s-late", "gpt-6.1-sol", "custom", "http://localhost:8787/v1", "approval", 1, 5, 1, 0, first, after},
		{"s-nofs", "gpt-6.1-sol", "localrouter-codex", "http://127.0.0.1:8787/v1", "", 2, 7, 1, 0, nil, before},
		{"s1", "deepseek-v4.1-flash", "localrouter-bg", "http://127.0.0.1:8787/v1", "", 9, 900, 90, 0, first, before},
	} {
		if _, err := db.Exec(ins, a...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "c.key"), []byte("lr-client-key-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `listen: 127.0.0.1:8787
data_dir: data
clients:
  - {name: c, class: interactive, key_file: c.key}
accounts:
  - {id: codex-main, provider: codex}
routes:
  - {name: r, models: [gpt-6.1-sol], interactive: [codex-main]}
hermes_logs:
  enabled: ` + map[bool]string{true: "true", false: "false"}[enabled] + `
  home: hermes
`
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, filepath.Join(dir, "data"), cutoff
}

func backfillRows(t *testing.T, dataDir string) (n int, in int64) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "localrouter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT count(*), COALESCE(sum(input_tokens),0) FROM requests WHERE id LIKE 'hermes-backfill:%' AND account_id='codex-main' AND provider='codex' AND usage_known=1`).Scan(&n, &in); err != nil {
		t.Fatal(err)
	}
	return n, in
}

func TestCmdHermeslogBackfillSelf(t *testing.T) {
	cfg, dataDir, cutoff := backfillEnv(t, true)
	args := []string{"backfill-self", "-config", cfg, "--model", "gpt-6.1-sol", "--account", "codex-main",
		"--after", cutoff.Add(-3 * time.Hour).Format(time.RFC3339), "--before", cutoff.Format(time.RFC3339)}

	var out strings.Builder
	if err := runHermeslog(args, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	t.Logf("dry run output:\n%s", s)
	if !strings.Contains(s, "dry run") || !strings.Contains(s, "selected: 1 rows") ||
		!strings.Contains(s, "excluded: outside the window: 1") || !strings.Contains(s, "s-late") ||
		!strings.Contains(s, "review: 1 rows have no first_seen; WARNING") || !strings.Contains(s, "s-nofs") ||
		!strings.Contains(s, "--accept-partial-sessions") || !strings.Contains(s, "pass --apply to record 1 new rows") ||
		strings.Contains(s, "deepseek") {
		t.Fatalf("dry run output:\n%s", s)
	}
	// A dry run writes nothing: it does not even create the ledger.
	if _, err := os.Stat(filepath.Join(dataDir, "localrouter.db")); !os.IsNotExist(err) {
		t.Fatalf("dry run created the ledger: %v", err)
	}

	out.Reset()
	if err := runHermeslog(append(args, "--apply"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "recorded: 1") {
		t.Fatalf("apply output:\n%s", out.String())
	}
	if n, in := backfillRows(t, dataDir); n != 1 || in != 1100 {
		t.Fatalf("after apply rows=%d input=%d", n, in)
	}

	out.Reset()
	if err := runHermeslog(append(args, "--apply"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "recorded: 0") || !strings.Contains(out.String(), "already in ledger: 1") {
		t.Fatalf("second apply output:\n%s", out.String())
	}
	if n, _ := backfillRows(t, dataDir); n != 1 {
		t.Fatalf("after second apply rows=%d", n)
	}
	// A dry run against the populated ledger reports the row as present and
	// leaves it alone.
	out.Reset()
	if err := runHermeslog(args, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already in ledger: 1") || !strings.Contains(out.String(), "pass --apply to record 0 new rows") {
		t.Fatalf("dry run after apply output:\n%s", out.String())
	}
	if n, _ := backfillRows(t, dataDir); n != 1 {
		t.Fatalf("after dry run rows=%d", n)
	}

	// A row from the first backfill (counter-hashed ID, no k1: prefix) with
	// the same labels also counts as present.
	ldb, err := sql.Open("sqlite", filepath.Join(dataDir, "localrouter.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ldb.Exec(`UPDATE requests SET id = 'hermes-backfill:0123456789abcdef0123456789abcdef' WHERE id LIKE 'hermes-backfill:k1:%'`); err != nil {
		t.Fatal(err)
	}
	ldb.Close()
	out.Reset()
	if err := runHermeslog(append(args, "--apply"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "recorded: 0") || !strings.Contains(out.String(), "already in ledger: 1") {
		t.Fatalf("apply over first-backfill row output:\n%s", out.String())
	}

	// --accept-partial-sessions records the review row too, once.
	out.Reset()
	if err := runHermeslog(append(args, "--apply", "--accept-partial-sessions"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "recorded: 1") || !strings.Contains(out.String(), "already in ledger: 1") {
		t.Fatalf("accept partial output:\n%s", out.String())
	}
	if n, in := backfillRows(t, dataDir); n != 2 || in != 1100+7 {
		t.Fatalf("after accept partial rows=%d input=%d", n, in)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "hermeslog-state.json")); !os.IsNotExist(err) {
		t.Fatalf("backfill wrote collector state: %v", err)
	}
}

func TestCmdHermeslogErrors(t *testing.T) {
	cfg, _, cutoff := backfillEnv(t, true)
	ok := cutoff.Format(time.RFC3339)
	af := cutoff.Add(-time.Hour).Format(time.RFC3339)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no subcommand":   {nil, "backfill-self"},
		"bad subcommand":  {[]string{"bogus"}, "backfill-self"},
		"no model":        {[]string{"backfill-self", "-config", cfg, "--account", "codex-main", "--after", af, "--before", ok}, "usage"},
		"no before":       {[]string{"backfill-self", "-config", cfg, "--model", "m", "--account", "codex-main", "--after", af}, "usage"},
		"no after":        {[]string{"backfill-self", "-config", cfg, "--model", "m", "--account", "codex-main", "--before", ok}, "usage"},
		"no account":      {[]string{"backfill-self", "-config", cfg, "--model", "m", "--after", af, "--before", ok}, "usage"},
		"bad before":      {[]string{"backfill-self", "-config", cfg, "--model", "m", "--account", "codex-main", "--after", af, "--before", "yesterday"}, "RFC3339"},
		"bad after":       {[]string{"backfill-self", "-config", cfg, "--model", "m", "--account", "codex-main", "--after", "yesterday", "--before", ok}, "RFC3339"},
		"after >= before": {[]string{"backfill-self", "-config", cfg, "--model", "m", "--account", "codex-main", "--after", ok, "--before", ok}, "usage"},
		"unknown acct":    {[]string{"backfill-self", "-config", cfg, "--model", "m", "--account", "nope", "--after", af, "--before", ok}, "unknown account"},
		"extra args":      {[]string{"backfill-self", "-config", cfg, "--model", "m", "--account", "codex-main", "--after", af, "--before", ok, "x"}, "usage"},
	} {
		var out strings.Builder
		err := runHermeslog(tc.args, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}

	disabled, _, _ := backfillEnv(t, false)
	var out strings.Builder
	err := runHermeslog([]string{"backfill-self", "-config", disabled, "--model", "m", "--account", "codex-main", "--after", af, "--before", ok}, &out)
	if err == nil || !strings.Contains(err.Error(), "hermes_logs") {
		t.Errorf("disabled: %v", err)
	}
}
