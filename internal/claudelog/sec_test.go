package claudelog

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hpst3r/localrouter/internal/core"
)

// Transcript lines with implausible timestamps are skipped like ingest would
// reject them; plausible lines in the same file still import.
func TestSecTimestampPlausibility(t *testing.T) {
	e := newEnv(t)
	at := func(ts time.Time) func(map[string]any) {
		return func(m map[string]any) { m["timestamp"] = ts.Format(time.RFC3339Nano) }
	}
	content := line("m1", "r1", 5, at(t0.Add(time.Hour))) +
		line("m2", "r2", 5, at(t0.Add(-401*24*time.Hour))) +
		line("m3", "r3", 5, func(m map[string]any) { m["timestamp"] = "0001-01-01T00:00:00Z" }) +
		line("m4", "r4", 5, at(t0.Add(4*time.Minute))) +
		line("m5", "r5", 5, at(t0.Add(-399*24*time.Hour)))
	e.write(t, "p/s.jsonl", content, t0.Add(-time.Hour))
	st := e.scan(t, e.collector(t))
	if st.Recorded != 2 || st.Skipped != 3 {
		t.Fatalf("stats %+v; want 2 recorded, 3 skipped", st)
	}
}

// Labels copied from transcripts are bounded and printable.
func TestSecLabelsSanitized(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("x", 10<<10)
	l := line("m1", "r1", 5, func(m map[string]any) {
		m["sessionId"] = "s\x01" + big
		m["message"].(map[string]any)["model"] = "claude-\x7f" + big
	})
	proj := strings.Repeat("p", 200)
	e.write(t, proj+"/s.jsonl", l, t0.Add(-time.Hour))
	c := e.collector(t)
	c.opts.Client = "c\x01" + big
	st := e.scan(t, c)
	rows := e.ledger.rowList()
	if st.Recorded != 1 || len(rows) != 1 {
		t.Fatalf("stats %+v", st)
	}
	r := rows[0]
	for name, v := range map[string]string{"model": r.Model, "session": r.Session, "task": r.Task, "client": r.Client} {
		if len(v) > core.MaxLabelBytes || !utf8.ValidString(v) || strings.ContainsAny(v, "\x01\x7f") {
			t.Errorf("%s not sanitized: len=%d", name, len(v))
		}
	}
}
