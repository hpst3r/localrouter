package ledger

// Tests for row ownership (schema version 5) and owner-scoped reads. A row's
// owner is the opaque internal user id of the credential that made the
// request; NULL is an unowned/legacy row, visible only to an all-users read.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// v4Schema is schema version 4 (v3 + upstream_model/pricing_model), frozen
// here so the v4 -> v5 upgrade stays tested independently of migrations[0..3].
const v4Schema = `CREATE TABLE requests (
	id TEXT PRIMARY KEY, started_at INTEGER NOT NULL, finished_at INTEGER,
	client TEXT NOT NULL, class TEXT NOT NULL, route TEXT NOT NULL,
	model TEXT NOT NULL, provider TEXT NOT NULL, account_id TEXT NOT NULL,
	upstream_identity TEXT NOT NULL, status INTEGER NOT NULL, failover_of TEXT,
	input_tokens INTEGER NOT NULL, cached_input_tokens INTEGER NOT NULL,
	output_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL,
	usage_known INTEGER NOT NULL, cost_usd REAL, cost_basis TEXT,
	latency_ms INTEGER NOT NULL, bytes_out INTEGER NOT NULL, session TEXT NOT NULL,
	task TEXT NOT NULL, agent TEXT NOT NULL, error TEXT NOT NULL,
	cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
	host TEXT NOT NULL DEFAULT '',
	upstream_model TEXT,
	pricing_model TEXT
);
CREATE INDEX requests_started_at ON requests(started_at);
CREATE INDEX requests_account_id ON requests(account_id);
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version (version) VALUES (4);
INSERT INTO requests VALUES ('old', 1, NULL, 'alice', 'background', 'r', 'gpt-x', 'p',
	'a', '', 200, NULL, 10, 2, 3, 0, 1, 0.5, 'metered', 5, 0, '', '', '', '', 7, 'vm1',
	'backend-a', 'backend-a');`

// TestMigrateV4ToV5AddsNullableOwner: a version-4 database upgrades in place to
// version 5 with nullable user_id/key_id columns and a (user_id, started_at)
// index. The legacy row keeps NULL owners — never a guess from its client
// text — and every other column is untouched.
func TestMigrateV4ToV5AddsNullableOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "localrouter.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(v4Schema); err != nil {
		t.Fatal(err)
	}
	db.Close()

	l, err := Open(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var v int
	if err := l.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != 5 || len(migrations) != 5 {
		t.Fatalf("version = %d (%v), migrations = %d; want 5/5", v, err, len(migrations))
	}
	var client, pricingModel string
	var cost float64
	var user, key sql.NullString
	if err := l.db.QueryRow(`SELECT client, cost_usd, pricing_model, user_id, key_id FROM requests WHERE id = 'old'`).
		Scan(&client, &cost, &pricingModel, &user, &key); err != nil {
		t.Fatal(err)
	}
	if client != "alice" || cost != 0.5 || pricingModel != "backend-a" {
		t.Fatalf("legacy row damaged: client=%q cost=%v pricing_model=%q", client, cost, pricingModel)
	}
	if user.Valid || key.Valid {
		t.Fatalf("legacy owner = %v/%v, want NULL/NULL", user, key)
	}
	var cols string
	if err := l.db.QueryRow(`SELECT group_concat(name) FROM pragma_index_info('requests_user_started')`).Scan(&cols); err != nil || cols != "user_id,started_at" {
		t.Fatalf("requests_user_started columns = %q (%v), want user_id,started_at", cols, err)
	}
}

// TestRecordPersistsOwner: Record, RecordBatch and a PricingView all bind the
// record's UserID/KeyID; an empty value is stored as NULL (unowned), never as
// an empty-string owner.
func TestRecordPersistsOwner(t *testing.T) {
	l, _ := openTest(t, testPricing())
	ctx := context.Background()
	ts := time.UnixMilli(1_000)
	if err := l.Record(ctx, core.RequestRecord{ID: "one", StartedAt: ts, UserID: "usr_1", KeyID: "key_1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordBatch(ctx, []core.RequestRecord{
		{ID: "batch", StartedAt: ts, UserID: "usr_2", KeyID: "key_2"},
		{ID: "unowned", StartedAt: ts, Client: "usr_1"},
	}); err != nil {
		t.Fatal(err)
	}
	v := l.WithPricing(testPricing())
	if err := v.Record(ctx, core.RequestRecord{ID: "view", StartedAt: ts, UserID: "usr_3"}); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordBatch(ctx, []core.RequestRecord{{ID: "viewbatch", StartedAt: ts, UserID: "usr_4", KeyID: "key_4"}}); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]sql.NullString{
		"one":       {{String: "usr_1", Valid: true}, {String: "key_1", Valid: true}},
		"batch":     {{String: "usr_2", Valid: true}, {String: "key_2", Valid: true}},
		"unowned":   {{}, {}},
		"view":      {{String: "usr_3", Valid: true}, {}},
		"viewbatch": {{String: "usr_4", Valid: true}, {String: "key_4", Valid: true}},
	}
	for id, w := range want {
		var user, key sql.NullString
		if err := l.db.QueryRow(`SELECT user_id, key_id FROM requests WHERE id = ?`, id).Scan(&user, &key); err != nil {
			t.Fatal(err)
		}
		if user != w[0] || key != w[1] {
			t.Fatalf("%s owner = %v/%v, want %v/%v", id, user, key, w[0], w[1])
		}
	}
}

// TestNewerLedgerSchemaRefused: a ledger written by a newer binary is refused
// and its version left untouched (migration 5 is one-way).
func TestNewerLedgerSchemaRefused(t *testing.T) {
	l, path := openTest(t, nil)
	if _, err := l.db.Exec(`UPDATE schema_version SET version = ?`, len(migrations)+1); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if l2, err := Open(path, nil, nil); err == nil {
		l2.Close()
		t.Fatal("Open of a newer schema succeeded, want refusal")
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != len(migrations)+1 {
		t.Fatalf("version after refusal = %d (%v)", v, err)
	}
}

// ownerFixture is a deterministic mix of rows owned by usr_1 (two keys), by
// usr_2, and unowned (NULL user, as legacy/static-client/collector rows are).
// usr_2 and the unowned rows deliberately dominate several dimensions so a
// leak would change rankings, top-N, __other__ and totals. Every reported
// cost is a multiple of 1/64 USD, so float sums are exact in any row order
// and results can be compared exactly.
func ownerFixture() []core.RequestRecord {
	base := time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC)
	owners := []struct{ user, key string }{
		{"usr_1", "key_1a"}, {"usr_1", "key_1b"}, {"usr_2", "key_2"}, {"", ""},
	}
	models := []string{"m-a", "m-b", "m-c", "m-d", "m-e"}
	var out []core.RequestRecord
	for i := range 120 {
		o := owners[i%len(owners)]
		r := core.RequestRecord{
			ID:        fmt.Sprintf("row-%03d", i),
			StartedAt: base.Add(time.Duration(i) * 37 * time.Minute),
			UserID:    o.user,
			KeyID:     o.key,
			Client:    []string{"shared", "alice", "bob"}[i%3],
			AccountID: []string{"acc-1", "acc-2"}[i%2],
			Class:     core.Class([]string{"interactive", "background"}[i%2]),
			Route:     []string{"r1", "r2", "r3"}[i%3],
			Host:      []string{"vm1", "vm2"}[i%2],
			Task:      []string{"proj-x", "proj-y", ""}[i%3],
			Agent:     []string{"main", "subagent"}[i%2],
			Model:     models[(i/3)%len(models)],
			Provider:  core.ProviderOpenRouter,
		}
		mult := int64(1)
		if o.user == "usr_2" || o.user == "" {
			mult = 50 // other owners dominate rankings
		}
		switch i % 5 {
		case 0, 1, 2: // known usage with a reported cost
			r.UsageKnown = true
			r.Usage = core.Usage{InputTokens: mult * int64(100+i), OutputTokens: mult * int64(10+i%7),
				CachedInputTokens: int64(i), CacheCreationInputTokens: int64(i % 4), ReasoningTokens: int64(i % 3)}
			c := float64(i%9) / 64
			r.ReportedCostUSD = &c
		case 3: // known usage, unpriced
			r.UsageKnown = true
			r.Usage = core.Usage{InputTokens: mult * 7, OutputTokens: mult * 3}
		case 4: // unknown usage
		}
		out = append(out, r)
	}
	return out
}

func rowsOf(rs []core.RequestRecord, user string) []core.RequestRecord {
	var out []core.RequestRecord
	for _, r := range rs {
		if r.UserID == user {
			out = append(out, r)
		}
	}
	return out
}

// oracleLedgers returns a ledger holding the whole fixture and one holding
// only usr_1's rows: a usr_1-scoped read of the first must equal the legacy
// unscoped read of the second, field for field.
func oracleLedgers(t *testing.T) (all, only1 *Ledger) {
	t.Helper()
	all, _ = openTest(t, nil)
	only1, _ = openTest(t, nil)
	rs := ownerFixture()
	record(t, all, rs...)
	record(t, only1, rowsOf(rs, "usr_1")...)
	return all, only1
}

var legacySummaryGroups = []string{"account", "model", "class", "client", "route", "host", "task", "agent", "day"}

// TestSummaryScopedUserSeesOnlyOwnRows: for every legacy group (including the
// Go-side day grouping) a usr_1 scope returns exactly what an unscoped Summary
// returns over usr_1's rows alone: no other user's or unowned row leaks into
// any key, count, token total, cost or unknown/unpriced counter.
func TestSummaryScopedUserSeesOnlyOwnRows(t *testing.T) {
	all, only1 := oracleLedgers(t)
	ctx := context.Background()
	since := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	for _, g := range legacySummaryGroups {
		got, err := all.SummaryScoped(ctx, since, g, core.DataScope{UserID: "usr_1"})
		if err != nil {
			t.Fatalf("%s: %v", g, err)
		}
		want, err := only1.Summary(ctx, since, g)
		if err != nil {
			t.Fatal(err)
		}
		if len(want) == 0 {
			t.Fatalf("%s: empty oracle", g)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("group %s scoped to usr_1:\n got %+v\nwant %+v", g, got, want)
		}
	}
	none, err := all.SummaryScoped(ctx, since, "model", core.DataScope{UserID: "usr_unknown"})
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown user: %+v, %v; want no rows", none, err)
	}
}

// TestSummaryScopedAllUsersAndInvalidScopes: AllUsers is exactly the legacy
// unscoped read (unowned rows included, under the "" owner key too), while a
// zero scope and a scope with both fields set are rejected with
// core.ErrInvalidScope rather than widened to all users.
func TestSummaryScopedAllUsersAndInvalidScopes(t *testing.T) {
	all, _ := oracleLedgers(t)
	ctx := context.Background()
	since := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	for _, g := range legacySummaryGroups {
		got, err := all.SummaryScoped(ctx, since, g, core.DataScope{AllUsers: true})
		if err != nil {
			t.Fatal(err)
		}
		want, err := all.Summary(ctx, since, g)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("group %s AllUsers != unscoped:\n got %+v\nwant %+v", g, got, want)
		}
	}
	for _, bad := range []core.DataScope{{}, {AllUsers: true, UserID: "usr_1"}} {
		rows, err := all.SummaryScoped(ctx, since, "model", bad)
		if !errors.Is(err, core.ErrInvalidScope) || rows != nil {
			t.Fatalf("scope %+v: rows %v err %v, want ErrInvalidScope", bad, rows, err)
		}
	}
}

// TestRequireScopeDeniesUnscopedReadsOnly: after RequireScope the legacy
// Summary and a nil-scope Analytics fail with core.ErrInvalidScope (never an
// "analytics: " validation error, never all rows), while writes and explicitly
// scoped reads keep working. The mode is per ledger instance: another ledger
// is unaffected. A PricingView over the strict ledger is strict too.
func TestRequireScopeDeniesUnscopedReadsOnly(t *testing.T) {
	strict, _ := oracleLedgers(t)
	other, _ := openTest(t, nil)
	strict.RequireScope()
	ctx := context.Background()
	since := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	q := core.AnalyticsQuery{From: since, To: since.Add(10 * 24 * time.Hour), Bucket: "day", Group: "model"}

	for _, g := range legacySummaryGroups {
		if rows, err := strict.Summary(ctx, since, g); !errors.Is(err, core.ErrInvalidScope) || rows != nil {
			t.Fatalf("strict Summary(%s) = %v, %v; want ErrInvalidScope", g, rows, err)
		}
	}
	view := strict.WithPricing(nil)
	if _, err := view.Summary(ctx, since, "model"); !errors.Is(err, core.ErrInvalidScope) {
		t.Fatalf("view Summary: %v, want ErrInvalidScope", err)
	}
	for _, a := range []core.AnalyticsLedger{strict, view} {
		_, err := a.Analytics(ctx, q)
		if !errors.Is(err, core.ErrInvalidScope) || strings.HasPrefix(err.Error(), "analytics:") {
			t.Fatalf("strict nil-scope Analytics: %v, want ErrInvalidScope without the analytics: prefix", err)
		}
	}

	if err := strict.Record(ctx, core.RequestRecord{ID: "w1", StartedAt: since, UserID: "usr_1"}); err != nil {
		t.Fatalf("strict Record: %v", err)
	}
	if err := view.RecordBatch(ctx, []core.RequestRecord{{ID: "w2", StartedAt: since}}); err != nil {
		t.Fatalf("strict view RecordBatch: %v", err)
	}
	if _, err := strict.SummaryScoped(ctx, since, "model", core.DataScope{UserID: "usr_1"}); err != nil {
		t.Fatalf("strict SummaryScoped: %v", err)
	}
	scoped := q
	scoped.Scope = &core.DataScope{AllUsers: true}
	if _, err := strict.Analytics(ctx, scoped); err != nil {
		t.Fatalf("strict scoped Analytics: %v", err)
	}
	if _, err := other.Summary(ctx, since, "model"); err != nil {
		t.Fatalf("unrelated ledger became strict: %v", err)
	}
	if _, err := other.Analytics(ctx, q); err != nil {
		t.Fatalf("unrelated ledger Analytics: %v", err)
	}
}

// TestAnalyticsUserScopeEqualsOwnRowsOnly: for every legacy dimension, both
// buckets, several top-N values (so ranking, breakdown truncation,
// BreakdownOmitted and the __other__ fold all come into play) and with and
// without filters, a usr_1-scoped Analytics equals the unscoped Analytics of a
// ledger holding only usr_1's rows. Rankings in the full fixture are dominated
// by other owners, so any leak into ranking, top-N membership, __other__,
// series points or totals changes the result.
func TestAnalyticsUserScopeEqualsOwnRowsOnly(t *testing.T) {
	all, only1 := oracleLedgers(t)
	ctx := context.Background()
	from := time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC)
	cases := 0
	for _, bucket := range []string{"hour", "day"} {
		for _, g := range core.AnalyticsDimensions {
			for _, top := range []int{1, 2, 0} {
				for _, filters := range []map[string]string{nil, {"host": "vm1"}, {"task": "", "agent": "main"}} {
					q := core.AnalyticsQuery{From: from, To: from.Add(4 * 24 * time.Hour), Bucket: bucket,
						Group: g, TopN: top, Filters: filters}
					want := mustAnalytics(t, only1, q, time.UTC)
					q.Scope = &core.DataScope{UserID: "usr_1"}
					got := mustAnalytics(t, all, q, time.UTC)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("%s/%s top=%d filters=%v:\n got %+v\nwant %+v", bucket, g, top, filters, got, want)
					}
					if want.Totals.Requests > 0 {
						cases++
					}
				}
			}
		}
	}
	if cases < 100 {
		t.Fatalf("only %d non-empty oracle cases; fixture too sparse", cases)
	}

	// AllUsers is exactly the unscoped read.
	q := core.AnalyticsQuery{From: from, To: from.Add(4 * 24 * time.Hour), Bucket: "day", Group: "model", TopN: 2}
	want := mustAnalytics(t, all, q, time.UTC)
	q.Scope = &core.DataScope{AllUsers: true}
	if got := mustAnalytics(t, all, q, time.UTC); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllUsers != unscoped:\n got %+v\nwant %+v", got, want)
	}
	// An invalid explicit scope is rejected even outside RequireScope mode.
	for _, bad := range []core.DataScope{{}, {AllUsers: true, UserID: "usr_1"}} {
		q.Scope = &bad
		if _, err := all.Analytics(ctx, q); !errors.Is(err, core.ErrInvalidScope) {
			t.Fatalf("scope %+v: %v, want ErrInvalidScope", bad, err)
		}
	}
}

// requestsBy counts fixture rows per owner field.
func requestsBy(rs []core.RequestRecord, field func(core.RequestRecord) string) map[string]int64 {
	out := map[string]int64{}
	for _, r := range rs {
		out[field(r)]++
	}
	return out
}

func analyticsRequests(res core.AnalyticsResult) map[string]int64 {
	out := map[string]int64{}
	for _, b := range res.Breakdown {
		out[b.Key] = b.Requests
	}
	return out
}

// TestOwnerDimensionsAreOptInAndScoped: "user" and "key" are accepted as a
// group or filter only on an explicitly scoped read; the legacy dimension list
// and unscoped reads are unchanged (they stay unknown there). Unowned rows
// group under "" and only an AllUsers scope sees them; a user scope sees only
// its own user and keys, and a filter naming another owner yields nothing.
func TestOwnerDimensionsAreOptInAndScoped(t *testing.T) {
	all, _ := oracleLedgers(t)
	ctx := context.Background()
	rs := ownerFixture()
	from := time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC)
	since := from
	base := core.AnalyticsQuery{From: from, To: from.Add(4 * 24 * time.Hour), Bucket: "day", TopN: 50}

	if want := []string{"host", "account", "model", "client", "class", "route", "task", "agent"}; !reflect.DeepEqual(core.AnalyticsDimensions, want) {
		t.Fatalf("legacy dimensions changed: %v", core.AnalyticsDimensions)
	}
	if !reflect.DeepEqual(AnalyticsOwnerDimensions, []string{"user", "key"}) {
		t.Fatalf("AnalyticsOwnerDimensions = %v", AnalyticsOwnerDimensions)
	}

	// Unscoped (legacy) reads keep rejecting the owner dimensions as unknown.
	for _, q := range []core.AnalyticsQuery{
		func() core.AnalyticsQuery { q := base; q.Group = "user"; return q }(),
		func() core.AnalyticsQuery {
			q := base
			q.Group = "model"
			q.Filters = map[string]string{"key": "key_1a"}
			return q
		}(),
	} {
		if _, err := all.Analytics(ctx, q); err == nil || !strings.HasPrefix(err.Error(), "analytics: unknown") {
			t.Fatalf("unscoped owner dimension %q/%v: %v, want analytics: unknown ...", q.Group, q.Filters, err)
		}
	}
	if _, err := all.Summary(ctx, since, "user"); err == nil {
		t.Fatal("legacy Summary accepted group user")
	}

	// AllUsers: every owner, unowned rows under "".
	q := base
	q.Group, q.Scope = "user", &core.DataScope{AllUsers: true}
	res := mustAnalytics(t, all, q, time.UTC)
	if got, want := analyticsRequests(res), requestsBy(rs, func(r core.RequestRecord) string { return r.UserID }); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllUsers by user = %v, want %v", got, want)
	}
	sum, err := all.SummaryScoped(ctx, since, "key", core.DataScope{AllUsers: true})
	if err != nil {
		t.Fatal(err)
	}
	gotKeys := map[string]int64{}
	for _, u := range sum {
		gotKeys[u.Key] = u.Requests
	}
	if want := requestsBy(rs, func(r core.RequestRecord) string { return r.KeyID }); !reflect.DeepEqual(gotKeys, want) {
		t.Fatalf("AllUsers summary by key = %v, want %v", gotKeys, want)
	}

	// A user scope sees only its own user and keys.
	own := rowsOf(rs, "usr_1")
	q.Group, q.Scope = "key", &core.DataScope{UserID: "usr_1"}
	res = mustAnalytics(t, all, q, time.UTC)
	if got, want := analyticsRequests(res), requestsBy(own, func(r core.RequestRecord) string { return r.KeyID }); !reflect.DeepEqual(got, want) {
		t.Fatalf("usr_1 by key = %v, want %v", got, want)
	}
	sum, err = all.SummaryScoped(ctx, since, "user", core.DataScope{UserID: "usr_1"})
	if err != nil || len(sum) != 1 || sum[0].Key != "usr_1" || sum[0].Requests != int64(len(own)) {
		t.Fatalf("usr_1 summary by user = %+v, %v", sum, err)
	}
	for _, f := range []map[string]string{{"user": "usr_2"}, {"user": ""}, {"key": "key_2"}, {"key": ""}} {
		q := base
		q.Group, q.Filters, q.Scope = "model", f, &core.DataScope{UserID: "usr_1"}
		if res := mustAnalytics(t, all, q, time.UTC); res.Totals.Requests != 0 || len(res.Series) != 0 || res.BreakdownOmitted != 0 {
			t.Fatalf("usr_1 with filter %v saw %+v", f, res.Totals)
		}
	}
	q = base
	q.Group, q.Filters, q.Scope = "model", map[string]string{"key": "key_1b"}, &core.DataScope{UserID: "usr_1"}
	if res := mustAnalytics(t, all, q, time.UTC); res.Totals.Requests != requestsBy(own, func(r core.RequestRecord) string { return r.KeyID })["key_1b"] {
		t.Fatalf("usr_1 filtered to key_1b = %d requests", res.Totals.Requests)
	}
	// Under AllUsers, user "" selects exactly the unowned rows.
	q.Filters, q.Scope = map[string]string{"user": ""}, &core.DataScope{AllUsers: true}
	if res := mustAnalytics(t, all, q, time.UTC); res.Totals.Requests != int64(len(rowsOf(rs, ""))) {
		t.Fatalf("AllUsers unowned filter = %d requests, want %d", res.Totals.Requests, len(rowsOf(rs, "")))
	}
}

// TestOwnerDimensionOracleAndBoundOwner: the key/user dimensions under a usr_1
// scope equal an AllUsers read of a ledger holding only usr_1's rows (ranking,
// top-N and __other__ included), and a hostile owner string is a bound value
// that matches nothing rather than SQL.
func TestOwnerDimensionOracleAndBoundOwner(t *testing.T) {
	all, only1 := oracleLedgers(t)
	ctx := context.Background()
	from := time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC)
	for _, g := range AnalyticsOwnerDimensions {
		for _, top := range []int{1, 2} {
			q := core.AnalyticsQuery{From: from, To: from.Add(4 * 24 * time.Hour), Bucket: "hour", Group: g, TopN: top,
				Scope: &core.DataScope{AllUsers: true}}
			want := mustAnalytics(t, only1, q, time.UTC)
			q.Scope = &core.DataScope{UserID: "usr_1"}
			if got := mustAnalytics(t, all, q, time.UTC); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s top=%d:\n got %+v\nwant %+v", g, top, got, want)
			}
		}
	}
	hostile := core.DataScope{UserID: "x' OR 1=1 OR user_id IS NULL --"}
	rows, err := all.SummaryScoped(ctx, from, "model", hostile)
	if err != nil || len(rows) != 0 {
		t.Fatalf("hostile owner summary = %v, %v; want no rows", rows, err)
	}
	q := core.AnalyticsQuery{From: from, To: from.Add(24 * time.Hour), Bucket: "day", Group: "model", Scope: &hostile}
	if res := mustAnalytics(t, all, q, time.UTC); res.Totals.Requests != 0 {
		t.Fatalf("hostile owner analytics saw %d requests", res.Totals.Requests)
	}
}

// TestMigration5IsTransactional: if migration 5 fails part-way (here its index
// name is already taken, so the last statement fails after both ALTERs ran),
// Open fails and the database is left exactly at version 4: no owner columns.
func TestMigration5IsTransactional(t *testing.T) {
	path := filepath.Join(t.TempDir(), "localrouter.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(v4Schema + `CREATE INDEX requests_user_started ON requests(started_at);`); err != nil {
		t.Fatal(err)
	}
	if l, err := Open(path, nil, nil); err == nil {
		l.Close()
		t.Fatal("Open succeeded despite a failing migration 5")
	}
	var v, ownerCols int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != 4 {
		t.Fatalf("version after failed migration = %d (%v), want 4", v, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests') WHERE name IN ('user_id', 'key_id')`).Scan(&ownerCols); err != nil || ownerCols != 0 {
		t.Fatalf("owner columns after failed migration = %d (%v), want 0", ownerCols, err)
	}
}
