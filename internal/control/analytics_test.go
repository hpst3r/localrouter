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

// fakeAnalyticsLedger is a core.Ledger that also implements core.AnalyticsLedger.
type fakeAnalyticsLedger struct {
	fakeLedger
	mu      sync.Mutex
	queries []core.AnalyticsQuery
	result  func(core.AnalyticsQuery) core.AnalyticsResult
	aerr    error
}

func (l *fakeAnalyticsLedger) Analytics(_ context.Context, q core.AnalyticsQuery) (core.AnalyticsResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queries = append(l.queries, q)
	if l.aerr != nil {
		return core.AnalyticsResult{}, l.aerr
	}
	if l.result != nil {
		return l.result(q), nil
	}
	return core.AnalyticsResult{}, nil
}

func (l *fakeAnalyticsLedger) last(t *testing.T) core.AnalyticsQuery {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queries) == 0 {
		t.Fatal("Analytics not called")
	}
	return l.queries[len(l.queries)-1]
}

func newAnalyticsFixture(requireAuth bool) (*fixture, *fakeAnalyticsLedger) {
	f := newFixture(requireAuth)
	al := &fakeAnalyticsLedger{}
	deps := f.srv.deps
	deps.Ledger = al
	f.srv = New(deps, f.srv.opts)
	f.h = f.srv.Handler()
	return f, al
}

func TestAnalyticsDefaults(t *testing.T) {
	f, al := newAnalyticsFixture(false)
	rec := f.do(t, "GET", "/control/v1/analytics", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	q := al.last(t)
	if !q.To.Equal(t0) || !q.From.Equal(t0.Add(-7*24*time.Hour)) {
		t.Errorf("range: from=%v to=%v", q.From, q.To)
	}
	if q.Bucket != "day" || q.Group != "host" || q.TopN != 8 || len(q.Filters) != 0 {
		t.Errorf("defaults: %+v", q)
	}
	// Empty result renders arrays/maps, never null.
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

	f.do(t, "GET", "/control/v1/analytics?range=24h", "", "")
	q = al.last(t)
	if q.Bucket != "hour" || !q.From.Equal(t0.Add(-24*time.Hour)) {
		t.Errorf("24h: bucket=%q from=%v", q.Bucket, q.From)
	}
	f.do(t, "GET", "/control/v1/analytics?range=90d&group=model&top=50", "", "")
	q = al.last(t)
	if q.Bucket != "day" || q.Group != "model" || q.TopN != 50 || !q.From.Equal(t0.Add(-90*24*time.Hour)) {
		t.Errorf("90d: %+v", q)
	}
}

func TestAnalyticsFromToAndFilters(t *testing.T) {
	f, al := newAnalyticsFixture(false)
	path := "/control/v1/analytics?from=2026-09-30T00:00:00Z&to=2026-09-30T12:00:00%2B02:00&range=90d" +
		"&bucket=hour&group=task&top=1&filter.host=&filter.model=gpt-5"
	rec := f.do(t, "GET", path, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	q := al.last(t)
	if !q.From.Equal(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)) || !q.To.Equal(time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("from/to: %v %v", q.From, q.To)
	}
	if q.Bucket != "hour" || q.Group != "task" || q.TopN != 1 {
		t.Errorf("shape: %+v", q)
	}
	if v, ok := q.Filters["host"]; !ok || v != "" || q.Filters["model"] != "gpt-5" || len(q.Filters) != 2 {
		t.Errorf("filters: %#v", q.Filters)
	}

	// from alone: to = now. to alone: from = to - range.
	f.do(t, "GET", "/control/v1/analytics?from=2026-09-01T00:00:00Z", "", "")
	q = al.last(t)
	if !q.To.Equal(t0) || !q.From.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) || q.Bucket != "day" {
		t.Errorf("from only: %+v", q)
	}
	f.do(t, "GET", "/control/v1/analytics?range=24h&to=2026-09-01T00:00:00Z", "", "")
	q = al.last(t)
	if !q.From.Equal(time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("to only: %+v", q)
	}
}

func TestAnalyticsValidation(t *testing.T) {
	f, al := newAnalyticsFixture(false)
	for _, q := range []string{
		"range=1y", "range=", "from=yesterday", "to=2026-13-01T00:00:00Z",
		"from=2026-10-02T00:00:00Z", // after now
		"from=2026-10-01T12:00:00Z", // == now
		"from=2025-01-01T00:00:00Z", // > 400 days
		"bucket=week", "bucket=hour&range=90d",
		"group=provider", "group=HOST",
		"top=0", "top=51", "top=x", "top=-1",
		"filter.provider=x", "filter.=x",
		"group=host&group=model", "filter.host=a&filter.host=b",
	} {
		rec := f.do(t, "GET", "/control/v1/analytics?"+q, "", "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d", q, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), `"message"`) {
			t.Errorf("%s: body %s", q, rec.Body)
		}
	}
	if len(al.queries) != 0 {
		t.Errorf("ledger called for invalid input: %d", len(al.queries))
	}
}

func TestAnalyticsLedgerErrors(t *testing.T) {
	f, al := newAnalyticsFixture(false)
	al.aerr = errors.New("analytics: hour buckets allow at most 31 days")
	rec := f.do(t, "GET", "/control/v1/analytics", "", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "31 days") {
		t.Errorf("validation error: %d %s", rec.Code, rec.Body)
	}
	al.aerr = errors.New("ledger: disk I/O error at /secret/path")
	rec = f.do(t, "GET", "/control/v1/analytics", "", "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("internal error: %d %s", rec.Code, rec.Body)
	}
	rec = f.do(t, "GET", "/control/v1/analytics/dimensions", "", "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("dimensions internal error: %d", rec.Code)
	}
}

func TestAnalyticsUnavailable(t *testing.T) {
	f := newFixture(false) // fakeLedger has no Analytics
	for _, p := range []string{"/control/v1/analytics", "/control/v1/analytics/dimensions"} {
		rec := f.do(t, "GET", p, "", "")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "analytics unavailable") {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body)
		}
	}
	deps := f.srv.deps
	deps.Ledger = nil
	h := New(deps, f.srv.opts).Handler()
	f.h = h
	if rec := f.do(t, "GET", "/control/v1/analytics", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil ledger: %d", rec.Code)
	}
}

func TestAnalyticsAuth(t *testing.T) {
	f, al := newAnalyticsFixture(true)
	for _, p := range []string{"/control/v1/analytics", "/control/v1/analytics/dimensions"} {
		if rec := f.do(t, "GET", p, "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s no key: %d", p, rec.Code)
		}
		if rec := f.do(t, "GET", p, "", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s bad key: %d", p, rec.Code)
		}
		if rec := f.do(t, "GET", p, "", secretKey); rec.Code != http.StatusOK {
			t.Errorf("%s good key: %d %s", p, rec.Code, rec.Body)
		}
	}
	if len(al.queries) == 0 {
		t.Error("authorized requests did not reach the ledger")
	}
}

func TestAnalyticsPassesResult(t *testing.T) {
	f, al := newAnalyticsFixture(false)
	cost := 0.5
	al.result = func(q core.AnalyticsQuery) core.AnalyticsResult {
		row := core.UsageRow{Key: "a", Requests: 2, InputTokens: 10, OutputTokens: 5, CostUSD: &cost}
		return core.AnalyticsResult{
			SchemaVersion: 1, From: q.From, To: q.To, Bucket: q.Bucket, Group: q.Group, Filters: q.Filters,
			BucketStarts: []time.Time{q.From}, Totals: row, Breakdown: []core.UsageRow{row},
			Series: []core.AnalyticsSeries{{Key: "a", Total: row, Points: []core.UsageRow{row}}},
		}
	}
	rec := f.do(t, "GET", "/control/v1/analytics?group=model&filter.host=pf3", "", "")
	var res core.AnalyticsResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Group != "model" || res.Filters["host"] != "pf3" || len(res.Series) != 1 || len(res.Series[0].Points) != 1 ||
		res.Totals.Requests != 2 || *res.Totals.CostUSD != 0.5 {
		t.Errorf("result: %+v", res)
	}
}

func TestAnalyticsDimensions(t *testing.T) {
	f, al := newAnalyticsFixture(false)
	al.result = func(q core.AnalyticsQuery) core.AnalyticsResult {
		var rows []core.UsageRow
		if q.Group == "host" {
			for i := range 60 {
				rows = append(rows, core.UsageRow{Key: string(rune('A' + i)), Requests: int64(i), InputTokens: 100, OutputTokens: int64(i)})
			}
		}
		if q.Group == "agent" {
			rows = []core.UsageRow{{Key: "", Requests: 3, InputTokens: 7, CachedInputTokens: 99, OutputTokens: 1}}
		}
		return core.AnalyticsResult{Breakdown: rows}
	}
	rec := f.do(t, "GET", "/control/v1/analytics/dimensions?filter.model=gpt-5", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if len(al.queries) != len(core.AnalyticsDimensions) {
		t.Fatalf("ledger calls = %d", len(al.queries))
	}
	for i, q := range al.queries {
		if q.Group != core.AnalyticsDimensions[i] || q.Bucket != "day" || q.TopN != 50 ||
			!q.To.Equal(t0) || !q.From.Equal(t0.Add(-30*24*time.Hour)) || q.Filters["model"] != "gpt-5" {
			t.Errorf("query %d: %+v", i, q)
		}
	}
	var doc struct {
		SchemaVersion int                         `json:"schema_version"`
		Dimensions    map[string][]dimensionValue `json:"dimensions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != SchemaVersion || len(doc.Dimensions) != len(core.AnalyticsDimensions) {
		t.Fatalf("doc: %+v", doc)
	}
	if len(doc.Dimensions["host"]) != 50 || doc.Dimensions["host"][3] != (dimensionValue{Key: "D", Requests: 3, Tokens: 103}) {
		t.Errorf("host: %d %+v", len(doc.Dimensions["host"]), doc.Dimensions["host"][3])
	}
	if got := doc.Dimensions["agent"]; len(got) != 1 || got[0] != (dimensionValue{Key: "", Requests: 3, Tokens: 8}) {
		t.Errorf("agent: %+v", got)
	}
	if !strings.Contains(rec.Body.String(), `"model":[]`) {
		t.Errorf("empty dimension should be []: %s", rec.Body)
	}

	for _, q := range []string{"range=1y", "group=host", "bucket=day", "top=5", "filter.nope=x"} {
		if rec := f.do(t, "GET", "/control/v1/analytics/dimensions?"+q, "", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
}

func TestUsageNewGroups(t *testing.T) {
	f := newFixture(false)
	for _, g := range []string{"route", "task", "agent"} {
		rec := f.do(t, "GET", "/control/v1/usage?group="+g, "", "")
		if rec.Code != http.StatusOK || f.ledger.group != g {
			t.Errorf("%s: %d group=%q", g, rec.Code, f.ledger.group)
		}
	}
}
