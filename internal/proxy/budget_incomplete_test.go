package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

// An OpenRouter stream that ends early (client disconnect, idle timeout,
// upstream read error) may already have carried a usage.cost on an
// intermediate record. The ledger keeps that observation, but the budget must
// not treat it as the final charge: it settles as unknown at
// max(hold, observed). These tests drive the real Gate over a real SQLite
// store.

const incompleteHold = int64(250_000)

const orStreamBody = `{"model":"anthropic/claude-sonnet-4.5","stream":true,"stream_options":{"include_usage":true},"messages":[]}`

// orUsageEvent is an intermediate chat chunk carrying usage with the given cost.
func orUsageEvent(cost string) string {
	return `data: {"choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":1,"cost":` + cost + "}}\n\n"
}

// parkAfter registers an OpenRouter upstream that streams event, then parks
// until the proxy cancels the upstream request.
func parkAfter(h *harness, event string) <-chan struct{} {
	done := make(chan struct{}, 8)
	h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		defer func() { done <- struct{}{} }()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, event)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	orRoute(h, "or")
	return done
}

// cancelAfterFirstEvent streams one complete SSE event to the client (so the
// proxy has already captured it), then disconnects and returns the ledger row.
func cancelAfterFirstEvent(t *testing.T, h *harness, rowsBefore int, upstreamDone <-chan struct{}) core.RequestRecord {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/chat/completions", strings.NewReader(orStreamBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d body=%q, want 200 stream", resp.StatusCode, b)
	}
	br := bufio.NewReader(resp.Body)
	for i := 0; i < 2; i++ { // data line, then the blank event terminator
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("read first event: %v", err)
		}
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never observed the client cancellation")
	}
	return h.waitRows(rowsBefore + 1)[rowsBefore]
}

func assertFourInstances(t *testing.T, st *budget.Store, want budget.Snapshot) {
	t.Helper()
	for name, got := range fourInstanceSnapshots(t, st, "alice", "or") {
		if got != want {
			t.Fatalf("%s = %+v, want %+v", name, got, want)
		}
	}
}

func TestBudgetIncompleteStreamCancelChargesAtLeastTheHold(t *testing.T) {
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
			h := newHarness(t)
			done := parkAfter(h, orUsageEvent(tc.cost))
			st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))
			h.startBudget(gate)

			row := cancelAfterFirstEvent(t, h, 0, done)
			if row.Error != "client disconnected" || row.UsageKnown {
				t.Fatalf("row = %+v, want client disconnected with usage unknown", row)
			}
			// The ledger keeps the observation for compatibility.
			if !equalCost(row.ReportedCostUSD, costPtr(tc.ledger)) {
				t.Fatalf("ledger reported cost = %v, want %v preserved", row.ReportedCostUSD, tc.ledger)
			}
			assertFourInstances(t, st, tc.want)
		})
	}
}

// With a client ceiling of exactly one hold, a cancelled stream that only saw
// a stale cost of zero still uses up the ceiling, so the next attempt is denied.
func TestBudgetIncompleteStreamCannotBypassTheCeiling(t *testing.T) {
	h := newHarness(t)
	done := parkAfter(h, orUsageEvent("0"))
	_, gate := openBudgetGate(t, []budget.Limit{
		{Scope: budget.ScopeClient, Key: "alice", Period: budget.PeriodDay, Micros: incompleteHold},
	}, incompleteHold, ledger.NewPricing(nil))
	h.startBudget(gate)

	cancelAfterFirstEvent(t, h, 0, done)
	// Judge by status alone: an admitted attempt would be a parked stream, so
	// close rather than drain it.
	resp := h.post("/v1/chat/completions", clientKey, orStreamBody, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second attempt status = %d, want 429 budget_exceeded", resp.StatusCode)
	}
}

func TestBudgetIncompleteStreamIdleTimeoutChargesAtLeastTheHold(t *testing.T) {
	h := newHarness(t)
	parkAfter(h, orUsageEvent("0"))
	h.opts.StreamIdleTimeout = 100 * time.Millisecond
	st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))
	h.startBudget(gate)

	resp := h.post("/v1/chat/completions", clientKey, orStreamBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	row := h.waitRows(1)[0]
	if row.Error != "stream idle timeout" || !equalCost(row.ReportedCostUSD, costPtr(0)) {
		t.Fatalf("row = %+v, want idle timeout with the observed zero kept", row)
	}
	assertFourInstances(t, st, budget.Snapshot{Unknown: incompleteHold})
}

func TestBudgetIncompleteStreamReadErrorChargesAtLeastTheHold(t *testing.T) {
	h := newHarness(t)
	h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// Promise more body than is sent, so the proxy's next read fails.
		_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 100000\r\n\r\n")
		_, _ = brw.WriteString(orUsageEvent("0"))
		_ = brw.Flush()
	})
	orRoute(h, "or")
	st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))
	h.startBudget(gate)

	resp := h.post("/v1/chat/completions", clientKey, orStreamBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	row := h.waitRows(1)[0]
	if !strings.Contains(row.Error, "upstream stream error") || !equalCost(row.ReportedCostUSD, costPtr(0)) {
		t.Fatalf("row = %+v, want upstream stream error with the observed zero kept", row)
	}
	assertFourInstances(t, st, budget.Snapshot{Unknown: incompleteHold})
}

// A stream that completes normally with a final reported cost of zero is a
// real, free outcome: the hold is released.
func TestBudgetCompletedStreamReportedZeroReleasesTheHold(t *testing.T) {
	h := newHarness(t)
	h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, orUsageEvent("0")+"data: [DONE]\n\n")
	})
	orRoute(h, "or")
	st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))
	h.startBudget(gate)

	resp := h.post("/v1/chat/completions", clientKey, orStreamBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if row := h.waitRows(1)[0]; row.Error != "" {
		t.Fatalf("row error = %q, want a completed stream", row.Error)
	}
	assertFourInstances(t, st, budget.Snapshot{})
}

// The reserve-failure log names a sanitized error class, never the raw store
// error, which can embed paths or DSNs.
func TestBudgetReserveErrorLogIsSanitized(t *testing.T) {
	const secret = "file:/srv/private/budget.db?_auth_pass=DSN-PASSWORD-SENTINEL"
	for _, tc := range []struct {
		name  string
		err   error
		class string
	}{
		{"opaque", errors.New("budget: reserve: open " + secret + ": disk I/O error"), "class=store"},
		{"closed", fmt.Errorf("%w: reserve %s", budget.ErrClosed, secret), "class=closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.upstream("a", core.ProviderOpenAICompat, okJSON)
			singleRoute(h, "a")
			fb := newFakeBudget()
			fb.reserveErr = tc.err
			h.startBudget(fb)

			resp := h.post("/v1/responses", clientKey, respBody, nil)
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", resp.StatusCode)
			}
			h.waitRows(1)
			logs := h.logs.String()
			if strings.Contains(logs, "DSN-PASSWORD-SENTINEL") {
				t.Fatalf("raw reserve error logged:\n%s", logs)
			}
			if !strings.Contains(logs, `msg="budget reserve failed"`) || !strings.Contains(logs, tc.class) {
				t.Fatalf("logs lack the reserve failure with %s:\n%s", tc.class, logs)
			}
		})
	}
}

// cancellingGate wraps a real Gate and cancels the request context as Reserve
// starts, so the real store sees a client that has already gone away.
type cancellingGate struct {
	*budget.Gate
	cancel context.CancelFunc
}

func (g *cancellingGate) Reserve(ctx context.Context, rec core.RequestRecord) error {
	g.cancel()
	return g.Gate.Reserve(ctx, rec)
}

// A client that disconnects during Reserve is recorded as a disconnect, not
// as a budget store outage, and gets no 503 budget_store_error.
func TestBudgetClientCancelDuringReserveIsNotAStoreError(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("a", core.ProviderOpenAICompat, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	singleRoute(h, "a")
	st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := h.budgetProxy(&cancellingGate{Gate: gate, cancel: cancel})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(respBody)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientKey)
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "budget_store_error") {
		t.Fatalf("cancelled client answered as a store outage: %d %s", w.Code, w.Body.String())
	}
	if row := h.waitRows(1)[0]; row.Error != "client disconnected" {
		t.Fatalf("row error = %q, want client disconnected", row.Error)
	}
	if logs := h.logs.String(); strings.Contains(logs, "level=ERROR") {
		t.Fatalf("client cancel logged as an error:\n%s", logs)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("upstream called %d times", n)
	}
	if got := snapshotInstance(t, st, budget.ScopeClient, "alice", budget.PeriodDay); got != (budget.Snapshot{}) {
		t.Fatalf("cancelled reserve left state %+v", got)
	}
}

// A settlement failure is logged with a sanitized class so an operator can
// tell a shutdown race from a store fault, without the store's own text.
func TestBudgetSettleFailureLogsSanitizedClass(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	fb := newFakeBudget()
	fb.settleErr = fmt.Errorf("%w: settle /srv/private/SETTLE-SENTINEL", budget.ErrClosed)
	h.startBudget(fb)

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	h.waitRows(1)
	logs := h.logs.String()
	if strings.Contains(logs, "SETTLE-SENTINEL") {
		t.Fatalf("raw settle error logged:\n%s", logs)
	}
	if !strings.Contains(logs, "budget settlement failed") || !strings.Contains(logs, "class=closed") {
		t.Fatalf("settle failure not logged with its class:\n%s", logs)
	}
}

// An OpenRouter 402 whose body stalls while it is read for affordability
// classification ends the attempt before any stream relay. The reservation
// taken for that attempt must still be settled, not left held until restart.
func TestBudgetStalled402ClassificationReadSettlesTheHold(t *testing.T) {
	h := newHarness(t)
	h.opts.MaxFailovers = 0
	h.opts.StreamIdleTimeout = 30 * time.Millisecond
	canceled := make(chan struct{})
	h.upstream("or", core.ProviderOpenRouter, stalledHandler(http.StatusPaymentRequired, `{"error":`, canceled))
	orRoute(h, "or")
	st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))
	h.startBudget(gate)

	resp := h.post("/v1/chat/completions", clientKey, orStreamBody, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want the stalled 402 relayed", resp.StatusCode)
	}
	waitClosed(t, canceled, "stalled upstream not canceled")
	if row := h.waitRows(1)[0]; row.Error != errIdleErrorBody {
		t.Fatalf("row error = %q, want %q", row.Error, errIdleErrorBody)
	}
	assertFourInstances(t, st, budget.Snapshot{Unknown: incompleteHold})
}

// OpenRouter's request-scoped answers still reached the upstream, so each
// attempt settles its hold as unknown: a 403 moderation rejection is relayed
// as final, and a 402 affordability preflight fails over to the next account.
func TestBudgetOpenRouterRequestScopedAnswersSettleTheHold(t *testing.T) {
	t.Run("403 moderation", func(t *testing.T) {
		h := newHarness(t)
		h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"input was flagged"}}`)
		})
		orRoute(h, "or")
		st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))
		h.startBudget(gate)

		resp := h.post("/v1/chat/completions", clientKey, orStreamBody, nil)
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 relayed", resp.StatusCode)
		}
		h.waitRows(1)
		assertFourInstances(t, st, budget.Snapshot{Unknown: incompleteHold})
	})
	t.Run("402 affordability failover", func(t *testing.T) {
		h := newHarness(t)
		h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, `{"error":{"code":402,"message":"This request requires more credits, or fewer max_tokens. You requested up to 4096 tokens, but can only afford 10."}}`)
		})
		h.upstream("b", core.ProviderOpenRouter, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"gen-1","usage":{"prompt_tokens":1,"completion_tokens":1,"cost":0}}`)
		})
		orRoute(h, "or", "b")
		st, gate := openBudgetGate(t, nil, incompleteHold, ledger.NewPricing(nil))
		h.startBudget(gate)

		resp := h.post("/v1/chat/completions", clientKey, orStreamBody, nil)
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 from the failover account", resp.StatusCode)
		}
		h.waitRows(2)
		if got := snapshotInstance(t, st, budget.ScopeClient, "alice", budget.PeriodDay); got != (budget.Snapshot{Unknown: incompleteHold}) {
			t.Fatalf("client/day = %+v, want only the 402 attempt's hold booked", got)
		}
		if got := snapshotInstance(t, st, budget.ScopeAccount, "or", budget.PeriodDay); got != (budget.Snapshot{Unknown: incompleteHold}) {
			t.Fatalf("account or/day = %+v, want {Unknown:%d}", got, incompleteHold)
		}
		if got := snapshotInstance(t, st, budget.ScopeAccount, "b", budget.PeriodDay); got != (budget.Snapshot{}) {
			t.Fatalf("account b/day = %+v, want the reported zero to release the hold", got)
		}
	})
}
