package proxy

import (
	"io"
	"net/http"
	"testing"

	"github.com/hpst3r/localrouter/internal/routing"
)

// TestR26DecisionCompletedIsJSONFramed (R2.6, FIN-L4): a decision response has
// no stream terminal; it is complete exactly when it is a JSON (non-stream)
// body, typed or untyped. No SSE terminal, not even [DONE], completes it.
func TestR26DecisionCompletedIsJSONFramed(t *testing.T) {
	for _, ep := range []string{endpointSystemOne, endpointDecisions} {
		for _, ct := range []string{"", "application/json"} {
			c := newUsageCapture(ct)
			feed(c, decAnswer, 7)
			_, _ = c.Result()
			if !c.Completed(ep) {
				t.Errorf("Completed(%s) with Content-Type %q = false, want true", ep, ct)
			}
		}
		c := newUsageCapture("text/event-stream")
		feed(c, "data: "+decAnswer+"\n\ndata: [DONE]\n\n", 7)
		_, _ = c.Result()
		if c.Completed(ep) {
			t.Errorf("Completed(%s) for an event stream ending in [DONE] = true, want false", ep)
		}
	}
}

// TestR26UntypedJSONDecisionAnswer: a 200 decision answer with no
// Content-Type is relayed and recorded complete, with usage and cost known.
func TestR26UntypedJSONDecisionAnswer(t *testing.T) {
	untyped := func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil // suppress sniffing
		_, _ = io.WriteString(w, decAnswer)
	}
	h, _ := decHarness(t, routing.ProtocolSystemOne, untyped, "or1")
	h.start()
	resp := h.post(pathSystemOne, clientKey, decClientBody, nil)
	if got := readAll(t, resp); resp.StatusCode != http.StatusOK || string(got) != decAnswer {
		t.Fatalf("relay = %d %s", resp.StatusCode, got)
	}
	rec := h.waitRows(1)[0]
	if !rec.UsageKnown || rec.Usage.InputTokens != 476 || rec.Usage.OutputTokens != 70 {
		t.Fatalf("usage = %+v known=%v", rec.Usage, rec.UsageKnown)
	}
	if rec.ReportedCostUSD == nil || *rec.ReportedCostUSD != 0.000019992 {
		t.Fatalf("reported cost = %v", paCostString(rec.ReportedCostUSD))
	}
}
