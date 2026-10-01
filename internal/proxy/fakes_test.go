package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	clientKey  = "sk-client-SENTINEL-7f3a"
	bgKey      = "sk-bg-SENTINEL-9c1d"
	tokenStem  = "upstream-token-SENTINEL-"
	testReason = "background reserve protected"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var testNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// fakeLease records its outcome.
type fakeLease struct {
	id       string
	pol      *fakePolicy
	mu       sync.Mutex
	released int
	outcome  core.Outcome
}

func (l *fakeLease) AccountID() string { return l.id }
func (l *fakeLease) Release(o core.Outcome) {
	l.mu.Lock()
	l.released++
	l.outcome = o
	l.mu.Unlock()
	l.pol.releasedCh <- l
}

// fakePolicy admits the first candidate not excluded and not in deny.
type fakePolicy struct {
	mu         sync.Mutex
	deny       map[string]bool
	calls      []core.Class
	leases     []*fakeLease
	releasedCh chan *fakeLease
}

func newFakePolicy() *fakePolicy {
	return &fakePolicy{deny: map[string]bool{}, releasedCh: make(chan *fakeLease, 64)}
}

func (p *fakePolicy) Acquire(class core.Class, cands []string, exclude map[string]bool) (core.Lease, core.Decision) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, class)
	for _, c := range cands {
		if exclude[c] || p.deny[c] {
			continue
		}
		l := &fakeLease{id: c, pol: p}
		p.leases = append(p.leases, l)
		return l, core.Decision{Allow: true, AccountID: c, Reason: "ok"}
	}
	return nil, core.Decision{Reason: testReason}
}
func (p *fakePolicy) DryRun(core.Class, []string) core.Decision { return core.Decision{} }
func (p *fakePolicy) Status(string) core.AccountState           { return core.AccountState{} }

func (p *fakePolicy) classes() []core.Class {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]core.Class(nil), p.calls...)
}

// fakeCreds issues a new token generation after each Invalidate.
type fakeCreds struct {
	mu          sync.Mutex
	gen         map[string]int
	invalidated map[string]int
	fail        map[string]bool
}

func newFakeCreds() *fakeCreds {
	return &fakeCreds{gen: map[string]int{}, invalidated: map[string]int{}, fail: map[string]bool{}}
}

func (c *fakeCreds) Credential(_ context.Context, id string) (core.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail[id] {
		return core.Credential{}, errors.New("no token for account")
	}
	h := http.Header{}
	h.Set("Authorization", fmt.Sprintf("Bearer %s%s-%d", tokenStem, id, c.gen[id]))
	h.Set("ChatGPT-Account-Id", "ident-"+id)
	return core.Credential{Headers: h, Identity: "ident-" + id}, nil
}

func (c *fakeCreds) Invalidate(id string) {
	c.mu.Lock()
	c.gen[id]++
	c.invalidated[id]++
	c.mu.Unlock()
}

type fakeQuota struct {
	mu       sync.Mutex
	observed map[string]int
	refresh  map[string]int
}

func newFakeQuota() *fakeQuota {
	return &fakeQuota{observed: map[string]int{}, refresh: map[string]int{}}
}
func (q *fakeQuota) Latest(string) (core.Snapshot, bool) { return core.Snapshot{}, false }
func (q *fakeQuota) ObserveHeaders(id string, _ http.Header) {
	q.mu.Lock()
	q.observed[id]++
	q.mu.Unlock()
}
func (q *fakeQuota) RequestRefresh(id string, _ bool) {
	q.mu.Lock()
	q.refresh[id]++
	q.mu.Unlock()
}

type fakeLedger struct {
	mu   sync.Mutex
	rows []core.RequestRecord
	ch   chan core.RequestRecord
	err  error
}

func newFakeLedger() *fakeLedger { return &fakeLedger{ch: make(chan core.RequestRecord, 64)} }

func (l *fakeLedger) Record(_ context.Context, r core.RequestRecord) error {
	l.mu.Lock()
	l.rows = append(l.rows, r)
	l.mu.Unlock()
	l.ch <- r
	return l.err
}
func (l *fakeLedger) Summary(context.Context, time.Time, string) ([]core.UsageRow, error) {
	return nil, nil
}
func (l *fakeLedger) Close() error { return nil }

func (l *fakeLedger) all() []core.RequestRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]core.RequestRecord(nil), l.rows...)
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness wires a Proxy to fakes and per-account upstream servers.
type harness struct {
	t        *testing.T
	policy   *fakePolicy
	creds    *fakeCreds
	quota    *fakeQuota
	ledger   *fakeLedger
	logs     *syncBuffer
	accounts map[string]core.Account
	routes   []core.Route
	opts     Options
	srv      *httptest.Server
}

func newHarness(t *testing.T) *harness {
	return &harness{
		t: t, policy: newFakePolicy(), creds: newFakeCreds(), quota: newFakeQuota(),
		ledger: newFakeLedger(), logs: &syncBuffer{}, accounts: map[string]core.Account{},
	}
}

// upstream registers an account served by handler and returns its server.
func (h *harness) upstream(id, provider string, handler http.HandlerFunc) *httptest.Server {
	s := httptest.NewServer(handler)
	h.t.Cleanup(s.Close)
	h.accounts[id] = core.Account{ID: id, Provider: provider, BaseURL: s.URL + "/v1"}
	return s
}

func (h *harness) start() {
	p := New(Deps{
		Accounts: h.accounts, Routes: h.routes, Creds: h.creds, Quota: h.quota,
		Policy: h.policy, Ledger: h.ledger, Clock: fixedClock{testNow},
		Logger: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Authenticate: func(bearer string) (core.Client, bool) {
			switch bearer {
			case clientKey:
				return core.Client{Name: "alice", Class: core.ClassInteractive}, true
			case bgKey:
				return core.Client{Name: "batch", Class: core.ClassBackground}, true
			}
			return core.Client{}, false
		},
	}, h.opts)
	h.srv = httptest.NewServer(p.Handler())
	h.t.Cleanup(h.srv.Close)
}

func (h *harness) post(path, key, body string, hdr map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, bytes.NewBufferString(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// waitRows waits until n ledger rows have been recorded.
func (h *harness) waitRows(n int) []core.RequestRecord {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for len(h.ledger.all()) < n {
		select {
		case <-h.ledger.ch:
		case <-deadline:
			h.t.Fatalf("timed out waiting for %d ledger rows, have %d", n, len(h.ledger.all()))
		}
	}
	return h.ledger.all()
}
