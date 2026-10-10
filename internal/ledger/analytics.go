package ledger

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"golang.org/x/sync/errgroup"
)

var _ core.AnalyticsLedger = (*Ledger)(nil)

// Analytics limits (see SPEC "Analytics").
const (
	defaultTopN     = 8
	maxTopN         = 50
	maxSpan         = 400 * 24 * time.Hour
	maxHourlySpan   = 31 * 24 * time.Hour
	analyticsSchema = 1
	// analyticsChunks bounds the concurrent queries per Analytics call.
	analyticsChunks = 4
)

// dimensionColumns maps core.AnalyticsDimensions to SQL columns.
var dimensionColumns = map[string]string{
	"host":    "host",
	"account": "account_id",
	"model":   "model",
	"client":  "client",
	"class":   "class",
	"route":   "route",
	"task":    "task",
	"agent":   "agent",
}

// Analytics answers q per SPEC: rows with From <= started_at < To matching
// every filter, bucketed by UTC hour or local calendar day and grouped by
// q.Group. Validation errors start with "analytics: ".
func (l *Ledger) Analytics(ctx context.Context, q core.AnalyticsQuery) (core.AnalyticsResult, error) {
	return l.analytics(ctx, q, time.Local)
}

// analytics is Analytics with an explicit location for day buckets.
func (l *Ledger) analytics(ctx context.Context, q core.AnalyticsQuery, loc *time.Location) (core.AnalyticsResult, error) {
	groupCol, topN, err := validateAnalytics(q)
	if err != nil {
		return core.AnalyticsResult{}, err
	}
	starts := bucketStarts(q.From, q.To, q.Bucket, loc)

	// SQL aggregates per slot of `slot` ms counted from base. slot divides
	// every bucket boundary's offset from base, so each slot lies within one
	// bucket even when local midnight is not on a UTC hour (e.g. +05:30).
	base := starts[0].UnixMilli()
	slot := int64(time.Hour / time.Millisecond)
	for _, s := range starts[1:] {
		slot = gcd(slot, s.UnixMilli()-base)
	}

	var filterSQL string
	var filterArgs []any
	keys := make([]string, 0, len(q.Filters))
	for k := range q.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		filterSQL += " AND " + dimensionColumns[k] + " = ?"
		filterArgs = append(filterArgs, q.Filters[k])
	}

	// The range is split at bucket boundaries into chunks queried
	// concurrently (WAL readers don't block each other); SQLite's GROUP BY
	// is the dominant cost on large ranges.
	nChunks := min(analyticsChunks, len(starts))
	bounds := make([][2]int64, nChunks)
	for c := range nChunks {
		lo, hi := q.From.UnixMilli(), q.To.UnixMilli()
		if c > 0 {
			lo = starts[c*len(starts)/nChunks].UnixMilli()
		}
		if c < nChunks-1 {
			hi = starts[(c+1)*len(starts)/nChunks].UnixMilli()
		}
		bounds[c] = [2]int64{lo, hi}
	}

	// Rank every key in range first and keep only the best
	// core.AnalyticsMaxBreakdown, so memory does not grow with the number
	// of distinct keys times buckets.
	breakdown, nKeys, err := l.rankKeys(ctx, groupCol, filterSQL, filterArgs, bounds)
	if err != nil {
		return core.AnalyticsResult{}, err
	}
	top := breakdown[:min(topN, len(breakdown))]

	// Per-slot aggregates name only the top keys; every other key is folded
	// into a NULL key (__other__) by SQL.
	keyExpr := groupCol
	var keyArgs []any
	if nKeys > len(top) {
		keyExpr = `CASE WHEN ` + groupCol + ` IN (?` + strings.Repeat(`,?`, len(top)-1) + `) THEN ` + groupCol + ` END`
		for _, b := range top {
			keyArgs = append(keyArgs, b.Key)
		}
	}
	query := `SELECT (started_at - ?) / ? AS s, ` + keyExpr + ` AS k, ` + aggCols + `
		FROM requests WHERE started_at >= ? AND started_at < ?` + filterSQL + ` GROUP BY s, k`

	chunks := make([][]slotRow, nChunks)
	eg, egctx := errgroup.WithContext(ctx)
	for c, b := range bounds {
		args := append(append([]any{base, slot}, keyArgs...), b[0], b[1])
		args = append(args, filterArgs...)
		eg.Go(func() (err error) {
			chunks[c], err = l.querySlots(egctx, query, args)
			return err
		})
	}
	if err := eg.Wait(); err != nil {
		return core.AnalyticsResult{}, err
	}

	series := make([]core.AnalyticsSeries, len(top), len(top)+1)
	index := make(map[string]int, len(top))
	for i, b := range top {
		index[b.Key] = i
		series[i] = core.AnalyticsSeries{Key: b.Key, Total: core.UsageRow{Key: b.Key}, Points: make([]core.UsageRow, len(starts))}
	}
	var other *core.AnalyticsSeries
	var totals core.UsageRow
	for _, chunk := range chunks {
		for _, r := range chunk {
			at := base + r.slot*slot
			i := sort.Search(len(starts), func(i int) bool { return starts[i].UnixMilli() > at }) - 1
			var dst *core.AnalyticsSeries
			if j, ok := index[r.key.String]; ok && r.key.Valid {
				dst = &series[j]
			} else {
				// NULL key, or a key that appeared after ranking.
				if other == nil {
					other = &core.AnalyticsSeries{Key: core.AnalyticsOtherKey,
						Total: core.UsageRow{Key: core.AnalyticsOtherKey}, Points: make([]core.UsageRow, len(starts))}
				}
				dst = other
			}
			addRow(&dst.Points[i], r.u)
			addRow(&dst.Total, r.u)
			addRow(&totals, r.u)
		}
	}
	// Top breakdown rows mirror their series totals (same scan as Points).
	for i := range top {
		breakdown[i] = series[i].Total
	}
	if other != nil {
		series = append(series, *other)
	}

	filters := make(map[string]string, len(q.Filters))
	for k, v := range q.Filters {
		filters[k] = v
	}
	return core.AnalyticsResult{
		SchemaVersion:    analyticsSchema,
		From:             q.From,
		To:               q.To,
		Bucket:           q.Bucket,
		BucketStarts:     starts,
		Group:            q.Group,
		Filters:          filters,
		Totals:           totals,
		Breakdown:        breakdown,
		BreakdownOmitted: nKeys - len(breakdown),
		Series:           series,
	}, nil
}

// rankKeys aggregates every group key matching the filters within bounds and
// returns the best core.AnalyticsMaxBreakdown rows in rank order (rankTokens
// desc, key asc) plus the number of distinct keys. Each chunk is grouped and
// ordered by key in SQL; the streams are merged by key and pass through a
// bounded heap, so memory does not depend on the number of keys.
func (l *Ledger) rankKeys(ctx context.Context, groupCol, filterSQL string, filterArgs []any, bounds [][2]int64) ([]core.UsageRow, int, error) {
	query := `SELECT ` + groupCol + `, ` + aggCols + `
		FROM requests WHERE started_at >= ? AND started_at < ?` + filterSQL + `
		GROUP BY ` + groupCol + ` ORDER BY ` + groupCol
	eg, egctx := errgroup.WithContext(ctx)
	streams := make([]chan core.UsageRow, len(bounds))
	for c, b := range bounds {
		ch := make(chan core.UsageRow, 64)
		streams[c] = ch
		args := append([]any{b[0], b[1]}, filterArgs...)
		eg.Go(func() error {
			defer close(ch)
			rows, err := l.db.QueryContext(egctx, query, args...)
			if err != nil {
				return fmt.Errorf("ledger: analytics: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var u core.UsageRow
				if err := scanAgg(rows, &u, &u.Key); err != nil {
					return fmt.Errorf("ledger: analytics: %w", err)
				}
				select {
				case ch <- u:
				case <-egctx.Done():
					return egctx.Err()
				}
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("ledger: analytics: %w", err)
			}
			return nil
		})
	}

	// SQLite's default BINARY collation orders like Go string comparison.
	heads := make([]core.UsageRow, len(streams))
	live := make([]bool, len(streams))
	for c, ch := range streams {
		heads[c], live[c] = <-ch
	}
	h := &rankHeap{}
	n := 0
	for {
		first := -1
		for c := range streams {
			if live[c] && (first < 0 || heads[c].Key < heads[first].Key) {
				first = c
			}
		}
		if first < 0 {
			break
		}
		u := core.UsageRow{Key: heads[first].Key}
		for c, ch := range streams {
			if live[c] && heads[c].Key == u.Key {
				addRow(&u, heads[c])
				heads[c], live[c] = <-ch
			}
		}
		n++
		switch {
		case h.Len() < core.AnalyticsMaxBreakdown:
			heap.Push(h, u)
		case rankBefore(u, (*h)[0]):
			(*h)[0] = u
			heap.Fix(h, 0)
		}
	}
	// A failed stream closes early; its error is reported here.
	if err := eg.Wait(); err != nil {
		return nil, 0, err
	}
	out := []core.UsageRow(*h)
	sort.Slice(out, func(i, j int) bool { return rankBefore(out[i], out[j]) })
	return out, n, nil
}

// rankBefore reports whether a ranks above b.
func rankBefore(a, b core.UsageRow) bool {
	ta, tb := rankTokens(a), rankTokens(b)
	if ta != tb {
		return ta > tb
	}
	return a.Key < b.Key
}

// rankHeap is a min-heap by rank: the lowest-ranked kept row is at [0].
type rankHeap []core.UsageRow

func (h rankHeap) Len() int           { return len(h) }
func (h rankHeap) Less(i, j int) bool { return rankBefore(h[j], h[i]) }
func (h rankHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *rankHeap) Push(x any)        { *h = append(*h, x.(core.UsageRow)) }
func (h *rankHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// slotRow is one (slot, group key) aggregate from querySlots.
type slotRow struct {
	slot int64
	key  sql.NullString // NULL: folded into __other__
	u    core.UsageRow
}

// querySlots runs the per-slot aggregate query.
func (l *Ledger) querySlots(ctx context.Context, query string, args []any) ([]slotRow, error) {
	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ledger: analytics: %w", err)
	}
	defer rows.Close()
	var out []slotRow
	for rows.Next() {
		var r slotRow
		if err := scanAgg(rows, &r.u, &r.slot, &r.key); err != nil {
			return nil, fmt.Errorf("ledger: analytics: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: analytics: %w", err)
	}
	return out, nil
}

// scanAgg scans the leading columns into lead and the aggCols that follow
// into u (token totals and cost saturated, NULL cost = nil).
func scanAgg(rows *sql.Rows, u *core.UsageRow, lead ...any) error {
	var cost sql.NullFloat64
	var in, cached, creation, outTok, reasoning float64
	dest := append(lead, &u.Requests, &in, &cached, &creation, &outTok, &reasoning,
		&cost, &u.UnknownUsageRequests, &u.UnpricedRequests)
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	u.InputTokens, u.CachedInputTokens = satInt(in), satInt(cached)
	u.CacheCreationInputTokens, u.OutputTokens, u.ReasoningTokens = satInt(creation), satInt(outTok), satInt(reasoning)
	if cost.Valid {
		c := satCost(cost.Float64)
		u.CostUSD = &c
	}
	return nil
}

// validateAnalytics checks q and returns the group column and effective TopN.
func validateAnalytics(q core.AnalyticsQuery) (string, int, error) {
	col, ok := dimensionColumns[q.Group]
	if !ok {
		return "", 0, fmt.Errorf("analytics: unknown group %q", q.Group)
	}
	for k := range q.Filters {
		if _, ok := dimensionColumns[k]; !ok {
			return "", 0, fmt.Errorf("analytics: unknown filter %q", k)
		}
	}
	if !q.From.Before(q.To) {
		return "", 0, errors.New("analytics: from must be before to")
	}
	span := q.To.Sub(q.From)
	if span > maxSpan {
		return "", 0, errors.New("analytics: range exceeds 400 days")
	}
	switch q.Bucket {
	case "hour":
		if span > maxHourlySpan {
			return "", 0, errors.New("analytics: hour buckets limited to 31 days")
		}
	case "day":
	default:
		return "", 0, fmt.Errorf("analytics: unknown bucket %q", q.Bucket)
	}
	topN := q.TopN
	switch {
	case topN == 0:
		topN = defaultTopN
	case topN < 0 || topN > maxTopN:
		return "", 0, fmt.Errorf("analytics: top must be between 1 and %d", maxTopN)
	}
	return col, topN, nil
}

// bucketStarts returns the starts of the buckets covering [from, to): UTC
// hours, or local midnights in loc for "day" (a DST day is one 23h/25h
// bucket).
func bucketStarts(from, to time.Time, bucket string, loc *time.Location) []time.Time {
	var out []time.Time
	if bucket == "hour" {
		for t := from.UTC().Truncate(time.Hour); t.Before(to); t = t.Add(time.Hour) {
			out = append(out, t)
		}
		return out
	}
	f := from.In(loc)
	for t := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc); t.Before(to); {
		out = append(out, t)
		t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
	}
	return out
}

// addRow adds u into dst with the Summary cost rule: CostUSD is the sum of
// priced costs, nil while nothing priced has been added.
func addRow(dst *core.UsageRow, u core.UsageRow) {
	dst.Requests += u.Requests
	dst.InputTokens = satAdd(dst.InputTokens, u.InputTokens)
	dst.CachedInputTokens = satAdd(dst.CachedInputTokens, u.CachedInputTokens)
	dst.CacheCreationInputTokens = satAdd(dst.CacheCreationInputTokens, u.CacheCreationInputTokens)
	dst.OutputTokens = satAdd(dst.OutputTokens, u.OutputTokens)
	dst.ReasoningTokens = satAdd(dst.ReasoningTokens, u.ReasoningTokens)
	dst.UnknownUsageRequests += u.UnknownUsageRequests
	dst.UnpricedRequests += u.UnpricedRequests
	if u.CostUSD != nil {
		c := *u.CostUSD
		if dst.CostUSD != nil {
			c = satCost(c + *dst.CostUSD)
		}
		dst.CostUSD = &c
	}
}

// rankTokens is the ranking metric: input + output tokens.
func rankTokens(u core.UsageRow) int64 { return satAdd(u.InputTokens, u.OutputTokens) }

func gcd(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

var hostNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RelabelHost sets host = to on every row whose host equals from, in one
// transaction, and returns the number of rows changed. Used once to backfill
// pre-attribution history (`localrouter ledger relabel-host`). Validation
// errors start with "relabel: ".
func (l *Ledger) RelabelHost(ctx context.Context, from, to string) (int64, error) {
	if !hostNameRE.MatchString(to) {
		return 0, fmt.Errorf("relabel: invalid host name %q (want [A-Za-z0-9._-]{1,64})", to)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("ledger: relabel host: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE requests SET host = ? WHERE host = ?`, to, from)
	if err != nil {
		return 0, fmt.Errorf("ledger: relabel host: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ledger: relabel host: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("ledger: relabel host: %w", err)
	}
	return n, nil
}
