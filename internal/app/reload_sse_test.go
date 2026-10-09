package app_test

// Real-SSE hot-reload regression. Unlike the other reload tests (which drive a
// JSON upstream), this one fronts a genuine text/event-stream upstream whose
// body format matches the OpenAI Responses wire format parsed by
// internal/proxy (see internal/proxy TestResponsesSSEUsage): the terminal
// "response.completed" event carries response.usage, which is the only place
// the proxy learns the token count and therefore the cost.
//
// The stream is held open with only its first event flushed. The outbound test
// client posts /v1/responses with stream:true and reads exactly that first
// event off the live body, proving the request was admitted and that real SSE
// bytes (not a buffered JSON body) reached the client. The config is then
// reloaded to a new client key, a new price and a smaller limit, and only then
// is the stream released. The held stream must complete with real SSE bytes and
// be costed at the OLD price under the OLD generation; the revoked key must be
// rejected; a fresh request must use the NEW key and the NEW price. The exact
// per-generation ledger sum (old 1000@$1 + new 1000@$3) uniquely pins both.
//
// Every goroutine is finite, every client call carries a deadline, and the gate
// is closed (via defer) before the httptest servers are torn down.

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hrSSEHead and hrSSETail are real Responses-API SSE frames: the created event
// flushed before the gate, and the terminal completed event (with usage)
// written only after the gate is released. CRLF framing matches
// internal/proxy TestResponsesSSEUsage.
const (
	hrSSEHead = "event: response.created\r\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"usage\":null}}\r\n\r\n"
	hrSSETail = "event: response.completed\r\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{" +
		"\"input_tokens\":1000,\"input_tokens_details\":{\"cached_tokens\":0}," +
		"\"output_tokens\":0}}}\r\n\r\n"
)

// hrReadEvent reads one complete SSE event block, up to and including its
// terminating blank line, straight from the live response body.
func hrReadEvent(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := br.ReadString('\n')
		b.WriteString(line)
		if err != nil {
			return b.String(), err
		}
		if line == "\n" || line == "\r\n" {
			return b.String(), nil
		}
	}
}

func TestHotReloadRealSSEStreamSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	keyA := "hr-client-keyA-0123456789"
	keyB := "hr-client-keyB-0123456789"
	hrWriteFile(t, filepath.Join(dir, "a.key"), keyA)
	hrWriteFile(t, filepath.Join(dir, "b.key"), keyB)
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), hrPricingYAML(1.0)) // $1 / 1M input

	// Gated SSE upstream. The handler is always unblocked by closeGate (called
	// on the normal path and deferred for every early-return path) so the
	// httptest server never tears down with a goroutine stuck on <-gate.
	gate := make(chan struct{})
	var closeOnce sync.Once
	closeGate := func() { closeOnce.Do(func() { close(gate) }) }
	arrived := make(chan struct{})
	var arrivedOnce sync.Once
	var (
		mu    sync.Mutex
		hits  int
		ctype string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, hrSSEHead)
		fl.Flush() // first event is on the wire before the gate
		mu.Lock()
		hits++
		ctype = w.Header().Get("Content-Type")
		mu.Unlock()
		arrivedOnce.Do(func() { close(arrived) })
		<-gate // released before the servers below are torn down
		_, _ = io.WriteString(w, hrSSETail)
		fl.Flush()
	})
	up := httptest.NewServer(mux)
	defer up.Close()
	defer closeGate() // LIFO: runs before up.Close()

	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, hrConfigYAML(up.URL, "a.key", hrPricedModel, 4))
	e := hrBuild(t, dir, cfgPath, nil)

	if g := e.a.ReloadStatus().Generation; g != 1 {
		t.Fatalf("initial generation = %d", g)
	}

	// Open the streaming request under generation 1 and read ONLY its first
	// event off the wire: proof of admission and of real relayed SSE bytes.
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/responses",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hi","stream":true}`, hrPricedModel)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+keyA)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream content-type = %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	head, err := hrReadEvent(br)
	if err != nil {
		t.Fatalf("read first event: %v (got %q)", err, head)
	}
	if !strings.Contains(head, "response.created") {
		t.Fatalf("first event is not response.created: %q", head)
	}
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("gated SSE request never reached the upstream")
	}

	// Reload: new client key, new price, smaller limit (same account/route).
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), hrPricingYAML(3.0)) // $3 / 1M input
	hrWriteFile(t, cfgPath, hrConfigYAML(up.URL, "b.key", hrPricedModel, 1))
	st, err := e.a.Reload(cfgPath)
	if err != nil || !st.OK || st.Generation != 2 {
		t.Fatalf("reload = %#v, %v", st, err)
	}
	if d := e.diagnostics(keyB); d.Inflight.GlobalLimit != 1 {
		t.Fatalf("post-reload limit = %d, want 1", d.Inflight.GlobalLimit)
	}

	// The revoked key no longer authenticates; the held stream is unaffected.
	if r := e.responses(keyA, hrPricedModel); r.status != http.StatusUnauthorized {
		t.Fatalf("revoked key accepted: %d %s", r.status, r.body)
	}

	// Release the gated stream and drain the terminal event over the live
	// connection: the old generation must still deliver real SSE bytes.
	closeGate()
	tail, err := hrReadEvent(br)
	if err != nil {
		t.Fatalf("read terminal event: %v (got %q)", err, tail)
	}
	if !strings.Contains(tail, "response.completed") || !strings.Contains(tail, "input_tokens") {
		t.Fatalf("terminal event malformed: %q", tail)
	}
	if rest, _ := io.ReadAll(br); len(rest) != 0 {
		t.Fatalf("unexpected bytes after terminal event: %q", rest)
	}

	// The freed slot admits a fresh request under the reloaded limit of 1.
	e.hrWaitActive(keyB, 0)
	if r := e.responses(keyB, hrPricedModel); r.status != http.StatusOK {
		t.Fatalf("new-key request status %d: %s", r.status, r.body)
	}

	// Ledger: held stream at the OLD price, new request at the NEW price. The
	// exact sum can only come from that split, so it pins both attributions.
	sum := e.summary()["acct"]
	if sum.Requests != 2 {
		t.Fatalf("ledger requests = %d, want 2", sum.Requests)
	}
	if sum.CostUSD == nil {
		t.Fatal("aggregate cost is nil")
	}
	const want = (1000*1.0 + 1000*3.0) / 1e6
	if got := *sum.CostUSD; got < want-1e-12 || got > want+1e-12 {
		t.Fatalf("aggregate cost = %v, want %v (held stream must keep the old price)", got, want)
	}

	mu.Lock()
	gotHits, gotCT := hits, ctype
	mu.Unlock()
	if gotHits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (revoked-key probe must not reach upstream)", gotHits)
	}
	if !strings.HasPrefix(gotCT, "text/event-stream") {
		t.Fatalf("upstream relayed content-type = %q", gotCT)
	}
}
