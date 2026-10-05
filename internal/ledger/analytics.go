package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
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
	query := `SELECT (started_at - ?) / ? AS s, ` + groupCol + `, ` + aggCols + `
		FROM requests WHERE started_at >= ? AND started_at < ?` + filterSQL + ` GROUP BY s, ` + groupCol

	// The range is split at bucket boundaries into chunks queried
	// concurrently (WAL readers don't block each other); SQLite's GROUP BY
	// is the dominant cost on large ranges.
	nChunks := min(analyticsChunks, len(starts))
	chunks := make([][]slotRow, nChunks)
	eg, egctx := errgroup.WithContext(ctx)
	for c := range nChunks {
		lo, hi := q.From.UnixMilli(), q.To.UnixMilli()
		if c > 0 {
			lo = starts[c*len(starts)/nChunks].UnixMilli()
		}
		if c < nChunks-1 {
			hi = starts[(c+1)*len(starts)/nChunks].UnixMilli()
		}
		args := append([]any{base, slot, lo, hi}, filterArgs...)
		eg.Go(func() (err error) {
			chunks[c], err = l.querySlots(egctx, query, args)
			return err
		})
	}
	if err := eg.Wait(); err != nil {
		return core.AnalyticsResult{}, err
	}

	type acc struct {
		total  core.UsageRow
		points []core.UsageRow
	}
	groups := map[string]*acc{}
	var totals core.UsageRow
	for _, chunk := range chunks {
		for _, r := range chunk {
			at := base + r.slot*slot
			i := sort.Search(len(starts), func(i int) bool { return starts[i].UnixMilli() > at }) - 1
			g := groups[r.key]
			if g == nil {
				g = &acc{total: core.UsageRow{Key: r.key}, points: make([]core.UsageRow, len(starts))}
				groups[r.key] = g
			}
			addRow(&g.points[i], r.u)
			addRow(&g.total, r.u)
			addRow(&totals, r.u)
		}
	}

	breakdown := make([]core.UsageRow, 0, len(groups))
	for _, g := range groups {
		breakdown = append(breakdown, g.total)
	}
	sort.Slice(breakdown, func(i, j int) bool {
		ti, tj := rankTokens(breakdown[i]), rankTokens(breakdown[j])
		if ti != tj {
			return ti > tj
		}
		return breakdown[i].Key < breakdown[j].Key
	})

	series := []core.AnalyticsSeries{}
	var other *core.AnalyticsSeries
	for i, b := range breakdown {
		g := groups[b.Key]
		if i < topN {
			series = append(series, core.AnalyticsSeries{Key: b.Key, Total: g.total, Points: g.points})
			continue
		}
		if other == nil {
			other = &core.AnalyticsSeries{Key: core.AnalyticsOtherKey,
				Total: core.UsageRow{Key: core.AnalyticsOtherKey}, Points: make([]core.UsageRow, len(starts))}
		}
		addRow(&other.Total, g.total)
		for j := range g.points {
			addRow(&other.Points[j], g.points[j])
		}
	}
	if other != nil {
		series = append(series, *other)
	}

	filters := make(map[string]string, len(q.Filters))
	for k, v := range q.Filters {
		filters[k] = v
	}
	return core.AnalyticsResult{
		SchemaVersion: analyticsSchema,
		From:          q.From,
		To:            q.To,
		Bucket:        q.Bucket,
		BucketStarts:  starts,
		Group:         q.Group,
		Filters:       filters,
		Totals:        totals,
		Breakdown:     breakdown,
		Series:        series,
	}, nil
}

// slotRow is one (slot, group key) aggregate from querySlots.
type slotRow struct {
	slot int64
	key  string
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
		var cost sql.NullFloat64
		var in, cached, creation, outTok, reasoning float64
		if err := rows.Scan(&r.slot, &r.key, &r.u.Requests, &in, &cached, &creation, &outTok, &reasoning,
			&cost, &r.u.UnknownUsageRequests, &r.u.UnpricedRequests); err != nil {
			return nil, fmt.Errorf("ledger: analytics: %w", err)
		}
		r.u.InputTokens, r.u.CachedInputTokens = satInt(in), satInt(cached)
		r.u.CacheCreationInputTokens, r.u.OutputTokens, r.u.ReasoningTokens = satInt(creation), satInt(outTok), satInt(reasoning)
		if cost.Valid {
			c := cost.Float64
			r.u.CostUSD = &c
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: analytics: %w", err)
	}
	return out, nil
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
			c += *dst.CostUSD
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
