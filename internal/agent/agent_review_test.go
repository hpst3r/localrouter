package agent

// Adversarial review tests; skipped until fixed. Run with LOCALROUTER_REVIEW=1
// to see them fail.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/claudelog"
	"github.com/hpst3r/localrouter/internal/control"
	"github.com/hpst3r/localrouter/internal/core"
)

func reviewBug(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("LOCALROUTER_REVIEW") == "" {
		t.Skip("BUG: " + msg)
	}
}

type memBatchLedger struct {
	mu   sync.Mutex
	rows map[string]core.RequestRecord
}

func (l *memBatchLedger) Record(ctx context.Context, r core.RequestRecord) error {
	return l.RecordBatch(ctx, []core.RequestRecord{r})
}
func (l *memBatchLedger) RecordBatch(_ context.Context, rs []core.RequestRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range rs {
		if _, ok := l.rows[r.ID]; !ok {
			l.rows[r.ID] = r
		}
	}
	return nil
}
func (l *memBatchLedger) Summary(context.Context, time.Time, string) ([]core.UsageRow, error) {
	return nil, nil
}
func (l *memBatchLedger) Close() error { return nil }

// One transcript line the server rejects (here: negative output_tokens; any
// 400 cause behaves the same) sits in front of valid lines. The 400 is
// treated like an outage: the file's offset never advances, so every scan
// re-sends the same chunk, gets 400 again, and the valid usage behind it —
// plus everything later appended to that session — is never delivered.
// The agent only logs a warning and backs off; there is no quarantine or
// skip. Expected: permanent 4xx rejections are isolated (e.g. drop/skip the
// offending record client-side, or have ingest report per-record rejects).
func TestReviewPoisonRecordStallsFile(t *testing.T) {
	reviewBug(t, "a single record rejected with 400 stalls its transcript forever (infinite retry, later usage never sent)")
	led := &memBatchLedger{rows: map[string]core.RequestRecord{}}
	h := control.New(control.Deps{
		Accounts: []core.Account{{ID: "claude-max", Provider: core.ProviderClaude, QuotaSource: "agent"}},
		Ledger:   led,
		Clock:    core.SystemClock{},
		Authenticate: func(b string) (core.Client, bool) {
			return core.Client{Name: "vm1", Ingest: true}, b == testKey
		},
	}, control.Options{}).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	projects := filepath.Join(t.TempDir(), "projects")
	p := filepath.Join(projects, "-home-u-proj", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	var content string
	content += `{"requestId":"req_bad","timestamp":"2026-10-01T00:00:00Z","message":{"id":"msg_bad","model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":-1}}}` + "\n"
	for i := range 3 {
		content += fmt.Sprintf(`{"requestId":"req_%d","timestamp":"2026-10-01T00:00:0%dZ","message":{"id":"msg_%d","model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n", i, i, i)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	ledger := NewRemoteLedger(NewClient(srv.URL, "vm1", testKey, srv.Client()))
	col := claudelog.New(ledger, claudelog.Options{
		Dir: projects, AccountID: "claude-max", StatePath: filepath.Join(t.TempDir(), "st.json"),
		Clock: fixedClock{time.Now().Add(time.Hour)}, Logger: quiet(),
	})
	a := New(Options{Client: ledger.c, AccountID: "claude-max", Logger: quiet(),
		Scan: func(ctx context.Context) error { _, err := col.ScanOnce(ctx); return err }})
	var lastErr error
	for range 3 {
		lastErr = a.RunOnce(context.Background())
	}
	led.mu.Lock()
	defer led.mu.Unlock()
	if len(led.rows) != 3 {
		t.Fatalf("after 3 scans: %d of 3 valid records delivered; last error: %v", len(led.rows), lastErr)
	}
}
