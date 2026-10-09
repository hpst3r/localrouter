package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	_ "modernc.org/sqlite" // registers driver "sqlite"
)

// Ledger is a SQLite-backed core.Ledger.
type Ledger struct {
	db      *sql.DB
	pricing *Pricing
	basis   func(accountID string) string
	mu      sync.Mutex // serializes writes
}

var (
	_ core.Ledger      = (*Ledger)(nil)
	_ core.BatchLedger = (*Ledger)(nil)
)

// MaxBatch is the maximum number of records accepted by RecordBatch.
const MaxBatch = 1000

// costBasisProviderReported is the reserved cost_basis provenance for a cost
// the upstream provider reported itself (e.g. OpenRouter usage.cost). It takes
// precedence over the local pricing table and is never overwritten by Reprice.
const costBasisProviderReported = "provider_reported"

// validReportedCost reports whether p is a usable provider-reported cost: an
// explicit, finite, non-negative USD amount. nil, NaN, ±Inf and negative values
// are unusable. It guards both the proxy's observations and records ingested
// from agents.
func validReportedCost(p *float64) (float64, bool) {
	if p == nil {
		return 0, false
	}
	c := *p
	if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
		return 0, false
	}
	return c, true
}

// migrations are applied in order; index+1 is the schema version.
// The requests table must never gain prompt/response content columns.
var migrations = []string{
	`CREATE TABLE requests (
		id TEXT PRIMARY KEY,
		started_at INTEGER NOT NULL,
		finished_at INTEGER,
		client TEXT NOT NULL,
		class TEXT NOT NULL,
		route TEXT NOT NULL,
		model TEXT NOT NULL,
		provider TEXT NOT NULL,
		account_id TEXT NOT NULL,
		upstream_identity TEXT NOT NULL,
		status INTEGER NOT NULL,
		failover_of TEXT,
		input_tokens INTEGER NOT NULL,
		cached_input_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL,
		reasoning_tokens INTEGER NOT NULL,
		usage_known INTEGER NOT NULL,
		cost_usd REAL,
		cost_basis TEXT,
		latency_ms INTEGER NOT NULL,
		bytes_out INTEGER NOT NULL,
		session TEXT NOT NULL,
		task TEXT NOT NULL,
		agent TEXT NOT NULL,
		error TEXT NOT NULL
	);
	CREATE INDEX requests_started_at ON requests(started_at);
	CREATE INDEX requests_account_id ON requests(account_id);`,
	`ALTER TABLE requests ADD COLUMN cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0;`,
	`ALTER TABLE requests ADD COLUMN host TEXT NOT NULL DEFAULT '';`,
}

// Open opens (creating if needed) the ledger database at path. pricing may be
// nil (everything unpriced). basis maps an account ID to its cost basis
// ("api_equivalent" or "metered"); it may be nil.
func Open(path string, pricing *Pricing, basis func(accountID string) string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	// Pre-create with 0600; SQLite gives WAL/SHM files the same mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}

	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("ledger: open: %w", err)
	}
	l := &Ledger{db: db, pricing: pricing, basis: basis}
	if err := l.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Ledger) migrate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx := context.Background()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: migrate: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("ledger: migrate: %w", err)
	}
	var v int
	err = tx.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return fmt.Errorf("ledger: migrate: %w", err)
		}
	case err != nil:
		return fmt.Errorf("ledger: migrate: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("ledger: database schema version %d is newer than supported %d", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if _, err := tx.Exec(migrations[i]); err != nil {
			return fmt.Errorf("ledger: migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, len(migrations)); err != nil {
		return fmt.Errorf("ledger: migrate: %w", err)
	}
	return tx.Commit()
}

// Close closes the database.
func (l *Ledger) Close() error { return l.db.Close() }

// Ping probes storage health with a bounded, read-only check. It uses a
// connection ping (SQLite issues no statement) and never mutates state, so it
// is safe to call from the readiness and diagnostics paths. It satisfies
// control.StoragePinger, letting the control server self-wire the ledger as
// its read-only storage probe.
func (l *Ledger) Ping(ctx context.Context) error {
	return l.db.PingContext(ctx)
}

const insertSQL = `INSERT INTO requests (
	id, started_at, finished_at, client, class, route, model, provider,
	account_id, upstream_identity, status, failover_of, input_tokens,
	cached_input_tokens, output_tokens, reasoning_tokens, usage_known,
	cost_usd, cost_basis, latency_ms, bytes_out, session, task, agent, error,
	cache_creation_input_tokens, host
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO NOTHING`

// Record inserts one request row, computing cost from pricing. An empty
// r.ID is replaced with a random one. Record is idempotent on ID: if a row
// with r.ID already exists it is left unchanged and nil is returned.
func (l *Ledger) Record(ctx context.Context, r core.RequestRecord) error {
	args := l.insertArgs(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.db.ExecContext(ctx, insertSQL, args...); err != nil {
		return fmt.Errorf("ledger: record: %w", err)
	}
	return nil
}

// RecordBatch inserts up to MaxBatch rows in one transaction with the same
// semantics as Record: rows whose ID already exists are skipped silently.
// Either every new row is written or none is.
func (l *Ledger) RecordBatch(ctx context.Context, rs []core.RequestRecord) error {
	if len(rs) > MaxBatch {
		return fmt.Errorf("ledger: record batch: %d records exceeds max %d", len(rs), MaxBatch)
	}
	if len(rs) == 0 {
		return nil
	}
	args := make([][]any, len(rs))
	for i, r := range rs {
		args[i] = l.insertArgs(r)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: record batch: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return fmt.Errorf("ledger: record batch: %w", err)
	}
	defer stmt.Close()
	for _, a := range args {
		if _, err := stmt.ExecContext(ctx, a...); err != nil {
			return fmt.Errorf("ledger: record batch: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: record batch: %w", err)
	}
	return nil
}

// insertArgs computes cost and returns the bind arguments for insertSQL.
func (l *Ledger) insertArgs(r core.RequestRecord) []any {
	if r.ID == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		r.ID = hex.EncodeToString(b[:])
	}
	cost, basis := l.resolveCost(r)
	var finished sql.NullInt64
	if !r.FinishedAt.IsZero() {
		finished = sql.NullInt64{Int64: r.FinishedAt.UnixMilli(), Valid: true}
	}
	var failoverOf sql.NullString
	if r.FailoverOf != "" {
		failoverOf = sql.NullString{String: r.FailoverOf, Valid: true}
	}
	usageKnown := 0
	if r.UsageKnown {
		usageKnown = 1
	}
	return []any{
		r.ID, r.StartedAt.UnixMilli(), finished, r.Client, string(r.Class), r.Route, r.Model, r.Provider,
		r.AccountID, r.UpstreamIdentity, r.Status, failoverOf, r.Usage.InputTokens,
		r.Usage.CachedInputTokens, r.Usage.OutputTokens, r.Usage.ReasoningTokens, usageKnown,
		cost, basis, r.LatencyMS, r.BytesOut, r.Session, r.Task, r.Agent, r.Error,
		r.Usage.CacheCreationInputTokens, r.Host,
	}
}

// resolveCost picks the persisted cost_usd/cost_basis for r. A usable
// provider-reported OpenRouter cost takes precedence over the local pricing
// table — including an explicit zero, and regardless of UsageKnown — and is
// stored with the reserved "provider_reported" provenance. Otherwise the
// record is priced from the table as before (only when usage is known).
func (l *Ledger) resolveCost(r core.RequestRecord) (sql.NullFloat64, sql.NullString) {
	if r.Provider == core.ProviderOpenRouter {
		if c, ok := validReportedCost(r.ReportedCostUSD); ok {
			return sql.NullFloat64{Float64: c, Valid: true},
				sql.NullString{String: costBasisProviderReported, Valid: true}
		}
	}
	var cost sql.NullFloat64
	var basis sql.NullString
	if r.UsageKnown {
		if c, ok := l.pricing.Cost(r.Model, r.Usage); ok {
			cost = sql.NullFloat64{Float64: c, Valid: true}
			if l.basis != nil {
				if b := l.basis(r.AccountID); b != "" {
					basis = sql.NullString{String: b, Valid: true}
				}
			}
		}
	}
	return cost, basis
}

// Reprice recomputes cost_usd and cost_basis for every row with known usage
// using the ledger's current pricing (e.g. after importing prices). Rows for
// unpriced models get NULL cost. Rows whose cost was reported by the provider
// (cost_basis "provider_reported") are never recomputed, overwritten, or
// cleared; the guard is applied both when selecting rows and in the UPDATE so
// a row that gains provider-reported provenance between the two is still
// protected. It returns how many rows are now priced.
func (l *Ledger) Reprice(ctx context.Context) (int, error) {
	type row struct {
		id, model, account string
		u                  core.Usage
	}
	rs, err := l.db.QueryContext(ctx, `SELECT id, model, account_id, input_tokens,
		cached_input_tokens, cache_creation_input_tokens, output_tokens, reasoning_tokens
		FROM requests WHERE usage_known = 1
		AND (cost_basis IS NULL OR cost_basis <> '`+costBasisProviderReported+`')`)
	if err != nil {
		return 0, fmt.Errorf("ledger: reprice: %w", err)
	}
	var rows []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.id, &r.model, &r.account, &r.u.InputTokens, &r.u.CachedInputTokens,
			&r.u.CacheCreationInputTokens, &r.u.OutputTokens, &r.u.ReasoningTokens); err != nil {
			rs.Close()
			return 0, fmt.Errorf("ledger: reprice: %w", err)
		}
		rows = append(rows, r)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return 0, fmt.Errorf("ledger: reprice: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("ledger: reprice: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE requests SET cost_usd = ?, cost_basis = ?
		WHERE id = ? AND (cost_basis IS NULL OR cost_basis <> '`+costBasisProviderReported+`')`)
	if err != nil {
		return 0, fmt.Errorf("ledger: reprice: %w", err)
	}
	defer stmt.Close()
	priced := 0
	for _, r := range rows {
		var cost sql.NullFloat64
		var basis sql.NullString
		if c, ok := l.pricing.Cost(r.model, r.u); ok {
			cost = sql.NullFloat64{Float64: c, Valid: true}
			priced++
			if l.basis != nil {
				if b := l.basis(r.account); b != "" {
					basis = sql.NullString{String: b, Valid: true}
				}
			}
		}
		if _, err := stmt.ExecContext(ctx, cost, basis, r.id); err != nil {
			return 0, fmt.Errorf("ledger: reprice: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("ledger: reprice: %w", err)
	}
	return priced, nil
}

// groupColumns maps Summary group names to SQL columns. "day" is handled
// in Go so the local time zone (including DST) is applied per row.
var groupColumns = map[string]string{
	"account": "account_id",
	"model":   "model",
	"class":   "class",
	"client":  "client",
	"route":   "route",
	"host":    "host",
	"task":    "task",
	"agent":   "agent",
}

// aggCols sums token columns with TOTAL() (REAL) rather than SUM(), which
// raises "integer overflow" past int64 and would fail the whole summary.
const aggCols = `COUNT(*), TOTAL(input_tokens), TOTAL(cached_input_tokens),
	TOTAL(cache_creation_input_tokens), TOTAL(output_tokens), TOTAL(reasoning_tokens), SUM(cost_usd),
	SUM(CASE WHEN usage_known = 0 THEN 1 ELSE 0 END),
	SUM(CASE WHEN usage_known = 1 AND cost_usd IS NULL THEN 1 ELSE 0 END)`

// Summary aggregates requests started at or after since, grouped by one of
// account, model, class, client, route, host, or day (local date YYYY-MM-DD).
// Rows are ordered by key.
func (l *Ledger) Summary(ctx context.Context, since time.Time, group string) ([]core.UsageRow, error) {
	if group == "day" {
		return l.summaryByDay(ctx, since)
	}
	col, ok := groupColumns[group]
	if !ok {
		return nil, fmt.Errorf("ledger: unknown summary group %q", group)
	}
	rows, err := l.db.QueryContext(ctx, `SELECT `+col+`, `+aggCols+`
		FROM requests WHERE started_at >= ? GROUP BY `+col+` ORDER BY `+col,
		since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("ledger: summary: %w", err)
	}
	defer rows.Close()
	out := []core.UsageRow{}
	for rows.Next() {
		var u core.UsageRow
		var cost sql.NullFloat64
		var in, cached, creation, outTok, reasoning float64
		if err := rows.Scan(&u.Key, &u.Requests, &in, &cached,
			&creation, &outTok, &reasoning, &cost, &u.UnknownUsageRequests, &u.UnpricedRequests); err != nil {
			return nil, fmt.Errorf("ledger: summary: %w", err)
		}
		u.InputTokens, u.CachedInputTokens = satInt(in), satInt(cached)
		u.CacheCreationInputTokens, u.OutputTokens, u.ReasoningTokens = satInt(creation), satInt(outTok), satInt(reasoning)
		if cost.Valid {
			c := cost.Float64
			u.CostUSD = &c
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: summary: %w", err)
	}
	return out, nil
}

func (l *Ledger) summaryByDay(ctx context.Context, since time.Time) ([]core.UsageRow, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT started_at, input_tokens, cached_input_tokens,
		cache_creation_input_tokens, output_tokens, reasoning_tokens, cost_usd, usage_known
		FROM requests WHERE started_at >= ?`, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("ledger: summary: %w", err)
	}
	defer rows.Close()
	byDay := map[string]*core.UsageRow{}
	for rows.Next() {
		var started int64
		var in, cached, creation, outTok, reasoning int64
		var cost sql.NullFloat64
		var known bool
		if err := rows.Scan(&started, &in, &cached, &creation, &outTok, &reasoning, &cost, &known); err != nil {
			return nil, fmt.Errorf("ledger: summary: %w", err)
		}
		key := time.UnixMilli(started).Local().Format(time.DateOnly)
		u := byDay[key]
		if u == nil {
			u = &core.UsageRow{Key: key}
			byDay[key] = u
		}
		u.Requests++
		u.InputTokens = satAdd(u.InputTokens, in)
		u.CachedInputTokens = satAdd(u.CachedInputTokens, cached)
		u.CacheCreationInputTokens = satAdd(u.CacheCreationInputTokens, creation)
		u.OutputTokens = satAdd(u.OutputTokens, outTok)
		u.ReasoningTokens = satAdd(u.ReasoningTokens, reasoning)
		// Token knowledge and cost knowledge are independent for reported
		// OpenRouter costs, matching the SQL aggregation in other groups.
		if !known {
			u.UnknownUsageRequests++
		}
		if cost.Valid {
			c := cost.Float64
			if u.CostUSD != nil {
				c += *u.CostUSD
			}
			u.CostUSD = &c
		} else if known {
			u.UnpricedRequests++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: summary: %w", err)
	}
	out := make([]core.UsageRow, 0, len(byDay))
	for _, u := range byDay {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// satInt converts a REAL token total to int64, saturating at the int64 range.
func satInt(f float64) int64 {
	switch {
	case f != f:
		return 0
	case f >= math.MaxInt64:
		return math.MaxInt64
	case f <= math.MinInt64:
		return math.MinInt64
	}
	return int64(f)
}

// satAdd returns a+b, saturating instead of wrapping on overflow.
func satAdd(a, b int64) int64 {
	s := a + b
	if a > 0 && b > 0 && s < 0 {
		return math.MaxInt64
	}
	if a < 0 && b < 0 && s >= 0 {
		return math.MinInt64
	}
	return s
}
