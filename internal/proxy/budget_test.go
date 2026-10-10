package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

// The proxy is the only place a reservation is placed, and it must place one
// on every attempt that can spend money — including the attempt that follows a
// 401 credential refresh and every account it fails over to — then settle it
// on every path that follows. These tests drive that through Deps.Budget.

// fakeBudget is the Deps.Budget seam: it records every reserve and settle the
// proxy makes and returns programmed failures.
type fakeBudget struct {
	mu            sync.Mutex
	reserves      []core.RequestRecord
	settles       []core.RequestRecord
	settledOnCxl  []bool // settleCanceled[i]: was Settle i's context already cancelled?
	incomplete    []bool // incomplete[i]: was settle i a SettleIncomplete?
	reserveErr    error
	reserveErrFor map[string]error
	settleErr     error
	reserveCh     chan core.RequestRecord
	settleCh      chan core.RequestRecord
}

func newFakeBudget() *fakeBudget {
	return &fakeBudget{
		reserveErrFor: map[string]error{},
		reserveCh:     make(chan core.RequestRecord, 64),
		settleCh:      make(chan core.RequestRecord, 64),
	}
}

func (b *fakeBudget) Reserve(_ context.Context, rec core.RequestRecord) error {
	b.mu.Lock()
	b.reserves = append(b.reserves, rec)
	err := b.reserveErr
	if e, ok := b.reserveErrFor[rec.AccountID]; ok {
		err = e
	}
	b.mu.Unlock()
	b.reserveCh <- rec
	return err
}

func (b *fakeBudget) Settle(ctx context.Context, rec core.RequestRecord) error {
	return b.settle(ctx, rec, false)
}

func (b *fakeBudget) SettleIncomplete(ctx context.Context, rec core.RequestRecord) error {
	return b.settle(ctx, rec, true)
}

func (b *fakeBudget) settle(ctx context.Context, rec core.RequestRecord, incomplete bool) error {
	b.mu.Lock()
	b.settles = append(b.settles, rec)
	b.settledOnCxl = append(b.settledOnCxl, ctx.Err() != nil)
	b.incomplete = append(b.incomplete, incomplete)
	err := b.settleErr
	b.mu.Unlock()
	b.settleCh <- rec
	return err
}

func (b *fakeBudget) reserved() []core.RequestRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]core.RequestRecord(nil), b.reserves...)
}

func (b *fakeBudget) settled() []core.RequestRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]core.RequestRecord(nil), b.settles...)
}

func (b *fakeBudget) settledOnCancelledCtx() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bool(nil), b.settledOnCxl...)
}

func (b *fakeBudget) waitReserves(t *testing.T, n int) []core.RequestRecord {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for len(b.reserved()) < n {
		select {
		case <-b.reserveCh:
		case <-deadline:
			t.Fatalf("timed out waiting for %d reservations, saw %d", n, len(b.reserved()))
		}
	}
	return b.reserved()
}

func (b *fakeBudget) waitSettles(t *testing.T, n int) []core.RequestRecord {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for len(b.settled()) < n {
		select {
		case <-b.settleCh:
		case <-deadline:
			t.Fatalf("timed out waiting for %d settlements, saw %d", n, len(b.settled()))
		}
	}
	return b.settled()
}

// startBudget serves the harness's proxy with Budget installed. It is
// harness.start with the extra dependency, so every other harness override
// (logHandler, clock, pol, led) and the handled counter still apply.
func (h *harness) startBudget(b Budget) {
	h.budget = b
	h.start()
}

// budgetProxy builds the harness's proxy with Budget installed, for tests that
// drive the handler directly rather than through startBudget's server.
func (h *harness) budgetProxy(b Budget) *Proxy {
	h.budget = b
	return h.newProxy()
}

// acquires is the number of times the policy admitted an attempt, i.e. how
// many accounts the request tried.
func (h *harness) acquires() int { return len(h.policy.classes()) }

// invalidations is the number of times the account's credential was retired.
func (h *harness) invalidations(id string) int {
	h.creds.mu.Lock()
	defer h.creds.mu.Unlock()
	return h.creds.invalidated[id]
}

// Budget is optional: a nil Budget must leave the proxy exactly as it was.
func TestBudgetNilIsANoOp(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	h.startBudget(nil)

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if rows := h.waitRows(1); len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
}

// A denied reservation is terminal: 429 budget_exceeded, no upstream call and
// no failover to another account, which would only spend budget the client
// does not have.
func TestBudgetDenialIsTerminal429WithoutFailover(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	miss := func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }
	h.upstream("a", core.ProviderOpenAICompat, miss)
	h.upstream("b", core.ProviderOpenAICompat, miss)
	singleRoute(h, "a", "b")

	fb := newFakeBudget()
	fb.reserveErr = budget.ErrExceeded
	h.startBudget(fb)

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if e := decodeErr(t, resp); e.Error.Type != "budget_exceeded" {
		t.Fatalf("error type = %q, want budget_exceeded", e.Error.Type)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("upstream called %d times despite a denied reservation", n)
	}
	if n := h.acquires(); n != 1 {
		t.Fatalf("policy Acquire called %d times, want 1: no failover on a budget denial", n)
	}
	if rows := h.waitRows(1); rows[0].Error != "budget exceeded" {
		t.Fatalf("ledger row error = %q, want %q", rows[0].Error, "budget exceeded")
	}
	if n := len(fb.reserved()); n != 1 {
		t.Fatalf("reserve calls = %d, want 1", n)
	}
	if n := len(fb.settled()); n != 0 {
		t.Fatalf("a denied reservation was settled: %d settle calls", n)
	}
}

// A store failure is terminal too, but answers 503 budget_store_error so the
// client knows to retry rather than that it is out of budget.
func TestBudgetStoreErrorIsTerminal503(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	miss := func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }
	h.upstream("a", core.ProviderOpenAICompat, miss)
	h.upstream("b", core.ProviderOpenAICompat, miss)
	singleRoute(h, "a", "b")

	fb := newFakeBudget()
	fb.reserveErr = errors.New("budget database is locked")
	h.startBudget(fb)

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if e := decodeErr(t, resp); e.Error.Type != "budget_store_error" {
		t.Fatalf("error type = %q, want budget_store_error", e.Error.Type)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("upstream called %d times despite a budget store failure", n)
	}
	if n := h.acquires(); n != 1 {
		t.Fatalf("policy Acquire called %d times, want 1: no failover on a store error", n)
	}
	if rows := h.waitRows(1); rows[0].Error != "budget store error" {
		t.Fatalf("ledger row error = %q, want %q", rows[0].Error, "budget store error")
	}
	if n := len(fb.settled()); n != 0 {
		t.Fatalf("an unreserved attempt was settled: %d settle calls", n)
	}
}

// The attempt after a 401 credential refresh is a second upstream attempt, so
// it is reserved and settled again: two durable charges for one client request.
func TestBudgetChargesBothAuthRetryAttempts(t *testing.T) {
	h := newHarness(t)
	var n atomic.Int32
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"expired"}}`)
			return
		}
		okJSON(w, r)
	})
	singleRoute(h, "a")

	fb := newFakeBudget()
	h.startBudget(fb)

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the credential refresh", resp.StatusCode)
	}
	rows := h.waitRows(2)
	if rows[0].Status != http.StatusUnauthorized || rows[1].Status != http.StatusOK {
		t.Fatalf("rows = [%d %d], want [401 200]", rows[0].Status, rows[1].Status)
	}

	reserved := fb.waitReserves(t, 2)
	settled := fb.waitSettles(t, 2)
	for i := range reserved {
		if reserved[i].AccountID != "a" {
			t.Fatalf("reservation %d on account %q, want a", i, reserved[i].AccountID)
		}
		if settled[i].ID != reserved[i].ID {
			t.Fatalf("attempt %d: settled %q, reserved %q", i, settled[i].ID, reserved[i].ID)
		}
	}
	if n := h.invalidations("a"); n != 1 {
		t.Fatalf("credential invalidations = %d, want 1", n)
	}
}

// Every account the request fails over to is a charged attempt.
func TestBudgetChargesEachFailoverAttempt(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a", "b")

	fb := newFakeBudget()
	h.startBudget(fb)

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the failover account", resp.StatusCode)
	}
	rows := h.waitRows(2)
	if rows[0].FailoverOf != "" || rows[1].FailoverOf != rows[0].ID {
		t.Fatalf("failover chain = [%q %q], want [\"\" %q]", rows[0].FailoverOf, rows[1].FailoverOf, rows[0].ID)
	}

	reserved := fb.waitReserves(t, 2)
	if reserved[0].AccountID != "a" || reserved[1].AccountID != "b" {
		t.Fatalf("reserved accounts = [%s %s], want [a b]", reserved[0].AccountID, reserved[1].AccountID)
	}
	settled := fb.waitSettles(t, 2)
	for i := range reserved {
		if settled[i].ID != reserved[i].ID {
			t.Fatalf("attempt %d: settled %q, reserved %q", i, settled[i].ID, reserved[i].ID)
		}
	}
}

// A client that disconnects mid-stream still settles: the hold is released on
// a fresh context, not on the cancelled request context, so a cancellation can
// never leak a reservation.
func TestBudgetSettlesCancelledAttemptOnAFreshContext(t *testing.T) {
	h := newHarness(t)
	upstreamDone := make(chan struct{})
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	singleRoute(h, "a")

	fb := newFakeBudget()
	h.startBudget(fb)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-x","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("read first stream event: %v", err)
	}

	// Abandon the stream: the attempt must still be settled.
	cancel()
	_ = resp.Body.Close()

	settled := fb.waitSettles(t, 1)
	if n := len(fb.reserved()); n != 1 {
		t.Fatalf("reserve calls = %d, want 1", n)
	}
	if settled[0].Error != "client disconnected" {
		t.Fatalf("settled error = %q, want %q", settled[0].Error, "client disconnected")
	}
	if on := fb.settledOnCancelledCtx(); on[0] {
		t.Fatal("settlement ran on the cancelled request context; the hold would be lost")
	}
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never observed the client cancellation")
	}
}

// openBudgetGate opens a real budget store and the adapter gate, so the proxy
// tests can assert the durable money a request leaves behind.
func openBudgetGate(t *testing.T, limits []budget.Limit, reserveMicros int64, pricing *ledger.Pricing) (*budget.Store, *budget.Gate) {
	t.Helper()
	s, err := budget.Open(filepath.Join(t.TempDir(), "budget.db"))
	if err != nil {
		t.Fatalf("open budget store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, budget.NewGate(s, pricing, limits, reserveMicros)
}

func snapshotOf(t *testing.T, s *budget.Store, key string) budget.Snapshot {
	t.Helper()
	got, err := s.Snapshot(context.Background(), budget.ScopeClient, key, budget.PeriodDay, testNow)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return got
}

// With the real adapter installed the proxy books money the way the store
// models it: an attempt whose outcome is unknown is charged at its hold, and a
// trusted provider-reported cost is booked to the micro.
func TestBudgetGateBooksUnknownFloorAndReportedCost(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	})
	h.upstream("b", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"gen-1","usage":{"prompt_tokens":21,"completion_tokens":128,"cost":0.000123}}`)
	})
	singleRoute(h, "a", "b")

	st, gate := openBudgetGate(t, []budget.Limit{
		{Scope: budget.ScopeClient, Key: "alice", Period: budget.PeriodDay, Micros: 10_000_000},
	}, 250_000, ledger.NewPricing(nil))
	h.startBudget(gate)

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the failover account", resp.StatusCode)
	}
	h.waitRows(2)

	reported, err := budget.ReportedUSDToMicros(0.000123)
	if err != nil {
		t.Fatal(err)
	}
	want := budget.Snapshot{Reported: reported, Unknown: 250_000}
	if got := snapshotOf(t, st, "alice"); got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

// runReportedCost drives one successful request against an OpenRouter account
// that reports the given cost, and returns what the client's budget recorded.
func runReportedCost(t *testing.T, costUSD float64) budget.Snapshot {
	t.Helper()
	h := newHarness(t)
	h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"gen-1","usage":{"prompt_tokens":10,"completion_tokens":2,"cost":%v}}`, costUSD)
	})
	singleRoute(h, "or")

	st, gate := openBudgetGate(t, nil, 250_000, ledger.NewPricing(nil))
	h.startBudget(gate)

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	h.waitRows(1)
	return snapshotOf(t, st, "alice")
}

// A reported zero is a real, free outcome: the hold is released, not floored.
func TestBudgetGateReportedZeroReleasesTheHold(t *testing.T) {
	if got := runReportedCost(t, 0); got != (budget.Snapshot{}) {
		t.Fatalf("snapshot = %+v, want all zero: a reported zero must not be floored like an unknown", got)
	}
}

// A reported overrun books the full observed cost, not the hold.
func TestBudgetGateReportedOverrunIsBookedInFull(t *testing.T) {
	over, err := budget.ReportedUSDToMicros(0.9)
	if err != nil {
		t.Fatal(err)
	}
	if over <= 250_000 {
		t.Fatalf("fixture cost %d micros does not overrun the 250000 hold", over)
	}
	if got := runReportedCost(t, 0.9); got != (budget.Snapshot{Reported: over}) {
		t.Fatalf("snapshot = %+v, want {Reported:%d}", got, over)
	}
}

// With no reported cost, a known-usage record the local table prices books an
// estimated cost and releases the hold.
func TestBudgetGateBooksEstimatedCostFromThePriceTable(t *testing.T) {
	h := newHarness(t)
	h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"gen-1","usage":{"prompt_tokens":10,"completion_tokens":3}}`)
	})
	singleRoute(h, "or")

	half := 0.5
	pricing := ledger.NewPricing(map[string]ledger.ModelPrice{
		"gpt-x": {Input: 2, CachedInput: &half, Output: 10},
	})
	st, gate := openBudgetGate(t, nil, 250_000, pricing)
	h.startBudget(gate)

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	h.waitRows(1)

	cost, ok := pricing.Cost("gpt-x", core.Usage{InputTokens: 10, OutputTokens: 3})
	if !ok {
		t.Fatal("fixture model gpt-x is not priced")
	}
	estimated, err := budget.ReportedUSDToMicros(cost)
	if err != nil {
		t.Fatal(err)
	}
	if estimated <= 0 {
		t.Fatalf("fixture estimate = %d micros, want a positive charge", estimated)
	}
	want := budget.Snapshot{Estimated: estimated}
	if got := snapshotOf(t, st, "alice"); got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

// snapshotInstance reads one durable period instance's state directly.
func snapshotInstance(t *testing.T, s *budget.Store, scope, key, period string) budget.Snapshot {
	t.Helper()
	got, err := s.Snapshot(context.Background(), scope, key, period, testNow)
	if err != nil {
		t.Fatalf("Snapshot(%s/%s/%s): %v", scope, key, period, err)
	}
	return got
}

// fourInstanceSnapshots reads the four instances every admitted reservation
// tracks — the client budget and the account budget, each for the UTC day and
// the UTC month containing testNow — so a test can assert what one attempt
// durably left behind on all of them.
func fourInstanceSnapshots(t *testing.T, s *budget.Store, client, account string) map[string]budget.Snapshot {
	t.Helper()
	return map[string]budget.Snapshot{
		"client/day":    snapshotInstance(t, s, budget.ScopeClient, client, budget.PeriodDay),
		"client/month":  snapshotInstance(t, s, budget.ScopeClient, client, budget.PeriodMonth),
		"account/day":   snapshotInstance(t, s, budget.ScopeAccount, account, budget.PeriodDay),
		"account/month": snapshotInstance(t, s, budget.ScopeAccount, account, budget.PeriodMonth),
	}
}

// cancellingStreamFixture is the shared fixture for the two cancellation
// regressions below: a REAL *budget.Gate over a REAL temp-SQLite *budget.Store
// (no fakeBudget), an upstream that streams one flushed event and then parks,
// and the handle needed to disconnect the client mid-stream.
type cancellingStreamFixture struct {
	h            *harness
	store        *budget.Store
	resp         *http.Response
	cancel       context.CancelFunc
	upstreamDone chan struct{}
	hold         int64
}

// startCancellingStream wires a real Gate and starts one streaming attempt,
// returning once the upstream has flushed its first event and the attempt's
// hold is durably visible. The caller cancels the client with f.cancel() and
// then waits on f.upstreamDone and the ledger row.
func startCancellingStream(t *testing.T, pricing *ledger.Pricing, hold int64) *cancellingStreamFixture {
	t.Helper()
	h := newHarness(t)
	streamed := make(chan struct{})
	upstreamDone := make(chan struct{})
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		close(streamed)
		<-r.Context().Done()
	})
	singleRoute(h, "a")

	st, gate := openBudgetGate(t, nil, hold, pricing)
	h.startBudget(gate)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-x","stream":true}`))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-streamed:
	case <-time.After(5 * time.Second):
		cancel()
		_ = resp.Body.Close()
		t.Fatal("upstream never flushed its first stream event")
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		cancel()
		_ = resp.Body.Close()
		t.Fatalf("read first stream event: %v", err)
	}
	f := &cancellingStreamFixture{h: h, store: st, resp: resp, cancel: cancel, upstreamDone: upstreamDone, hold: hold}
	t.Cleanup(func() {
		cancel()
		_ = resp.Body.Close()
	})
	return f
}

// TestBudgetGateCancellationChargesUnknownOnceAndReleasesEveryInstance is a
// VERIFICATION regression, not a new-behaviour (TDD) test: the production
// proxy/gate path already behaves this way, so it is expected to pass as
// written. Unlike the fakeBudget-based cancellation test above, it drives the
// real budget adapter and store end to end and pins the durable money a client
// cancellation leaves behind:
//
//   - mid-flight, before the client cancels, all four tracked instances hold a
//     positive reservation and nothing has been charged;
//   - after the cancelled attempt completes on the proxy's detached settlement
//     context, every instance's Reserved is back to zero and the attempt is
//     booked ONCE as unknown spend charged at its hold (never twice, so there is
//     no duplicated reservation charge);
//   - the charge is unknown, not estimated, even though gpt-x is priced by the
//     installed table: an aborted attempt has no usage, and the absence of
//     usage must not be turned into an estimate.
func TestBudgetGateCancellationChargesUnknownOnceAndReleasesEveryInstance(t *testing.T) {
	const hold = int64(250_000)
	half := 0.5
	// gpt-x is deliberately priced so that a wrongly-estimated settlement would
	// be observable as Estimated spend.
	pricing := ledger.NewPricing(map[string]ledger.ModelPrice{
		"gpt-x": {Input: 2, CachedInput: &half, Output: 10},
	})
	f := startCancellingStream(t, pricing, hold)

	// Before the cancel, every tracked instance holds the positive reservation
	// and nothing has been charged yet.
	for name, snap := range fourInstanceSnapshots(t, f.store, "alice", "a") {
		if snap.Reserved <= 0 {
			t.Fatalf("before cancel %s: Reserved = %d, want a positive hold", name, snap.Reserved)
		}
		if snap != (budget.Snapshot{Reserved: hold}) {
			t.Fatalf("before cancel %s: snapshot = %+v, want exactly the %d hold with nothing charged", name, snap, hold)
		}
	}

	// Disconnect the client while the upstream is mid-stream.
	f.cancel()
	_ = f.resp.Body.Close()
	select {
	case <-f.upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never observed the client cancellation")
	}
	// The settlement runs before the ledger row on the same path, so this bounds
	// the cancelled attempt's completion.
	f.h.waitRows(1)

	for name, snap := range fourInstanceSnapshots(t, f.store, "alice", "a") {
		if snap.Reserved != 0 {
			t.Fatalf("after cancel %s: Reserved = %d, want 0: the detached settlement must release the hold", name, snap.Reserved)
		}
		if snap.Unknown != hold {
			t.Fatalf("after cancel %s: Unknown = %d, want exactly %d (at least the hold, with no duplicated reservation charge)", name, snap.Unknown, hold)
		}
		if snap.Estimated != 0 || snap.Reported != 0 {
			t.Fatalf("after cancel %s: snapshot = %+v, want only unknown spend: an absent usage must not be estimated", name, snap)
		}
	}
}

// TestBudgetGateSettlementContextIsDetachedFromTheCancelledClient is a
// VERIFICATION regression (expected to pass as written) for the detached
// settlement context: a client that disconnects mid-stream must still have its
// hold released, which is only possible because recordBudget settles on
// context.WithoutCancel(ctx). It uses the real Gate/store, and it includes a
// teeth check proving the assertion is meaningful: a Settle issued on an
// already-cancelled context fails in BeginTx and leaves the reservation open.
func TestBudgetGateSettlementContextIsDetachedFromTheCancelledClient(t *testing.T) {
	const hold = int64(250_000)
	f := startCancellingStream(t, nil, hold)

	f.cancel()
	_ = f.resp.Body.Close()
	select {
	case <-f.upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never observed the client cancellation")
	}
	f.h.waitRows(1)

	// The hold is released even though the request context is cancelled; had the
	// settlement used that context it would have failed and left the hold open.
	if got := snapshotInstance(t, f.store, budget.ScopeClient, "alice", budget.PeriodDay); got != (budget.Snapshot{Unknown: hold}) {
		t.Fatalf("after a cancelled client, client/day = %+v, want {Unknown:%d}: the settlement must detach from the request context", got, hold)
	}

	// Teeth: a settlement on an already-cancelled context does NOT release a hold.
	if err := f.store.Reserve(context.Background(), budget.Reservation{
		ID: "detached-context-probe", Client: "alice", Account: "a", At: testNow, Micros: hold,
	}, nil); err != nil {
		t.Fatalf("probe reserve: %v", err)
	}
	canceled, cancelProbe := context.WithCancel(context.Background())
	cancelProbe()
	if err := f.store.Settle(canceled, budget.Settlement{ID: "detached-context-probe", Micros: 0, Basis: budget.BasisUnknown}); err == nil {
		t.Fatal("Settle on an already-cancelled context succeeded; the detachment assertion above would have no teeth")
	}
	if got := snapshotInstance(t, f.store, budget.ScopeClient, "alice", budget.PeriodDay); got.Reserved != hold {
		t.Fatalf("client/day after a cancelled-context settle = %+v, want the probe hold %d still open", got, hold)
	}
}
