package ledger

// Adversarial review of Performance (SPEC "Performance metrics"): a
// brute-force reference over random data, and Analytics alignment.

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// refSet computes the expected PerfRow of one sample set straight from the
// SPEC formulas.
type refSet struct {
	n, streamed              int64
	lat, ttft, dec, e2e      []float64
	decTok, decMS, eTok, eMS float64
}

func (s *refSet) add(r core.RequestRecord) {
	s.n++
	if r.Streamed {
		s.streamed++
	}
	s.lat = append(s.lat, float64(r.LatencyMS))
	if r.TTFTMS != nil {
		s.ttft = append(s.ttft, float64(*r.TTFTMS))
	}
	if r.UsageKnown && r.Usage.OutputTokens > 0 {
		s.e2e = append(s.e2e, float64(r.Usage.OutputTokens)*1000/float64(r.LatencyMS))
		s.eTok += float64(r.Usage.OutputTokens)
		s.eMS += float64(r.LatencyMS)
	}
	if r.Streamed && r.TTFTMS != nil && r.UsageKnown {
		d := r.Usage.OutputTokens
		if !r.FirstTokenReasoning {
			d -= r.Usage.ReasoningTokens
		}
		g := r.LatencyMS - *r.TTFTMS
		if d >= 2 && g >= 50 { // SPEC Amendment 1, A1
			s.dec = append(s.dec, float64(d-1)*1000/float64(g))
			s.decTok += float64(d - 1)
			s.decMS += float64(g)
		}
	}
}

func refDist(x []float64) *core.Dist {
	if len(x) == 0 {
		return nil
	}
	x = slices.Clone(x)
	sort.Float64s(x)
	n := len(x)
	p := func(q float64) float64 {
		h := float64(n-1) * q
		lo := int(h)
		hi := lo
		if float64(lo) < h {
			hi = lo + 1
		}
		return x[lo] + (h-float64(lo))*(x[hi]-x[lo])
	}
	var sum float64
	for _, v := range x {
		sum += v
	}
	return &core.Dist{N: int64(n), Mean: sum / float64(n), Min: x[0], Max: x[n-1],
		P01: p(.01), P05: p(.05), P10: p(.1), P50: p(.5), P90: p(.9), P95: p(.95), P99: p(.99)}
}

func refRate(tok, ms float64) *float64 {
	if ms <= 0 {
		return nil
	}
	v := tok * 1000 / ms
	return &v
}

func distEq(a, b *core.Dist) bool {
	if a == nil || b == nil {
		return a == b
	}
	f := func(x, y float64) bool { return math.Abs(x-y) <= 1e-9*math.Max(1, math.Abs(y)) }
	return a.N == b.N && f(a.Mean, b.Mean) && f(a.Min, b.Min) && f(a.Max, b.Max) && f(a.P01, b.P01) &&
		f(a.P05, b.P05) && f(a.P10, b.P10) && f(a.P50, b.P50) && f(a.P90, b.P90) && f(a.P95, b.P95) && f(a.P99, b.P99)
}

func rateEq(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return math.Abs(*a-*b) <= 1e-9*math.Max(1, math.Abs(*b))
}

func checkRow(t *testing.T, where string, got core.PerfRow, want *refSet) {
	t.Helper()
	if want == nil {
		want = &refSet{}
	}
	if got.Requests != want.n || got.StreamedRequests != want.streamed ||
		!distEq(got.LatencyMS, refDist(want.lat)) || !distEq(got.TTFTMS, refDist(want.ttft)) ||
		!distEq(got.DecodeTPS, refDist(want.dec)) || !distEq(got.E2ETPS, refDist(want.e2e)) ||
		!rateEq(got.DecodeTPSWeighted, refRate(want.decTok, want.decMS)) ||
		!rateEq(got.E2ETPSWeighted, refRate(want.eTok, want.eMS)) {
		t.Fatalf("%s: got %+v, want n=%d streamed=%d", where, got, want.n, want.streamed)
	}
}

// TestPerformanceMatchesReference cross-checks Performance against a
// brute-force reference on random data: eligibility, formulas, buckets
// (UTC hours, DST days in New York, +05:30 days), filters, top N, __other__
// pooling and breakdown, and that bucket starts equal Analytics'.
func TestPerformanceMatchesReference(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	ist := time.FixedZone("IST", 5*3600+1800)
	l, err := Open(filepath.Join(t.TempDir(), "l.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	rng := rand.New(rand.NewSource(7))
	base := time.Date(2026, 10, 28, 3, 17, 0, 0, time.UTC) // spans the Nov 1 DST change
	models := []string{"a", "b", "c", "d", "e", "f", "", "zz"}
	hosts := []string{"h1", "h2", ""}
	var recs []core.RequestRecord
	for i := range 3000 {
		r := core.RequestRecord{
			ID:        fmt.Sprintf("r%05d", i),
			StartedAt: base.Add(time.Duration(rng.Int63n(int64(9 * 24 * time.Hour)))).Truncate(time.Millisecond),
			Model:     models[rng.Intn(len(models))],
			Host:      hosts[rng.Intn(len(hosts))],
			Status:    []int{200, 200, 200, 201, 299, 300, 199, 500, 0}[rng.Intn(9)],
			LatencyMS: []int64{0, 1, 2, 5, 100, 3000, 120000}[rng.Intn(7)],
			Streamed:  rng.Intn(3) > 0,
		}
		if rng.Intn(6) == 0 {
			r.Error = "boom"
		}
		if rng.Intn(4) > 0 {
			v := rng.Int63n(r.LatencyMS + 1)
			r.TTFTMS = &v
			r.FirstTokenReasoning = rng.Intn(3) == 0
		}
		if rng.Intn(5) > 0 {
			r.UsageKnown = true
			r.Usage = core.Usage{OutputTokens: rng.Int63n(5), ReasoningTokens: rng.Int63n(3)}
			if rng.Intn(2) == 0 {
				r.Usage.OutputTokens = rng.Int63n(4000)
				r.Usage.ReasoningTokens = rng.Int63n(2000)
			}
		}
		recs = append(recs, r)
	}
	for i := 0; i < len(recs); i += MaxBatch {
		if err := l.RecordBatch(context.Background(), recs[i:min(i+MaxBatch, len(recs))]); err != nil {
			t.Fatal(err)
		}
	}

	type tc struct {
		q   core.AnalyticsQuery
		loc *time.Location
	}
	from := base.Add(5 * time.Hour)
	to := base.Add(8*24*time.Hour + 7*time.Minute)
	cases := []tc{
		{core.AnalyticsQuery{From: from, To: to, Bucket: "day", Group: "model"}, ny},
		{core.AnalyticsQuery{From: from, To: to, Bucket: "day", Group: "model", TopN: 2}, ist},
		{core.AnalyticsQuery{From: from, To: from.Add(70 * time.Hour), Bucket: "hour", Group: "host", TopN: 1}, ny},
		{core.AnalyticsQuery{From: from, To: to, Bucket: "day", Group: "model", TopN: 50, Filters: map[string]string{"host": ""}}, time.UTC},
		{core.AnalyticsQuery{From: from, To: to, Bucket: "day", Group: "host", Filters: map[string]string{"model": "a"}}, ny},
		{core.AnalyticsQuery{From: from, To: to, Bucket: "day", Group: "model", TopN: 3, Filters: map[string]string{"model": "nope"}}, ny},
	}
	col := map[string]func(core.RequestRecord) string{
		"model": func(r core.RequestRecord) string { return r.Model },
		"host":  func(r core.RequestRecord) string { return r.Host },
	}
	for ci, c := range cases {
		res := mustPerf(t, l, c.q, c.loc)
		ar, err := l.analytics(context.Background(), c.q, c.loc)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.EqualFunc(res.BucketStarts, ar.BucketStarts, time.Time.Equal) {
			t.Fatalf("case %d: bucket starts differ from Analytics", ci)
		}
		starts := res.BucketStarts
		perKey := map[string]*refSet{}
		perPoint := map[string][]refSet{}
		var totals refSet
		for _, r := range recs {
			if r.StartedAt.Before(c.q.From) || !r.StartedAt.Before(c.q.To) {
				continue
			}
			if !(r.LatencyMS > 0 && r.Status >= 200 && r.Status <= 299 && r.Error == "") {
				continue
			}
			ok := true
			for k, v := range c.q.Filters {
				ok = ok && col[k](r) == v
			}
			if !ok {
				continue
			}
			k := col[c.q.Group](r)
			if perKey[k] == nil {
				perKey[k] = &refSet{}
				perPoint[k] = make([]refSet, len(starts))
			}
			b := sort.Search(len(starts), func(i int) bool { return starts[i].After(r.StartedAt) }) - 1
			perKey[k].add(r)
			perPoint[k][b].add(r)
			totals.add(r)
		}
		keys := make([]string, 0, len(perKey))
		for k := range perKey {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if perKey[keys[i]].n != perKey[keys[j]].n {
				return perKey[keys[i]].n > perKey[keys[j]].n
			}
			return keys[i] < keys[j]
		})
		top := c.q.TopN
		if top == 0 {
			top = 8
		}
		top = min(top, len(keys))
		checkRow(t, fmt.Sprintf("case %d totals", ci), res.Totals, &totals)
		if len(res.Breakdown) != len(keys) {
			t.Fatalf("case %d: breakdown %d rows, want %d", ci, len(res.Breakdown), len(keys))
		}
		for i, k := range keys {
			if res.Breakdown[i].Key != k {
				t.Fatalf("case %d: breakdown[%d] = %q, want %q", ci, i, res.Breakdown[i].Key, k)
			}
			checkRow(t, fmt.Sprintf("case %d breakdown %q", ci, k), res.Breakdown[i], perKey[k])
		}
		wantSeries := top
		other := make([]refSet, len(starts))
		var otherTotal refSet
		// Pool other from the raw records again to keep the reference simple.
		for _, r := range recs {
			if r.StartedAt.Before(c.q.From) || !r.StartedAt.Before(c.q.To) ||
				!(r.LatencyMS > 0 && r.Status >= 200 && r.Status <= 299 && r.Error == "") {
				continue
			}
			ok := true
			for k, v := range c.q.Filters {
				ok = ok && col[k](r) == v
			}
			k := col[c.q.Group](r)
			if !ok || slices.Contains(keys[:top], k) {
				continue
			}
			b := sort.Search(len(starts), func(i int) bool { return starts[i].After(r.StartedAt) }) - 1
			other[b].add(r)
			otherTotal.add(r)
		}
		if otherTotal.n > 0 {
			wantSeries++
		}
		if len(res.Series) != wantSeries {
			t.Fatalf("case %d: %d series, want %d", ci, len(res.Series), wantSeries)
		}
		for i, s := range res.Series {
			pts := other
			tot := &otherTotal
			if i < top {
				if s.Key != keys[i] {
					t.Fatalf("case %d: series %d = %q, want %q", ci, i, s.Key, keys[i])
				}
				pts, tot = perPoint[keys[i]], perKey[keys[i]]
			} else if s.Key != core.AnalyticsOtherKey {
				t.Fatalf("case %d: series %d = %q, want other", ci, i, s.Key)
			}
			checkRow(t, fmt.Sprintf("case %d series %q total", ci, s.Key), s.Total, tot)
			for b := range starts {
				checkRow(t, fmt.Sprintf("case %d series %q bucket %d", ci, s.Key, b), s.Points[b], &pts[b])
			}
		}
	}
}
