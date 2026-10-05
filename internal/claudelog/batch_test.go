package claudelog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// fakeBatchLedger implements core.BatchLedger on top of fakeLedger. The first
// failN RecordBatch calls (after skipping failedAfter successful ones) fail;
// Record must never be called.
type fakeBatchLedger struct {
	*fakeLedger
	failN       int
	failedAfter int
	batches     []int // sizes of successful batches
	sent        int   // records in successful batches, including duplicates
	single      int   // Record calls (should stay 0)
}

func (l *fakeBatchLedger) Record(ctx context.Context, r core.RequestRecord) error {
	l.mu.Lock()
	l.single++
	l.mu.Unlock()
	return l.fakeLedger.Record(ctx, r)
}

func (l *fakeBatchLedger) RecordBatch(_ context.Context, rs []core.RequestRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(rs) > MaxBatch {
		return fmt.Errorf("batch too large: %d", len(rs))
	}
	if l.failedAfter > 0 {
		l.failedAfter--
	} else if l.failN > 0 {
		l.failN--
		return errors.New("server down")
	}
	l.batches = append(l.batches, len(rs))
	l.sent += len(rs)
	for _, r := range rs {
		if _, ok := l.rows[r.ID]; !ok {
			l.rows[r.ID] = r
			l.order = append(l.order, r.ID)
		}
	}
	return nil
}

func (e *env) batchCollector(t *testing.T, l *fakeBatchLedger, host string) *Collector {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	return New(l, Options{
		Dir: e.dir, AccountID: "claude-max", Host: host, StatePath: e.statePath, Clock: e.clock,
	})
}

// stateOffsets reads the persisted offsets (nil map if no state file).
func stateOffsets(t *testing.T, path string) map[string]int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for rel, f := range p.Files {
		out[rel] = f.Offset
	}
	return out
}

func manyLines(prefix string, n int) string {
	var sb strings.Builder
	for i := range n {
		sb.WriteString(line(fmt.Sprintf("%s%d", prefix, i), "r", 1))
	}
	return sb.String()
}

func TestBatchChunking(t *testing.T) {
	e := newEnv(t)
	content := manyLines("m", 1200)
	e.write(t, "p/big.jsonl", content, t0.Add(-time.Hour))
	l := &fakeBatchLedger{fakeLedger: newFakeLedger()}
	st := e.scan(t, e.batchCollector(t, l, ""))
	if st.Recorded != 1200 {
		t.Fatalf("stats = %+v", st)
	}
	if fmt.Sprint(l.batches) != "[500 500 200]" {
		t.Fatalf("batches = %v", l.batches)
	}
	if l.single != 0 {
		t.Fatalf("Record called %d times on a BatchLedger", l.single)
	}
	if got := stateOffsets(t, e.statePath)["p/big.jsonl"]; got != int64(len(content)) {
		t.Fatalf("offset = %d, want %d", got, len(content))
	}
}

func TestBatchFailureKeepsOffsets(t *testing.T) {
	e := newEnv(t)
	big := manyLines("m", 1200)
	e.write(t, "p/a.jsonl", big, t0.Add(-time.Hour))
	l := &fakeBatchLedger{fakeLedger: newFakeLedger()}
	c := e.batchCollector(t, l, "")

	// First chunk fails: nothing recorded, no offset persisted.
	l.failN = 1
	if _, err := c.ScanOnce(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if off, ok := stateOffsets(t, e.statePath)["p/a.jsonl"]; ok && off != 0 {
		t.Fatalf("offset advanced to %d after failure", off)
	}

	// Second chunk fails after the first succeeded: offset still not advanced.
	l.failedAfter, l.failN = 1, 1 // next call succeeds, the one after fails
	if _, err := c.ScanOnce(context.Background()); err == nil {
		t.Fatal("want error on partial batch failure")
	}
	if len(l.batches) != 1 || len(l.rowList()) != MaxBatch {
		t.Fatalf("partial: batches=%v rows=%d", l.batches, len(l.rowList()))
	}
	if off, ok := stateOffsets(t, e.statePath)["p/a.jsonl"]; ok && off != 0 {
		t.Fatalf("offset advanced to %d after partial failure", off)
	}

	// A restarted collector (from the persisted state) retries everything.
	c = e.batchCollector(t, l, "")
	st := e.scan(t, c)
	if st.Recorded != 1200 {
		t.Fatalf("retry stats = %+v", st)
	}
	if got := stateOffsets(t, e.statePath)["p/a.jsonl"]; got != int64(len(big)) {
		t.Fatalf("offset = %d, want %d", got, len(big))
	}
	rows := l.rowList()
	if len(rows) != 1200 {
		t.Fatalf("rows = %d, want exactly 1200", len(rows))
	}
	if l.sent != 500+1200 {
		t.Fatalf("sent = %d (re-sent records dedupe by ID)", l.sent)
	}

	// Nothing new: no further batches.
	n := len(l.batches)
	if st := e.scan(t, c); st.Recorded != 0 || len(l.batches) != n {
		t.Fatalf("idle rescan: stats=%+v batches=%v", st, l.batches)
	}
}

func TestBatchFailureIsPerFile(t *testing.T) {
	e := newEnv(t)
	e.write(t, "p/a.jsonl", line("m1", "r1", 1), t0.Add(-time.Hour))
	e.write(t, "p/b.jsonl", line("m2", "r2", 1), t0.Add(-time.Hour))
	l := &fakeBatchLedger{fakeLedger: newFakeLedger(), failN: 1}
	c := e.batchCollector(t, l, "")
	st, err := c.ScanOnce(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if st.Recorded != 1 {
		t.Fatalf("stats = %+v", st)
	}
	offs := stateOffsets(t, e.statePath)
	advanced := 0
	for _, o := range offs {
		if o > 0 {
			advanced++
		}
	}
	if advanced != 1 {
		t.Fatalf("offsets = %v, want exactly one file advanced", offs)
	}
	if st := e.scan(t, c); st.Recorded != 1 {
		t.Fatalf("retry stats = %+v", st)
	}
	if len(l.rowList()) != 2 || l.sent != 2 {
		t.Fatalf("rows=%d sent=%d", len(l.rowList()), l.sent)
	}
}

func TestHostPropagated(t *testing.T) {
	e := newEnv(t)
	e.write(t, "p/s.jsonl", line("m1", "r1", 1)+line("m2", "r2", 1), t0.Add(-time.Hour))
	l := &fakeBatchLedger{fakeLedger: newFakeLedger()}
	e.scan(t, e.batchCollector(t, l, "vm1"))
	for _, r := range l.rowList() {
		if r.Host != "vm1" {
			t.Fatalf("batch Host = %q", r.Host)
		}
	}

	e2 := newEnv(t)
	e2.write(t, "p/s.jsonl", line("m1", "r1", 1), t0.Add(-time.Hour))
	c := New(e2.ledger, Options{Dir: e2.dir, AccountID: "claude-max", Host: "vm2", Clock: e2.clock})
	e2.scan(t, c)
	rows := e2.ledger.rowList()
	if len(rows) != 1 || rows[0].Host != "vm2" {
		t.Fatalf("record rows = %+v", rows)
	}

	// Server-local collector leaves Host empty.
	e3 := newEnv(t)
	e3.write(t, "p/s.jsonl", line("m1", "r1", 1), t0.Add(-time.Hour))
	e3.scan(t, e3.collector(t))
	if rows := e3.ledger.rowList(); len(rows) != 1 || rows[0].Host != "" {
		t.Fatalf("local rows = %+v", rows)
	}
}
