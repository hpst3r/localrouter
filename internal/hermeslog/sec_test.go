package hermeslog

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hpst3r/localrouter/internal/core"
)

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

var secNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func secCollector(t *testing.T, home string, l core.Ledger) *Collector {
	return New(l, Options{
		Home:      home,
		Accounts:  map[string]string{"anthropic": "claude-max"},
		StatePath: filepath.Join(t.TempDir(), "state.json"),
		Clock:     fixedClock(secNow),
	})
}

func secUnix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// Implausible Hermes rows (huge or overflowing counters, negative counters,
// bad last_seen) are skipped; valid rows in the same scan still import and
// no recorded field is out of range.
func TestSecImplausibleRowsSkipped(t *testing.T) {
	home := t.TempDir()
	db := mkdb(t, filepath.Join(home, "state.db"))
	ok := secUnix(secNow.Add(-time.Hour))
	q := `INSERT INTO session_model_usage (session_id,model,billing_provider,api_call_count,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,last_seen) VALUES (?,?,?,?,?,?,?,?,?)`
	exec(t, db, q, "good", "claude-x", "anthropic", 1, 10, 5, 0, 0, ok)
	exec(t, db, q, "overflow", "m", "anthropic", 1, int64(9e18), 1, int64(9e18), 0, ok)
	exec(t, db, q, "huge", "m", "anthropic", 1, 1, core.MaxRecordTokens+1, 0, 0, ok)
	exec(t, db, q, "sum", "m", "anthropic", 1, core.MaxRecordTokens, 1, core.MaxRecordTokens, 0, ok)
	exec(t, db, q, "neg", "m", "anthropic", 1, -5, 1, 0, 0, ok)
	exec(t, db, q, "future", "m", "anthropic", 1, 1, 1, 0, 0, secUnix(secNow.Add(time.Hour)))
	exec(t, db, q, "old", "m", "anthropic", 1, 1, 1, 0, 0, secUnix(secNow.Add(-401*24*time.Hour)))
	exec(t, db, q, "inf", "m", "anthropic", 1, 1, 1, 0, 0, 1e300)
	exec(t, db, q, "epoch", "m", "anthropic", 1, 1, 1, 0, 0, 1)
	l := &memLedger{}
	c := secCollector(t, home, l)
	st, err := c.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// "neg" goes below the zero baseline and is rebased like a counter reset
	// (not counted as invalid); the other seven are implausible.
	if st.Recorded != 1 || st.SkippedInvalid != 7 {
		t.Fatalf("stats %+v", st)
	}
	for _, r := range l.rows {
		if r.Session != "good" {
			t.Fatalf("implausible row recorded: %+v", r)
		}
	}
	// Skipped rows are rebased, not re-evaluated every scan; a later
	// plausible increase on top of them still imports.
	if st, _ := c.ScanOnce(context.Background()); st.SkippedInvalid != 0 || st.Recorded != 0 {
		t.Fatalf("rescan %+v", st)
	}
	exec(t, db, `UPDATE session_model_usage SET output_tokens=output_tokens+7 WHERE session_id='overflow'`)
	if st, _ := c.ScanOnce(context.Background()); st.Recorded != 0 {
		// The baseline is implausible: rebased silently, nothing recorded.
		t.Fatalf("post-skip delta on implausible counters %+v", st)
	}
	exec(t, db, `UPDATE session_model_usage SET output_tokens=output_tokens+7 WHERE session_id='good'`)
	if st, _ := c.ScanOnce(context.Background()); st.Recorded != 1 || st.SkippedInvalid != 0 {
		t.Fatalf("valid delta %+v", st)
	}
}

// Labels taken from Hermes rows are bounded and valid UTF-8.
func TestSecLabelsSanitized(t *testing.T) {
	home := t.TempDir()
	db := mkdb(t, filepath.Join(home, "state.db"))
	big := strings.Repeat("x", 10<<10)
	bad := "bad\xff\x01" + big
	exec(t, db, `INSERT INTO sessions VALUES (?, 'cli', NULL, 1, NULL, ?)`, bad, "/r/"+big)
	exec(t, db, `INSERT INTO session_model_usage (session_id,model,billing_provider,task,api_call_count,input_tokens,output_tokens,last_seen) VALUES (?,?,?,?,1,10,5,?)`,
		bad, bad, "prov"+big, big, secUnix(secNow.Add(-time.Minute)))
	l := &memLedger{}
	if st, err := secCollector(t, home, l).ScanOnce(context.Background()); err != nil || st.Recorded != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	for _, r := range l.rows {
		for name, v := range map[string]string{"model": r.Model, "session": r.Session, "task": r.Task, "agent": r.Agent, "provider": r.Provider, "client": r.Client} {
			if len(v) > core.MaxLabelBytes || !utf8.ValidString(v) || strings.ContainsRune(v, '\x01') {
				t.Errorf("%s not sanitized: len=%d", name, len(v))
			}
		}
	}
}

type permErr struct{}

func (permErr) Error() string   { return "rejected" }
func (permErr) Permanent() bool { return true }

// rejectLedger permanently rejects records for one session.
type rejectLedger struct {
	memLedger
	bad string
}

func (l *rejectLedger) Record(ctx context.Context, r core.RequestRecord) error {
	if r.Session == l.bad {
		return permErr{}
	}
	return l.memLedger.Record(ctx, r)
}

// A permanent ledger rejection drops that row (rebased, not retried) instead
// of stalling every later row of the database on every scan.
func TestSecPermanentRejectDoesNotStall(t *testing.T) {
	home := t.TempDir()
	db := mkdb(t, filepath.Join(home, "state.db"))
	ok := secUnix(secNow.Add(-time.Hour))
	q := `INSERT INTO session_model_usage (session_id,model,billing_provider,api_call_count,input_tokens,output_tokens,last_seen) VALUES (?,?,?,1,10,5,?)`
	exec(t, db, q, "a-bad", "m", "anthropic", ok)
	exec(t, db, q, "b-good", "m", "anthropic", ok)
	l := &rejectLedger{bad: "a-bad"}
	c := secCollector(t, home, l)
	st, err := c.ScanOnce(context.Background())
	if err != nil || st.Recorded != 1 || st.Dropped != 1 {
		t.Fatalf("stats %+v err %v", st, err)
	}
	if st, err := c.ScanOnce(context.Background()); err != nil || st.Dropped != 0 || st.Recorded != 0 {
		t.Fatalf("rescan %+v %v", st, err)
	}
}
