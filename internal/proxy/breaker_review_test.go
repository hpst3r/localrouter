package proxy

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/policy"
)

// TestReviewBR2StreamingProbeBlocksRecoveredAccount: the half-open probe's
// outcome is only applied when its lease is released, which for a relayed
// response is at the END of the body. A probe that got 200 headers from a
// recovered upstream (reachability proven) keeps the account "half-open,
// probe in flight" for the whole stream, so every other request to that
// single-candidate route is refused 503 upstream_unavailable for minutes
// while the upstream is serving.
func TestReviewBR2StreamingProbeBlocksRecoveredAccount(t *testing.T) {
	if os.Getenv("LR_REVIEW") == "" {
		t.Skip("BUG BR-2: a streaming probe holds the account refused until its body ends, after headers proved reachability")
	}
	h := newHarness(t)
	gate := make(chan struct{})
	h.gates = append(h.gates, gate)
	h.upstream("llm", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"stream":true`) {
			okJSON(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-gate // a long generation still in progress
	})
	h.routes = []core.Route{{Name: "r", Models: []string{"m"}, Interactive: []string{"llm"}}}
	clk := &stepClock{t: testNow}
	h.clock = clk
	p := breakerPolicyFor(h, clk, policy.BreakerOptions{Threshold: 1, Base: 5 * time.Second, Max: time.Minute})
	// The upstream was unreachable once: the breaker opens; 5s later it is
	// half-open and the upstream has recovered.
	l, d := p.Acquire(core.ClassInteractive, []string{"llm"}, nil)
	if !d.Allow {
		t.Fatalf("acquire: %s", d.Reason)
	}
	l.Release(core.Outcome{Unreachable: "dial"})
	clk.Add(5 * time.Second)
	h.start()

	probe := h.post("/v1/responses", clientKey, `{"model":"m","input":"hi","stream":true}`, nil)
	defer probe.Body.Close()
	if probe.StatusCode != http.StatusOK {
		t.Fatalf("probe status %d", probe.StatusCode)
	}
	// The probe has its response headers: the upstream is reachable.
	resp := h.post("/v1/responses", clientKey, modelBody("m"), nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("request during a successful streaming probe: %d %s (want 200: reachability was already proven)", resp.StatusCode, b)
	}
	if state, _, _, _ := p.Breaker("llm"); state != policy.BreakerClosed {
		t.Errorf("breaker %q while the probe streams a 200, want closed", state)
	}
}
