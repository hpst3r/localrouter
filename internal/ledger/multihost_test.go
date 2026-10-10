package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// v2Schema is schema version 2 (v1 + cache_creation_input_tokens), frozen
// here so the v2 -> v3 upgrade path stays tested.
const v2Schema = `CREATE TABLE requests (
	id TEXT PRIMARY KEY, started_at INTEGER NOT NULL, finished_at INTEGER,
	client TEXT NOT NULL, class TEXT NOT NULL, route TEXT NOT NULL,
	model TEXT NOT NULL, provider TEXT NOT NULL, account_id TEXT NOT NULL,
	upstream_identity TEXT NOT NULL, status INTEGER NOT NULL, failover_of TEXT,
	input_tokens INTEGER NOT NULL, cached_input_tokens INTEGER NOT NULL,
	output_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL,
	usage_known INTEGER NOT NULL, cost_usd REAL, cost_basis TEXT,
	latency_ms INTEGER NOT NULL, bytes_out INTEGER NOT NULL, session TEXT NOT NULL,
	task TEXT NOT NULL, agent TEXT NOT NULL, error TEXT NOT NULL,
	cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX requests_started_at ON requests(started_at);
CREATE INDEX requests_account_id ON requests(account_id);
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version (version) VALUES (2);
INSERT INTO requests VALUES ('old', 1, NULL, 'c', 'background', 'r', 'm', 'p',
	'a', '', 200, NULL, 10, 2, 3, 0, 1, NULL, NULL, 5, 0, '', '', '', '', 7);`

func TestMigrateV2ToV3Host(t *testing.T) {
	path := filepath.Join(t.TempDir(), "localrouter.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(v2Schema); err != nil {
		t.Fatal(err)
	}
	db.Close()

	l, err := Open(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var v int
	l.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != 4 {
		t.Fatalf("version = %d", v)
	}
	var host string
	var creation int64
	var upstream, pricing sql.NullString
	if err := l.db.QueryRow(`SELECT host, cache_creation_input_tokens, upstream_model, pricing_model FROM requests WHERE id='old'`).
		Scan(&host, &creation, &upstream, &pricing); err != nil {
		t.Fatal(err)
	}
	if host != "" || creation != 7 {
		t.Fatalf("old row: host=%q creation=%d", host, creation)
	}
	if upstream.Valid || pricing.Valid {
		t.Fatalf("old row attribution = %v/%v; want NULL/NULL", upstream, pricing)
	}
	ctx := context.Background()
	if err := l.Record(ctx, core.RequestRecord{ID: "new", StartedAt: time.UnixMilli(2), Host: "vm1", UsageKnown: true}); err != nil {
		t.Fatal(err)
	}
	rows, err := l.Summary(ctx, time.Time{}, "host")
	if err != nil || len(rows) != 2 || rows[0].Key != "" || rows[1].Key != "vm1" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestSummaryGroupHost(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	now := time.Now()
	recs := []core.RequestRecord{
		{ID: "1", StartedAt: now, Model: "model-a", AccountID: "sub", Host: "vm1", Usage: core.Usage{InputTokens: 10}, UsageKnown: true},
		{ID: "2", StartedAt: now, Model: "model-a", AccountID: "sub", Host: "vm1", Usage: core.Usage{InputTokens: 5}, UsageKnown: true},
		{ID: "3", StartedAt: now, Model: "model-a", AccountID: "sub", Usage: core.Usage{InputTokens: 1}, UsageKnown: true},
	}
	for _, r := range recs {
		if err := l.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := l.Summary(ctx, now.Add(-time.Hour), "host")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Key != "" || rows[0].Requests != 1 ||
		rows[1].Key != "vm1" || rows[1].Requests != 2 || rows[1].InputTokens != 15 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestRecordBatchDedupe(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	now := time.Now()
	batch := []core.RequestRecord{
		{ID: "a", StartedAt: now, Model: "model-a", AccountID: "sub", Host: "vm1", Usage: core.Usage{InputTokens: 100, OutputTokens: 10}, UsageKnown: true},
		{ID: "b", StartedAt: now, Model: "model-a", AccountID: "sub", Host: "vm1", Usage: core.Usage{InputTokens: 50}, UsageKnown: true},
		{ID: "a", StartedAt: now, Model: "model-a", AccountID: "sub", Host: "vm1", Usage: core.Usage{InputTokens: 999}, UsageKnown: true},
	}
	if err := l.RecordBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordBatch(ctx, batch[:2]); err != nil {
		t.Fatalf("re-send: %v", err)
	}
	var n int
	var in int64
	var cost sql.NullFloat64
	l.db.QueryRow(`SELECT COUNT(*), SUM(input_tokens), SUM(cost_usd) FROM requests WHERE host='vm1'`).Scan(&n, &in, &cost)
	if n != 2 || in != 150 || !cost.Valid {
		t.Fatalf("rows=%d in=%d cost=%v", n, in, cost)
	}
	if err := l.RecordBatch(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRecordBatchMax(t *testing.T) {
	l, _ := openTest(t, nil)
	rs := make([]core.RequestRecord, MaxBatch+1)
	for i := range rs {
		rs[i] = core.RequestRecord{ID: fmt.Sprint(i), StartedAt: time.Now()}
	}
	if err := l.RecordBatch(context.Background(), rs); err == nil {
		t.Fatal("expected error for oversized batch")
	}
	var n int
	l.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
	if n != 0 {
		t.Fatalf("rows=%d; want none written", n)
	}
	if err := l.RecordBatch(context.Background(), rs[:MaxBatch]); err != nil {
		t.Fatal(err)
	}
	l.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
	if n != MaxBatch {
		t.Fatalf("rows=%d", n)
	}
}
