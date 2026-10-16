package ledger

import (
	"container/heap"
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"golang.org/x/sync/errgroup"
)

var _ core.PerfLedger = (*Ledger)(nil)

// perfMaxRows caps the eligible rows one Performance call reads (SPEC
// "Performance metrics"); a variable so tests can lower it.
var perfMaxRows int64 = 2_000_000

// perfMaxBreakdown caps the breakdown rows (core.AnalyticsMaxBreakdown); a
// variable so tests can lower it.
var perfMaxBreakdown = core.AnalyticsMaxBreakdown

// perfMinDecodeMS is the shortest decode window G that yields a decode
// sample (SPEC Amendment 1, A1).
const perfMinDecodeMS = 50

// perfEligible selects perf-eligible rows: proxied (latency known), 2xx and
// no error.
const perfEligible = ` AND latency_ms > 0 AND status BETWEEN 200 AND 299 AND error = ''`

// Performance answers q per SPEC "Performance metrics": latency, TTFT, decode
// and end-to-end speed distributions over perf-eligible rows, with the same
// validation, windowing, buckets (local days via time.Local), filters, top N
// and breakdown cap as Analytics. Validation errors start with "analytics: ".
func (l *Ledger) Performance(ctx context.Context, q core.AnalyticsQuery) (core.PerfResult, error) {
	return l.performance(ctx, q, time.Local)
}

// performance is Performance with an explicit location for day buckets.
func (l *Ledger) performance(ctx context.Context, q core.AnalyticsQuery, loc *time.Location) (core.PerfResult, error) {
	groupCol, topN, err := validateAnalytics(q)
	if err != nil {
		return core.PerfResult{}, err
	}
	starts := bucketStarts(q.From, q.To, q.Bucket, loc)
	filterSQL, filterArgs := filterClause(q.Filters)
	where := ` FROM requests WHERE started_at >= ? AND started_at < ?` + perfEligible + filterSQL

	// Both queries are split at bucket boundaries into chunks run
	// concurrently, as in Analytics.
	bounds := chunkBounds(q.From, q.To, starts)
	chunkArgs := func(b [2]int64) []any { return append([]any{b[0], b[1]}, filterArgs...) }

	ranked, nKeys, err := l.perfRank(ctx, `SELECT `+groupCol+`, COUNT(*)`+where+` GROUP BY `+groupCol+` ORDER BY `+groupCol, bounds, chunkArgs)
	if err != nil {
		return core.PerfResult{}, err
	}
	nBreak := len(ranked)
	nTop := min(topN, nBreak)
	acc := perfAcc{
		startMS: make([]int64, len(starts)),
		rank:    make(map[string]int, nBreak),
		nTop:    nTop,
		points:  make([][]sampleSet, nTop+1), // [nTop] is __other__
	}
	for i, s := range starts {
		acc.startMS[i] = s.UnixMilli()
	}
	for i, k := range ranked {
		acc.rank[k.key] = i
	}
	for i := range acc.points {
		acc.points[i] = make([]sampleSet, len(starts))
	}

	// Chunks cover disjoint buckets, so each writes only its own points;
	// samples of breakdown keys outside the top N go to per-chunk sets.
	query := `SELECT started_at, ` + groupCol + `, latency_ms, ttft_ms, streamed,
		first_token_reasoning, usage_known, output_tokens, reasoning_tokens` + where
	rest := make([][]sampleSet, len(bounds))
	eg, egctx := errgroup.WithContext(ctx)
	for c, b := range bounds {
		rest[c] = make([]sampleSet, nBreak-nTop)
		eg.Go(func() error { return l.perfScan(egctx, query, chunkArgs(b), &acc, rest[c]) })
	}
	if err := eg.Wait(); err != nil {
		return core.PerfResult{}, err
	}

	// Every set is summarized from its own pooled raw samples, level by
	// level: each level is summarized (row sorts in place), then pooled into
	// the next and released, so at most two copies of the samples are live.
	breakdown := make([]core.PerfRow, nBreak)
	var g errgroup.Group
	g.SetLimit(analyticsChunks)
	for i := range nBreak - nTop {
		g.Go(func() error {
			sets := make([]*sampleSet, len(rest))
			for c := range rest {
				sets[c] = &rest[c][i]
			}
			s := pool(sets)
			breakdown[nTop+i] = s.row(ranked[nTop+i].key)
			return nil
		})
	}
	_ = g.Wait()
	rest = nil

	seriesTotals := make([]sampleSet, nTop+1)
	series := make([]core.PerfSeries, nTop+1)
	for i := range series {
		key := core.AnalyticsOtherKey
		if i < nTop {
			key = ranked[i].key
		}
		g.Go(func() error {
			pts := acc.points[i]
			s := core.PerfSeries{Key: key, Points: make([]core.PerfRow, len(pts))}
			sets := make([]*sampleSet, len(pts))
			for j := range pts {
				s.Points[j] = pts[j].row("")
				sets[j] = &pts[j]
			}
			seriesTotals[i] = pool(sets)
			s.Total = seriesTotals[i].row(key)
			series[i] = s
			return nil
		})
	}
	_ = g.Wait()
	if series[nTop].Total.Requests == 0 {
		series = series[:nTop] // no __other__
	}
	for i := range nTop {
		breakdown[i] = series[i].Total
	}
	sets := make([]*sampleSet, len(seriesTotals))
	for i := range seriesTotals {
		sets[i] = &seriesTotals[i]
	}
	totals := pool(sets)

	filters := make(map[string]string, len(q.Filters))
	for k, v := range q.Filters {
		filters[k] = v
	}
	return core.PerfResult{
		SchemaVersion:    analyticsSchema,
		From:             q.From,
		To:               q.To,
		Bucket:           q.Bucket,
		BucketStarts:     starts,
		Group:            q.Group,
		Filters:          filters,
		Totals:           totals.row(""),
		Breakdown:        breakdown,
		BreakdownOmitted: nKeys - nBreak,
		Series:           series,
	}, nil
}

// perfKey is one group key's eligible request count.
type perfKey struct {
	key      string
	requests int64
}

// perfRank runs the per-key COUNT query (ordered by key) over every chunk,
// enforces perfMaxRows and returns the best perfMaxBreakdown keys ranked by
// requests desc, then key asc, plus the number of distinct keys. As in
// rankKeys, the chunk streams are merged by key through a bounded heap, so
// memory does not depend on the number of keys.
func (l *Ledger) perfRank(ctx context.Context, query string, bounds [][2]int64, chunkArgs func([2]int64) []any) ([]perfKey, int, error) {
	eg, egctx := errgroup.WithContext(ctx)
	streams := make([]chan perfKey, len(bounds))
	for c, b := range bounds {
		ch := make(chan perfKey, 64)
		streams[c] = ch
		eg.Go(func() error {
			defer close(ch)
			rows, err := l.db.QueryContext(egctx, query, chunkArgs(b)...)
			if err != nil {
				return fmt.Errorf("ledger: performance: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var k perfKey
				if err := rows.Scan(&k.key, &k.requests); err != nil {
					return fmt.Errorf("ledger: performance: %w", err)
				}
				select {
				case ch <- k:
				case <-egctx.Done():
					return egctx.Err()
				}
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("ledger: performance: %w", err)
			}
			return nil
		})
	}

	// SQLite's default BINARY collation orders like Go string comparison.
	heads := make([]perfKey, len(streams))
	live := make([]bool, len(streams))
	for c, ch := range streams {
		heads[c], live[c] = <-ch
	}
	h := &perfHeap{}
	nKeys, n := 0, int64(0)
	for {
		first := -1
		for c := range streams {
			if live[c] && (first < 0 || heads[c].key < heads[first].key) {
				first = c
			}
		}
		if first < 0 {
			break
		}
		k := perfKey{key: heads[first].key}
		for c, ch := range streams {
			if live[c] && heads[c].key == k.key {
				k.requests += heads[c].requests
				heads[c], live[c] = <-ch
			}
		}
		nKeys++
		n += k.requests
		switch {
		case h.Len() < perfMaxBreakdown:
			heap.Push(h, k)
		case perfRankBefore(k, (*h)[0]):
			(*h)[0] = k
			heap.Fix(h, 0)
		}
	}
	// A failed stream closes early; its error is reported here.
	if err := eg.Wait(); err != nil {
		return nil, 0, err
	}
	if n > perfMaxRows {
		return nil, 0, fmt.Errorf("analytics: too many requests in range for performance stats (%d); narrow the range or add filters", n)
	}
	out := []perfKey(*h)
	sort.Slice(out, func(i, j int) bool { return perfRankBefore(out[i], out[j]) })
	return out, nKeys, nil
}

// perfRankBefore reports whether a ranks above b.
func perfRankBefore(a, b perfKey) bool {
	if a.requests != b.requests {
		return a.requests > b.requests
	}
	return a.key < b.key
}

// perfHeap is a min-heap by rank: the lowest-ranked kept key is at [0].
type perfHeap []perfKey

func (h perfHeap) Len() int           { return len(h) }
func (h perfHeap) Less(i, j int) bool { return perfRankBefore(h[j], h[i]) }
func (h perfHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *perfHeap) Push(x any)        { *h = append(*h, x.(perfKey)) }
func (h *perfHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// perfAcc routes scanned rows to sample sets. points is shared by the scan
// chunks, which touch disjoint buckets.
type perfAcc struct {
	startMS []int64
	rank    map[string]int // breakdown keys -> rank
	nTop    int
	points  [][]sampleSet // [series][bucket]; [nTop] is __other__
}

// perfScan streams one chunk's eligible rows into acc.points and, for
// breakdown keys ranked below the top N, into rest (indexed rank - nTop).
func (l *Ledger) perfScan(ctx context.Context, query string, args []any, acc *perfAcc, rest []sampleSet) error {
	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("ledger: performance: %w", err)
	}
	defer rows.Close()
	var (
		startedAt, streamed, ftr, usageKnown int64
		key                                  sql.RawBytes
		ttft                                 sql.NullInt64
		o                                    perfObs
	)
	for rows.Next() {
		if err := rows.Scan(&startedAt, &key, &o.latency, &ttft, &streamed, &ftr, &usageKnown, &o.out, &o.reasoning); err != nil {
			return fmt.Errorf("ledger: performance: %w", err)
		}
		o.ttft, o.hasTTFT = ttft.Int64, ttft.Valid
		o.streamed, o.firstReasoning, o.usageKnown = streamed == 1, ftr == 1, usageKnown == 1
		b := sort.Search(len(acc.startMS), func(i int) bool { return acc.startMS[i] > startedAt }) - 1
		// A key missing from the ranking appeared after it; it is "other".
		i, ok := acc.rank[string(key)]
		if ok && i < acc.nTop {
			acc.points[i][b].add(o)
			continue
		}
		acc.points[acc.nTop][b].add(o)
		if ok {
			rest[i-acc.nTop].add(o)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger: performance: %w", err)
	}
	return nil
}

// perfObs is one eligible row's timing and usage.
type perfObs struct {
	latency, ttft, out, reasoning                 int64
	hasTTFT, streamed, firstReasoning, usageKnown bool
}

// sampleSet pools the raw samples (and weighted-rate sums) of one set of
// eligible rows.
type sampleSet struct {
	requests, streamed         int64
	latency, ttft, decode, e2e []float64
	decodeTok, decodeMS        float64 // sum(D - 1), sum(G)
	e2eTok, e2eMS              float64 // sum(output_tokens), sum(latency_ms)
}

// add records o per SPEC "Eligibility and formulas".
func (s *sampleSet) add(o perfObs) {
	s.requests++
	if o.streamed {
		s.streamed++
	}
	s.latency = append(s.latency, float64(o.latency))
	if o.hasTTFT {
		s.ttft = append(s.ttft, float64(o.ttft))
	}
	if !o.usageKnown {
		return
	}
	if o.out > 0 {
		s.e2e = append(s.e2e, float64(o.out)*1000/float64(o.latency))
		s.e2eTok += float64(o.out)
		s.e2eMS += float64(o.latency)
	}
	if o.streamed && o.hasTTFT {
		d := o.out
		if !o.firstReasoning {
			d -= o.reasoning // hidden reasoning finished before the first visible token
		}
		if g := o.latency - o.ttft; d >= 2 && g >= perfMinDecodeMS {
			s.decode = append(s.decode, float64(d-1)*1000/float64(g))
			s.decodeTok += float64(d - 1)
			s.decodeMS += float64(g)
		}
	}
}

// merge appends o's samples to s.
func (s *sampleSet) merge(o *sampleSet) {
	s.requests += o.requests
	s.streamed += o.streamed
	s.latency = append(s.latency, o.latency...)
	s.ttft = append(s.ttft, o.ttft...)
	s.decode = append(s.decode, o.decode...)
	s.e2e = append(s.e2e, o.e2e...)
	s.decodeTok += o.decodeTok
	s.decodeMS += o.decodeMS
	s.e2eTok += o.e2eTok
	s.e2eMS += o.e2eMS
}

// pool moves the samples of sets into one new, exactly sized set, releasing
// each input's samples once copied.
func pool(sets []*sampleSet) sampleSet {
	var nLat, nTTFT, nDec, nE2E int
	for _, s := range sets {
		nLat += len(s.latency)
		nTTFT += len(s.ttft)
		nDec += len(s.decode)
		nE2E += len(s.e2e)
	}
	p := sampleSet{
		latency: make([]float64, 0, nLat), ttft: make([]float64, 0, nTTFT),
		decode: make([]float64, 0, nDec), e2e: make([]float64, 0, nE2E),
	}
	for _, s := range sets {
		p.merge(s)
		*s = sampleSet{}
	}
	return p
}

// row summarizes s. It sorts s's sample slices in place.
func (s *sampleSet) row(key string) core.PerfRow {
	return core.PerfRow{
		Key:               key,
		Requests:          s.requests,
		StreamedRequests:  s.streamed,
		LatencyMS:         distOf(s.latency),
		TTFTMS:            distOf(s.ttft),
		DecodeTPS:         distOf(s.decode),
		E2ETPS:            distOf(s.e2e),
		DecodeTPSWeighted: perSecond(s.decodeTok, s.decodeMS),
		E2ETPSWeighted:    perSecond(s.e2eTok, s.e2eMS),
	}
}

// perSecond returns tokens*1000/ms, nil when ms is not positive.
func perSecond(tokens, ms float64) *float64 {
	if ms <= 0 {
		return nil
	}
	v := tokens * 1000 / ms
	return &v
}

// distOf summarizes samples (nil when empty), interpolating percentiles as
// documented on core.Dist. It sorts samples in place.
func distOf(samples []float64) *core.Dist {
	n := len(samples)
	if n == 0 {
		return nil
	}
	slices.Sort(samples)
	pct := func(p float64) float64 {
		h := float64(n-1) * p
		lo, hi := int(math.Floor(h)), int(math.Ceil(h))
		return samples[lo] + (h-float64(lo))*(samples[hi]-samples[lo])
	}
	var sum float64
	for _, v := range samples {
		sum += v
	}
	return &core.Dist{
		N: int64(n), Mean: sum / float64(n), Min: samples[0], Max: samples[n-1],
		P01: pct(0.01), P05: pct(0.05), P10: pct(0.10), P50: pct(0.50),
		P90: pct(0.90), P95: pct(0.95), P99: pct(0.99),
	}
}
