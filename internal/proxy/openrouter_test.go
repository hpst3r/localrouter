package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const orChatBody = `{"model":"anthropic/claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`

func orRoute(h *harness, ids ...string) {
	h.routes = []core.Route{{Name: "or", Models: []string{"anthropic/claude-sonnet-4.5"}, Interactive: ids, Background: ids}}
}

func payment402(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Retry-After", "90")
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = io.WriteString(w, `{"error":{"code":402,"message":"Insufficient credits"}}`)
}

// A 402 from OpenRouter fails over before any byte reaches the client and
// reports the status (with its Retry-After) to policy.
func TestOpenRouter402FailsOver(t *testing.T) {
	h := newHarness(t)
	var gotModel string
	h.upstream("or", core.ProviderOpenRouter, payment402)
	h.upstream("b", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotModel = string(b)
		okJSON(w, r)
	})
	orRoute(h, "or", "b")
	h.start()

	resp := h.post("/v1/chat/completions", clientKey, orChatBody, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if !strings.Contains(gotModel, `"anthropic/claude-sonnet-4.5"`) {
		t.Errorf("slash model id not preserved: %s", gotModel)
	}
	rows := h.waitRows(2)
	if rows[0].AccountID != "or" || rows[0].Status != 402 || !strings.Contains(rows[0].Error, "failing over") {
		t.Errorf("first row %+v", rows[0])
	}
	if rows[1].AccountID != "b" || rows[1].FailoverOf != rows[0].ID {
		t.Errorf("second row %+v", rows[1])
	}
	l := h.policy.leases[0]
	if l.outcome.Status != 402 || !l.outcome.ResetAt.Equal(testNow.Add(90*time.Second)) {
		t.Errorf("lease outcome %+v", l.outcome)
	}
}

// With no other account the buffered 402 is relayed unchanged.
func TestOpenRouter402RelayedWhenNoFailover(t *testing.T) {
	h := newHarness(t)
	h.upstream("or", core.ProviderOpenRouter, payment402)
	orRoute(h, "or")
	h.start()
	resp := h.post("/v1/chat/completions", clientKey, orChatBody, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(string(body), "Insufficient credits") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if o := h.policy.leases[0].outcome; o.Status != 402 || !o.ResetAt.Equal(testNow.Add(90*time.Second)) {
		t.Errorf("outcome %+v", o)
	}
}

// 402 stays a final answer for other providers.
func TestGeneric402NotRetried(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, payment402)
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	orRoute(h, "a", "b")
	h.start()
	resp := h.post("/v1/chat/completions", clientKey, orChatBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if rows := h.waitRows(1); len(rows) != 1 || rows[0].AccountID != "a" {
		t.Errorf("rows %+v", rows)
	}
	if o := h.policy.leases[0].outcome; !o.ResetAt.IsZero() {
		t.Errorf("generic 402 should not carry a reset hint: %+v", o)
	}
}

// A 200 stream that has started is never repeated, whatever follows.
func TestOpenRouterStreamNotRepeated(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.upstream("or", core.ProviderOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	orRoute(h, "or", "b")
	h.start()
	resp := h.post("/v1/chat/completions", clientKey, `{"model":"anthropic/claude-sonnet-4.5","stream":true,"messages":[]}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "[DONE]") || calls != 1 {
		t.Fatalf("status %d calls %d body %s", resp.StatusCode, calls, body)
	}
	rows := h.waitRows(1)
	if len(rows) != 1 || !rows[0].UsageKnown || rows[0].Usage.InputTokens != 7 || rows[0].Usage.OutputTokens != 2 || rows[0].Provider != core.ProviderOpenRouter {
		t.Errorf("rows %+v", rows)
	}
}
