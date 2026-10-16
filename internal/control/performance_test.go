package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const perfPath = "/control/v1/analytics/performance"

// fakePerfLedger is a core.Ledger that also implements core.PerfLedger.
type fakePerfLedger struct {
	fakeLedger
	mu      sync.Mutex
	queries []core.AnalyticsQuery
	result  func(core.AnalyticsQuery) core.PerfResult
	perr    error
}

func (l *fakePerfLedger) Performance(_ context.Context, q core.AnalyticsQuery) (core.PerfResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queries = append(l.queries, q)
	if l.perr != nil {
		return core.PerfResult{}, l.perr
	}
	if l.result != nil {
		return l.result(q), nil
	}
	return core.PerfResult{}, nil
}

func (l *fakePerfLedger) last(t *testing.T) core.AnalyticsQuery {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queries) == 0 {
		t.Fatal("Performance not called")
	}
	return l.queries[len(l.queries)-1]
}

func newPerfFixture(requireAuth bool) (*fixture, *fakePerfLedger) {
	f := newFixture(requireAuth)
	pl := &fakePerfLedger{}
	deps := f.srv.deps
	deps.Ledger = pl
	f.srv = New(deps, f.srv.opts)
	f.h = f.srv.Handler()
	return f, pl
}

func TestPerformanceDefaults(t *testing.T) {
	f, pl := newPerfFixture(false)
	rec := f.do(t, "GET", perfPath, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	q := pl.last(t)
	if !q.To.Equal(t0) || !q.From.Equal(t0.Add(-7*24*time.Hour)) || q.Bucket != "day" || q.Group != "host" ||
		q.TopN != 8 || len(q.Filters) != 0 {
		t.Errorf("defaults: %+v", q)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"bucket_starts", "breakdown", "series", "filters"} {
		if doc[k] == nil {
			t.Errorf("%s is null", k)
		}
	}
	if doc["schema_version"] != float64(SchemaVersion) {
		t.Errorf("schema_version = %v", doc["schema_version"])
	}

	f.do(t, "GET", perfPath+"?range=24h", "", "")
	if q = pl.last(t); q.Bucket != "hour" || !q.From.Equal(t0.Add(-24*time.Hour)) {
		t.Errorf("24h: %+v", q)
	}
	f.do(t, "GET", perfPath+"?from=2026-09-30T00:00:00Z&to=2026-09-30T12:00:00Z&bucket=hour&group=model&top=3&filter.host=", "", "")
	q = pl.last(t)
	if q.Bucket != "hour" || q.Group != "model" || q.TopN != 3 || !q.From.Equal(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("explicit: %+v", q)
	}
	if v, ok := q.Filters["host"]; !ok || v != "" || len(q.Filters) != 1 {
		t.Errorf("filters: %#v", q.Filters)
	}
}

func TestPerformanceValidation(t *testing.T) {
	f, pl := newPerfFixture(false)
	af, _ := newAnalyticsFixture(false)
	for _, q := range []string{
		"range=1y", "from=yesterday", "from=2026-10-01T12:00:00Z", "from=2025-01-01T00:00:00Z",
		"bucket=week", "bucket=hour&range=90d", "group=provider", "top=0", "top=51", "top=x",
		"filter.provider=x", "group=host&group=model",
	} {
		rec := f.do(t, "GET", perfPath+"?"+q, "", "")
		want := af.do(t, "GET", "/control/v1/analytics?"+q, "", "")
		if rec.Code != http.StatusBadRequest || rec.Body.String() != want.Body.String() {
			t.Errorf("%s: %d %s, analytics says %s", q, rec.Code, rec.Body, want.Body)
		}
	}
	if len(pl.queries) != 0 {
		t.Errorf("ledger called for invalid input: %d", len(pl.queries))
	}
}

func TestPerformanceLedgerErrors(t *testing.T) {
	f, pl := newPerfFixture(false)
	pl.perr = errors.New("analytics: too many requests in range for performance stats (3); narrow the range or add filters")
	rec := f.do(t, "GET", perfPath, "", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "too many requests") {
		t.Errorf("row cap: %d %s", rec.Code, rec.Body)
	}
	pl.perr = errors.New("ledger: disk I/O error at /secret/path")
	rec = f.do(t, "GET", perfPath, "", "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("internal error: %d %s", rec.Code, rec.Body)
	}
}

func TestPerformanceUnavailable(t *testing.T) {
	// fakeAnalyticsLedger has Analytics but no Performance.
	f, _ := newAnalyticsFixture(false)
	rec := f.do(t, "GET", perfPath, "", "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "performance analytics unavailable") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	deps := f.srv.deps
	deps.Ledger = nil
	f.h = New(deps, f.srv.opts).Handler()
	if rec := f.do(t, "GET", perfPath, "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil ledger: %d", rec.Code)
	}
}

func TestPerformanceAuth(t *testing.T) {
	f, pl := newPerfFixture(true)
	if rec := f.do(t, "GET", perfPath, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: %d", rec.Code)
	}
	if rec := f.do(t, "GET", perfPath, "", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad key: %d", rec.Code)
	}
	if len(pl.queries) != 0 {
		t.Error("unauthorized request reached the ledger")
	}
	if rec := f.do(t, "GET", perfPath, "", secretKey); rec.Code != http.StatusOK {
		t.Errorf("good key: %d %s", rec.Code, rec.Body)
	}
}

func TestPerformanceJSONShape(t *testing.T) {
	f, pl := newPerfFixture(false)
	w := 42.5
	pl.result = func(q core.AnalyticsQuery) core.PerfResult {
		row := core.PerfRow{Key: "a", Requests: 2, StreamedRequests: 1,
			LatencyMS: &core.Dist{N: 2, Mean: 150, Min: 100, P50: 150, Max: 200}, DecodeTPSWeighted: &w}
		return core.PerfResult{
			SchemaVersion: 1, From: q.From, To: q.To, Bucket: q.Bucket, Group: q.Group,
			BucketStarts: []time.Time{q.From}, Totals: row, Breakdown: []core.PerfRow{row},
			Series: []core.PerfSeries{{Key: "a", Total: row}}, // nil Points
		}
	}
	rec := f.do(t, "GET", perfPath+"?group=model", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	var doc struct {
		Filters map[string]any `json:"filters"`
		Totals  map[string]any `json:"totals"`
		Series  []struct {
			Points []any `json:"points"`
		} `json:"series"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Filters == nil || len(doc.Series) != 1 || doc.Series[0].Points == nil {
		t.Errorf("null arrays: %s", rec.Body)
	}
	for _, k := range []string{"ttft_ms", "decode_tps", "e2e_tps", "e2e_tps_weighted"} {
		if v, ok := doc.Totals[k]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want null", k, v, ok)
		}
	}
	if doc.Totals["decode_tps_weighted"] != 42.5 || doc.Totals["latency_ms"].(map[string]any)["p50"] != float64(150) {
		t.Errorf("totals: %v", doc.Totals)
	}
}
