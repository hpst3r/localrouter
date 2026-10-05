package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/claudelog"
	"github.com/hpst3r/localrouter/internal/core"
)

// fakeIngest mimics POST /control/v1/ingest: it checks the key, fails the
// first failFirst requests with 503, and stores records deduped by ID.
type fakeIngest struct {
	mu        sync.Mutex
	failFirst int
	calls     int
	delivered int // records in successful requests (incl. dedupes)
	rows      map[string]core.RequestRecord
}

func (f *fakeIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.calls++
	if f.calls <= f.failFirst {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	var req core.IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SchemaVersion != 1 || req.Host == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, rec := range req.Records {
		rec.Host = req.Host
		if _, dup := f.rows[rec.ID]; !dup {
			f.rows[rec.ID] = rec
		}
	}
	f.delivered += len(req.Records)
	json.NewEncoder(w).Encode(core.IngestResponse{SchemaVersion: 1, RecordsAccepted: len(req.Records)})
}

func writeTranscript(t *testing.T, path string, n int, prefix string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := range n {
		fmt.Fprintf(f, `{"type":"assistant","requestId":"req_%s%d","sessionId":"s1","timestamp":"2026-10-01T00:00:%02dZ","message":{"id":"msg_%s%d","model":"claude-opus-5-5","content":[{"type":"text","text":"SECRET PROMPT"}],"usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":%d}}}`+"\n",
			prefix, i, i, prefix, i, i+1)
	}
}

// TestMH7AgentEndToEnd: real claudelog Collector over a fixture dir pushes
// through RemoteLedger; the server's first push fails with 503, so offsets
// are not advanced; the next run (after a restart) delivers every record
// exactly once, and a further run delivers nothing.
func TestMH7AgentEndToEnd(t *testing.T) {
	projects := filepath.Join(t.TempDir(), "projects")
	state := filepath.Join(t.TempDir(), "claudelog-state.json")
	writeTranscript(t, filepath.Join(projects, "-home-u-proj-a", "s1.jsonl"), 3, "a")
	writeTranscript(t, filepath.Join(projects, "-home-u-proj-b", "s2.jsonl"), 2, "b")
	const want = 5

	fake := &fakeIngest{failFirst: 1, rows: map[string]core.RequestRecord{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	// Far enough ahead that every message is final (QuietPeriod elapsed).
	clock := fixedClock{time.Now().Add(time.Hour)}
	newAgent := func() *Agent {
		ledger := NewRemoteLedger(NewClient(srv.URL, "vm1", testKey, srv.Client()))
		col := claudelog.New(ledger, claudelog.Options{
			Dir: projects, AccountID: "claude-max", StatePath: state, Clock: clock, Logger: quiet(),
		})
		return New(Options{
			Client: ledger.c, AccountID: "claude-max", Logger: quiet(),
			Scan: func(ctx context.Context) error { _, err := col.ScanOnce(ctx); return err },
		})
	}
	ctx := context.Background()

	if err := newAgent().RunOnce(ctx); err == nil {
		t.Fatal("first run should report the 503")
	}
	if err := newAgent().RunOnce(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	fake.mu.Lock()
	if len(fake.rows) != want || fake.delivered != want {
		t.Fatalf("after recovery: %d unique rows, %d delivered; want %d each", len(fake.rows), fake.delivered, want)
	}
	for id, r := range fake.rows {
		if r.Host != "vm1" || r.AccountID != "claude-max" || !r.UsageKnown || r.Usage.InputTokens != 15 {
			t.Fatalf("row %s: %+v", id, r)
		}
	}
	fake.mu.Unlock()

	if err := newAgent().RunOnce(ctx); err != nil {
		t.Fatalf("third run: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.delivered != want {
		t.Fatalf("third run re-sent records: delivered %d", fake.delivered)
	}
}
