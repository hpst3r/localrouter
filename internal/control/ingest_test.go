package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	ingestKey = "sk-ingest-SECRET-abcdef"
	plainKey  = "sk-plain-SECRET-abcdef"
)

// dedupeLedger is an idempotent in-memory ledger that optionally supports
// core.BatchLedger.
type dedupeLedger struct {
	mu      sync.Mutex
	rows    map[string]core.RequestRecord
	batches int
	singles int
}

func (l *dedupeLedger) Record(_ context.Context, r core.RequestRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.singles++
	if _, ok := l.rows[r.ID]; !ok {
		l.rows[r.ID] = r
	}
	return nil
}
func (l *dedupeLedger) Summary(context.Context, time.Time, string) ([]core.UsageRow, error) {
	return nil, nil
}
func (l *dedupeLedger) Close() error { return nil }

func (l *dedupeLedger) snapshot() map[string]core.RequestRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]core.RequestRecord, len(l.rows))
	for k, v := range l.rows {
		out[k] = v
	}
	return out
}

type batchLedger struct{ dedupeLedger }

func (l *batchLedger) RecordBatch(_ context.Context, rs []core.RequestRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.batches++
	for _, r := range rs {
		if _, ok := l.rows[r.ID]; !ok {
			l.rows[r.ID] = r
		}
	}
	return nil
}

var errStale = errors.New("quota: snapshot is stale")

// fakeIngester stores the newest snapshot per account and doubles as the
// QuotaSource so the server's pre-check sees ingested snapshots.
type fakeIngester struct {
	mu    sync.Mutex
	snaps map[string]core.Snapshot
	calls int
}

func (f *fakeIngester) IngestSnapshot(s core.Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if cur, ok := f.snaps[s.AccountID]; ok && !s.FetchedAt.After(cur.FetchedAt) {
		return errStale
	}
	f.snaps[s.AccountID] = s
	return nil
}
func (f *fakeIngester) Latest(id string) (core.Snapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.snaps[id]
	return s, ok
}
func (f *fakeIngester) ObserveHeaders(string, http.Header) {}
func (f *fakeIngester) RequestRefresh(string, bool)        {}

type ingestFixture struct {
	h      http.Handler
	ledger core.Ledger
	rows   func() map[string]core.RequestRecord
	ing    *fakeIngester
}

func newIngestFixture(t *testing.T, batch bool, quotaPrecheck bool) *ingestFixture {
	t.Helper()
	ing := &fakeIngester{snaps: map[string]core.Snapshot{}}
	f := &ingestFixture{ing: ing}
	if batch {
		l := &batchLedger{dedupeLedger{rows: map[string]core.RequestRecord{}}}
		f.ledger, f.rows = l, l.snapshot
	} else {
		l := &dedupeLedger{rows: map[string]core.RequestRecord{}}
		f.ledger, f.rows = l, l.snapshot
	}
	deps := Deps{
		Accounts: []core.Account{
			{ID: "codex", Provider: core.ProviderCodex},
			{ID: "claude-local", Provider: core.ProviderClaude, QuotaSource: "local"},
			{ID: "claude-max", Provider: core.ProviderClaude, QuotaSource: "agent"},
		},
		Ledger:   f.ledger,
		Ingester: ing,
		Clock:    fakeClock{t0},
		Authenticate: func(b string) (core.Client, bool) {
			switch b {
			case ingestKey:
				return core.Client{Name: "agent-vm1", Class: core.ClassBackground, Ingest: true}, true
			case plainKey:
				return core.Client{Name: "hermes", Class: core.ClassInteractive}, true
			}
			return core.Client{}, false
		},
	}
	if quotaPrecheck {
		deps.Quota = ing
	}
	// RequireAuth off: ingest must still require an ingest key.
	f.h = New(deps, Options{}).Handler()
	return f
}

func (f *ingestFixture) post(t *testing.T, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	switch v := body.(type) {
	case string:
		b = []byte(v)
	default:
		var err error
		if b, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/control/v1/ingest", strings.NewReader(string(b)))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("response leaks key: %s", rec.Body.String())
	}
	return rec
}

func goodRecord(id string) core.RequestRecord {
	return core.RequestRecord{
		ID: id, StartedAt: t0.Add(-time.Minute), Model: "claude-opus", AccountID: "claude-max",
		Usage: core.Usage{InputTokens: 100, OutputTokens: 10}, UsageKnown: true,
		Client: "spoofed", Provider: "codex", Host: "other-host",
	}
}

func ingestResp(t *testing.T, rec *httptest.ResponseRecorder) core.IngestResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var r core.IngestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// MH3: no key -> 401; non-ingest key -> 403.
func TestIngestAuth(t *testing.T) {
	f := newIngestFixture(t, true, true)
	body := core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{goodRecord("a")}}
	for _, tc := range []struct {
		key  string
		want int
	}{{"", 401}, {"wrong", 401}, {plainKey, 403}} {
		if rec := f.post(t, tc.key, body); rec.Code != tc.want {
			t.Fatalf("key %q: status %d, want %d", tc.key, rec.Code, tc.want)
		}
	}
	if n := len(f.rows()); n != 0 {
		t.Fatalf("rows written without permission: %d", n)
	}
	// Nil Authenticate -> 401 as well.
	h := New(Deps{Clock: fakeClock{t0}}, Options{}).Handler()
	req := httptest.NewRequest(http.MethodPost, "/control/v1/ingest", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+ingestKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil Authenticate: %d", rec.Code)
	}
}

// MH4: Host/Client/Provider overwritten; unknown account -> 400 with nothing written.
func TestIngestOverwritesAndValidates(t *testing.T) {
	for _, batch := range []bool{true, false} {
		f := newIngestFixture(t, batch, true)
		bad := goodRecord("b")
		bad.AccountID = "codex"
		rec := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1",
			Records: []core.RequestRecord{goodRecord("a"), bad}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("batch=%v unknown account: %d", batch, rec.Code)
		}
		if n := len(f.rows()); n != 0 {
			t.Fatalf("batch=%v partial write: %d rows", batch, n)
		}

		r := ingestResp(t, f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1",
			Records: []core.RequestRecord{goodRecord("a"), goodRecord("b")}}))
		if r.RecordsAccepted != 2 || r.SchemaVersion != 1 {
			t.Fatalf("resp %+v", r)
		}
		rows := f.rows()
		if len(rows) != 2 {
			t.Fatalf("rows %d", len(rows))
		}
		for id, row := range rows {
			if row.Host != "vm1" || row.Client != "agent-vm1" || row.Provider != core.ProviderClaude || row.Route != "claude" {
				t.Fatalf("row %s not normalized: %+v", id, row)
			}
		}
		if bl, ok := f.ledger.(*batchLedger); ok && (bl.batches != 1 || bl.singles != 0) {
			t.Fatalf("batch ledger: batches=%d singles=%d", bl.batches, bl.singles)
		}
	}
}

func TestIngestRejectsInvalid(t *testing.T) {
	neg := goodRecord("n")
	neg.Usage.OutputTokens = -1
	unknown := goodRecord("u")
	unknown.UsageKnown = false
	noID := goodRecord("")
	old := goodRecord("old")
	old.StartedAt = t0.AddDate(0, 0, -401)
	backwards := goodRecord("back")
	backwards.FinishedAt = backwards.StartedAt.Add(-time.Second)
	cases := map[string]any{
		"old record":       core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{old}},
		"finished<started": core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{backwards}},
		"negative window_seconds": core.IngestRequest{SchemaVersion: 1, Host: "vm1",
			Snapshots: []core.Snapshot{{AccountID: "claude-max", FetchedAt: t0,
				Windows: []core.Window{{Kind: core.Window5h, WindowSeconds: -1}}}}},
		"bad window kind": core.IngestRequest{SchemaVersion: 1, Host: "vm1",
			Snapshots: []core.Snapshot{{AccountID: "claude-max", FetchedAt: t0,
				Windows: []core.Window{{Kind: "5H"}}}}},
		"not json":       "{",
		"schema":         core.IngestRequest{SchemaVersion: 2, Host: "vm1"},
		"empty host":     core.IngestRequest{SchemaVersion: 1},
		"bad host":       core.IngestRequest{SchemaVersion: 1, Host: "vm 1"},
		"long host":      core.IngestRequest{SchemaVersion: 1, Host: strings.Repeat("h", 65)},
		"no id":          core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{noID}},
		"usage unknown":  core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{unknown}},
		"negative usage": core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{neg}},
		"local snapshot": core.IngestRequest{SchemaVersion: 1, Host: "vm1",
			Snapshots: []core.Snapshot{{AccountID: "claude-local", FetchedAt: t0}}},
		"unknown snapshot": core.IngestRequest{SchemaVersion: 1, Host: "vm1",
			Snapshots: []core.Snapshot{{AccountID: "nope", FetchedAt: t0}}},
		"future snapshot": core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{goodRecord("a")},
			Snapshots: []core.Snapshot{{AccountID: "claude-max", FetchedAt: t0.Add(6 * time.Minute)}}},
	}
	for name, body := range cases {
		f := newIngestFixture(t, true, true)
		if rec := f.post(t, ingestKey, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", name, rec.Code)
		}
		if len(f.rows()) != 0 || f.ing.calls != 0 {
			t.Fatalf("%s: wrote rows=%d snapshots=%d", name, len(f.rows()), f.ing.calls)
		}
	}
	f := newIngestFixture(t, true, true)
	tooMany := make([]core.RequestRecord, maxIngestRecords+1)
	for i := range tooMany {
		tooMany[i] = goodRecord(strings.Repeat("x", i%7+1) + string(rune('a'+i%26)))
	}
	if rec := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: tooMany}); rec.Code != http.StatusBadRequest {
		t.Fatalf("too many records: %d", rec.Code)
	}
	huge := `{"schema_version":1,"host":"vm1","records":[],"pad":"` + strings.Repeat("x", maxIngestBody) + `"}`
	if rec := f.post(t, ingestKey, huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge body: %d", rec.Code)
	}
}

// MH5: re-sending the same batch leaves ledger rows unchanged.
func TestIngestIdempotent(t *testing.T) {
	f := newIngestFixture(t, true, true)
	body := core.IngestRequest{SchemaVersion: 1, Host: "vm1",
		Records: []core.RequestRecord{goodRecord("a"), goodRecord("b")}}
	first := ingestResp(t, f.post(t, ingestKey, body))
	before := f.rows()
	second := ingestResp(t, f.post(t, ingestKey, body))
	after := f.rows()
	if first.RecordsAccepted != 2 || second.RecordsAccepted != 2 {
		t.Fatalf("accepted %d then %d", first.RecordsAccepted, second.RecordsAccepted)
	}
	if len(before) != 2 || len(after) != 2 {
		t.Fatalf("rows %d -> %d", len(before), len(after))
	}
	for id, r := range before {
		if after[id] != r {
			t.Fatalf("row %s changed", id)
		}
	}
}

func TestIngestSnapshots(t *testing.T) {
	for _, precheck := range []bool{true, false} {
		f := newIngestFixture(t, true, precheck)
		snap := core.Snapshot{AccountID: "claude-max", FetchedAt: t0.Add(-time.Minute), Source: "usage_api",
			Windows: []core.Window{{Kind: core.Window5h, UsedFrac: 0.4}}}
		r := ingestResp(t, f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1",
			Snapshots: []core.Snapshot{snap}}))
		if r.SnapshotsAccepted != 1 || r.SnapshotsIgnored != 0 {
			t.Fatalf("precheck=%v first: %+v", precheck, r)
		}
		older := snap
		older.FetchedAt = snap.FetchedAt.Add(-time.Minute)
		// Within the 5 minute skew allowance.
		newer := snap
		newer.FetchedAt = t0.Add(4 * time.Minute)
		r = ingestResp(t, f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1",
			Snapshots: []core.Snapshot{older, snap, newer}}))
		if r.SnapshotsAccepted != 1 || r.SnapshotsIgnored != 2 {
			t.Fatalf("precheck=%v second: %+v", precheck, r)
		}
		// The skewed FetchedAt is stored capped at server now.
		if got, _ := f.ing.Latest("claude-max"); !got.FetchedAt.Equal(t0) {
			t.Fatalf("stored %v", got.FetchedAt)
		}
	}
}

func TestIngestSnapshotIngesterUnavailable(t *testing.T) {
	h := New(Deps{
		Accounts: []core.Account{{ID: "claude-max", Provider: core.ProviderClaude, QuotaSource: "agent"}},
		Clock:    fakeClock{t0},
		Authenticate: func(string) (core.Client, bool) {
			return core.Client{Name: "a", Ingest: true}, true
		},
	}, Options{}).Handler()
	body, _ := json.Marshal(core.IngestRequest{SchemaVersion: 1, Host: "vm1",
		Snapshots: []core.Snapshot{{AccountID: "claude-max", FetchedAt: t0}}})
	req := httptest.NewRequest(http.MethodPost, "/control/v1/ingest", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

// MH8 (control half): usage accepts group=host.
func TestUsageGroupHost(t *testing.T) {
	f := newFixture(false)
	rec := f.do(t, http.MethodGet, "/control/v1/usage?group=host", "", "")
	if rec.Code != http.StatusOK || f.ledger.group != "host" {
		t.Fatalf("status %d group %q", rec.Code, f.ledger.group)
	}
}
