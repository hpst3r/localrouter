package proxy

// provider_acceptance_helpers_test.go holds the shared plumbing and table types
// for the provider acceptance suite in provider_acceptance_test.go. It composes
// only the existing in-process harness (fakes_test.go) and adds no production
// seams. Every wait is bounded by the single hard deadline paBound; nothing
// sleeps to sequence test phases.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// paBound is the single hard deadline for every wait and every outbound request
// in the acceptance suite. It is short enough that a regression fails fast with
// a message rather than tripping the package -timeout.
const paBound = 5 * time.Second

// paReqExpect describes what the proxy must send upstream for one table entry.
type paReqExpect struct {
	provider      string // account Provider string
	endpoint      string // pathResponses or pathChat
	model         string // client-facing model
	upstreamModel string // route.UpstreamModel ("" = unchanged)
	request       string // exact client request body
	stream        bool   // client asked for a stream
	// upstream is the exact body the proxy must send, written out by hand.
	// Required for Codex entries so the expectation never derives from the
	// production rejected-field list; "" = derive it from request.
	upstream string
}

// paProfile is one non-streaming (provider, endpoint) protocol the suite drives.
type paProfile struct {
	name string
	paReqExpect
	upstream         string // exact JSON body the fake upstream returns
	wantUsage        core.Usage
	wantUsageKnown   bool
	wantReportedCost *float64 // nil = the ledger row must carry no reported cost
}

// paStreamProfile is the streaming counterpart of paProfile: the exact SSE
// bytes the fake upstream flushes plus the terminal usage they must yield.
type paStreamProfile struct {
	name string
	paReqExpect
	sse              string
	wantUsage        core.Usage
	wantUsageKnown   bool
	wantReportedCost *float64
}

// paPost issues one inference request bounded by paBound. The returned body is
// wrapped so that closing it releases the request context.
func paPost(t *testing.T, h *harness, path, key, body string, hdr map[string]string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), paBound)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+path, strings.NewReader(body))
	if err != nil {
		cancel()
		t.Fatalf("provider acceptance: build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("provider acceptance: request failed: %v", err)
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp
}

// paAwait waits up to paBound for a signal channel to close.
func paAwait(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(paBound):
		t.Fatalf("provider acceptance: timed out waiting for %s", what)
	}
}

// paAwaitLease waits up to paBound for the next lease release.
func paAwaitLease(t *testing.T, what string, ch <-chan *fakeLease) *fakeLease {
	t.Helper()
	select {
	case l := <-ch:
		return l
	case <-time.After(paBound):
		t.Fatalf("provider acceptance: timed out waiting for %s", what)
		return nil
	}
}

// paRecvBytes waits up to paBound for a captured upstream request body.
func paRecvBytes(t *testing.T, what string, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(paBound):
		t.Fatalf("provider acceptance: timed out waiting for %s", what)
		return nil
	}
}

// paReadBody drains and closes a response, failing the test on a read error.
func paReadBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("provider acceptance: read response body: %v", err)
	}
	return b
}

// paFlushChunks writes s upstream in small flushed chunks so the response is
// genuinely streamed rather than buffered by the fake server.
func paFlushChunks(w http.ResponseWriter, s string, step int) {
	fl, _ := w.(http.Flusher)
	for i := 0; i < len(s); i += step {
		end := min(i+step, len(s))
		_, _ = io.WriteString(w, s[i:end])
		if fl != nil {
			fl.Flush()
		}
	}
}

// paEvent renders one SSE frame: the given field lines followed by the blank
// line that terminates the event. Compose frames with + so table entries read
// as the wire bytes they describe.
func paEvent(fields ...string) string {
	return strings.Join(fields, "\n") + "\n\n"
}

// paCleanupGate returns a gate that is registered with the harness, so harness
// teardown always releases an upstream handler blocked on it even if an
// assertion fails first. A test uses it to hold a handler open while it awaits
// an independent signal (such as a real upstream cancellation) without leaking
// that handler and deadlocking httptest.Server.Close.
func paCleanupGate(h *harness) chan struct{} {
	gate := make(chan struct{})
	h.gates = append(h.gates, gate)
	return gate
}

// paWaitRows waits until n ledger rows have been recorded, then waits (bounded
// by paBound) for every proxy handler to finish by closing the proxy server,
// and only then asserts the count is EXACTLY n. Production releases a lease
// BEFORE it writes the attempt's row, so an observed lease release does not
// mean the row has landed; and a handler still in flight after the first n
// rows could write a stray extra one. Ledger writes are synchronous inside the
// proxy handler, so once httptest.Server.Close has waited out the outstanding
// requests the row set is final. Call it only after the client side of every
// request is done; no request can be sent through h.srv afterwards.
func paWaitRows(t testing.TB, h *harness, n int) []core.RequestRecord {
	t.Helper()
	h.waitRows(n)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		h.srv.Close()
	}()
	select {
	case <-closed:
	case <-time.After(paBound):
		t.Fatalf("provider acceptance: timed out waiting for proxy handlers to finish")
	}
	rows := h.ledger.all()
	if len(rows) != n {
		t.Fatalf("provider acceptance: ledger rows = %d, want exactly %d", len(rows), n)
	}
	return rows
}

// paCanonicalBody returns the canonical expected upstream body for exp. Codex
// entries carry a hand-written exp.upstream, so a field wrongly added to the
// production rejected-field list (say "instructions") changes what the proxy
// sends but not what the test expects. Otherwise the body is derived from
// exp.request rather than from what the proxy actually sent, applying only the
// documented forwarding rewrites with a test-local encoder. Because every
// unrelated field is carried through unchanged, a rewrite that drops, mutates
// or re-encodes a field it was not supposed to touch cannot match:
//
//	model          -> route.UpstreamModel, when the route rewrites it
//	stream_options -> include_usage:true merged in, chat + stream only
func paCanonicalBody(t *testing.T, exp paReqExpect) []byte {
	t.Helper()
	if exp.upstream != "" {
		return []byte(exp.upstream)
	}
	if exp.provider == core.ProviderCodex {
		t.Fatalf("provider acceptance: codex table entries must spell out the expected upstream body")
	}
	forceUsage := exp.endpoint == pathChat && exp.stream
	if exp.upstreamModel == "" && !forceUsage {
		// Nothing triggers a rewrite: the body is forwarded verbatim.
		return []byte(exp.request)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(exp.request), &obj); err != nil {
		t.Fatalf("provider acceptance: table request is not a JSON object: %v (%s)", err, exp.request)
	}
	if exp.upstreamModel != "" {
		m, err := paMarshal(exp.upstreamModel)
		if err != nil {
			t.Fatalf("provider acceptance: marshal upstream model: %v", err)
		}
		obj["model"] = m
	}
	if forceUsage {
		opts := map[string]json.RawMessage{}
		if raw, ok := obj["stream_options"]; ok {
			// Mirror rewriteBody: a non-object stream_options is replaced.
			_ = json.Unmarshal(raw, &opts)
			if opts == nil {
				opts = map[string]json.RawMessage{}
			}
		}
		opts["include_usage"] = json.RawMessage("true")
		raw, err := paMarshal(opts)
		if err != nil {
			t.Fatalf("provider acceptance: marshal stream_options: %v", err)
		}
		obj["stream_options"] = raw
	}
	out, err := paMarshal(obj)
	if err != nil {
		t.Fatalf("provider acceptance: marshal canonical body: %v", err)
	}
	return out
}

// paMarshal encodes v as compact JSON without HTML escaping (sorted object
// keys, as encoding/json always does), independent of production helpers.
func paMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// paCheckRequest asserts the body the proxy sent upstream IS the canonical
// expected body for exp, byte for byte; the failure message names any fields
// that differ.
func paCheckRequest(t *testing.T, exp paReqExpect, got []byte) {
	t.Helper()
	want := paCanonicalBody(t, exp)
	if bytes.Equal(got, want) {
		return
	}
	t.Fatalf("provider acceptance: upstream body differs from the canonical expected body\n got %s\nwant %s\n%s",
		got, want, paBodyDiff(got, want))
}

// paBodyDiff names the top-level fields that differ between two JSON objects,
// for diagnostics only. It returns "" when either side is not an object.
func paBodyDiff(got, want []byte) string {
	var g, w map[string]json.RawMessage
	if json.Unmarshal(got, &g) != nil || json.Unmarshal(want, &w) != nil {
		return ""
	}
	var extra, missing, changed []string
	for k, gv := range g {
		wv, ok := w[k]
		switch {
		case !ok:
			extra = append(extra, k)
		case !bytes.Equal(bytes.TrimSpace(gv), bytes.TrimSpace(wv)):
			changed = append(changed, k)
		}
	}
	for k := range w {
		if _, ok := g[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	sort.Strings(changed)
	var parts []string
	if len(extra) > 0 {
		parts = append(parts, "unexpected fields: "+strings.Join(extra, ", "))
	}
	if len(missing) > 0 {
		parts = append(parts, "missing fields: "+strings.Join(missing, ", "))
	}
	if len(changed) > 0 {
		parts = append(parts, "changed fields: "+strings.Join(changed, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "diff: " + strings.Join(parts, "; ")
}

// paCostString renders an optional USD cost for failure messages.
func paCostString(p *float64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatFloat(*p, 'g', -1, 64)
}
