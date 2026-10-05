package ledger

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // deterministic zone data for DST tests

	"github.com/hpst3r/localrouter/internal/core"
)

func loadLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func record(t *testing.T, l *Ledger, rs ...core.RequestRecord) {
	t.Helper()
	if err := l.RecordBatch(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
}

// at returns a known-usage record for model m at time ts with in/out tokens.
func at(ts time.Time, m string, in, out int64) core.RequestRecord {
	return core.RequestRecord{StartedAt: ts, Model: m, UsageKnown: true,
		Usage: core.Usage{InputTokens: in, OutputTokens: out}}
}

func mustAnalytics(t *testing.T, l *Ledger, q core.AnalyticsQuery, loc *time.Location) core.AnalyticsResult {
	t.Helper()
	res, err := l.analytics(context.Background(), q, loc)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, res)
	return res
}

// checkInvariants verifies point counts, per-point series sums == Totals and
// breakdown ranking for any result.
func checkInvariants(t *testing.T, res core.AnalyticsResult) {
	t.Helper()
	var sum core.UsageRow
	for _, s := range res.Series {
		if len(s.Points) != len(res.BucketStarts) {
			t.Fatalf("series %q has %d points, want %d", s.Key, len(s.Points), len(res.BucketStarts))
		}
		var st core.UsageRow
		for _, p := range s.Points {
			if p.Key != "" {
				t.Fatalf("point key %q", p.Key)
			}
			addRow(&st, p)
		}
		st.Key = s.Key
		if !rowsEqual(st, s.Total) {
			t.Fatalf("series %q points sum %+v != total %+v", s.Key, st, s.Total)
		}
		addRow(&sum, s.Total)
	}
	if !rowsEqual(sum, res.Totals) {
		t.Fatalf("series sum %+v != totals %+v", sum, res.Totals)
	}
	var bsum core.UsageRow
	for i, b := range res.Breakdown {
		addRow(&bsum, b)
		if i > 0 {
			p := res.Breakdown[i-1]
			if rankTokens(p) < rankTokens(b) || (rankTokens(p) == rankTokens(b) && p.Key >= b.Key) {
				t.Fatalf("breakdown not ranked at %d: %q then %q", i, p.Key, b.Key)
			}
		}
	}
	if !rowsEqual(bsum, res.Totals) {
		t.Fatalf("breakdown sum %+v != totals %+v", bsum, res.Totals)
	}
}

func rowsEqual(a, b core.UsageRow) bool {
	if (a.CostUSD == nil) != (b.CostUSD == nil) {
		return false
	}
	if a.CostUSD != nil && !approx(*a.CostUSD, *b.CostUSD) {
		return false
	}
	a.CostUSD, b.CostUSD = nil, nil
	return a == b
}

func TestAnalyticsHourBuckets(t *testing.T) {
	l, _ := openTest(t, nil)
	h := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	record(t, l,
		at(h.Add(29*time.Minute), "m", 1, 0), // before From
		at(h.Add(30*time.Minute), "m", 2, 0),
		at(h.Add(2*time.Hour-time.Millisecond), "m", 4, 0),
		at(h.Add(2*time.Hour), "m", 8, 0),
		at(h.Add(3*time.Hour), "m", 16, 0), // == To, excluded
	)
	// A non-UTC From still yields UTC-aligned hours.
	from := h.Add(30 * time.Minute).In(loadLoc(t, "Asia/Kolkata"))
	res := mustAnalytics(t, l, core.AnalyticsQuery{From: from, To: h.Add(3 * time.Hour),
		Bucket: "hour", Group: "model"}, time.UTC)
	want := []time.Time{h, h.Add(time.Hour), h.Add(2 * time.Hour)}
	if !reflect.DeepEqual(res.BucketStarts, want) {
		t.Fatalf("starts = %v", res.BucketStarts)
	}
	got := []int64{}
	for _, p := range res.Series[0].Points {
		got = append(got, p.InputTokens)
	}
	if !reflect.DeepEqual(got, []int64{2, 4, 8}) || res.Totals.Requests != 3 {
		t.Fatalf("points = %v totals = %+v", got, res.Totals)
	}
	// To one ns past an hour boundary opens one more bucket.
	res = mustAnalytics(t, l, core.AnalyticsQuery{From: h, To: h.Add(3*time.Hour + time.Nanosecond),
		Bucket: "hour", Group: "model"}, time.UTC)
	if len(res.BucketStarts) != 4 {
		t.Fatalf("starts = %v", res.BucketStarts)
	}
}

func TestAnalyticsDayBucketsDST(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	l, _ := openTest(t, nil)
	d := func(m time.Month, day, hh, mm int) time.Time { return time.Date(2026, m, day, hh, mm, 0, 0, ny) }

	// Spring forward: 2026-03-08 is 23h long.
	record(t, l,
		at(d(3, 7, 23, 59), "m", 1, 0),
		at(d(3, 8, 0, 0), "m", 2, 0),
		at(d(3, 8, 3, 30), "m", 4, 0),
		at(d(3, 8, 23, 59), "m", 8, 0),
		at(d(3, 9, 0, 0), "m", 16, 0),
	)
	res := mustAnalytics(t, l, core.AnalyticsQuery{From: d(3, 7, 12, 0), To: d(3, 10, 0, 0),
		Bucket: "day", Group: "model"}, ny)
	want := []time.Time{d(3, 7, 0, 0), d(3, 8, 0, 0), d(3, 9, 0, 0)}
	if !reflect.DeepEqual(res.BucketStarts, want) {
		t.Fatalf("starts = %v", res.BucketStarts)
	}
	if got := res.BucketStarts[2].Sub(res.BucketStarts[1]); got != 23*time.Hour {
		t.Fatalf("Mar 8 length = %v", got)
	}
	var got []int64
	for _, p := range res.Series[0].Points {
		got = append(got, p.InputTokens)
	}
	if !reflect.DeepEqual(got, []int64{1, 14, 16}) {
		t.Fatalf("points = %v", got)
	}

	// Fall back: 2026-11-01 is 25h long and 01:30 happens twice.
	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)  // 01:30 EDT
	second := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC) // 01:30 EST
	record(t, l,
		at(d(11, 1, 0, 0), "n", 1, 0),
		at(first, "n", 2, 0),
		at(second, "n", 4, 0),
		at(d(11, 1, 23, 59), "n", 8, 0),
		at(d(11, 2, 0, 0), "n", 16, 0),
	)
	// To is exactly a midnight: the bucket containing To-1ns is Nov 1.
	res = mustAnalytics(t, l, core.AnalyticsQuery{From: d(10, 31, 0, 0), To: d(11, 2, 0, 0),
		Bucket: "day", Group: "model"}, ny)
	if len(res.BucketStarts) != 2 || !res.BucketStarts[1].Equal(d(11, 1, 0, 0)) {
		t.Fatalf("starts = %v", res.BucketStarts)
	}
	if got := d(11, 2, 0, 0).Sub(res.BucketStarts[1]); got != 25*time.Hour {
		t.Fatalf("Nov 1 length = %v", got)
	}
	if p := res.Series[0].Points; p[0].Requests != 0 || p[1].InputTokens != 15 || p[1].Requests != 4 {
		t.Fatalf("points = %+v", p)
	}
}

func TestAnalyticsDayBucketsHalfHourZone(t *testing.T) {
	// Local midnight in +05:30 is not on a UTC hour; rows 30 min either side
	// of it must land in the right day.
	kol := loadLoc(t, "Asia/Kolkata")
	l, _ := openTest(t, nil)
	mid := time.Date(2026, 6, 2, 0, 0, 0, 0, kol)
	record(t, l,
		at(mid.Add(-10*time.Minute), "m", 1, 0),
		at(mid.Add(10*time.Minute), "m", 2, 0),
	)
	res := mustAnalytics(t, l, core.AnalyticsQuery{From: mid.Add(-24 * time.Hour), To: mid.Add(24 * time.Hour),
		Bucket: "day", Group: "model"}, kol)
	p := res.Series[0].Points
	if len(p) != 2 || p[0].InputTokens != 1 || p[1].InputTokens != 2 {
		t.Fatalf("points = %+v", p)
	}
}

// withDim sets the RequestRecord field behind dimension dim to v.
func withDim(r core.RequestRecord, dim, v string) core.RequestRecord {
	switch dim {
	case "host":
		r.Host = v
	case "account":
		r.AccountID = v
	case "model":
		r.Model = v
	case "client":
		r.Client = v
	case "class":
		r.Class = core.Class(v)
	case "route":
		r.Route = v
	case "task":
		r.Task = v
	case "agent":
		r.Agent = v
	default:
		panic(dim)
	}
	return r
}

func TestAnalyticsGroupEveryDimension(t *testing.T) {
	l, _ := openTest(t, nil)
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	full := at(ts, "", 10, 0)
	for _, d := range core.AnalyticsDimensions {
		full = withDim(full, d, "v-"+d)
	}
	record(t, l, full, at(ts, "", 3, 0))
	if len(dimensionColumns) != len(core.AnalyticsDimensions) {
		t.Fatalf("dimensionColumns out of sync with core.AnalyticsDimensions")
	}
	for _, d := range core.AnalyticsDimensions {
		res := mustAnalytics(t, l, core.AnalyticsQuery{From: ts.Add(-time.Hour), To: ts.Add(time.Hour),
			Bucket: "hour", Group: d}, time.UTC)
		if len(res.Breakdown) != 2 || res.Breakdown[0].Key != "v-"+d || res.Breakdown[0].InputTokens != 10 ||
			res.Breakdown[1].Key != "" || res.Breakdown[1].InputTokens != 3 {
			t.Fatalf("group %s: breakdown = %+v", d, res.Breakdown)
		}
		if len(res.Series) != 2 || res.Series[1].Key != "" || res.Series[1].Total.Key != "" {
			t.Fatalf("group %s: series = %+v", d, res.Series)
		}
		// Filter on this dimension (value and empty value).
		res = mustAnalytics(t, l, core.AnalyticsQuery{From: ts.Add(-time.Hour), To: ts.Add(time.Hour),
			Bucket: "hour", Group: "model", Filters: map[string]string{d: ""}}, time.UTC)
		if res.Totals.InputTokens != 3 || res.Filters[d] != "" || len(res.Filters) != 1 {
			t.Fatalf("filter %s=\"\": %+v", d, res)
		}
		res = mustAnalytics(t, l, core.AnalyticsQuery{From: ts.Add(-time.Hour), To: ts.Add(time.Hour),
			Bucket: "hour", Group: "model", Filters: map[string]string{d: "v-" + d}}, time.UTC)
		if res.Totals.InputTokens != 10 {
			t.Fatalf("filter %s: %+v", d, res.Totals)
		}
	}
}

func TestAnalyticsMultipleFiltersAND(t *testing.T) {
	l, _ := openTest(t, nil)
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	mk := func(host, model string, in int64) core.RequestRecord {
		r := at(ts, model, in, 0)
		r.Host = host
		return r
	}
	record(t, l, mk("a", "x", 1), mk("a", "y", 2), mk("b", "x", 4), mk("", "x", 8))
	q := core.AnalyticsQuery{From: ts, To: ts.Add(time.Hour), Bucket: "hour", Group: "task",
		Filters: map[string]string{"host": "a", "model": "x"}}
	if res := mustAnalytics(t, l, q, time.UTC); res.Totals.InputTokens != 1 || res.Totals.Requests != 1 {
		t.Fatalf("totals = %+v", res.Totals)
	}
	q.Filters = map[string]string{"host": "", "model": "x"}
	if res := mustAnalytics(t, l, q, time.UTC); res.Totals.InputTokens != 8 {
		t.Fatalf("totals = %+v", res.Totals)
	}
	q.Filters = map[string]string{"host": "b", "model": "y"}
	res := mustAnalytics(t, l, q, time.UTC)
	if res.Totals.Requests != 0 || len(res.Breakdown) != 0 || len(res.Series) != 0 || res.Totals.CostUSD != nil {
		t.Fatalf("empty result = %+v", res)
	}
	if len(res.BucketStarts) != 1 || res.Series == nil || res.Breakdown == nil {
		t.Fatalf("empty result must keep buckets and non-nil slices: %+v", res)
	}
}

func TestAnalyticsTopNOther(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ny := loadLoc(t, "America/New_York")
	day := time.Date(2026, 4, 1, 0, 0, 0, 0, ny)
	var rs []core.RequestRecord
	// Totals (in+out): m1=100, tie-b=50, tie-a=50, model-a=30 (priced), m4=5, ""=1.
	for i, r := range []core.RequestRecord{
		at(day.Add(time.Hour), "m1", 60, 40),
		at(day.Add(25*time.Hour), "tie-b", 50, 0),
		at(day.Add(26*time.Hour), "tie-a", 25, 25),
		at(day.Add(2*time.Hour), "model-a", 10, 0),
		at(day.Add(49*time.Hour), "model-a", 10, 10),
		at(day.Add(50*time.Hour), "m4", 5, 0),
		at(day.Add(51*time.Hour), "", 1, 0),
	} {
		r.ID = fmt.Sprint(i)
		rs = append(rs, r)
	}
	record(t, l, rs...)
	q := core.AnalyticsQuery{From: day, To: day.Add(72 * time.Hour), Bucket: "day", Group: "model", TopN: 3}
	res := mustAnalytics(t, l, q, ny)
	var keys []string
	for _, b := range res.Breakdown {
		keys = append(keys, b.Key)
	}
	if !reflect.DeepEqual(keys, []string{"m1", "tie-a", "tie-b", "model-a", "m4", ""}) {
		t.Fatalf("breakdown keys = %q", keys)
	}
	keys = nil
	for _, s := range res.Series {
		keys = append(keys, s.Key)
	}
	if !reflect.DeepEqual(keys, []string{"m1", "tie-a", "tie-b", core.AnalyticsOtherKey}) {
		t.Fatalf("series keys = %q", keys)
	}
	o := res.Series[3]
	if o.Total.Key != core.AnalyticsOtherKey || o.Total.Requests != 4 || o.Total.InputTokens != 26 ||
		o.Points[0].InputTokens != 10 || o.Points[1].Requests != 0 || o.Points[2].Requests != 3 {
		t.Fatalf("other = %+v", o)
	}
	if res.Totals.Requests != 7 || res.SchemaVersion != 1 || res.Group != "model" || res.Bucket != "day" {
		t.Fatalf("result = %+v", res)
	}

	// TopN covering every key: no __other__.
	q.TopN = 50
	res = mustAnalytics(t, l, q, ny)
	if len(res.Series) != 6 || res.Series[5].Key != "" {
		t.Fatalf("series = %+v", res.Series)
	}
	// Default TopN is 8.
	q.TopN = 0
	if res = mustAnalytics(t, l, q, ny); len(res.Series) != 6 {
		t.Fatalf("series = %d", len(res.Series))
	}
}

func TestAnalyticsCostPerPoint(t *testing.T) {
	l, _ := openTest(t, testPricing())
	h := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	unknown := at(h.Add(2*time.Hour), "model-a", 0, 0)
	unknown.UsageKnown = false
	record(t, l,
		at(h, "model-a", 1_000_000, 0),              // hour 0: priced $2
		at(h.Add(time.Hour), "model-a", 0, 0),       // hour 1: priced $0
		at(h.Add(time.Hour), "unpriced", 5, 0),      //   + unpriced
		at(h.Add(2*time.Hour), "unpriced", 5, 0),    // hour 2: unpriced only
		unknown,                                     //   + unknown usage
		at(h.Add(4*time.Hour), "model-a", 0, 1e6),   // hour 4: priced $10
		at(h.Add(4*time.Hour+1), "model-a", 0, 1e6), //   + $10
	)
	res := mustAnalytics(t, l, core.AnalyticsQuery{From: h, To: h.Add(5 * time.Hour), Bucket: "hour", Group: "host"}, time.UTC)
	if len(res.Series) != 1 {
		t.Fatalf("series = %+v", res.Series)
	}
	p := res.Series[0].Points
	cost := func(i int) string {
		if p[i].CostUSD == nil {
			return "nil"
		}
		return fmt.Sprint(*p[i].CostUSD)
	}
	got := []string{cost(0), cost(1), cost(2), cost(3), cost(4)}
	if !reflect.DeepEqual(got, []string{"2", "0", "nil", "nil", "20"}) {
		t.Fatalf("costs = %v", got)
	}
	if p[1].UnpricedRequests != 1 || p[2].UnpricedRequests != 1 || p[2].UnknownUsageRequests != 1 {
		t.Fatalf("points = %+v", p)
	}
	if res.Totals.CostUSD == nil || *res.Totals.CostUSD != 22 || res.Totals.UnpricedRequests != 2 ||
		res.Totals.UnknownUsageRequests != 1 {
		t.Fatalf("totals = %+v", res.Totals)
	}
	// Same rule as Summary for the whole range.
	sum, err := l.Summary(context.Background(), h, "host")
	if err != nil || len(sum) != 1 || !rowsEqual(sum[0], res.Breakdown[0]) {
		t.Fatalf("summary %+v vs breakdown %+v (%v)", sum, res.Breakdown, err)
	}
}

func TestAnalyticsValidation(t *testing.T) {
	l, _ := openTest(t, nil)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	ok := core.AnalyticsQuery{From: now.Add(-24 * time.Hour), To: now, Bucket: "hour", Group: "host"}
	cases := map[string]func(q *core.AnalyticsQuery){
		"bad group":       func(q *core.AnalyticsQuery) { q.Group = "day" },
		"empty group":     func(q *core.AnalyticsQuery) { q.Group = "" },
		"bad filter":      func(q *core.AnalyticsQuery) { q.Filters = map[string]string{"session": "x"} },
		"bad bucket":      func(q *core.AnalyticsQuery) { q.Bucket = "week" },
		"from == to":      func(q *core.AnalyticsQuery) { q.From = q.To },
		"from > to":       func(q *core.AnalyticsQuery) { q.From = q.To.Add(time.Second) },
		"span > 400d":     func(q *core.AnalyticsQuery) { q.Bucket = "day"; q.From = q.To.Add(-400*24*time.Hour - 1) },
		"hourly > 31d":    func(q *core.AnalyticsQuery) { q.From = q.To.Add(-31*24*time.Hour - 1) },
		"top > 50":        func(q *core.AnalyticsQuery) { q.TopN = 51 },
		"top negative":    func(q *core.AnalyticsQuery) { q.TopN = -1 },
		"zero from/to":    func(q *core.AnalyticsQuery) { q.From, q.To = time.Time{}, time.Time{} },
		"sql inject attr": func(q *core.AnalyticsQuery) { q.Group = "host; DROP TABLE requests" },
	}
	for name, mut := range cases {
		q := ok
		mut(&q)
		_, err := l.Analytics(context.Background(), q)
		if err == nil || !strings.HasPrefix(err.Error(), "analytics: ") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Boundaries are inclusive.
	for _, q := range []core.AnalyticsQuery{
		ok,
		{From: now.Add(-31 * 24 * time.Hour), To: now, Bucket: "hour", Group: "host", TopN: 50},
		{From: now.Add(-400 * 24 * time.Hour), To: now, Bucket: "day", Group: "agent"},
	} {
		if _, err := l.Analytics(context.Background(), q); err != nil {
			t.Errorf("%+v: %v", q, err)
		}
	}
	// Internal errors do not carry the validation prefix.
	l.Close()
	if _, err := l.Analytics(context.Background(), ok); err == nil || strings.HasPrefix(err.Error(), "analytics: ") {
		t.Errorf("closed db err = %v", err)
	}
}

func TestRelabelHost(t *testing.T) {
	l, _ := openTest(t, nil)
	ctx := context.Background()
	ts := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mk := func(host string) core.RequestRecord { r := at(ts, "m", 1, 0); r.Host = host; return r }
	record(t, l, mk(""), mk(""), mk("other"))
	n, err := l.RelabelHost(ctx, "", "box-1.lan")
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	res := mustAnalytics(t, l, core.AnalyticsQuery{From: ts, To: ts.Add(time.Hour), Bucket: "hour", Group: "host"}, time.UTC)
	if len(res.Breakdown) != 2 || res.Breakdown[0].Key != "box-1.lan" || res.Breakdown[0].Requests != 2 ||
		res.Breakdown[1].Key != "other" {
		t.Fatalf("breakdown = %+v", res.Breakdown)
	}
	if n, err := l.RelabelHost(ctx, "", "x"); err != nil || n != 0 {
		t.Fatalf("second run n=%d err=%v", n, err)
	}
	for _, bad := range []string{"", "has space", "a/b", strings.Repeat("a", 65), "ünï"} {
		if _, err := l.RelabelHost(ctx, "other", bad); err == nil || !strings.HasPrefix(err.Error(), "relabel: ") {
			t.Errorf("to=%q err=%v", bad, err)
		}
	}
	if n, err := l.RelabelHost(ctx, "other", strings.Repeat("a", 64)); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	l.Close()
	if _, err := l.RelabelHost(ctx, "", "x"); err == nil || strings.HasPrefix(err.Error(), "relabel: ") {
		t.Errorf("closed db err = %v", err)
	}
}

func TestAnalyticsPerformance(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("slow; timings are meaningless under -race")
	}
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	end := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	start := end.Add(-90 * 24 * time.Hour)
	const n = 200_000
	step := end.Sub(start) / n
	models := []string{"model-a", "Model-B", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10", "m11", "m12"}
	hosts := []string{"", "h1", "h2", "h3"}
	batch := make([]core.RequestRecord, 0, MaxBatch)
	for i := 0; i < n; i++ {
		r := at(start.Add(time.Duration(i)*step), models[i%len(models)], int64(i%1000), int64(i%97))
		r.ID = fmt.Sprintf("p%07d", i)
		r.Host = hosts[i%len(hosts)]
		batch = append(batch, r)
		if len(batch) == MaxBatch {
			if err := l.RecordBatch(ctx, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	q := core.AnalyticsQuery{From: start, To: end, Bucket: "day", Group: "model"}
	t0 := time.Now()
	res, err := l.Analytics(ctx, q)
	el := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Totals.Requests != n || len(res.BucketStarts) < 90 {
		t.Fatalf("requests = %d buckets = %d", res.Totals.Requests, len(res.BucketStarts))
	}
	t.Logf("Analytics(day, group=model) over %d rows / 90 days: %v", n, el)
	q.Filters = map[string]string{"host": "h2"}
	t0 = time.Now()
	if _, err := l.Analytics(ctx, q); err != nil {
		t.Fatal(err)
	}
	t.Logf("Analytics(day, group=model, filter host) : %v", time.Since(t0))
	if el > time.Second {
		t.Fatalf("Analytics took %v, want < 1s", el)
	}
}
