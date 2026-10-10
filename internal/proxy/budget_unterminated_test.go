package proxy

// A 2xx event stream that ends with a clean EOF but without its protocol's
// terminal (chat data: [DONE]; Responses response.completed/incomplete/failed)
// was cut short somewhere upstream: any usage or cost it carried may come
// from an intermediate record. These tests drive the real Gate over a real
// SQLite store and pin that such a stream settles like any other incomplete
// relay — unknown at max(hold, observed) — while a stream that did reach its
// terminal still settles at its reported or estimated cost.

import (
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

const unterminatedErr = "upstream stream ended without completion"

// sseUpstream registers an upstream that writes body as an event stream and
// returns, so the proxy sees a clean EOF after body.
func sseUpstream(h *harness, id, provider, body string) {
	h.upstream(id, provider, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	})
}

// drain posts one request and returns its status and full body.
func drain(h *harness, path, body string) (int, string) {
	resp := h.post(path, clientKey, body, nil)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

func assertInstances(t *testing.T, st *budget.Store, account string, want budget.Snapshot) {
	t.Helper()
	for name, got := range fourInstanceSnapshots(t, st, "alice", account) {
		if got != want {
			t.Fatalf("%s = %+v, want %+v", name, got, want)
		}
	}
}

// An OpenRouter chat stream that carried usage.cost and then ended cleanly
// without data: [DONE] is relayed unchanged, keeps the observed cost in the
// ledger, but settles the budget at max(hold, observed) as unknown and reports
// one transport_error event with the relayed status.
func TestBudgetUnterminatedStreamChargesAtLeastTheHold(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cost   string
		ledger float64
		want   budget.Snapshot
	}{
		{"stale zero", "0", 0, budget.Snapshot{Unknown: incompleteHold}},
		{"partial below hold", "0.00001", 0.00001, budget.Snapshot{Unknown: incompleteHold}},
		{"above hold", "0.9", 0.9, budget.Snapshot{Unknown: 900_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, clk := newObsHarness(t)
			sseUpstream(h, "or", core.ProviderOpenRouter, orUsageEvent(tc.cost))
			orRoute(h, "or")
			st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))
			h.startBudget(gate)

			status, body := drain(h, "/v1/chat/completions", orStreamBody)
			if status != http.StatusOK || body != orUsageEvent(tc.cost) {
				t.Fatalf("relay = %d %q, want the upstream bytes unchanged", status, body)
			}
			rows, evs := settle(t, h, clk, 1, 2)
			row := rows[0]
			if row.Error != unterminatedErr || row.UsageKnown || row.Status != http.StatusOK {
				t.Fatalf("row = %+v, want %q with usage unknown and status 200", row, unterminatedErr)
			}
			if !equalCost(row.ReportedCostUSD, costPtr(tc.ledger)) {
				t.Fatalf("ledger reported cost = %v, want %v preserved", row.ReportedCostUSD, tc.ledger)
			}
			assertInstances(t, st, "or", tc.want)
			if got := strField(t, evs[0], "outcome"); got != "transport_error" {
				t.Fatalf("event outcome = %q, want transport_error", got)
			}
			if got := numField(t, evs[0], "status"); got != http.StatusOK {
				t.Fatalf("event status = %v, want 200", got)
			}
		})
	}
}

// With a client ceiling of exactly one hold, a cleanly truncated stream that
// only saw a stale zero cost uses the ceiling up, so the next attempt is
// denied instead of being admitted for free.
func TestBudgetUnterminatedStreamCannotBypassTheCeiling(t *testing.T) {
	h := newHarness(t)
	sseUpstream(h, "or", core.ProviderOpenRouter, orUsageEvent("0"))
	orRoute(h, "or")
	_, gate := openBudgetGate(t, []budget.Limit{
		{Scope: budget.ScopeClient, Key: "alice", Period: budget.PeriodDay, Micros: incompleteHold},
	}, incompleteHold, ledger.NewPricing(nil))
	h.startBudget(gate)

	if status, _ := drain(h, "/v1/chat/completions", orStreamBody); status != http.StatusOK {
		t.Fatalf("first attempt status = %d, want 200", status)
	}
	h.waitRows(1)
	if status, body := drain(h, "/v1/chat/completions", orStreamBody); status != http.StatusTooManyRequests {
		t.Fatalf("second attempt status = %d body=%q, want 429 budget_exceeded", status, body)
	}
}

// A chat chunk with finish_reason is not the chat terminal: without the
// trailing data: [DONE] the stream is still unterminated, even though that
// chunk carried usage and a cost.
func TestBudgetChatFinishReasonWithoutDoneIsIncomplete(t *testing.T) {
	h := newHarness(t)
	final := `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0}}` + "\n\n"
	sseUpstream(h, "or", core.ProviderOpenRouter, orUsageEvent("0")+final)
	orRoute(h, "or")
	st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))
	h.startBudget(gate)

	drain(h, "/v1/chat/completions", orStreamBody)
	if row := h.waitRows(1)[0]; row.Error != unterminatedErr || row.UsageKnown {
		t.Fatalf("row = %+v, want %q with usage unknown", row, unterminatedErr)
	}
	assertInstances(t, st, "or", budget.Snapshot{Unknown: incompleteHold})
}

// A Responses stream completes with response.completed and never sends a chat
// [DONE]; a reported zero there is a real, free outcome.
func TestBudgetResponsesCompletedReportedZeroReleasesTheHold(t *testing.T) {
	h := newHarness(t)
	sseUpstream(h, "or", core.ProviderOpenRouter,
		"event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n"+
			"event: response.completed\n"+
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":1,"cost":0}}}`+"\n\n")
	orRoute(h, "or")
	st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))
	h.startBudget(gate)

	drain(h, "/v1/responses", `{"model":"anthropic/claude-sonnet-4.5","input":"hi","stream":true}`)
	if row := h.waitRows(1)[0]; row.Error != "" || !row.UsageKnown || !equalCost(row.ReportedCostUSD, costPtr(0)) {
		t.Fatalf("row = %+v, want a completed stream with the reported zero", row)
	}
	assertInstances(t, st, "or", budget.Snapshot{})
}

// A Codex Responses stream that streamed output deltas and then ended without
// any terminal event is incomplete; one that reached response.completed is a
// success even if that event omitted usage (the hold is then booked as
// unknown through the normal path, without an error).
func TestBudgetCodexResponsesTerminal(t *testing.T) {
	deltas := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"
	completedNoUsage := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}` + "\n\n"
	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
		outcome string
	}{
		{"deltas then EOF", deltas, unterminatedErr, "transport_error"},
		{"completed without usage", deltas + completedNoUsage, "", "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, clk := newObsHarness(t)
			sseUpstream(h, "a", core.ProviderCodex, tc.body)
			singleRoute(h, "a")
			st, gate := openBudgetGate(t, nil, incompleteHold, stackPricing())
			h.startBudget(gate)

			drain(h, "/v1/responses", `{"model":"gpt-x","input":"hi","stream":true}`)
			rows, evs := settle(t, h, clk, 1, 2)
			if rows[0].Error != tc.wantErr || rows[0].UsageKnown {
				t.Fatalf("row = %+v, want error %q with usage unknown", rows[0], tc.wantErr)
			}
			if got := strField(t, evs[0], "outcome"); got != tc.outcome {
				t.Fatalf("event outcome = %q, want %q", got, tc.outcome)
			}
			assertInstances(t, st, "a", budget.Snapshot{Unknown: incompleteHold})
		})
	}
}

// A chat stream with continuous usage stats (usage on every chunk) is priced
// from the table only when it reached [DONE]; truncated, its latest usage is
// an intermediate count, so usage is unknown and the hold is the floor.
func TestBudgetEstimatedContinuousUsageNeedsTerminal(t *testing.T) {
	chunk := func(in, out int) string {
		return `data: {"choices":[{"delta":{"content":"x"}}],"usage":{"prompt_tokens":` +
			strconv.Itoa(in) + `,"completion_tokens":` + strconv.Itoa(out) + `}}` + "\n\n"
	}
	stream := chunk(1000, 100) + chunk(1000, 500)
	// gpt-x is priced at $50/MTok in and out: 1000 in + 500 out = $0.075.
	const estimated = int64(75_000)
	for _, tc := range []struct {
		name      string
		body      string
		wantErr   string
		wantKnown bool
		want      budget.Snapshot
	}{
		{"terminated", stream + "data: [DONE]\n\n", "", true, budget.Snapshot{Estimated: estimated}},
		{"clean EOF without DONE", stream, unterminatedErr, false, budget.Snapshot{Unknown: incompleteHold}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			sseUpstream(h, "a", core.ProviderOpenAICompat, tc.body)
			singleRoute(h, "a")
			st, gate := openBudgetGate(t, nil, incompleteHold, stackPricing())
			h.startBudget(gate)

			drain(h, "/v1/chat/completions", `{"model":"gpt-x","stream":true,"messages":[]}`)
			row := h.waitRows(1)[0]
			if row.Error != tc.wantErr || row.UsageKnown != tc.wantKnown {
				t.Fatalf("row = %+v, want error %q usage_known %v", row, tc.wantErr, tc.wantKnown)
			}
			assertInstances(t, st, "a", tc.want)
		})
	}
}

// A correctly framed upstream error is unchanged: it is an upstream_error even
// when its content type is an event stream without any terminal.
func TestBudgetErrorStatusEventStreamIsNotUnterminated(t *testing.T) {
	h, clk := newObsHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `data: {"error":{"message":"bad request"}}`+"\n\n")
	})
	singleRoute(h, "a")
	st, gate := openBudgetGate(t, nil, incompleteHold, stackPricing())
	h.startBudget(gate)

	if status, _ := drain(h, "/v1/chat/completions", `{"model":"gpt-x","stream":true,"messages":[]}`); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 relayed", status)
	}
	rows, evs := settle(t, h, clk, 1, 2)
	if rows[0].Error != "upstream 400" {
		t.Fatalf("row error = %q, want upstream 400", rows[0].Error)
	}
	if got := strField(t, evs[0], "outcome"); got != "upstream_error" {
		t.Fatalf("event outcome = %q, want upstream_error", got)
	}
	assertInstances(t, st, "a", budget.Snapshot{Unknown: incompleteHold})
}
