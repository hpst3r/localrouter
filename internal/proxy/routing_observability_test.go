package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// obsEventName is the stable value of the "event" attribute on every
// structured per-attempt completion record. The tests deliberately use the
// literal (not the production constant) so the first run fails on assertions
// rather than a compile error.
const obsEventName = "routing_attempt_completed"

// obsAllowedKeys is the exact attribute set a routing_attempt_completed record
// may carry. Anything else (model, route, session, error, url, headers, body,
// ...) is a privacy or contract regression. The legacy over-claiming key
// "failover" is intentionally absent: eligibility is reported as
// "failover_eligible", and an *actual* failover is evidenced only by the next
// completed event's failover_of + a different account.
var obsAllowedKeys = map[string]bool{
	"event": true, "request_id": true, "attempt_id": true, "failover_of": true,
	"client": true, "account": true, "provider": true, "class": true,
	"status": true, "outcome": true, "auth_retry": true, "failover_eligible": true,
	"latency_ms": true,
}

// obsEnvelopeKeys are added by slog itself to every record; they are not part
// of the event payload and are skipped by the contract check.
var obsEnvelopeKeys = map[string]bool{"time": true, "level": true, "msg": true}

func jsonLogger(buf *syncBuffer) slog.Handler {
	return slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
}

// parseLogRecords decodes every JSON log line in logs. Lines are skipped when
// they are not a JSON object (the harness may also carry text from other
// handlers).
func parseLogRecords(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

func routingEvents(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range parseLogRecords(t, logs) {
		if m["event"] == obsEventName {
			out = append(out, m)
		}
	}
	return out
}

// seqClock is a deterministic clock whose k-th read (1-based) advances time by
// k*7ms. Every read is distinct, so a latency derived from any read other than
// the ledger row's own Start/Finish pair cannot match the ledger, and reads
// are counted so an extra read is visible.
type seqClock struct {
	mu sync.Mutex
	n  int
	t  time.Time
}

func newSeqClock() *seqClock { return &seqClock{t: testNow} }

func (c *seqClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	c.t = c.t.Add(time.Duration(c.n*7) * time.Millisecond)
	return c.t
}

func (c *seqClock) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// newObsHarness returns a harness logging JSON and running on a seqClock.
func newObsHarness(t *testing.T) (*harness, *seqClock) {
	h := newHarness(t)
	h.logHandler = jsonLogger(h.logs)
	clk := newSeqClock()
	h.clock = clk
	return h, clk
}

// settle waits until the proxy handler for the test's single client request
// has returned. Every row and event is written synchronously by that handler,
// so afterwards nothing more can appear and the counts are final. It then
// asserts the exactly-once invariant (one event per ledger row, in order,
// keyed by the row ID, sharing the first row's ID as request_id, linked by the
// row's FailoverOf, with the row's nonzero latency) and that the proxy read
// the clock exactly reads times.
func settle(t *testing.T, h *harness, clk *seqClock, wantRows, reads int) ([]core.RequestRecord, []map[string]any) {
	t.Helper()
	h.waitHandled(1)
	rows := h.ledger.all()
	evs := routingEvents(t, h.logs.String())
	if len(rows) != wantRows || len(evs) != wantRows {
		t.Fatalf("ledger rows = %d, events = %d, want %d of each:\n%s", len(rows), len(evs), wantRows, h.logs.String())
	}
	for i, ev := range evs {
		row := rows[i]
		if got := strField(t, ev, "attempt_id"); got != row.ID {
			t.Fatalf("event %d attempt_id = %q, ledger row id %q", i, got, row.ID)
		}
		if got := strField(t, ev, "request_id"); got != rows[0].ID {
			t.Fatalf("event %d request_id = %q, want root %q", i, got, rows[0].ID)
		}
		got, ok := ev["failover_of"]
		if row.FailoverOf == "" && ok || row.FailoverOf != "" && got != row.FailoverOf {
			t.Fatalf("event %d failover_of = %v, ledger FailoverOf %q", i, got, row.FailoverOf)
		}
		assertLatencyMatchesLedger(t, ev, row)
	}
	if got := clk.reads(); got != reads {
		t.Fatalf("clock reads = %d, want %d", got, reads)
	}
	return rows, evs
}

// waitLogMessage waits until a record with msg=want is visible.
func waitLogMessage(t *testing.T, h *harness, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, m := range parseLogRecords(t, h.logs.String()) {
			if m["msg"] == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for log message %q:\n%s", want, h.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func strField(t *testing.T, ev map[string]any, key string) string {
	t.Helper()
	v, ok := ev[key]
	if !ok {
		t.Fatalf("event missing %q: %v", key, ev)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("event key %q = %v (%T); want string", key, v, v)
	}
	return s
}

func numField(t *testing.T, ev map[string]any, key string) float64 {
	t.Helper()
	v, ok := ev[key]
	if !ok {
		t.Fatalf("event missing %q: %v", key, ev)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("event key %q = %v (%T); want number", key, v, v)
	}
	return f
}

func boolField(t *testing.T, ev map[string]any, key string) bool {
	t.Helper()
	v, ok := ev[key]
	if !ok {
		t.Fatalf("event missing %q: %v", key, ev)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("event key %q = %v (%T); want bool", key, v, v)
	}
	return b
}

// assertLatencyMatchesLedger asserts the event's latency_ms equals the ledger
// row's LatencyMS exactly. Both are the single Finish-time derived in record();
// the event does not take a second clock read. The row latency must be
// nonzero, which a seqClock guarantees, so a constant clock cannot hide a
// mismatch.
func assertLatencyMatchesLedger(t *testing.T, ev map[string]any, row core.RequestRecord) {
	t.Helper()
	if row.LatencyMS <= 0 {
		t.Fatalf("ledger row LatencyMS = %d, want > 0 (use a seqClock)", row.LatencyMS)
	}
	got := numField(t, ev, "latency_ms")
	if got != float64(row.LatencyMS) {
		t.Fatalf("event latency_ms = %v, ledger row LatencyMS = %d: %v", got, row.LatencyMS, ev)
	}
}

// assertNoActualFailoverLink fails if the event carries an actual-failover
// linkage (failover_of) or the legacy over-claiming "failover" key.
func assertNoActualFailoverLink(t *testing.T, ev map[string]any) {
	t.Helper()
	if _, ok := ev["failover_of"]; ok {
		t.Fatalf("no actual failover should be linked, got failover_of: %v", ev)
	}
	if _, ok := ev["failover"]; ok {
		t.Fatalf("obsolete over-claiming key \"failover\" present; want failover_eligible: %v", ev)
	}
}

// assertEventHygiene fails when an event carries a key outside the contract or
// any value containing a forbidden substring (model names, body/header/error
// sentinels).
func assertEventHygiene(t *testing.T, evs []map[string]any, forbidden []string) {
	t.Helper()
	for i, ev := range evs {
		for k := range ev {
			if !obsAllowedKeys[k] && !obsEnvelopeKeys[k] {
				t.Fatalf("event %d carries disallowed key %q: %v", i, k, ev)
			}
		}
		for k, v := range ev {
			s := fmt.Sprint(v)
			for _, f := range forbidden {
				if strings.Contains(s, f) {
					t.Fatalf("event %d key %q leaked %q: %v", i, k, f, ev)
				}
			}
		}
	}
}

// Success: one attempt, outcome success, correlation IDs equal the row's.
func TestRoutingObservabilitySuccessEvent(t *testing.T) {
	h, clk := newObsHarness(t)
	h.upstream("a", core.ProviderCodex, okJSON)
	singleRoute(h, "a")
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	rows, evs := settle(t, h, clk, 1, 2)
	ev := evs[0]
	if got := strField(t, ev, "event"); got != obsEventName {
		t.Fatalf("event attr = %q", got)
	}
	if got := strField(t, ev, "attempt_id"); got != rows[0].ID {
		t.Fatalf("attempt_id = %q, row id %q", got, rows[0].ID)
	}
	if got := strField(t, ev, "request_id"); got != rows[0].ID {
		t.Fatalf("request_id = %q, want root %q", got, rows[0].ID)
	}
	if _, ok := ev["failover_of"]; ok {
		t.Fatalf("root attempt must not carry failover_of: %v", ev)
	}
	if got := strField(t, ev, "client"); got != "alice" {
		t.Fatalf("client = %q", got)
	}
	if got := strField(t, ev, "account"); got != "a" {
		t.Fatalf("account = %q", got)
	}
	if got := strField(t, ev, "provider"); got != core.ProviderCodex {
		t.Fatalf("provider = %q", got)
	}
	if got := strField(t, ev, "class"); got != string(core.ClassInteractive) {
		t.Fatalf("class = %q", got)
	}
	if got := numField(t, ev, "status"); got != 200 {
		t.Fatalf("status = %v", got)
	}
	if got := strField(t, ev, "outcome"); got != "success" {
		t.Fatalf("outcome = %q", got)
	}
	if boolField(t, ev, "auth_retry") || boolField(t, ev, "failover_eligible") {
		t.Fatalf("unexpected linkage flags: %v", ev)
	}
	assertLatencyMatchesLedger(t, ev, rows[0])
	// The client model string, the body content and the route alias must never
	// appear (alias/upstream model deferred to chunk 2 integration).
	assertEventHygiene(t, evs, []string{"gpt-x", "hi <b>", "main", "SENTINEL"})
}

// 401 refresh: two attempts on the same account, linked by failover_of.
func TestRoutingObservabilityAuthRefreshLinkage(t *testing.T) {
	h, clk := newObsHarness(t)
	var n atomic.Int32
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		okJSON(w, r)
	})
	singleRoute(h, "a")
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows, evs := settle(t, h, clk, 2, 5) // the refresh rate check reads the clock once
	first, second := evs[0], evs[1]
	if got := strField(t, first, "attempt_id"); got != rows[0].ID {
		t.Fatalf("first attempt_id %q, row %q", got, rows[0].ID)
	}
	if got := strField(t, first, "outcome"); got != "upstream_error" || numField(t, first, "status") != 401 {
		t.Fatalf("first event %v", first)
	}
	if !boolField(t, first, "auth_retry") || boolField(t, first, "failover_eligible") {
		t.Fatalf("first linkage flags %v", first)
	}
	if got := strField(t, first, "request_id"); got != rows[0].ID {
		t.Fatalf("first request_id %q, want root %q", got, rows[0].ID)
	}
	// Auth retry stays on the SAME account; the linkage is distinguished from a
	// failover by the predecessor's auth_retry=true, not by an account change.
	if strField(t, first, "account") != strField(t, second, "account") {
		t.Fatalf("auth retry must stay on the same account: %v %v", first, second)
	}
	if got := strField(t, second, "failover_of"); got != rows[0].ID {
		t.Fatalf("second failover_of %q, want %q", got, rows[0].ID)
	}
	if got := strField(t, second, "attempt_id"); got != rows[1].ID {
		t.Fatalf("second attempt_id %q, row %q", got, rows[1].ID)
	}
	if got := strField(t, second, "request_id"); got != rows[0].ID {
		t.Fatalf("second request_id %q, want root %q", got, rows[0].ID)
	}
	if got := strField(t, second, "outcome"); got != "success" {
		t.Fatalf("second outcome %q", got)
	}
	if boolField(t, second, "auth_retry") || boolField(t, second, "failover_eligible") {
		t.Fatalf("second linkage flags %v", second)
	}
	assertLatencyMatchesLedger(t, first, rows[0])
	assertLatencyMatchesLedger(t, second, rows[1])
	assertEventHygiene(t, evs, []string{"gpt-x", "hi <b>", "main"})
}

// Failover: first attempt 429 marks failover_eligible=true, second attempt on a
// different account links back via failover_of.
func TestRoutingObservabilityFailoverLinkage(t *testing.T) {
	h, clk := newObsHarness(t)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	})
	h.upstream("b", core.ProviderCodex, okJSON)
	singleRoute(h, "a", "b")
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows, evs := settle(t, h, clk, 2, 5)
	first, second := evs[0], evs[1]
	if got := strField(t, first, "account"); got != "a" || strField(t, first, "outcome") != "upstream_error" || numField(t, first, "status") != 429 {
		t.Fatalf("first event %v", first)
	}
	if !boolField(t, first, "failover_eligible") || boolField(t, first, "auth_retry") {
		t.Fatalf("first linkage flags %v", first)
	}
	if got := strField(t, second, "account"); got != "b" || strField(t, second, "outcome") != "success" || numField(t, second, "status") != 200 {
		t.Fatalf("second event %v", second)
	}
	if got := strField(t, second, "failover_of"); got != rows[0].ID {
		t.Fatalf("second failover_of %q, want %q", got, rows[0].ID)
	}
	if strField(t, first, "request_id") != rows[0].ID || strField(t, second, "request_id") != rows[0].ID {
		t.Fatalf("request ids not shared: %v %v", first, second)
	}
	if got := strField(t, second, "attempt_id"); got != rows[1].ID {
		t.Fatalf("second attempt_id %q, row %q", got, rows[1].ID)
	}
	assertLatencyMatchesLedger(t, first, rows[0])
	assertLatencyMatchesLedger(t, second, rows[1])
	assertEventHygiene(t, evs, []string{"gpt-x", "hi <b>", "main", "rate limited"})
}

// Real failover across two accounts: the *only* evidence of an actual failover
// is the next completed event carrying failover_of AND a different account.
func TestRoutingObservabilityRealFailoverAccountChange(t *testing.T) {
	h, clk := newObsHarness(t)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	})
	h.upstream("b", core.ProviderCodex, okJSON)
	singleRoute(h, "a", "b")
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	rows, evs := settle(t, h, clk, 2, 5)
	first, second := evs[0], evs[1]
	// The failing attempt claims only eligibility, never the occurrence.
	if !boolField(t, first, "failover_eligible") {
		t.Fatalf("first event not eligible: %v", first)
	}
	if _, ok := first["failover_of"]; ok {
		t.Fatalf("first event must not link back: %v", first)
	}
	// Actual failover evidence: next completed event links back and changes account.
	if got := strField(t, second, "failover_of"); got != rows[0].ID {
		t.Fatalf("second failover_of %q, want %q", got, rows[0].ID)
	}
	if strField(t, first, "account") == strField(t, second, "account") {
		t.Fatalf("failover must change account: %v %v", first, second)
	}
	assertLatencyMatchesLedger(t, first, rows[0])
	assertLatencyMatchesLedger(t, second, rows[1])
	assertEventHygiene(t, evs, []string{"gpt-x", "hi <b>", "main", "rate limited"})
}

// Single-account route, upstream 429: the attempt is failover-ELIGIBLE (budget
// permits) but no second account exists, so NO actual failover occurs —
// exactly one ledger row, exactly one event, no failover_of link, and the
// client receives the relayed 429.
func TestRoutingObservabilitySingleAccount429NoActualFailover(t *testing.T) {
	h, clk := newObsHarness(t)
	var hits atomic.Int32
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	})
	singleRoute(h, "a")
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d, want relayed 429", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits %d, want 1 (no second account exists)", hits.Load())
	}
	rows, evs := settle(t, h, clk, 1, 3)
	ev := evs[0]
	if strField(t, ev, "account") != "a" || numField(t, ev, "status") != 429 {
		t.Fatalf("event %v", ev)
	}
	if got := strField(t, ev, "outcome"); got != "upstream_error" {
		t.Fatalf("outcome %q", got)
	}
	if !boolField(t, ev, "failover_eligible") {
		t.Fatalf("failover_eligible = false, want true (budget permits): %v", ev)
	}
	assertNoActualFailoverLink(t, ev)
	assertLatencyMatchesLedger(t, ev, rows[0])
	assertEventHygiene(t, evs, []string{"gpt-x", "hi <b>", "main", "rate limited"})
}

// Two-account route where the only alternate is policy-denied: the first
// attempt is failover-ELIGIBLE but no second account is ever admitted, so no
// actual failover occurs (one row, one event, no failover_of, "b" never hit).
func TestRoutingObservabilityFailoverThenAllRemainingDenied(t *testing.T) {
	h, clk := newObsHarness(t)
	var hitsA, hitsB atomic.Int32
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	})
	h.upstream("b", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		okJSON(w, r)
	})
	singleRoute(h, "a", "b")
	h.policy.deny["b"] = true
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d, want relayed 429", resp.StatusCode)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("upstream hits a=%d b=%d, want a=1 b=0", hitsA.Load(), hitsB.Load())
	}
	rows, evs := settle(t, h, clk, 1, 3)
	ev := evs[0]
	if strField(t, ev, "account") != "a" || numField(t, ev, "status") != 429 {
		t.Fatalf("event %v", ev)
	}
	if !boolField(t, ev, "failover_eligible") {
		t.Fatalf("failover_eligible = false, want true (budget permits): %v", ev)
	}
	assertNoActualFailoverLink(t, ev)
	assertLatencyMatchesLedger(t, ev, rows[0])
	assertEventHygiene(t, evs, []string{"gpt-x", "hi <b>", "main", "rate limited"})
}

// Credential unavailable (transport_error) with a single account: eligible, but
// no account is tried next, so no actual failover — one row, one event, no link.
func TestRoutingObservabilityCredentialUnavailableNoActualFailover(t *testing.T) {
	h, clk := newObsHarness(t)
	var hits atomic.Int32
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		okJSON(w, r)
	})
	h.creds.fail["a"] = true
	singleRoute(h, "a")
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream hits %d, want 0 (no credential, no upstream call)", hits.Load())
	}
	rows, evs := settle(t, h, clk, 1, 2)
	ev := evs[0]
	if got := strField(t, ev, "outcome"); got != "transport_error" {
		t.Fatalf("outcome %q, want transport_error", got)
	}
	if numField(t, ev, "status") != 0 {
		t.Fatalf("status = %v, want 0", ev["status"])
	}
	if !boolField(t, ev, "failover_eligible") {
		t.Fatalf("failover_eligible = false, want true: %v", ev)
	}
	assertNoActualFailoverLink(t, ev)
	assertLatencyMatchesLedger(t, ev, rows[0])
	assertEventHygiene(t, evs, []string{"gpt-x", "hi <b>", "main"})
}

// Client cancel mid-stream: outcome client_cancelled.
func TestRoutingObservabilityStreamCancelEvent(t *testing.T) {
	h, clk := newObsHarness(t)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	singleRoute(h, "a")
	h.start()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-x","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "event: response.created") {
		t.Fatalf("first line %q err %v", line, err)
	}
	cancel()
	resp.Body.Close()

	rows, evs := settle(t, h, clk, 1, 2)
	ev := evs[0]
	if got := strField(t, ev, "outcome"); got != "client_cancelled" {
		t.Fatalf("outcome = %q, want client_cancelled: %v", got, ev)
	}
	if got := strField(t, ev, "client"); got != "alice" || strField(t, ev, "account") != "a" {
		t.Fatalf("attribution %v", ev)
	}
	if boolField(t, ev, "auth_retry") || boolField(t, ev, "failover_eligible") {
		t.Fatalf("linkage flags %v", ev)
	}
	assertLatencyMatchesLedger(t, ev, rows[0])
	assertEventHygiene(t, evs, []string{"gpt-x", "response.created", "main"})
}

// Denial: no upstream attempt happens, so no completion event is emitted and
// the existing policy-denied diagnostic still fires.
func TestRoutingObservabilityDenialNoEvent(t *testing.T) {
	h, clk := newObsHarness(t)
	var hits atomic.Int32
	h.upstream("a", core.ProviderCodex, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	singleRoute(h, "a")
	h.policy.deny["a"] = true
	h.start()

	resp := h.post("/v1/responses", bgKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d", resp.StatusCode)
	}
	waitLogMessage(t, h, "policy denied")
	if hits.Load() != 0 {
		t.Fatalf("upstream called %d times", hits.Load())
	}
	// No attempt: no ledger row, no completion event, no clock read.
	settle(t, h, clk, 0, 0)
}

const obsSecret = "SENTINEL-OBS-6f2a9d"

// Synthetic secrets placed in the request body, a request header and the
// provider error body must never reach a completion event (or any log line).
func TestRoutingObservabilityNoSecretsInEvents(t *testing.T) {
	h, clk := newObsHarness(t)
	var n atomic.Int32
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"`+obsSecret+`"}}`)
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"`+obsSecret+`"}}`)
		default:
			okJSON(w, r)
		}
	})
	h.upstream("b", core.ProviderCodex, okJSON)
	singleRoute(h, "a", "b")
	h.start()

	in := `{"model":"gpt-x","input":"` + obsSecret + `","stream":false}`
	resp := h.post("/v1/responses", clientKey, in, map[string]string{
		"X-LocalRouter-Session": obsSecret,
		"X-Sentinel":            obsSecret,
	})
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows, evs := settle(t, h, clk, 3, 7) // 401 refresh (+1 rate-check read) + 500 failover + success
	assertEventHygiene(t, evs, []string{obsSecret, "gpt-x", "main"})
	for i, ev := range evs {
		assertLatencyMatchesLedger(t, ev, rows[i])
	}
	if strings.Contains(h.logs.String(), obsSecret) {
		t.Fatalf("log output leaked secret:\n%s", h.logs.String())
	}
	for _, row := range rows {
		if strings.Contains(row.Error, obsSecret) {
			t.Fatalf("ledger row leaked secret: %+v", row)
		}
	}
}

// A deterministic advancing clock gives each attempt a known, nonzero
// latency. The 401 attempt starts at read 1 (7ms), takes read 2 (21ms) for the
// credential-refresh rate check and finishes at read 3 (42ms): 35ms. The retry
// spans reads 4-5 (70ms, 105ms): 35ms. A sixth read, or an event latency taken
// from any other pair of reads, fails.
func TestRoutingObservabilitySequenceClockLatency(t *testing.T) {
	h, clk := newObsHarness(t)
	var n atomic.Int32
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		okJSON(w, r)
	})
	singleRoute(h, "a")
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows, evs := settle(t, h, clk, 2, 5)
	for i, want := range []float64{35, 35} {
		if rows[i].LatencyMS != int64(want) || numField(t, evs[i], "latency_ms") != want {
			t.Fatalf("attempt %d: ledger LatencyMS = %d, event latency_ms = %v, want %v", i, rows[i].LatencyMS, evs[i]["latency_ms"], want)
		}
	}
}

// obsForbidden are values no completion event may contain: the client model
// string, route alias, request body, provider error body sentinel and raw
// transport error fragments.
var obsForbidden = []string{"gpt-x", "main", "hi <b>", obsSecret, "refused", "EOF", "deadline", "canceled", "127.0.0.1"}

type obsWant struct {
	account   string
	status    int
	outcome   string
	authRetry bool
	eligible  bool
}

type obsCase struct {
	name  string
	setup func(t *testing.T, h *harness)
	// do sends the single client request and returns the client-visible
	// status, or 0 when the client gave up. nil posts respBody.
	do         func(t *testing.T, h *harness) int
	wantStatus int
	want       []obsWant
	reads      int // proxy clock reads: 2 per attempt, plus 1 per 429/402 reset hint and per credential-refresh rate check
}

func runObsCases(t *testing.T, cases []obsCase) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, clk := newObsHarness(t)
			tc.setup(t, h)
			h.start()
			do := tc.do
			if do == nil {
				do = func(t *testing.T, h *harness) int {
					resp := h.post("/v1/responses", clientKey, respBody, nil)
					_, _ = io.ReadAll(resp.Body)
					resp.Body.Close()
					return resp.StatusCode
				}
			}
			if got := do(t, h); got != tc.wantStatus {
				t.Fatalf("client status %d, want %d", got, tc.wantStatus)
			}
			_, evs := settle(t, h, clk, len(tc.want), tc.reads)
			for i, w := range tc.want {
				ev := evs[i]
				if strField(t, ev, "account") != w.account || numField(t, ev, "status") != float64(w.status) || strField(t, ev, "outcome") != w.outcome {
					t.Fatalf("event %d = %v, want account=%s status=%d outcome=%s", i, ev, w.account, w.status, w.outcome)
				}
				if boolField(t, ev, "auth_retry") != w.authRetry || boolField(t, ev, "failover_eligible") != w.eligible {
					t.Fatalf("event %d flags = %v, want auth_retry=%v failover_eligible=%v", i, ev, w.authRetry, w.eligible)
				}
			}
			assertEventHygiene(t, evs, obsForbidden)
		})
	}
}

// statusHandler answers status with a provider error body carrying obsSecret.
func statusHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"`+obsSecret+`"}}`)
	}
}

// neverHit fails the test if the upstream is contacted.
func neverHit(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream must not be contacted")
		w.WriteHeader(http.StatusTeapot)
	}
}

// closedUpstream registers an account whose loopback server is already shut,
// so dialing it fails.
func closedUpstream(t *testing.T, h *harness, id string) {
	h.upstream(id, core.ProviderCodex, neverHit(t)).Close()
}

// With the failover budget spent, every failing attempt reports
// failover_eligible=false, whatever the failure kind, and no further account
// is contacted.
func TestRoutingObservabilityFailoverBudgetExhausted(t *testing.T) {
	runObsCases(t, []obsCase{
		{
			name: "retryable_status_streamed",
			setup: func(t *testing.T, h *harness) {
				h.opts.MaxFailovers = -1
				h.upstream("a", core.ProviderCodex, statusHandler(http.StatusTooManyRequests))
				h.upstream("b", core.ProviderCodex, neverHit(t))
				singleRoute(h, "a", "b")
			},
			wantStatus: http.StatusTooManyRequests,
			want:       []obsWant{{"a", 429, "upstream_error", false, false}},
			reads:      3,
		},
		{
			name: "budget_spent_mid_request",
			setup: func(t *testing.T, h *harness) {
				h.opts.MaxFailovers = 1
				h.upstream("a", core.ProviderCodex, statusHandler(http.StatusTooManyRequests))
				h.upstream("b", core.ProviderCodex, statusHandler(http.StatusServiceUnavailable))
				h.upstream("c", core.ProviderCodex, neverHit(t))
				singleRoute(h, "a", "b", "c")
			},
			wantStatus: http.StatusServiceUnavailable,
			want: []obsWant{
				{"a", 429, "upstream_error", false, true},
				{"b", 503, "upstream_error", false, false},
			},
			reads: 5,
		},
		{
			name: "transport_error",
			setup: func(t *testing.T, h *harness) {
				h.opts.MaxFailovers = -1
				closedUpstream(t, h, "a")
				h.upstream("b", core.ProviderCodex, neverHit(t))
				singleRoute(h, "a", "b")
			},
			wantStatus: http.StatusBadGateway,
			want:       []obsWant{{"a", 0, "transport_error", false, false}},
			reads:      2,
		},
		{
			name: "credential_unavailable",
			setup: func(t *testing.T, h *harness) {
				h.opts.MaxFailovers = -1
				h.creds.fail["a"] = true
				h.upstream("a", core.ProviderCodex, neverHit(t))
				h.upstream("b", core.ProviderCodex, neverHit(t))
				singleRoute(h, "a", "b")
			},
			wantStatus: http.StatusBadGateway,
			want:       []obsWant{{"a", 0, "transport_error", false, false}},
			reads:      2,
		},
		{
			name: "second_401_streamed",
			setup: func(t *testing.T, h *harness) {
				h.opts.MaxFailovers = -1
				h.upstream("a", core.ProviderCodex, statusHandler(http.StatusUnauthorized))
				h.upstream("b", core.ProviderCodex, neverHit(t))
				singleRoute(h, "a", "b")
			},
			wantStatus: http.StatusUnauthorized,
			want: []obsWant{
				{"a", 401, "upstream_error", true, false},
				{"a", 401, "upstream_error", false, false},
			},
			reads: 5,
		},
	})
}

// Every terminal branch that writes a ledger row emits exactly one event for
// it, checked only after the proxy handler has returned so a late duplicate
// cannot slip past.
func TestRoutingObservabilityExactlyOncePerTerminalPath(t *testing.T) {
	blockUntilCancelled := func(arrived chan<- struct{}) http.HandlerFunc {
		return func(_ http.ResponseWriter, r *http.Request) {
			// net/http only watches for a peer close once the body is consumed.
			_, _ = io.Copy(io.Discard, r.Body)
			if arrived != nil {
				arrived <- struct{}{}
			}
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
	sseThenBlock := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	arrived := make(chan struct{}, 1)
	runObsCases(t, []obsCase{
		{
			name: "client_cancel_before_headers",
			setup: func(t *testing.T, h *harness) {
				h.upstream("a", core.ProviderCodex, blockUntilCancelled(arrived))
				singleRoute(h, "a")
			},
			do: func(t *testing.T, h *harness) int {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/responses", strings.NewReader(respBody))
				req.Header.Set("Authorization", "Bearer "+clientKey)
				errc := make(chan error, 1)
				go func() {
					resp, err := http.DefaultClient.Do(req)
					if err == nil {
						resp.Body.Close()
					}
					errc <- err
				}()
				select {
				case <-arrived:
				case <-time.After(5 * time.Second):
					t.Fatal("upstream never received the request")
				}
				cancel()
				if err := <-errc; err == nil {
					t.Fatal("cancelled client request succeeded")
				}
				return 0
			},
			wantStatus: 0,
			want:       []obsWant{{"a", 0, "client_cancelled", false, false}},
			reads:      2,
		},
		{
			name: "response_header_timeout",
			setup: func(t *testing.T, h *harness) {
				h.opts.ResponseHeaderTimeout = 100 * time.Millisecond
				h.upstream("a", core.ProviderCodex, blockUntilCancelled(nil))
				singleRoute(h, "a")
			},
			wantStatus: http.StatusBadGateway,
			want:       []obsWant{{"a", 0, "transport_error", false, true}},
			reads:      2,
		},
		{
			name: "dial_error",
			setup: func(t *testing.T, h *harness) {
				closedUpstream(t, h, "a")
				singleRoute(h, "a")
			},
			wantStatus: http.StatusBadGateway,
			want:       []obsWant{{"a", 0, "transport_error", false, true}},
			reads:      2,
		},
		{
			name: "stream_idle_timeout",
			setup: func(t *testing.T, h *harness) {
				h.opts.StreamIdleTimeout = 100 * time.Millisecond
				h.upstream("a", core.ProviderCodex, sseThenBlock)
				singleRoute(h, "a")
			},
			wantStatus: http.StatusOK,
			want:       []obsWant{{"a", 200, "transport_error", false, false}},
			reads:      2,
		},
		{
			name: "stream_read_error",
			setup: func(t *testing.T, h *harness) {
				h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, _ *http.Request) {
					conn, brw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("hijack: %v", err)
						return
					}
					defer conn.Close()
					// Promise more bytes than are sent, then close.
					_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 100000\r\n\r\n")
					_, _ = brw.WriteString("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
					_ = brw.Flush()
				})
				singleRoute(h, "a")
			},
			wantStatus: http.StatusOK,
			want:       []obsWant{{"a", 200, "transport_error", false, false}},
			reads:      2,
		},
		{
			name: "nonretryable_4xx",
			setup: func(t *testing.T, h *harness) {
				h.upstream("a", core.ProviderCodex, statusHandler(http.StatusBadRequest))
				h.upstream("b", core.ProviderCodex, neverHit(t))
				singleRoute(h, "a", "b")
			},
			wantStatus: http.StatusBadRequest,
			want:       []obsWant{{"a", 400, "upstream_error", false, false}},
			reads:      2,
		},
		{
			name: "second_401_fails_over",
			setup: func(t *testing.T, h *harness) {
				h.upstream("a", core.ProviderCodex, statusHandler(http.StatusUnauthorized))
				h.upstream("b", core.ProviderCodex, okJSON)
				singleRoute(h, "a", "b")
			},
			wantStatus: http.StatusOK,
			want: []obsWant{
				{"a", 401, "upstream_error", true, false},
				{"a", 401, "upstream_error", false, true},
				{"b", 200, "success", false, false},
			},
			reads: 7,
		},
		{
			name: "second_401_no_alternate",
			setup: func(t *testing.T, h *harness) {
				h.upstream("a", core.ProviderCodex, statusHandler(http.StatusForbidden))
				singleRoute(h, "a")
			},
			wantStatus: http.StatusForbidden,
			want: []obsWant{
				{"a", 403, "upstream_error", true, false},
				{"a", 403, "upstream_error", false, true},
			},
			reads: 5,
		},
	})
}

// orChat posts an OpenRouter-style chat request and returns the client status.
func orChat(t *testing.T, h *harness) int {
	resp := h.post("/v1/chat/completions", clientKey, `{"model":"gpt-x","max_tokens":999999999,"messages":[]}`, nil)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// afford402 answers OpenRouter's request-scoped affordability preflight 402.
func afford402(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = io.WriteString(w, `{"error":{"code":402,"message":"This request requires more credits, or fewer max_tokens. You requested up to 999999999 tokens, but can only afford 5. `+obsSecret+`"}}`)
}

// stalled402 answers 402 and stalls the body until the proxy gives up on it.
func stalled402(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = io.WriteString(w, `{"error":`)
	w.(http.Flusher).Flush()
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

// OpenRouter 402/403 handling (affordability, moderation, stalled error body)
// keeps the exactly-once contract: every ledger row has one event, a final
// 403 is never eligible, and a 402 is eligible only with failover budget left.
// A 402 reads the clock once more for its reset hint.
func TestRoutingObservabilityOpenRouter402And403(t *testing.T) {
	runObsCases(t, []obsCase{
		{
			name: "moderation_403_final",
			setup: func(t *testing.T, h *harness) {
				h.upstream("a", core.ProviderOpenRouter, statusHandler(http.StatusForbidden))
				h.upstream("b", core.ProviderOpenRouter, neverHit(t))
				singleRoute(h, "a", "b")
			},
			do:         orChat,
			wantStatus: http.StatusForbidden,
			want:       []obsWant{{"a", 403, "upstream_error", false, false}},
			reads:      2,
		},
		{
			name: "affordability_402_fails_over",
			setup: func(t *testing.T, h *harness) {
				h.upstream("a", core.ProviderOpenRouter, afford402)
				h.upstream("b", core.ProviderOpenRouter, okJSON)
				singleRoute(h, "a", "b")
			},
			do:         orChat,
			wantStatus: http.StatusOK,
			want: []obsWant{
				{"a", 402, "upstream_error", false, true},
				{"b", 200, "success", false, false},
			},
			reads: 5,
		},
		{
			name: "affordability_402_budget_exhausted",
			setup: func(t *testing.T, h *harness) {
				h.opts.MaxFailovers = -1
				h.upstream("a", core.ProviderOpenRouter, afford402)
				h.upstream("b", core.ProviderOpenRouter, neverHit(t))
				singleRoute(h, "a", "b")
			},
			do:         orChat,
			wantStatus: http.StatusPaymentRequired,
			want:       []obsWant{{"a", 402, "upstream_error", false, false}},
			reads:      3,
		},
		{
			name: "account_402_fails_over",
			setup: func(t *testing.T, h *harness) {
				h.upstream("a", core.ProviderOpenRouter, statusHandler(http.StatusPaymentRequired))
				h.upstream("b", core.ProviderOpenRouter, okJSON)
				singleRoute(h, "a", "b")
			},
			do:         orChat,
			wantStatus: http.StatusOK,
			want: []obsWant{
				{"a", 402, "upstream_error", false, true},
				{"b", 200, "success", false, false},
			},
			reads: 5,
		},
		{
			name: "stalled_402_body_fails_over",
			setup: func(t *testing.T, h *harness) {
				h.opts.StreamIdleTimeout = 50 * time.Millisecond
				h.upstream("a", core.ProviderOpenRouter, stalled402)
				h.upstream("b", core.ProviderOpenRouter, okJSON)
				singleRoute(h, "a", "b")
			},
			do:         orChat,
			wantStatus: http.StatusOK,
			want: []obsWant{
				{"a", 402, "upstream_error", false, true},
				{"b", 200, "success", false, false},
			},
			reads: 5,
		},
		{
			name: "stalled_402_body_relayed",
			setup: func(t *testing.T, h *harness) {
				h.opts.MaxFailovers = -1
				h.opts.StreamIdleTimeout = 50 * time.Millisecond
				h.upstream("a", core.ProviderOpenRouter, stalled402)
				h.upstream("b", core.ProviderOpenRouter, neverHit(t))
				singleRoute(h, "a", "b")
			},
			do:         orChat,
			wantStatus: http.StatusPaymentRequired,
			want:       []obsWant{{"a", 402, "upstream_error", false, false}},
			reads:      3,
		},
	})
}
