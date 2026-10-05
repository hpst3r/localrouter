package claudelog

// Adversarial review tests; skipped until fixed. Run with LOCALROUTER_REVIEW=1
// to see them fail.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reviewBug(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("LOCALROUTER_REVIEW") == "" {
		t.Skip("BUG: " + msg)
	}
}

// SPEC: logs never contain file paths beyond the project dir name. The
// "scan file" warning logs the project slug, but its err is an
// *fs.PathError carrying the full session path (and the walk warning logs
// "path" outright). An unreadable transcript leaks /home/<user>/... and the
// session file name into agent and server logs.
func TestReviewScanErrorLogsNoPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	e := newEnv(t)
	p := e.write(t, "-home-u-proj/secret-session.jsonl", line("m1", "r1", 5), t0.Add(-time.Hour))
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o600) })
	locked := filepath.Join(e.dir, "-home-u-locked")
	if err := os.Mkdir(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })

	var logs bytes.Buffer
	c := New(e.ledger, Options{Dir: e.dir, AccountID: "claude-max", Clock: e.clock,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	_, _ = c.ScanOnce(context.Background())
	out := logs.String()
	if out == "" {
		t.Fatal("expected a warning")
	}
	for _, leak := range []string{"secret-session", e.dir} {
		if strings.Contains(out, leak) {
			t.Errorf("log contains %q:\n%s", leak, out)
		}
	}
}

// parse only rejects all-zero usage; negative token counts (malformed or
// overflowing transcript values) become a record that the server's ingest
// validation rejects with 400 — permanently, see agent
// TestReviewPoisonRecordStallsFile.
func TestReviewParseRejectsNegativeUsage(t *testing.T) {
	e := newEnv(t)
	e.write(t, "p/s.jsonl", line("m1", "r1", -5), t0.Add(-time.Hour))
	st := e.scan(t, e.collector(t))
	for _, r := range e.ledger.rowList() {
		if r.Usage.OutputTokens < 0 || r.Usage.ReasoningTokens < 0 {
			t.Fatalf("recorded negative usage %+v (stats %+v)", r.Usage, st)
		}
	}
}
