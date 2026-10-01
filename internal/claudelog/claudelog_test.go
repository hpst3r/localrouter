package claudelog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// fakeLedger records every Record call and dedupes rows by ID like the real
// ledger.
type fakeLedger struct {
	mu    sync.Mutex
	calls []core.RequestRecord
	rows  map[string]core.RequestRecord
	order []string
	err   error
}

func newFakeLedger() *fakeLedger { return &fakeLedger{rows: map[string]core.RequestRecord{}} }

func (l *fakeLedger) Record(_ context.Context, r core.RequestRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.calls = append(l.calls, r)
	if _, ok := l.rows[r.ID]; !ok {
		l.rows[r.ID] = r
		l.order = append(l.order, r.ID)
	}
	return nil
}

func (l *fakeLedger) Summary(context.Context, time.Time, string) ([]core.UsageRow, error) {
	return nil, nil
}
func (l *fakeLedger) Close() error { return nil }

func (l *fakeLedger) numCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

func (l *fakeLedger) rowList() []core.RequestRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]core.RequestRecord, 0, len(l.order))
	for _, id := range l.order {
		out = append(out, l.rows[id])
	}
	return out
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 10, 1, 21, 0, 0, 0, time.UTC)

type env struct {
	dir, statePath string
	clock          *fakeClock
	ledger         *fakeLedger
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "projects")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &env{
		dir:       dir,
		statePath: filepath.Join(root, "data", "claudelog-state.json"),
		clock:     &fakeClock{t: t0},
		ledger:    newFakeLedger(),
	}
}

func (e *env) collector(t *testing.T) *Collector {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	return New(e.ledger, Options{
		Dir: e.dir, AccountID: "claude-max", StatePath: e.statePath, Clock: e.clock,
	})
}

func (e *env) scan(t *testing.T, c *Collector) Stats {
	t.Helper()
	st, err := c.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	return st
}

// write writes content to rel under Dir and sets its mtime to mtime.
func (e *env) write(t *testing.T, rel, content string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(e.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, p, mtime)
	return p
}

func appendTo(t *testing.T, p, content string, mtime time.Time) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	touch(t, p, mtime)
}

func touch(t *testing.T, p string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// line builds an assistant transcript line.
func line(msgID, reqID string, out int64, mod ...func(map[string]any)) string {
	usage := map[string]any{
		"input_tokens": 3, "cache_creation_input_tokens": 100, "cache_read_input_tokens": 1000,
		"output_tokens": out, "output_tokens_details": map[string]any{"thinking_tokens": out / 2},
	}
	msg := map[string]any{
		"model": "claude-opus-5-5", "id": msgID, "role": "assistant", "usage": usage,
		"content": []any{map[string]any{"type": "text", "text": "SECRET-CONTENT"}},
	}
	m := map[string]any{
		"type": "assistant", "requestId": reqID, "sessionId": "sess-1",
		"timestamp": "2026-10-01T20:50:51.427Z", "cwd": "/home/someone/secret-path",
		"isSidechain": false, "message": msg,
	}
	if msgID == "" {
		delete(msg, "id")
	}
	if reqID == "" {
		delete(m, "requestId")
	}
	for _, f := range mod {
		f(m)
	}
	b, _ := json.Marshal(m)
	return string(b) + "\n"
}

func readFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/real-shape.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFixture(t *testing.T) {
	e := newEnv(t)
	e.write(t, "proj-slug/4314e43e.jsonl", readFixture(t), t0.Add(-time.Hour))
	st := e.scan(t, e.collector(t))

	rows := e.ledger.rowList()
	// The fixture holds 5 distinct (message.id, requestId) keys; lines 8 and 9
	// share a key.
	wantOut := []int64{144, 167, 910, 113, 2104}
	if len(rows) != len(wantOut) || e.ledger.numCalls() != len(wantOut) {
		t.Fatalf("rows=%d calls=%d, want %d", len(rows), e.ledger.numCalls(), len(wantOut))
	}
	for i, r := range rows {
		if r.Usage.OutputTokens != wantOut[i] {
			t.Errorf("row %d output=%d want %d", i, r.Usage.OutputTokens, wantOut[i])
		}
	}
	if st.Files != 1 || st.Lines != 12 || st.Recorded != 5 || st.Pending != 0 || st.Skipped != 6 {
		t.Errorf("stats = %+v", st)
	}

	r := rows[2] // msg_011CfcBGeX4fkwoHHyHLxJxH, the repeated key
	want := core.Usage{
		InputTokens:              2 + 10037 + 24133,
		CachedInputTokens:        24133,
		CacheCreationInputTokens: 10037,
		OutputTokens:             910,
		ReasoningTokens:          763,
	}
	if r.Usage != want {
		t.Errorf("usage = %+v want %+v", r.Usage, want)
	}
	ts := time.Date(2026, 10, 1, 20, 51, 2, 40e6, time.UTC)
	if !r.StartedAt.Equal(ts) || !r.FinishedAt.Equal(ts) {
		t.Errorf("timestamps = %v / %v", r.StartedAt, r.FinishedAt)
	}
	if r.ID != recordID("msg_011CfcBGeX4fkwoHHyHLxJxH:req_011CfcBGe72H6X5P7HUEgBLs") {
		t.Errorf("id = %s", r.ID)
	}
	if !strings.HasPrefix(r.ID, "claude:") || len(r.ID) != len("claude:")+32 {
		t.Errorf("id format = %s", r.ID)
	}
	if r.Client != "claude-code" || r.Class != core.ClassInteractive || r.Route != "claude" ||
		r.Provider != "claude" || r.AccountID != "claude-max" || r.Model != "claude-opus-5-5" ||
		r.Status != 200 || !r.UsageKnown || r.Session != "4314e43e-bc0d-4c1b-ad3d-8de6950e2754" ||
		r.Task != "proj-slug" || r.Agent != "main" {
		t.Errorf("record fields = %+v", r)
	}
	if last := rows[4]; last.Usage.ReasoningTokens != 430 || last.Usage.InputTokens != 2+1978+37162 {
		t.Errorf("last usage = %+v", last.Usage)
	}
}

// recordID mirrors the SPEC formula independently of the implementation.
func recordID(key string) string {
	return "claude:" + fmt.Sprintf("%x", sha256.Sum256([]byte(key)))[:32]
}

func TestRepeatedKeyKeepsLargestOutput(t *testing.T) {
	e := newEnv(t)
	content := line("msg_a", "req_a", 144) + line("msg_a", "req_a", 167) + line("msg_a", "req_a", 910) +
		line("msg_a", "req_a", 910) + `{"type":"user","message":{"content":"x"}}` + "\n" +
		line("msg_b", "req_b", 113) + line("msg_b", "req_b", 2104)
	e.write(t, "p/s.jsonl", content, t0.Add(-time.Hour))
	e.scan(t, e.collector(t))
	rows := e.ledger.rowList()
	if len(rows) != 2 || rows[0].Usage.OutputTokens != 910 || rows[1].Usage.OutputTokens != 2104 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Usage.ReasoningTokens != 455 || rows[0].Usage.InputTokens != 1103 ||
		rows[0].Usage.CacheCreationInputTokens != 100 || rows[0].Usage.CachedInputTokens != 1000 {
		t.Errorf("usage = %+v", rows[0].Usage)
	}
}

func TestIdempotence(t *testing.T) {
	e := newEnv(t)
	e.write(t, "proj-slug/s.jsonl", readFixture(t), t0.Add(-time.Hour))
	c := e.collector(t)
	e.scan(t, c)
	first := e.ledger.rowList()
	n := e.ledger.numCalls()

	if st := e.scan(t, c); st.Recorded != 0 || st.Lines != 0 {
		t.Errorf("second scan stats = %+v", st)
	}
	if st := e.scan(t, e.collector(t)); st.Recorded != 0 || st.Lines != 0 {
		t.Errorf("restart scan stats = %+v", st)
	}
	if e.ledger.numCalls() != n {
		t.Fatalf("calls grew %d -> %d", n, e.ledger.numCalls())
	}

	if err := os.Remove(e.statePath); err != nil {
		t.Fatal(err)
	}
	st := e.scan(t, e.collector(t))
	if st.Recorded != n || e.ledger.numCalls() != 2*n {
		t.Fatalf("rescan recorded %d, calls %d", st.Recorded, e.ledger.numCalls())
	}
	for i, r := range e.ledger.calls[n:] {
		if r.ID != e.ledger.calls[i].ID {
			t.Errorf("call %d id %s != %s", i, r.ID, e.ledger.calls[i].ID)
		}
	}
	if got := e.ledger.rowList(); len(got) != len(first) {
		t.Errorf("rows %d want %d", len(got), len(first))
	}
}

func TestIncrementalAndPartialLine(t *testing.T) {
	e := newEnv(t)
	old := t0.Add(-time.Hour)
	p := e.write(t, "p/s.jsonl", line("m1", "r1", 10)+line("m2", "r2", 20), old)
	c := e.collector(t)
	if st := e.scan(t, c); st.Lines != 2 || st.Recorded != 2 {
		t.Fatalf("scan1 = %+v", st)
	}

	l3 := line("m3", "r3", 30)
	appendTo(t, p, l3[:len(l3)/2], old) // partial, no trailing newline
	if st := e.scan(t, c); st.Lines != 0 || st.Recorded != 0 {
		t.Fatalf("partial scan = %+v", st)
	}
	appendTo(t, p, l3[len(l3)/2:], old)
	if st := e.scan(t, c); st.Lines != 1 || st.Recorded != 1 {
		t.Fatalf("completed scan = %+v", st)
	}
	rows := e.ledger.rowList()
	if len(rows) != 3 || rows[2].Usage.OutputTokens != 30 || e.ledger.numCalls() != 3 {
		t.Fatalf("rows = %+v", rows)
	}

	// Restart: persisted offset means nothing is re-read.
	if st := e.scan(t, e.collector(t)); st.Lines != 0 {
		t.Errorf("restart read %d lines", st.Lines)
	}
}

func TestFinality(t *testing.T) {
	e := newEnv(t)
	p := e.write(t, "p/s.jsonl", line("m1", "r1", 5)+line("m1", "r1", 50), t0)
	c := e.collector(t)

	st := e.scan(t, c)
	if st.Pending != 1 || st.Recorded != 0 {
		t.Fatalf("recent file: %+v", st)
	}

	// A new key finalizes the previous one; the new last key is pending.
	appendTo(t, p, line("m2", "r2", 7), t0)
	st = e.scan(t, c)
	if st.Recorded != 1 || st.Pending != 1 {
		t.Fatalf("after new key: %+v", st)
	}
	if r := e.ledger.rowList(); r[0].Usage.OutputTokens != 50 {
		t.Errorf("m1 output = %d", r[0].Usage.OutputTokens)
	}

	// Restart while m2 is pending: its lines are re-read and it stays pending.
	c = e.collector(t)
	st = e.scan(t, c)
	if st.Lines != 1 || st.Pending != 1 || st.Recorded != 0 {
		t.Fatalf("restart with pending: %+v", st)
	}

	// Growing usage for the pending key, then quiet period elapses.
	appendTo(t, p, line("m2", "r2", 70), t0.Add(time.Second))
	e.clock.Advance(20 * time.Second)
	if st = e.scan(t, c); st.Recorded != 0 || st.Pending != 1 {
		t.Fatalf("before quiet: %+v", st)
	}
	e.clock.Advance(11 * time.Second)
	if st = e.scan(t, c); st.Recorded != 1 || st.Pending != 0 {
		t.Fatalf("after quiet: %+v", st)
	}
	rows := e.ledger.rowList()
	if len(rows) != 2 || rows[1].Usage.OutputTokens != 70 {
		t.Fatalf("rows = %+v", rows)
	}
	if st = e.scan(t, e.collector(t)); st.Lines != 0 || st.Recorded != 0 {
		t.Errorf("restart after final: %+v", st)
	}
}

func TestTruncationRescans(t *testing.T) {
	e := newEnv(t)
	old := t0.Add(-time.Hour)
	e.write(t, "p/s.jsonl", line("m1", "r1", 1)+line("m2", "r2", 2)+line("m3", "r3", 3), old)
	c := e.collector(t)
	e.scan(t, c)
	e.write(t, "p/s.jsonl", line("m9", "r9", 9), old)
	st := e.scan(t, c)
	if st.Lines != 1 || st.Recorded != 1 {
		t.Fatalf("after truncate: %+v", st)
	}
	rows := e.ledger.rowList()
	if len(rows) != 4 || rows[3].Usage.OutputTokens != 9 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestRecursiveSubagents(t *testing.T) {
	e := newEnv(t)
	old := t0.Add(-time.Hour)
	e.write(t, "proj-a/sess.jsonl", line("m1", "r1", 1), old)
	e.write(t, "proj-a/sess/subagents/agent-1.jsonl", line("m2", "r2", 2, func(m map[string]any) {
		m["isSidechain"] = true
	}), old)
	e.write(t, "proj-a/notes.txt", line("m3", "r3", 3), old)
	st := e.scan(t, e.collector(t))
	if st.Files != 2 || st.Recorded != 2 {
		t.Fatalf("stats = %+v", st)
	}
	agents := map[string]string{}
	for _, r := range e.ledger.rowList() {
		if r.Task != "proj-a" {
			t.Errorf("task = %q", r.Task)
		}
		agents[fmt.Sprint(r.Usage.OutputTokens)] = r.Agent
	}
	if agents["1"] != "main" || agents["2"] != "subagent" {
		t.Errorf("agents = %v", agents)
	}
}

func TestSkips(t *testing.T) {
	e := newEnv(t)
	huge := line("mbig", "rbig", 99, func(m map[string]any) {
		m["pad"] = strings.Repeat("x", maxLineBytes)
	})
	content := huge +
		line("ms", "rs", 5, func(m map[string]any) { m["message"].(map[string]any)["model"] = "<synthetic>" }) +
		line("mu", "ru", 5, func(m map[string]any) { delete(m["message"].(map[string]any), "usage") }) +
		line("mz", "rz", 0, func(m map[string]any) {
			m["message"].(map[string]any)["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		}) +
		line("", "", 5) +
		`{"type":"assistant","message":{"id":` + "\n" +
		line("", "req_only", 11) +
		line("msg_only", "", 12)
	e.write(t, "p/s.jsonl", content, t0.Add(-time.Hour))
	st := e.scan(t, e.collector(t))
	if st.Lines != 8 || st.Skipped != 6 || st.Recorded != 2 {
		t.Fatalf("stats = %+v", st)
	}
	rows := e.ledger.rowList()
	if len(rows) != 2 || rows[0].ID != recordID("req_only") || rows[1].ID != recordID("msg_only") {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestPrivacyAndStateFile(t *testing.T) {
	e := newEnv(t)
	e.write(t, "proj-slug/s.jsonl", readFixture(t)+line("m1", "r1", 3), t0.Add(-time.Hour))
	e.scan(t, e.collector(t))
	rows := e.ledger.rowList()
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	dump := fmt.Sprintf("%+v", rows)
	for _, bad := range []string{"redacted", "SECRET", "secret-path", "/home", "text"} {
		if strings.Contains(dump, bad) {
			t.Errorf("rows contain %q", bad)
		}
	}
	for _, r := range rows {
		if r.Task != "proj-slug" {
			t.Errorf("task = %q", r.Task)
		}
	}
	fi, err := os.Stat(e.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode = %v", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(e.statePath)
	if strings.Contains(string(b), e.dir) || strings.Contains(string(b), "redacted") {
		t.Errorf("state file leaks paths/content: %s", b)
	}
}

func TestLedgerErrorRetries(t *testing.T) {
	e := newEnv(t)
	e.write(t, "p/s.jsonl", line("m1", "r1", 1)+line("m2", "r2", 2), t0.Add(-time.Hour))
	c := e.collector(t)
	e.ledger.err = errors.New("db down")
	if _, err := c.ScanOnce(context.Background()); err == nil {
		t.Fatal("want error")
	}
	e.ledger.err = nil
	if st := e.scan(t, c); st.Recorded != 2 {
		t.Fatalf("retry stats = %+v", st)
	}
}

func TestMissingDir(t *testing.T) {
	c := New(newFakeLedger(), Options{Dir: filepath.Join(t.TempDir(), "nope")})
	st, err := c.ScanOnce(context.Background())
	if err != nil || st.Files != 0 {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}

func TestStart(t *testing.T) {
	e := newEnv(t)
	e.write(t, "p/s.jsonl", line("m1", "r1", 1), t0.Add(-time.Hour))
	c := New(e.ledger, Options{Dir: e.dir, Clock: e.clock, ScanInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for e.ledger.numCalls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Start did not scan")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.write(t, "p/t.jsonl", line("m2", "r2", 2), t0.Add(-time.Hour))
	for e.ledger.numCalls() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("Start did not rescan")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
