package control

// Adversarial review tests for POST /control/v1/ingest. Each test proves a
// suspected defect and is skipped until fixed; run with LOCALROUTER_REVIEW=1
// to see it fail.

import (
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// reviewBug skips a review test documenting a known bug unless
// LOCALROUTER_REVIEW is set (then it runs and is expected to fail).
func reviewBug(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("LOCALROUTER_REVIEW") == "" {
		t.Skip("BUG: " + msg)
	}
}

// json.Decoder.Decode stops after the first value, so trailing bytes after a
// valid request (a second object, garbage, a truncated concatenation) are
// silently ignored and the request is written.
func TestReviewIngestRejectsTrailingGarbage(t *testing.T) {
	reviewBug(t, "ingest accepts trailing data after the JSON body (Decoder.Decode without EOF check)")
	f := newIngestFixture(t, true, true)
	body := `{"schema_version":1,"host":"vm1","records":[{"id":"a","account_id":"claude-max","usage_known":true,"usage":{"input_tokens":1}}]}` +
		`{"schema_version":1,"host":"vm2"} trailing-garbage`
	if rec := f.post(t, ingestKey, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; rows=%d", rec.Code, len(f.rows()))
	}
}

// Usage is only checked for >= 0. Two rows near MaxInt64 make SQLite's SUM()
// raise "integer overflow", so every /control/v1/usage summary covering them
// fails (see ledger TestReviewSummaryOverflowFromHugeUsage). One bad record
// from any ingest host poisons usage reporting for all accounts.
func TestReviewIngestRejectsImplausibleUsage(t *testing.T) {
	reviewBug(t, "ingest accepts token counts up to MaxInt64; ledger SUM overflows and breaks /usage")
	f := newIngestFixture(t, true, true)
	r := goodRecord("huge")
	r.Usage.InputTokens = math.MaxInt64
	body := core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{r}}
	if rec := f.post(t, ingestKey, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for input_tokens=MaxInt64", rec.Code)
	}
}

// IDs are only checked for non-empty: a 1 MiB ID (or one containing control
// characters) is stored as a primary key.
func TestReviewIngestBoundsRecordID(t *testing.T) {
	reviewBug(t, "ingest record ID has no length/charset limit")
	f := newIngestFixture(t, true, true)
	for _, id := range []string{strings.Repeat("x", 1<<20), "a\x00b\nc"} {
		r := goodRecord(id)
		body := core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{r}}
		if rec := f.post(t, ingestKey, body); rec.Code != http.StatusBadRequest {
			t.Errorf("id len %d: status %d, want 400", len(id), rec.Code)
		}
	}
}

// StartedAt is not validated. A zero time is stored as a large negative epoch
// and silently excluded from every since-window; a far-future one is counted
// in every "last 24h" summary until that date.
func TestReviewIngestValidatesTimestamps(t *testing.T) {
	reviewBug(t, "ingest accepts zero / far-future started_at")
	f := newIngestFixture(t, true, true)
	zero := goodRecord("zero")
	zero.StartedAt, zero.FinishedAt = time.Time{}, time.Time{}
	future := goodRecord("future")
	future.StartedAt = t0.AddDate(5, 0, 0)
	for _, r := range []core.RequestRecord{zero, future} {
		body := core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{r}}
		if rec := f.post(t, ingestKey, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", r.ID, rec.Code)
		}
	}
}

type stepClock struct{ t *time.Time }

func (c stepClock) Now() time.Time { return *c.t }

// A snapshot dated up to 5 minutes in the future is accepted and stored with
// that FetchedAt, so every genuine snapshot until then is "not newer" and
// ignored (counted in snapshots_ignored). One host whose clock runs 4m59s
// fast — or any ingest key, since snapshots are not bound to the pushing
// client — freezes the account's quota view (e.g. at used 0%) for ~5 min.
// Fix: store min(FetchedAt, server now) or compare against receive time.
func TestReviewFutureSnapshotFreezesAccount(t *testing.T) {
	reviewBug(t, "future-dated snapshot (<5m skew) is stored as-is and masks newer real snapshots")
	now := t0
	ing := &fakeIngester{snaps: map[string]core.Snapshot{}}
	h := New(Deps{
		Accounts: []core.Account{{ID: "claude-max", Provider: core.ProviderClaude, QuotaSource: "agent"}},
		Ingester: ing, Quota: ing, Clock: stepClock{&now},
		Authenticate: func(b string) (core.Client, bool) {
			return core.Client{Name: "agent", Ingest: true}, b == ingestKey
		},
	}, Options{}).Handler()
	f := &ingestFixture{h: h, ing: ing}
	push := func(at time.Time, used float64) core.IngestResponse {
		return ingestResp(t, f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1",
			Snapshots: []core.Snapshot{{AccountID: "claude-max", FetchedAt: at,
				Windows: []core.Window{{Kind: core.Window5h, UsedFrac: used}}}}}))
	}
	push(t0.Add(4*time.Minute+59*time.Second), 0) // skewed clock
	now = t0.Add(time.Minute)
	if r := push(now, 0.95); r.SnapshotsAccepted != 1 {
		t.Fatalf("real snapshot taken after the skewed one was ignored: %+v", r)
	}
}

// Snapshot windows are passed through unvalidated: used_frac outside [0,1]
// (e.g. -5) is stored, and policy then sees a huge amount of headroom and
// admits background work on a reserved account. The poll path clamps via
// clampFrac; the ingest path does not.
func TestReviewIngestValidatesSnapshotWindows(t *testing.T) {
	reviewBug(t, "ingest accepts snapshot windows with used_frac outside [0,1] / empty kind")
	f := newIngestFixture(t, true, true)
	snap := core.Snapshot{AccountID: "claude-max", FetchedAt: t0, Source: "usage_api",
		Windows: []core.Window{{Kind: core.Window5h, UsedFrac: -5}, {Kind: "", UsedFrac: 42}}}
	body := core.IngestRequest{SchemaVersion: 1, Host: "vm1", Snapshots: []core.Snapshot{snap}}
	if rec := f.post(t, ingestKey, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for used_frac -5 / 42", rec.Code)
	}
}
