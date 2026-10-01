package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
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

var _ core.Ledger = (*Ledger)(nil)

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

// Record inserts one request row, computing cost from pricing. An empty
// r.ID is replaced with a random one.
func (l *Ledger) Record(ctx context.Context, r core.RequestRecord) error {
	if r.ID == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		r.ID = hex.EncodeToString(b[:])
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

	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.db.ExecContext(ctx, `INSERT INTO requests (
		id, started_at, finished_at, client, class, route, model, provider,
		account_id, upstream_identity, status, failover_of, input_tokens,
		cached_input_tokens, output_tokens, reasoning_tokens, usage_known,
		cost_usd, cost_basis, latency_ms, bytes_out, session, task, agent, error
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.StartedAt.UnixMilli(), finished, r.Client, string(r.Class), r.Route, r.Model, r.Provider,
		r.AccountID, r.UpstreamIdentity, r.Status, failoverOf, r.Usage.InputTokens,
		r.Usage.CachedInputTokens, r.Usage.OutputTokens, r.Usage.ReasoningTokens, usageKnown,
		cost, basis, r.LatencyMS, r.BytesOut, r.Session, r.Task, r.Agent, r.Error)
	if err != nil {
		return fmt.Errorf("ledger: record: %w", err)
	}
	return nil
}

// groupColumns maps Summary group names to SQL columns. "day" is handled
// in Go so the local time zone (including DST) is applied per row.
var groupColumns = map[string]string{
	"account": "account_id",
	"model":   "model",
	"class":   "class",
	"client":  "client",
	"route":   "route",
}

const aggCols = `COUNT(*), SUM(input_tokens), SUM(cached_input_tokens),
	SUM(output_tokens), SUM(reasoning_tokens), SUM(cost_usd),
	SUM(CASE WHEN usage_known = 0 THEN 1 ELSE 0 END),
	SUM(CASE WHEN usage_known = 1 AND cost_usd IS NULL THEN 1 ELSE 0 END)`

// Summary aggregates requests started at or after since, grouped by one of
// account, model, class, client, route, or day (local date YYYY-MM-DD).
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
		if err := rows.Scan(&u.Key, &u.Requests, &u.InputTokens, &u.CachedInputTokens,
			&u.OutputTokens, &u.ReasoningTokens, &cost, &u.UnknownUsageRequests, &u.UnpricedRequests); err != nil {
			return nil, fmt.Errorf("ledger: summary: %w", err)
		}
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
		output_tokens, reasoning_tokens, cost_usd, usage_known
		FROM requests WHERE started_at >= ?`, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("ledger: summary: %w", err)
	}
	defer rows.Close()
	byDay := map[string]*core.UsageRow{}
	for rows.Next() {
		var started int64
		var in, cached, outTok, reasoning int64
		var cost sql.NullFloat64
		var known bool
		if err := rows.Scan(&started, &in, &cached, &outTok, &reasoning, &cost, &known); err != nil {
			return nil, fmt.Errorf("ledger: summary: %w", err)
		}
		key := time.UnixMilli(started).Local().Format(time.DateOnly)
		u := byDay[key]
		if u == nil {
			u = &core.UsageRow{Key: key}
			byDay[key] = u
		}
		u.Requests++
		u.InputTokens += in
		u.CachedInputTokens += cached
		u.OutputTokens += outTok
		u.ReasoningTokens += reasoning
		switch {
		case cost.Valid:
			c := cost.Float64
			if u.CostUSD != nil {
				c += *u.CostUSD
			}
			u.CostUSD = &c
		case !known:
			u.UnknownUsageRequests++
		default:
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
