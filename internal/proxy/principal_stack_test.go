package proxy

// Multi-user spend controls through the real Gate over a real SQLite budget
// store (and a real SQLite ledger): a user's default budget is one budget
// across all of their API keys, each key keeps its own client budget, and the
// existing hold/floor/incomplete/alias-pricing semantics hold per user. Events
// carry only the opaque key id as the client — never the owner id, a token or
// any personal data.

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

// openUserBudgetGate opens a real store and a gate with a per-user default
// day ceiling and generous account limits.
func openUserBudgetGate(t *testing.T, userDay int64, pricing *ledger.Pricing, accounts ...string) (*budget.Store, *budget.Gate) {
	t.Helper()
	var limits []budget.Limit
	for _, a := range accounts {
		limits = append(limits, budget.Limit{Scope: budget.ScopeAccount, Key: a, Period: budget.PeriodDay, Micros: 10_000_000})
	}
	st, gate := openBudgetGate(t, limits, stackHold, pricing)
	g, err := gate.WithUserLimits(&userDay, nil)
	if err != nil {
		t.Fatal(err)
	}
	return st, g
}

// principalForbidden are strings no event or response may carry in these
// tests: the owner ids, tokens and the usual stack sentinels.
var principalForbidden = append([]string{puserA, puserB}, stackForbidden...)

// Two keys of one user draw on one default user budget at the resolved
// backend's price; the third request is denied although each key's own client
// budget is untouched by the other key; another user is unaffected.
func TestStackUserKeysShareUserDefaultBudget(t *testing.T) {
	h, _ := newObsHarness(t)
	auth := newPrincipalAuth()
	h.multiUser, h.authPrincipal = true, auth.authenticate
	sent := make(chan string, 8)
	h.upstream("a", core.ProviderOpenAICompat, stackUsage(sent))
	stackRouteFor(h, map[string]string{"a": stackBackendA}, "a")
	pricing := stackPricing()
	lg := stackLedger(t, h, pricing)
	const cost = int64(4000) // backend-a price for the fixed usage
	st, gate := openUserBudgetGate(t, stackHold+cost, pricing, "a")
	h.startBudget(gate)

	post := func(tok string) (int, string) {
		resp := h.post("/v1/responses", tok, stackBody, nil)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if code, _ := post(userTokenA1); code != http.StatusOK {
		t.Fatalf("A key1: %d", code)
	}
	if code, _ := post(userTokenA2); code != http.StatusOK {
		t.Fatalf("A key2: %d", code)
	}
	code, body := post(userTokenA1)
	if code != http.StatusTooManyRequests || !strings.Contains(body, "budget_exceeded") {
		t.Fatalf("A third request: %d %s, want 429 budget_exceeded", code, body)
	}
	for _, f := range principalForbidden {
		if strings.Contains(body, f) {
			t.Fatalf("denial body leaks %q: %s", f, body)
		}
	}
	if code, _ := post(userTokenB1); code != http.StatusOK {
		t.Fatalf("user B: %d", code)
	}
	h.waitHandled(4)

	if got := snapshotInstance(t, st, budget.ScopeUser, puserA, budget.PeriodDay); got != (budget.Snapshot{Estimated: 2 * cost}) {
		t.Fatalf("user A day = %+v, want {Estimated:%d} (backend price, both keys)", got, 2*cost)
	}
	if got := snapshotInstance(t, st, budget.ScopeUser, puserB, budget.PeriodDay); got != (budget.Snapshot{Estimated: cost}) {
		t.Fatalf("user B day = %+v", got)
	}
	for _, k := range []string{pkeyA1, pkeyA2} {
		if got := snapshotInstance(t, st, budget.ScopeClient, k, budget.PeriodDay); got != (budget.Snapshot{Estimated: cost}) {
			t.Fatalf("client %s day = %+v, want one request each", k, got)
		}
	}
	// The real ledger attributes rows to owner and key.
	rows, err := lg.SummaryScoped(context.Background(), testNow.Add(-time.Hour), "key", core.DataScope{UserID: puserA})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]int64{}
	for _, r := range rows {
		keys[r.Key] = r.Requests
	}
	if keys[pkeyA1] != 2 || keys[pkeyA2] != 1 || len(keys) != 2 {
		t.Fatalf("ledger rows by key for user A = %+v", rows)
	}

	evs := routingEvents(t, h.logs.String())
	if len(evs) != 4 {
		t.Fatalf("events = %d, want 4", len(evs))
	}
	assertEventHygiene(t, evs, principalForbidden)
	if got := strField(t, evs[0], "client"); got != pkeyA1 {
		t.Fatalf("event client = %q, want the opaque key id", got)
	}
	if strings.Contains(h.logs.String(), "SENTINEL-user") {
		t.Fatal("a bearer token reached the logs")
	}
}

// Concurrent requests from several keys of one user never hold more than the
// user's default allows: with room for two holds, exactly two reach upstream.
func TestStackUserBudgetConcurrentAcrossKeys(t *testing.T) {
	h := newHarness(t)
	auth := newPrincipalAuth()
	h.multiUser, h.authPrincipal = true, auth.authenticate
	gate := make(chan struct{})
	h.blockingUpstream("a", gate, nil)
	singleRoute(h, "a")
	st, g := openUserBudgetGate(t, 2*stackHold, nil, "a")
	h.startBudget(g)

	tokens := []string{userTokenA1, userTokenA2, userTokenA1, userTokenA2, userTokenA1, userTokenA2}
	var wg sync.WaitGroup
	codes := make(chan int, len(tokens))
	for _, tok := range tokens {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			resp, err := h.do("/v1/responses", tok, respBody)
			if err != nil {
				codes <- -1
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		}(tok)
	}
	// Every denial answers immediately; the two admitted requests park
	// upstream until the gate opens.
	deadline := time.Now().Add(5 * time.Second)
	for h.handled.Load() < int64(len(tokens)-2) {
		if time.Now().After(deadline) {
			t.Fatalf("handled %d, want %d denials", h.handled.Load(), len(tokens)-2)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := snapshotInstance(t, st, budget.ScopeUser, puserA, budget.PeriodDay); got != (budget.Snapshot{Reserved: 2 * stackHold}) {
		t.Fatalf("user hold while parked = %+v", got)
	}
	unblock(gate)
	wg.Wait()
	close(codes)
	var ok, denied int
	for c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			denied++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if ok != 2 || denied != len(tokens)-2 {
		t.Fatalf("ok=%d denied=%d", ok, denied)
	}
	// No pricing: each completed attempt books its hold as unknown.
	if got := snapshotInstance(t, st, budget.ScopeUser, puserA, budget.PeriodDay); got != (budget.Snapshot{Unknown: 2 * stackHold}) {
		t.Fatalf("user day after settle = %+v", got)
	}
}

// cancelAfterFirstEventAs is cancelAfterFirstEvent for a given bearer token.
func cancelAfterFirstEventAs(t *testing.T, h *harness, token string, upstreamDone <-chan struct{}) core.RequestRecord {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/chat/completions", strings.NewReader(orStreamBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 200 stream", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	for i := 0; i < 2; i++ {
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
	return h.waitRows(1)[0]
}

// An aliased OpenRouter stream a user key abandons books max(hold, observed)
// as unknown on the user's budget as well as the key's client budget.
func TestStackUserIncompleteStreamFloorsAtHold(t *testing.T) {
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
			auth := newPrincipalAuth()
			h.multiUser, h.authPrincipal = true, auth.authenticate
			done := parkAfter(h, orUsageEvent(tc.cost))
			h.routes = []core.Route{{
				Name: stackRoute, Models: []string{"anthropic/claude-sonnet-4.5"},
				Upstreams:   map[string]core.UpstreamSpec{"or": {UpstreamModel: stackBackendA, Stream: true}},
				Interactive: []string{"or"}, Background: []string{"or"},
			}}
			pricing := stackPricing()
			stackLedger(t, h, pricing)
			st, gate := openUserBudgetGate(t, 10_000_000, pricing, "or")
			h.startBudget(gate)

			row := cancelAfterFirstEventAs(t, h, userTokenA2, done)
			if row.UserID != puserA || row.KeyID != pkeyA2 || row.PricingModel != stackBackendA || row.UsageKnown {
				t.Fatalf("row = %+v", row)
			}
			_, evs := settle(t, h, clk, 1, 2)
			for _, sc := range []struct{ scope, key string }{{budget.ScopeUser, puserA}, {budget.ScopeClient, pkeyA2}} {
				if got := snapshotInstance(t, st, sc.scope, sc.key, budget.PeriodDay); got != (budget.Snapshot{Unknown: tc.want}) {
					t.Fatalf("%s/day = %+v, want {Unknown:%d}", sc.scope, got, tc.want)
				}
			}
			assertEventHygiene(t, evs, append(principalForbidden, "claude-sonnet"))
		})
	}
}
