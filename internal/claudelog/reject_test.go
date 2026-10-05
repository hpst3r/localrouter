package claudelog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

type classifiedErr struct{ perm bool }

func (e classifiedErr) Error() string   { return fmt.Sprintf("rejected (permanent=%v)", e.perm) }
func (e classifiedErr) Permanent() bool { return e.perm }

// rejectingLedger fails any batch containing a record with OutputTokens ==
// badOut, with a permanent or transient classifiedErr.
type rejectingLedger struct {
	mu        sync.Mutex
	badOut    int64
	permanent bool
	calls     int
	rows      map[string]core.RequestRecord
}

func (l *rejectingLedger) Record(ctx context.Context, r core.RequestRecord) error {
	return l.RecordBatch(ctx, []core.RequestRecord{r})
}

func (l *rejectingLedger) RecordBatch(_ context.Context, rs []core.RequestRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	for _, r := range rs {
		if r.Usage.OutputTokens == l.badOut {
			return fmt.Errorf("ingest: %w", classifiedErr{perm: l.permanent})
		}
	}
	for _, r := range rs {
		l.rows[r.ID] = r
	}
	return nil
}

func (l *rejectingLedger) Summary(context.Context, time.Time, string) ([]core.UsageRow, error) {
	return nil, nil
}
func (l *rejectingLedger) Close() error { return nil }

// singleLedger hides RecordBatch so the per-record path is used.
type singleLedger struct{ l *rejectingLedger }

func (s singleLedger) Record(ctx context.Context, r core.RequestRecord) error {
	return s.l.Record(ctx, r)
}
func (s singleLedger) Summary(ctx context.Context, since time.Time, by string) ([]core.UsageRow, error) {
	return s.l.Summary(ctx, since, by)
}
func (s singleLedger) Close() error { return nil }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeMixed(t *testing.T, e *env) {
	t.Helper()
	var content string
	for i, out := range []int64{5, 6, 7, 8, 7, 9} {
		content += line(fmt.Sprintf("m%d", i), fmt.Sprintf("r%d", i), out)
	}
	e.write(t, "-home-u-proj/s.jsonl", content, t0.Add(-time.Hour))
}

func TestPermanentRejectBisectsAndAdvances(t *testing.T) {
	e := newEnv(t)
	writeMixed(t, e)
	l := &rejectingLedger{badOut: 7, permanent: true, rows: map[string]core.RequestRecord{}}
	c := New(l, Options{Dir: e.dir, AccountID: "claude-max", Clock: e.clock, Logger: quietLogger()})
	st, err := c.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if st.Recorded != 4 || st.Dropped != 2 || len(l.rows) != 4 {
		t.Fatalf("stats %+v, rows %d; want 4 recorded, 2 dropped", st, len(l.rows))
	}
	calls := l.calls
	st, err = c.ScanOnce(context.Background())
	if err != nil || st.Recorded != 0 || st.Dropped != 0 || l.calls != calls {
		t.Fatalf("rescan resent records: stats %+v err %v calls %d->%d", st, err, calls, l.calls)
	}
}

func TestTransientRejectDoesNotAdvance(t *testing.T) {
	e := newEnv(t)
	writeMixed(t, e)
	l := &rejectingLedger{badOut: 7, permanent: false, rows: map[string]core.RequestRecord{}}
	c := New(l, Options{Dir: e.dir, AccountID: "claude-max", Clock: e.clock, Logger: quietLogger()})
	for range 2 {
		st, err := c.ScanOnce(context.Background())
		var ce classifiedErr
		if !errors.As(err, &ce) || st.Dropped != 0 || len(l.rows) != 0 {
			t.Fatalf("stats %+v err %v rows %d; want transient error, nothing dropped", st, err, len(l.rows))
		}
	}
	if l.calls != 2 {
		t.Fatalf("calls = %d, want one batch per scan (no bisection)", l.calls)
	}
}

func TestPermanentRejectSingleRecordPath(t *testing.T) {
	e := newEnv(t)
	writeMixed(t, e)
	l := &rejectingLedger{badOut: 7, permanent: true, rows: map[string]core.RequestRecord{}}
	c := New(singleLedger{l}, Options{Dir: e.dir, AccountID: "claude-max", Clock: e.clock, Logger: quietLogger()})
	st, err := c.ScanOnce(context.Background())
	if err != nil || st.Recorded != 4 || st.Dropped != 2 {
		t.Fatalf("stats %+v err %v; want 4 recorded, 2 dropped", st, err)
	}
}

func TestParseSkipsInvalidUsage(t *testing.T) {
	e := newEnv(t)
	huge := line("m1", "r1", 5, func(m map[string]any) {
		m["message"].(map[string]any)["usage"].(map[string]any)["cache_read_input_tokens"] = int64(math.MaxInt64)
	})
	noTS := line("m2", "r2", 5, func(m map[string]any) { delete(m, "timestamp") })
	e.write(t, "p/s.jsonl", huge+noTS+line("m3", "r3", 5), t0.Add(-time.Hour))
	st := e.scan(t, e.collector(t))
	if st.Recorded != 1 || st.Skipped != 2 || len(e.ledger.rowList()) != 1 {
		t.Fatalf("stats %+v; want 1 recorded, 2 skipped", st)
	}
}
