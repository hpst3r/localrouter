package ledger

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// pr returns an eligible proxied record: 200, latency lat ms, optional TTFT.
func pr(ts time.Time, m string, lat int64, ttft *int64) core.RequestRecord {
	return core.RequestRecord{StartedAt: ts, Model: m, Status: 200, LatencyMS: lat, TTFTMS: ttft}
}

// streamed returns an eligible streamed record with known usage.
func streamed(ts time.Time, m string, lat, ttft, out, reasoning int64, firstReasoning bool) core.RequestRecord {
	r := pr(ts, m, lat, &ttft)
	r.Streamed, r.FirstTokenReasoning, r.UsageKnown = true, firstReasoning, true
	r.Usage = core.Usage{OutputTokens: out, ReasoningTokens: reasoning}
	return r
}

func i64(v int64) *int64 { return &v }

func mustPerf(t *testing.T, l *Ledger, q core.AnalyticsQuery, loc *time.Location) core.PerfResult {
	t.Helper()
	res, err := l.performance(context.Background(), q, loc)
	if err != nil {
		t.Fatal(err)
	}
	checkPerfInvariants(t, res)
	return res
}

// checkPerfInvariants verifies point counts, request sums and ranking.
func checkPerfInvariants(t *testing.T, res core.PerfResult) {
	t.Helper()
	var sum int64
	for _, s := range res.Series {
		if len(s.Points) != len(res.BucketStarts) {
			t.Fatalf("series %q has %d points, want %d", s.Key, len(s.Points), len(res.BucketStarts))
		}
		var st int64
		for _, p := range s.Points {
			if p.Key != "" {
				t.Fatalf("point key %q", p.Key)
			}
			if p.Requests == 0 && (p.LatencyMS != nil || p.TTFTMS != nil || p.DecodeTPS != nil || p.E2ETPS != nil ||
				p.DecodeTPSWeighted != nil || p.E2ETPSWeighted != nil) {
				t.Fatalf("empty point has distributions: %+v", p)
			}
			st += p.Requests
		}
		if st != s.Total.Requests || s.Key != s.Total.Key {
			t.Fatalf("series %q points sum %d != total %+v", s.Key, st, s.Total)
		}
		sum += st
	}
	if sum != res.Totals.Requests {
		t.Fatalf("series sum %d != totals %d", sum, res.Totals.Requests)
	}
	for i := 1; i < len(res.Breakdown); i++ {
		p, b := res.Breakdown[i-1], res.Breakdown[i]
		if p.Requests < b.Requests || (p.Requests == b.Requests && p.Key >= b.Key) {
			t.Fatalf("breakdown not ranked at %d: %q then %q", i, p.Key, b.Key)
		}
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPerfDistOf(t *testing.T) {
	if distOf(nil) != nil || distOf([]float64{}) != nil {
		t.Fatal("empty dist not nil")
	}
	s := make([]float64, 100)
	for i := range s {
		s[99-i] = float64(i + 1) // unsorted input
	}
	d := distOf(s)
	want := core.Dist{N: 100, Mean: 50.5, Min: 1, P01: 1.99, P05: 5.95, P10: 10.9, P50: 50.5, P90: 90.1, P95: 95.05, P99: 99.01, Max: 100}
	got := []float64{d.Mean, d.Min, d.P01, d.P05, d.P10, d.P50, d.P90, d.P95, d.P99, d.Max}
	exp := []float64{want.Mean, want.Min, want.P01, want.P05, want.P10, want.P50, want.P90, want.P95, want.P99, want.Max}
	for i := range got {
		if !near(got[i], exp[i]) || d.N != 100 {
			t.Fatalf("dist = %+v, want %+v", *d, want)
		}
	}
	if d := distOf([]float64{7}); *d != (core.Dist{N: 1, Mean: 7, Min: 7, P01: 7, P05: 7, P10: 7, P50: 7, P90: 7, P95: 7, P99: 7, Max: 7}) {
		t.Fatalf("single = %+v", *d)
	}
	d = distOf([]float64{30, 10})
	if d.N != 2 || d.Min != 10 || d.Max != 30 || d.Mean != 20 || !near(d.P50, 20) || !near(d.P01, 10.2) || !near(d.P99, 29.8) {
		t.Fatalf("two = %+v", *d)
	}
}

func TestPerfEligibility(t *testing.T) {
	l, _ := openTest(t, nil)
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	imported := pr(ts, "m", 0, nil)
	failed := pr(ts, "m", 100, nil)
	failed.Status = 500
	errored := pr(ts, "m", 100, i64(10))
	errored.Error = "upstream stream ended without completion"
	redirect := pr(ts, "m", 100, nil)
	redirect.Status = 302
	ok := pr(ts, "m", 250, nil)
	ok299 := pr(ts, "m", 50, nil)
	ok299.Status = 299
	record(t, l, imported, failed, errored, redirect, ok, ok299)
	res := mustPerf(t, l, core.AnalyticsQuery{From: ts, To: ts.Add(time.Hour), Bucket: "hour", Group: "model"}, time.UTC)
	if res.Totals.Requests != 2 || res.Totals.LatencyMS.N != 2 || res.Totals.LatencyMS.Min != 50 || res.Totals.LatencyMS.Max != 250 {
		t.Fatalf("totals = %+v %+v", res.Totals, res.Totals.LatencyMS)
	}
	if res.Totals.TTFTMS != nil || res.Totals.DecodeTPS != nil || res.Totals.E2ETPS != nil ||
		res.Totals.DecodeTPSWeighted != nil || res.Totals.E2ETPSWeighted != nil {
		t.Fatalf("no-sample metrics not nil: %+v", res.Totals)
	}
	if res.SchemaVersion != 1 || len(res.Series) != 1 || res.Series[0].Key != "m" || len(res.Breakdown) != 1 {
		t.Fatalf("shape: %+v", res)
	}
}

func TestPerfFormulas(t *testing.T) {
	l, _ := openTest(t, nil)
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	unknown := streamed(ts, "m", 2000, 300, 999, 0, false)
	unknown.UsageKnown = false
	nonStream := pr(ts, "m", 1000, nil)
	nonStream.UsageKnown, nonStream.Usage.OutputTokens = true, 50
	record(t, l,
		// Visible first token: D = 101 - 1 = 100 -> 99 tokens over 1000 ms.
		streamed(ts, "m", 1500, 500, 101, 1, false),
		// Reasoning first token: D = output = 21 -> 20 tokens over 400 ms.
		streamed(ts, "m", 500, 100, 21, 10, true),
		// D = 11 - 10 = 1 < 2: no decode sample, but e2e (11 tokens).
		streamed(ts, "m", 1000, 100, 11, 10, false),
		// G = 0 < 1: no decode sample.
		streamed(ts, "m", 400, 400, 50, 0, false),
		// Unknown usage: latency and TTFT only.
		unknown,
		// Non-streamed with usage: latency and e2e only.
		nonStream,
	)
	res := mustPerf(t, l, core.AnalyticsQuery{From: ts, To: ts.Add(time.Hour), Bucket: "hour", Group: "model"}, time.UTC)
	tot := res.Totals
	if tot.Requests != 6 || tot.StreamedRequests != 5 || tot.LatencyMS.N != 6 || tot.TTFTMS.N != 5 {
		t.Fatalf("counts: %+v latency=%+v ttft=%+v", tot, tot.LatencyMS, tot.TTFTMS)
	}
	if d := tot.DecodeTPS; d == nil || d.N != 2 || !near(d.Min, 50) || !near(d.Max, 99) {
		t.Fatalf("decode = %+v", d)
	}
	if w := tot.DecodeTPSWeighted; w == nil || !near(*w, (99.0+20)*1000/(1000+400)) {
		t.Fatalf("decode weighted = %v", w)
	}
	// e2e samples: 101/1.5s, 21/0.5s, 11/1s, 50/0.4s, 50/1s.
	e := tot.E2ETPS
	if e == nil || e.N != 5 || !near(e.Min, 11) || !near(e.Max, 125) || !near(e.P50, 50) {
		t.Fatalf("e2e = %+v", e)
	}
	if w := tot.E2ETPSWeighted; w == nil || !near(*w, (101.0+21+11+50+50)*1000/(1500+500+1000+400+1000)) {
		t.Fatalf("e2e weighted = %v", w)
	}
}

func TestPerfTopOtherPooled(t *testing.T) {
	l, _ := openTest(t, nil)
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	var rs []core.RequestRecord
	add := func(m string, lats ...int64) {
		for _, v := range lats {
			rs = append(rs, pr(ts, m, v, i64(v/2)))
		}
	}
	add("a", 1, 2, 3, 4, 5) // top: 5 requests
	add("b", 10, 20, 30)    // other: p50 20
	add("c", 100, 200, 300) // other (tie with b, ranked after): p50 200
	add("d", 1000)          // other
	rs[0].Host = "x"        // filter coverage below
	record(t, l, rs...)
	q := core.AnalyticsQuery{From: ts, To: ts.Add(time.Hour), Bucket: "hour", Group: "model", TopN: 1}
	res := mustPerf(t, l, q, time.UTC)
	keys := []string{}
	for _, b := range res.Breakdown {
		keys = append(keys, b.Key)
	}
	if !reflect.DeepEqual(keys, []string{"a", "b", "c", "d"}) || res.BreakdownOmitted != 0 {
		t.Fatalf("breakdown keys = %v", keys)
	}
	if len(res.Series) != 2 || res.Series[0].Key != "a" || res.Series[1].Key != core.AnalyticsOtherKey {
		t.Fatalf("series = %+v", res.Series)
	}
	other := res.Series[1].Total
	// Pooled {10,20,30,100,200,300,1000}: p50 = 100; the average of the
	// members' medians would be (20+200+1000)/3.
	if other.Requests != 7 || other.LatencyMS.P50 != 100 || other.TTFTMS.P50 != 50 || res.Series[1].Points[0].LatencyMS.P50 != 100 {
		t.Fatalf("other = %+v %+v", other, other.LatencyMS)
	}
	if res.Breakdown[1].LatencyMS.P50 != 20 || res.Breakdown[2].LatencyMS.P50 != 200 || res.Breakdown[3].LatencyMS.N != 1 {
		t.Fatalf("breakdown dists: %+v %+v", res.Breakdown[1].LatencyMS, res.Breakdown[2].LatencyMS)
	}
	if res.Totals.LatencyMS.N != 12 || res.Totals.LatencyMS.Max != 1000 || !near(res.Totals.LatencyMS.P50, 15) {
		t.Fatalf("totals = %+v", res.Totals.LatencyMS)
	}
	if !reflect.DeepEqual(res.Breakdown[0], res.Series[0].Total) {
		t.Fatalf("top breakdown %+v != series total %+v", res.Breakdown[0], res.Series[0].Total)
	}

	// Filters: an empty value matches empty columns; values AND together.
	q.Filters = map[string]string{"host": ""}
	res = mustPerf(t, l, q, time.UTC)
	if res.Totals.Requests != 11 || res.Filters["host"] != "" || len(res.Filters) != 1 {
		t.Fatalf("empty filter: %+v", res.Totals)
	}
	q.Filters = map[string]string{"host": "x", "model": "a"}
	if res = mustPerf(t, l, q, time.UTC); res.Totals.Requests != 1 || len(res.Series) != 1 {
		t.Fatalf("filters: %+v", res)
	}
	q.Filters = map[string]string{"host": "x", "model": "b"}
	if res = mustPerf(t, l, q, time.UTC); res.Totals.Requests != 0 || len(res.Series) != 0 || res.Totals.LatencyMS != nil {
		t.Fatalf("no match: %+v", res)
	}
	// No other keys: __other__ omitted.
	q.Filters, q.TopN = nil, 8
	if res = mustPerf(t, l, q, time.UTC); len(res.Series) != 4 || res.Series[3].Key != "d" {
		t.Fatalf("top 8: %+v", res.Series)
	}
}

func TestPerfGroupEveryDimension(t *testing.T) {
	l, _ := openTest(t, nil)
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	r := pr(ts, "mod", 10, nil)
	r.Host, r.AccountID, r.Client, r.Class, r.Route, r.Task, r.Agent = "hst", "acct", "cli", "background", "rte", "tsk", "agt"
	record(t, l, r)
	want := map[string]string{"host": "hst", "account": "acct", "model": "mod", "client": "cli",
		"class": "background", "route": "rte", "task": "tsk", "agent": "agt"}
	for _, dim := range core.AnalyticsDimensions {
		res := mustPerf(t, l, core.AnalyticsQuery{From: ts, To: ts.Add(time.Hour), Bucket: "hour", Group: dim}, time.UTC)
		if len(res.Series) != 1 || res.Series[0].Key != want[dim] || res.Group != dim {
			t.Errorf("%s: %+v", dim, res.Series)
		}
	}
}

func TestPerfBreakdownCap(t *testing.T) {
	l, _ := openTest(t, nil)
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	n := core.AnalyticsMaxBreakdown + 5
	var rs []core.RequestRecord
	for i := range n {
		for range 1 + i%3 { // ranking by requests, many ties broken by key
			rs = append(rs, pr(ts.Add(time.Duration(i)*time.Second), fmt.Sprintf("k%03d", i), int64(i+1), nil))
		}
	}
	record(t, l, rs...)
	res := mustPerf(t, l, core.AnalyticsQuery{From: ts, To: ts.Add(time.Hour), Bucket: "hour", Group: "model", TopN: 3}, time.UTC)
	if len(res.Breakdown) != core.AnalyticsMaxBreakdown || res.BreakdownOmitted != 5 {
		t.Fatalf("breakdown %d omitted %d", len(res.Breakdown), res.BreakdownOmitted)
	}
	if res.Breakdown[0].Key != "k002" || res.Breakdown[1].Key != "k005" || res.Series[2].Key != "k008" {
		t.Fatalf("ranking: %q %q %q", res.Breakdown[0].Key, res.Breakdown[1].Key, res.Series[2].Key)
	}
	if res.Totals.Requests != int64(len(rs)) || len(res.Series) != 4 || res.Series[3].Total.Requests != int64(len(rs))-9 {
		t.Fatalf("totals %d series %d other %+v", res.Totals.Requests, len(res.Series), res.Series[3].Total)
	}
	var bsum int64
	for _, b := range res.Breakdown {
		bsum += b.Requests
		if b.LatencyMS == nil || b.LatencyMS.N != b.Requests {
			t.Fatalf("breakdown %q latency %+v", b.Key, b.LatencyMS)
		}
	}
	if bsum >= res.Totals.Requests {
		t.Fatalf("omitted keys missing from totals: %d vs %d", bsum, res.Totals.Requests)
	}
}

// TestPerfBucketsMatchAnalytics checks that the same query yields the same
// bucket starts and per-point requests as Analytics (hour and DST days).
func TestPerfBucketsMatchAnalytics(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	l, _ := openTest(t, nil)
	d := func(m time.Month, day, hh, mm int) time.Time { return time.Date(2026, m, day, hh, mm, 0, 0, ny) }
	var rs []core.RequestRecord
	for _, ts := range []time.Time{
		d(3, 7, 23, 59), d(3, 8, 0, 0), d(3, 8, 1, 59), d(3, 8, 3, 0), d(3, 8, 23, 59), d(3, 9, 0, 0),
		d(11, 1, 0, 30), d(11, 1, 1, 30), d(11, 1, 23, 0), d(11, 2, 0, 0),
	} {
		for _, m := range []string{"a", "b"} {
			r := pr(ts, m, 100, nil)
			r.Usage = core.Usage{InputTokens: 1}
			rs = append(rs, r)
		}
	}
	record(t, l, rs...)
	for _, q := range []core.AnalyticsQuery{
		{From: d(3, 7, 12, 0), To: d(3, 9, 12, 0), Bucket: "day", Group: "model", TopN: 1},
		{From: d(10, 31, 12, 0), To: d(11, 2, 1, 0), Bucket: "day", Group: "model", TopN: 1},
		{From: d(3, 7, 23, 30), To: d(3, 8, 4, 0), Bucket: "hour", Group: "model", TopN: 1},
		{From: d(11, 1, 0, 0), To: d(11, 1, 3, 0), Bucket: "hour", Group: "model", TopN: 1},
	} {
		a := mustAnalytics(t, l, q, ny)
		p := mustPerf(t, l, q, ny)
		if !reflect.DeepEqual(a.BucketStarts, p.BucketStarts) || len(a.Series) != len(p.Series) {
			t.Fatalf("%+v: starts %v vs %v", q, a.BucketStarts, p.BucketStarts)
		}
		for i := range a.Series {
			for j := range a.Series[i].Points {
				if a.Series[i].Key != p.Series[i].Key || a.Series[i].Points[j].Requests != p.Series[i].Points[j].Requests {
					t.Fatalf("%+v: series %d point %d: %d vs %d", q, i, j, a.Series[i].Points[j].Requests, p.Series[i].Points[j].Requests)
				}
			}
		}
	}
	// The 23h spring-forward day is one bucket holding its 4 rows of "a".
	p := mustPerf(t, l, core.AnalyticsQuery{From: d(3, 8, 0, 0), To: d(3, 9, 0, 0), Bucket: "day", Group: "model", TopN: 1}, ny)
	if len(p.BucketStarts) != 1 || p.Series[0].Points[0].Requests != 4 {
		t.Fatalf("DST day: %v %+v", p.BucketStarts, p.Series[0].Points)
	}
}

func TestPerfValidationMatchesAnalytics(t *testing.T) {
	l, _ := openTest(t, nil)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	ok := core.AnalyticsQuery{From: now.Add(-24 * time.Hour), To: now, Bucket: "hour", Group: "host"}
	for name, mut := range map[string]func(q *core.AnalyticsQuery){
		"bad group":    func(q *core.AnalyticsQuery) { q.Group = "day" },
		"bad filter":   func(q *core.AnalyticsQuery) { q.Filters = map[string]string{"session": "x"} },
		"bad bucket":   func(q *core.AnalyticsQuery) { q.Bucket = "week" },
		"from == to":   func(q *core.AnalyticsQuery) { q.From = q.To },
		"span > 400d":  func(q *core.AnalyticsQuery) { q.Bucket = "day"; q.From = q.To.Add(-400*24*time.Hour - 1) },
		"hourly > 31d": func(q *core.AnalyticsQuery) { q.From = q.To.Add(-31*24*time.Hour - 1) },
		"top > 50":     func(q *core.AnalyticsQuery) { q.TopN = 51 },
		"top negative": func(q *core.AnalyticsQuery) { q.TopN = -1 },
		"sql inject":   func(q *core.AnalyticsQuery) { q.Group = "host; DROP TABLE requests" },
	} {
		q := ok
		mut(&q)
		_, aerr := l.Analytics(context.Background(), q)
		_, perr := l.Performance(context.Background(), q)
		if aerr == nil || perr == nil || aerr.Error() != perr.Error() {
			t.Errorf("%s: analytics %v, performance %v", name, aerr, perr)
		}
	}
	if _, err := l.Performance(context.Background(), ok); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if _, err := l.Performance(context.Background(), ok); err == nil || strings.HasPrefix(err.Error(), "analytics: ") {
		t.Errorf("closed db err = %v", err)
	}
}

func TestPerfRowCap(t *testing.T) {
	l, _ := openTest(t, nil)
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	record(t, l, pr(ts, "a", 1, nil), pr(ts, "b", 1, nil), pr(ts, "c", 1, nil), pr(ts, "c", 0, nil))
	defer func(v int64) { perfMaxRows = v }(perfMaxRows)
	perfMaxRows = 3
	q := core.AnalyticsQuery{From: ts, To: ts.Add(time.Hour), Bucket: "hour", Group: "model"}
	if _, err := l.performance(context.Background(), q, time.UTC); err != nil {
		t.Fatalf("at cap (ineligible row not counted): %v", err)
	}
	perfMaxRows = 2
	_, err := l.performance(context.Background(), q, time.UTC)
	if err == nil || err.Error() != "analytics: too many requests in range for performance stats (3); narrow the range or add filters" {
		t.Fatalf("err = %v", err)
	}
}
