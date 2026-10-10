package proxy

// Cross-feature regressions for the integrated stack: capability routing
// (per-candidate backend aliases and PricingModel), durable spend controls
// (per-attempt Reserve/Settle through the real Gate over a real SQLite budget
// store) and privacy-safe attempt observability (one event per finalized
// ledger row). Each piece is tested on its own elsewhere; these tests pin how
// they compose:
//
//   - a client alias resolved to a backend is settled at THAT backend's price,
//     in both the budget store (client and account scopes) and the real SQLite
//     ledger, never at the client alias's price;
//   - an alias failover across two differently priced backends reserves per
//     actual attempt and settles each at its own outcome;
//   - an incomplete stream on an aliased backend books max(hold, observed);
//   - the event attempt_id is the ledger row ID and the budget reservation ID,
//     and budget denials emit exactly one event without leaking model names.
//
// Every upstream is an httptest.Server on loopback; nothing here is a live
// provider or a real credential.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

const (
	stackHold     = int64(250_000)
	stackModel    = "gpt-x" // client-facing alias; deliberately the most expensive price
	stackBackendA = "backend-a-SENTINEL"
	stackBackendB = "backend-b-SENTINEL"
	stackRoute    = "cap-route-SENTINEL"
	stackBody     = `{"model":"gpt-x","input":"hi","stream":false}`
)

// stackPricing prices the client alias and both backends differently, so any
// cost booked at the wrong key is visible. For 1000 input + 500 output tokens:
// gpt-x = $0.075, backend-a = $0.004, backend-b = $0.006.
func stackPricing() *ledger.Pricing {
	return ledger.NewPricing(map[string]ledger.ModelPrice{
		stackModel:    {Input: 50, Output: 50},
		stackBackendA: {Input: 2, Output: 4},
		stackBackendB: {Input: 3, Output: 6},
	})
}

// stackForbidden are strings no completion event may carry: the client alias,
// both backend aliases, the route name and the loopback host.
var stackForbidden = []string{stackModel, stackBackendA, stackBackendB, stackRoute, "SENTINEL", "127.0.0.1"}

// stackUsage answers a successful Responses call with fixed token usage and
// records the model the proxy actually sent upstream.
func stackUsage(sent chan<- string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent <- string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r1","usage":{"input_tokens":1000,"output_tokens":500}}`)
	}
}

func stackStatus(status int, sent chan<- string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent <- string(b)
		http.Error(w, "unavailable", status)
	}
}

// stackRouteFor installs one capability-constrained route whose candidates
// each resolve the client alias to their own backend.
func stackRouteFor(h *harness, backends map[string]string, order ...string) {
	ups := map[string]core.UpstreamSpec{}
	for id, model := range backends {
		ups[id] = core.UpstreamSpec{UpstreamModel: model, Stream: true}
	}
	h.routes = []core.Route{{
		Name: stackRoute, Models: []string{stackModel}, Upstreams: ups,
		Interactive: order, Background: order,
	}}
}

// teeLedger writes every row to a real SQLite ledger first and then to the
// harness's fake, so the observability helpers (which wait on the fake) only
// see a row after it is durable in the real ledger.
type teeLedger struct {
	real *ledger.Ledger
	fake *fakeLedger
}

func (l teeLedger) Record(ctx context.Context, r core.RequestRecord) error {
	if err := l.real.Record(ctx, r); err != nil {
		return err
	}
	return l.fake.Record(ctx, r)
}
func (l teeLedger) Summary(ctx context.Context, since time.Time, group string) ([]core.UsageRow, error) {
	return l.real.Summary(ctx, since, group)
}
func (l teeLedger) Close() error { return nil }

// stackLedger opens a real SQLite ledger priced by pricing and installs it in
// front of the harness's fake ledger.
func stackLedger(t *testing.T, h *harness, pricing *ledger.Pricing) *ledger.Ledger {
	t.Helper()
	lg, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"), pricing, nil)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	h.led = teeLedger{real: lg, fake: h.ledger}
	return lg
}

// stackLimits gives the client and both accounts generous day ceilings so
// every scope is booked and reportable.
func stackLimits(accounts ...string) []budget.Limit {
	ls := []budget.Limit{{Scope: budget.ScopeClient, Key: "alice", Period: budget.PeriodDay, Micros: 10_000_000}}
	for _, a := range accounts {
		ls = append(ls, budget.Limit{Scope: budget.ScopeAccount, Key: a, Period: budget.PeriodDay, Micros: 10_000_000})
	}
	return ls
}

func usdMicros(t *testing.T, usd float64) int64 {
	t.Helper()
	m, err := budget.ReportedUSDToMicros(usd)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ledgerAccountCost returns the real ledger's summed cost for one account.
func ledgerAccountCost(t *testing.T, lg *ledger.Ledger, account string) *float64 {
	t.Helper()
	rows, err := lg.Summary(context.Background(), testNow.Add(-time.Hour), "account")
	if err != nil {
		t.Fatalf("ledger summary: %v", err)
	}
	for _, r := range rows {
		if r.Key == account {
			return r.CostUSD
		}
	}
	t.Fatalf("ledger has no row for account %q: %+v", account, rows)
	return nil
}

// assertReservationSettled proves the budget store holds a settled reservation
// under exactly id with exactly this charge: an identical settle retry is a
// no-op, while a different charge would be a conflict and an unknown id would
// be ErrUnknownReservation.
func assertReservationSettled(t *testing.T, st *budget.Store, id string, micros int64, basis string) {
	t.Helper()
	if err := st.Settle(context.Background(), budget.Settlement{ID: id, Micros: micros, Basis: basis}); err != nil {
		t.Fatalf("reservation %q not settled at %d/%s: %v", id, micros, basis, err)
	}
}

func upstreamModel(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case b := <-ch:
		return capField(t, b)
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the request")
		return ""
	}
}

func capField(t *testing.T, body string) string {
	t.Helper()
	const k = `"model":"`
	i := strings.Index(body, k)
	if i < 0 {
		t.Fatalf("upstream body has no model: %s", body)
	}
	rest := body[i+len(k):]
	return rest[:strings.IndexByte(rest, '"')]
}

// A client alias served by a capability-routed backend is charged at the
// backend's price — in the client and account budgets and in the real ledger
// — not at the client alias's (much higher) price.
func TestStackAliasSettlesAtResolvedBackendPrice(t *testing.T) {
	h, clk := newObsHarness(t)
	sentA := make(chan string, 4)
	h.upstream("a", core.ProviderOpenAICompat, stackUsage(sentA))
	h.upstream("b", core.ProviderOpenAICompat, neverHit(t))
	stackRouteFor(h, map[string]string{"a": stackBackendA, "b": stackBackendB}, "a", "b")
	pricing := stackPricing()
	lg := stackLedger(t, h, pricing)
	st, gate := openBudgetGate(t, stackLimits("a", "b"), stackHold, pricing)
	h.startBudget(gate)

	resp := h.post("/v1/responses", clientKey, stackBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := upstreamModel(t, sentA); got != stackBackendA {
		t.Fatalf("upstream model = %q, want %q", got, stackBackendA)
	}
	rows, evs := settle(t, h, clk, 1, 2)
	row := rows[0]
	if row.Model != stackModel || row.UpstreamModel != stackBackendA || row.PricingModel != stackBackendA {
		t.Fatalf("row attribution = model %q upstream %q pricing %q", row.Model, row.UpstreamModel, row.PricingModel)
	}

	want := usdMicros(t, 0.004)
	if want != 4000 {
		t.Fatalf("fixture drift: backend-a cost = %d micros, want 4000", want)
	}
	for _, sc := range []struct{ scope, key string }{{budget.ScopeClient, "alice"}, {budget.ScopeAccount, "a"}} {
		if got := snapshotInstance(t, st, sc.scope, sc.key, budget.PeriodDay); got != (budget.Snapshot{Estimated: want}) {
			t.Fatalf("%s %s/day = %+v, want {Estimated:%d} (backend price, not alias)", sc.scope, sc.key, got, want)
		}
	}
	if got := snapshotInstance(t, st, budget.ScopeAccount, "b", budget.PeriodDay); got != (budget.Snapshot{}) {
		t.Fatalf("unused account b/day = %+v, want empty", got)
	}
	if c := ledgerAccountCost(t, lg, "a"); c == nil || usdMicros(t, *c) != want {
		t.Fatalf("real ledger cost for a = %v, want %d micros", c, want)
	}
	assertReservationSettled(t, st, row.ID, want, budget.BasisEstimated)
	if got := strField(t, evs[0], "outcome"); got != "success" {
		t.Fatalf("event outcome = %q, want success", got)
	}
	assertEventHygiene(t, evs, stackForbidden)
}

// An alias failover across two differently priced backends reserves once per
// actual attempt: the failed attempt books its hold as unknown on its own
// account, the serving attempt books its own backend's price on its account,
// and the client pays both. Event, ledger row and reservation share one ID per
// attempt, linked by failover_of under one request_id.
func TestStackAliasFailoverSettlesEachBackendAtItsOwnPrice(t *testing.T) {
	h, clk := newObsHarness(t)
	sentA, sentB := make(chan string, 4), make(chan string, 4)
	h.upstream("a", core.ProviderOpenAICompat, stackStatus(http.StatusServiceUnavailable, sentA))
	h.upstream("b", core.ProviderOpenAICompat, stackUsage(sentB))
	stackRouteFor(h, map[string]string{"a": stackBackendA, "b": stackBackendB}, "a", "b")
	pricing := stackPricing()
	lg := stackLedger(t, h, pricing)
	st, gate := openBudgetGate(t, stackLimits("a", "b"), stackHold, pricing)
	h.startBudget(gate)

	resp := h.post("/v1/responses", clientKey, stackBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the failover backend", resp.StatusCode)
	}
	if a, b := upstreamModel(t, sentA), upstreamModel(t, sentB); a != stackBackendA || b != stackBackendB {
		t.Fatalf("upstream models = %q, %q; want each candidate's own backend", a, b)
	}
	rows, evs := settle(t, h, clk, 2, 4)
	if rows[0].PricingModel != stackBackendA || rows[1].PricingModel != stackBackendB {
		t.Fatalf("pricing models = %q, %q", rows[0].PricingModel, rows[1].PricingModel)
	}

	costB := usdMicros(t, 0.006)
	if costB != 6000 {
		t.Fatalf("fixture drift: backend-b cost = %d micros, want 6000", costB)
	}
	checks := []struct {
		scope, key string
		want       budget.Snapshot
	}{
		{budget.ScopeClient, "alice", budget.Snapshot{Estimated: costB, Unknown: stackHold}},
		{budget.ScopeAccount, "a", budget.Snapshot{Unknown: stackHold}},
		{budget.ScopeAccount, "b", budget.Snapshot{Estimated: costB}},
	}
	for _, c := range checks {
		if got := snapshotInstance(t, st, c.scope, c.key, budget.PeriodDay); got != c.want {
			t.Fatalf("%s %s/day = %+v, want %+v", c.scope, c.key, got, c.want)
		}
	}
	if c := ledgerAccountCost(t, lg, "a"); c != nil {
		t.Fatalf("real ledger cost for failed attempt = %v, want NULL", *c)
	}
	if c := ledgerAccountCost(t, lg, "b"); c == nil || usdMicros(t, *c) != costB {
		t.Fatalf("real ledger cost for b = %v, want %d micros", c, costB)
	}
	assertReservationSettled(t, st, rows[0].ID, stackHold, budget.BasisUnknown)
	assertReservationSettled(t, st, rows[1].ID, costB, budget.BasisEstimated)

	if got := strField(t, evs[0], "outcome"); got != "upstream_error" || !boolField(t, evs[0], "failover_eligible") {
		t.Fatalf("first event = %v, want eligible upstream_error", evs[0])
	}
	if strField(t, evs[1], "account") != "b" || strField(t, evs[1], "failover_of") != rows[0].ID {
		t.Fatalf("second event = %v, want account b linked to %s", evs[1], rows[0].ID)
	}
	assertEventHygiene(t, evs, stackForbidden)
}

// An aliased OpenRouter stream cut short by the client books
// max(hold, observed) as unknown, keeps the observation in the ledger, records
// the backend attribution, and emits one client_cancelled event.
func TestStackAliasIncompleteStreamChargesAtLeastTheHold(t *testing.T) {
	for _, tc := range []struct {
		name string
		cost string
		want int64
	}{
		{"observed below hold", "0.00001", stackHold},
		{"observed above hold", "0.9", 900_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, clk := newObsHarness(t)
			done := parkAfter(h, orUsageEvent(tc.cost))
			h.routes = []core.Route{{
				Name: stackRoute, Models: []string{"anthropic/claude-sonnet-4.5"},
				Upstreams:   map[string]core.UpstreamSpec{"or": {UpstreamModel: stackBackendA, Stream: true}},
				Interactive: []string{"or"}, Background: []string{"or"},
			}}
			pricing := stackPricing()
			lg := stackLedger(t, h, pricing)
			st, gate := openBudgetGate(t, stackLimits("or"), stackHold, pricing)
			h.startBudget(gate)

			row := cancelAfterFirstEvent(t, h, 0, done)
			rows, evs := settle(t, h, clk, 1, 2)
			if rows[0].ID != row.ID || row.PricingModel != stackBackendA || row.UsageKnown {
				t.Fatalf("row = %+v, want aliased row with unknown usage", row)
			}
			for _, sc := range []struct{ scope, key string }{{budget.ScopeClient, "alice"}, {budget.ScopeAccount, "or"}} {
				if got := snapshotInstance(t, st, sc.scope, sc.key, budget.PeriodDay); got != (budget.Snapshot{Unknown: tc.want}) {
					t.Fatalf("%s %s/day = %+v, want {Unknown:%d}", sc.scope, sc.key, got, tc.want)
				}
			}
			// The ledger keeps the provider observation; the budget floors it.
			if c := ledgerAccountCost(t, lg, "or"); c == nil {
				t.Fatal("real ledger dropped the observed provider cost")
			}
			if got := strField(t, evs[0], "outcome"); got != "client_cancelled" {
				t.Fatalf("event outcome = %q, want client_cancelled", got)
			}
			assertEventHygiene(t, evs, append(stackForbidden, "claude-sonnet"))
		})
	}
}

// A budget denial writes a ledger row, so it emits exactly one completion
// event after that row, keyed by the row's ID and carrying a dedicated
// outcome — never a model name.
func TestStackBudgetDenialEmitsOneEvent(t *testing.T) {
	t.Run("exceeded on first attempt", func(t *testing.T) {
		h, clk := newObsHarness(t)
		h.upstream("a", core.ProviderOpenAICompat, neverHit(t))
		stackRouteFor(h, map[string]string{"a": stackBackendA}, "a")
		_, gate := openBudgetGate(t, []budget.Limit{
			{Scope: budget.ScopeClient, Key: "alice", Period: budget.PeriodDay, Micros: 0},
		}, stackHold, stackPricing())
		h.startBudget(gate)

		resp := h.post("/v1/responses", clientKey, stackBody, nil)
		if resp.StatusCode != http.StatusTooManyRequests || decodeErr(t, resp).Error.Type != "budget_exceeded" {
			t.Fatalf("status = %d, want 429 budget_exceeded", resp.StatusCode)
		}
		_, evs := settle(t, h, clk, 1, 2)
		assertDenialEvent(t, evs[0], "a", "budget_exceeded")
		assertNoActualFailoverLink(t, evs[0])
		assertEventHygiene(t, evs, stackForbidden)
	})
	t.Run("exceeded on failover attempt", func(t *testing.T) {
		h, clk := newObsHarness(t)
		sentA := make(chan string, 4)
		h.upstream("a", core.ProviderOpenAICompat, stackStatus(http.StatusServiceUnavailable, sentA))
		h.upstream("b", core.ProviderOpenAICompat, neverHit(t))
		stackRouteFor(h, map[string]string{"a": stackBackendA, "b": stackBackendB}, "a", "b")
		st, gate := openBudgetGate(t, []budget.Limit{
			{Scope: budget.ScopeAccount, Key: "b", Period: budget.PeriodDay, Micros: 0},
		}, stackHold, stackPricing())
		h.startBudget(gate)

		resp := h.post("/v1/responses", clientKey, stackBody, nil)
		if resp.StatusCode != http.StatusTooManyRequests || decodeErr(t, resp).Error.Type != "budget_exceeded" {
			t.Fatalf("status = %d, want 429 budget_exceeded", resp.StatusCode)
		}
		rows, evs := settle(t, h, clk, 2, 4)
		assertDenialEvent(t, evs[1], "b", "budget_exceeded")
		if got := strField(t, evs[1], "failover_of"); got != rows[0].ID {
			t.Fatalf("denial event failover_of = %q, want %q", got, rows[0].ID)
		}
		if got := snapshotInstance(t, st, budget.ScopeClient, "alice", budget.PeriodDay); got != (budget.Snapshot{Unknown: stackHold}) {
			t.Fatalf("client/day = %+v, want only the sent attempt's hold", got)
		}
		assertEventHygiene(t, evs, stackForbidden)
	})
	t.Run("store unavailable", func(t *testing.T) {
		h, clk := newObsHarness(t)
		h.upstream("a", core.ProviderOpenAICompat, neverHit(t))
		stackRouteFor(h, map[string]string{"a": stackBackendA}, "a")
		st, gate := openBudgetGate(t, stackLimits("a"), stackHold, stackPricing())
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		h.startBudget(gate)

		resp := h.post("/v1/responses", clientKey, stackBody, nil)
		if resp.StatusCode != http.StatusServiceUnavailable || decodeErr(t, resp).Error.Type != "budget_store_error" {
			t.Fatalf("status = %d, want 503 budget_store_error", resp.StatusCode)
		}
		_, evs := settle(t, h, clk, 1, 2)
		assertDenialEvent(t, evs[0], "a", "budget_store_error")
		assertEventHygiene(t, evs, stackForbidden)
	})
	t.Run("client cancelled during reserve", func(t *testing.T) {
		h, clk := newObsHarness(t)
		h.upstream("a", core.ProviderOpenAICompat, neverHit(t))
		stackRouteFor(h, map[string]string{"a": stackBackendA}, "a")
		_, gate := openBudgetGate(t, stackLimits("a"), stackHold, stackPricing())

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := h.budgetProxy(&cancellingGate{Gate: gate, cancel: cancel})
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(stackBody)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+clientKey)
		p.Handler().ServeHTTP(httptest.NewRecorder(), req)

		rows := h.ledger.all()
		evs := routingEvents(t, h.logs.String())
		if len(rows) != 1 || len(evs) != 1 {
			t.Fatalf("rows = %d, events = %d, want 1 each:\n%s", len(rows), len(evs), h.logs.String())
		}
		if got := strField(t, evs[0], "attempt_id"); got != rows[0].ID {
			t.Fatalf("attempt_id = %q, want row %q", got, rows[0].ID)
		}
		assertLatencyMatchesLedger(t, evs[0], rows[0])
		assertDenialEvent(t, evs[0], "a", "client_cancelled")
		if clk.reads() != 2 {
			t.Fatalf("clock reads = %d, want 2", clk.reads())
		}
		assertEventHygiene(t, evs, stackForbidden)
	})
}

func assertDenialEvent(t *testing.T, ev map[string]any, account, outcome string) {
	t.Helper()
	if got := strField(t, ev, "account"); got != account {
		t.Fatalf("event account = %q, want %q", got, account)
	}
	if got := strField(t, ev, "outcome"); got != outcome {
		t.Fatalf("event outcome = %q, want %q: %v", got, outcome, ev)
	}
	if numField(t, ev, "status") != 0 || boolField(t, ev, "failover_eligible") || boolField(t, ev, "auth_retry") {
		t.Fatalf("denial event = %v, want status 0, not eligible, no auth retry", ev)
	}
}

// OpenRouter's request-scoped answers on an aliased route — a final 403
// moderation rejection and a 402 whose error body stalls — settle their hold
// as unknown and emit exactly one upstream_error event per row.
func TestStackOpenRouter402And403SettleAndEmitOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		handler  http.HandlerFunc
		status   int
		eligible bool
	}{
		{"403 moderation final", statusHandler(http.StatusForbidden), http.StatusForbidden, false},
		{"stalled 402 relayed", stalled402, http.StatusPaymentRequired, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, clk := newObsHarness(t)
			h.opts.MaxFailovers = -1
			h.opts.StreamIdleTimeout = 50 * time.Millisecond
			h.upstream("a", core.ProviderOpenRouter, tc.handler)
			stackRouteFor(h, map[string]string{"a": stackBackendA}, "a")
			st, gate := openBudgetGate(t, stackLimits("a"), stackHold, stackPricing())
			h.startBudget(gate)

			resp := h.post("/v1/chat/completions", clientKey, `{"model":"gpt-x","messages":[]}`, nil)
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			reads := 2
			if tc.status == http.StatusPaymentRequired {
				reads = 3 // the 402 reset hint reads the clock once more
			}
			rows, evs := settle(t, h, clk, 1, reads)
			if got := strField(t, evs[0], "outcome"); got != "upstream_error" || boolField(t, evs[0], "failover_eligible") != tc.eligible {
				t.Fatalf("event = %v", evs[0])
			}
			for _, sc := range []struct{ scope, key string }{{budget.ScopeClient, "alice"}, {budget.ScopeAccount, "a"}} {
				if got := snapshotInstance(t, st, sc.scope, sc.key, budget.PeriodDay); got != (budget.Snapshot{Unknown: stackHold}) {
					t.Fatalf("%s %s/day = %+v, want {Unknown:%d}", sc.scope, sc.key, got, stackHold)
				}
			}
			assertReservationSettled(t, st, rows[0].ID, stackHold, budget.BasisUnknown)
			assertEventHygiene(t, evs, stackForbidden)
		})
	}
}
