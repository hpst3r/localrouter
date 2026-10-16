package ledger

import (
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

	ranked, err := l.perfRank(ctx, `SELECT `+groupCol+`, COUNT(*)`+where+` GROUP BY `+groupCol, bounds, chunkArgs)
	if err != nil {
		return core.PerfResult{}, err
	}
	nBreak := min(len(ranked), core.AnalyticsMaxBreakdown)
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
	for i, k := range ranked[:nBreak] {
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

	// Every set is summarized from its own pooled raw samples: pool (copy)
	// first, then summarize concurrently (row sorts a set's slices in place).
	var totals sampleSet
	seriesTotals := make([]sampleSet, nTop+1)
	for i, pts := range acc.points {
		for j := range pts {
			seriesTotals[i].merge(&pts[j])
		}
		totals.merge(&seriesTotals[i])
	}
	restTotals := make([]sampleSet, nBreak-nTop)
	for i := range restTotals {
		for c := range rest {
			restTotals[i].merge(&rest[c][i])
		}
	}
	rest = nil

	nSeries := nTop
	if seriesTotals[nTop].requests > 0 {
		nSeries++ // __other__
	}
	series := make([]core.PerfSeries, nSeries)
	breakdown := make([]core.PerfRow, nBreak)
	var totalsRow core.PerfRow
	var g errgroup.Group
	g.SetLimit(analyticsChunks)
	g.Go(func() error { totalsRow = totals.row(""); return nil })
	for i := range series {
		key := core.AnalyticsOtherKey
		if i < nTop {
			key = ranked[i].key
		}
		g.Go(func() error {
			pts := acc.points[i]
			s := core.PerfSeries{Key: key, Total: seriesTotals[i].row(key), Points: make([]core.PerfRow, len(pts))}
			for j := range pts {
				s.Points[j] = pts[j].row("")
			}
			series[i] = s
			return nil
		})
	}
	for i := range restTotals {
		g.Go(func() error { breakdown[nTop+i] = restTotals[i].row(ranked[nTop+i].key); return nil })
	}
	_ = g.Wait()
	for i := range nTop {
		breakdown[i] = series[i].Total
	}

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
		Totals:           totalsRow,
		Breakdown:        breakdown,
		BreakdownOmitted: len(ranked) - nBreak,
		Series:           series,
	}, nil
}

// perfKey is one group key's eligible request count.
type perfKey struct {
	key      string
	requests int64
}

// perfRank runs the per-key COUNT query over every chunk, enforces
// perfMaxRows and returns the keys ranked by requests desc, then key asc.
func (l *Ledger) perfRank(ctx context.Context, query string, bounds [][2]int64, chunkArgs func([2]int64) []any) ([]perfKey, error) {
	counts := make([]map[string]int64, len(bounds))
	eg, egctx := errgroup.WithContext(ctx)
	for c, b := range bounds {
		eg.Go(func() error {
			rows, err := l.db.QueryContext(egctx, query, chunkArgs(b)...)
			if err != nil {
				return fmt.Errorf("ledger: performance: %w", err)
			}
			defer rows.Close()
			m := map[string]int64{}
			for rows.Next() {
				var k string
				var n int64
				if err := rows.Scan(&k, &n); err != nil {
					return fmt.Errorf("ledger: performance: %w", err)
				}
				m[k] = n
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("ledger: performance: %w", err)
			}
			counts[c] = m
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	merged := map[string]int64{}
	var n int64
	for _, m := range counts {
		for k, v := range m {
			merged[k] += v
			n += v
		}
	}
	out := make([]perfKey, 0, len(merged))
	for k, v := range merged {
		out = append(out, perfKey{k, v})
	}
	if n > perfMaxRows {
		return nil, fmt.Errorf("analytics: too many requests in range for performance stats (%d); narrow the range or add filters", n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].requests != out[j].requests {
			return out[i].requests > out[j].requests
		}
		return out[i].key < out[j].key
	})
	return out, nil
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
		if g := o.latency - o.ttft; d >= 2 && g >= 1 {
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
