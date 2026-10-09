package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

func secCollectorLedger(t *testing.T) (collectorLedger, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	led, err := ledger.Open(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })
	a := &App{}
	a.current.Store(&runtimeGeneration{pricing: led.WithPricing(nil)})
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return collectorLedger{Ledger: led, app: a}, db
}

func secRecord(id string) core.RequestRecord {
	ts := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	return core.RequestRecord{
		ID: id, StartedAt: ts, FinishedAt: ts, Client: "hermes", Class: core.ClassInteractive,
		Route: "hermes", Model: "claude-x", Provider: "anthropic", AccountID: "acct", Status: 200,
		Usage:      core.Usage{InputTokens: 10, OutputTokens: 5},
		UsageKnown: true, Session: "s", Task: "t", Agent: "main", Error: "e",
	}
}

type secRow struct{ client, class, route, model, provider, session, task, agent, errs, host string }

func secRows(t *testing.T, db *sql.DB) map[string]secRow {
	t.Helper()
	rows, err := db.Query(`SELECT id, client, class, route, model, provider, session, task, agent, error, host FROM requests`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]secRow{}
	for rows.Next() {
		var id string
		var r secRow
		if err := rows.Scan(&id, &r.client, &r.class, &r.route, &r.model, &r.provider, &r.session, &r.task, &r.agent, &r.errs, &r.host); err != nil {
			t.Fatal(err)
		}
		out[id] = r
	}
	return out
}

// Every local collector write passes through collectorLedger: labels are
// bounded and out-of-range token counts are refused, while valid records are
// stored unchanged.
func TestSecCollectorLedgerSanitizes(t *testing.T) {
	cl, db := secCollectorLedger(t)
	ctx := context.Background()
	big := strings.Repeat("x", 10<<10)

	if err := cl.Record(ctx, secRecord("valid")); err != nil {
		t.Fatal(err)
	}
	long := secRecord("long")
	long.Client, long.Class, long.Route, long.Model = big, core.Class(big), big, "m\x01"+big
	long.Provider, long.Session, long.Task, long.Agent, long.Host = big, big, big, big, big
	long.Error = big
	if err := cl.RecordBatch(ctx, []core.RequestRecord{long}); err != nil {
		t.Fatal(err)
	}
	for i, mut := range []func(*core.RequestRecord){
		func(r *core.RequestRecord) { r.Usage.InputTokens = -1 },
		func(r *core.RequestRecord) { r.Usage.OutputTokens = core.MaxRecordTokens + 1 },
		func(r *core.RequestRecord) { r.Usage.CachedInputTokens = -1 },
		func(r *core.RequestRecord) { r.Usage.CacheCreationInputTokens = core.MaxRecordTokens + 1 },
		func(r *core.RequestRecord) { r.Usage.ReasoningTokens = -1 },
	} {
		r := secRecord("bad" + string(rune('a'+i)))
		mut(&r)
		if err := cl.Record(ctx, r); err == nil {
			t.Errorf("Record accepted out-of-range usage %+v", r.Usage)
		}
		err := cl.RecordBatch(ctx, []core.RequestRecord{secRecord("ok" + string(rune('a'+i))), r})
		if err == nil {
			t.Errorf("RecordBatch accepted out-of-range usage %+v", r.Usage)
		} else if pe, ok := err.(interface{ Permanent() bool }); !ok || !pe.Permanent() {
			t.Errorf("RecordBatch error %v is not permanent; claudelog would retry forever", err)
		}
	}

	got := secRows(t, db)
	if len(got) != 2 {
		t.Fatalf("rows %d, want 2 (valid, long)", len(got))
	}
	want := secRecord("valid")
	if v := got["valid"]; v != (secRow{want.Client, string(want.Class), want.Route, want.Model, want.Provider, want.Session, want.Task, want.Agent, want.Error, want.Host}) {
		t.Errorf("valid record changed: %+v", v)
	}
	l := got["long"]
	for name, v := range map[string]string{"client": l.client, "class": l.class, "route": l.route, "model": l.model,
		"provider": l.provider, "session": l.session, "task": l.task, "agent": l.agent, "host": l.host} {
		if len(v) > core.MaxLabelBytes || strings.ContainsRune(v, '\x01') {
			t.Errorf("%s not bounded: len=%d", name, len(v))
		}
	}
	if len(l.errs) > core.MaxErrorBytes {
		t.Errorf("error not bounded: len=%d", len(l.errs))
	}
}
