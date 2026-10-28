package proxy

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// headerRecorder is an upstream RoundTripper that keeps each outgoing
// request's raw header map (keys exactly as they go on the wire) before
// delegating to the default transport.
type headerRecorder struct {
	mu   sync.Mutex
	sent []http.Header
}

func (r *headerRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.sent = append(r.sent, req.Header.Clone())
	r.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

func (r *headerRecorder) all() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.sent...)
}

// install makes h send upstream requests through r.
func (r *headerRecorder) install(h *harness) { h.opts.HTTPClient = &http.Client{Transport: r} }

// sessionIDs returns every value under any key equal to session_id ignoring
// case, and the raw keys they were under.
func sessionIDs(h http.Header) (keys, vals []string) {
	for k, v := range h {
		if strings.EqualFold(k, "session_id") {
			keys = append(keys, k)
			vals = append(vals, v...)
		}
	}
	return keys, vals
}

const cacheKeyBody = `{"model":"gpt-x","input":"hi","stream":false,"prompt_cache_key":"k1"}`

func TestCodexSynthesizesSessionIDFromPromptCacheKey(t *testing.T) {
	h := newHarness(t)
	rec := &headerRecorder{}
	rec.install(h)
	h.upstream("a", core.ProviderCodex, okJSON)
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, cacheKeyBody, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	sent := rec.all()
	if len(sent) != 1 {
		t.Fatalf("upstream requests %d", len(sent))
	}
	keys, vals := sessionIDs(sent[0])
	if len(keys) != 1 || keys[0] != "session_id" || len(vals) != 1 || vals[0] != "k1" {
		t.Fatalf("session_id keys %q vals %q", keys, vals)
	}
}

func TestCodexClientSessionIDWinsOverPromptCacheKey(t *testing.T) {
	h := newHarness(t)
	rec := &headerRecorder{}
	rec.install(h)
	var got []string
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Values("session_id")
		okJSON(w, r)
	})
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, cacheKeyBody, map[string]string{"session_id": "s1"})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if len(got) != 1 || got[0] != "s1" {
		t.Fatalf("upstream session_id %q", got)
	}
	_, vals := sessionIDs(rec.all()[0])
	if len(vals) != 1 || vals[0] != "s1" {
		t.Fatalf("raw session_id values %q", vals)
	}
}

func TestNonCodexDoesNotSynthesizeSessionID(t *testing.T) {
	for _, prov := range []string{core.ProviderOpenAICompat, core.ProviderOpenRouter} {
		t.Run(prov, func(t *testing.T) {
			h := newHarness(t)
			rec := &headerRecorder{}
			rec.install(h)
			h.upstream("a", prov, okJSON)
			singleRoute(h, "a")
			h.start()
			resp := h.post("/v1/responses", clientKey, cacheKeyBody, nil)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if keys, _ := sessionIDs(rec.all()[0]); len(keys) != 0 {
				t.Fatalf("session_id sent to %s: %q", prov, keys)
			}
		})
	}
}

func TestCodexSessionIDNotSynthesizedFromUnusableKey(t *testing.T) {
	cases := map[string]string{
		"missing":   `{"model":"gpt-x","input":"hi"}`,
		"empty":     `{"model":"gpt-x","input":"hi","prompt_cache_key":""}`,
		"number":    `{"model":"gpt-x","input":"hi","prompt_cache_key":42}`,
		"null":      `{"model":"gpt-x","input":"hi","prompt_cache_key":null}`,
		"too_long":  `{"model":"gpt-x","input":"hi","prompt_cache_key":"` + strings.Repeat("k", 300) + `"}`,
		"newline":   `{"model":"gpt-x","input":"hi","prompt_cache_key":"a\nb"}`,
		"non_ascii": `{"model":"gpt-x","input":"hi","prompt_cache_key":"café"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			rec := &headerRecorder{}
			rec.install(h)
			h.upstream("a", core.ProviderCodex, okJSON)
			singleRoute(h, "a")
			h.start()
			resp := h.post("/v1/responses", clientKey, body, nil)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			sent := rec.all()
			if len(sent) != 1 {
				t.Fatalf("upstream requests %d", len(sent))
			}
			if keys, vals := sessionIDs(sent[0]); len(keys) != 0 {
				t.Fatalf("session_id synthesized: %q", vals)
			}
		})
	}
}

// The proxy rejects non-JSON bodies before any upstream, so codexBody's own
// handling is checked directly, along with the key surviving a rewrite.
func TestCodexBodyCacheKey(t *testing.T) {
	for in, want := range map[string]string{
		`not json`: "",
		`[1,2]`:    "",
		`{"prompt_cache_key":"k1","store":false}`:                 "k1",
		`{"prompt_cache_key":"k1","metadata":{},"store":true}`:    "k1",
		`{"prompt_cache_key":"` + strings.Repeat("k", 256) + `"}`: strings.Repeat("k", 256),
		`{"prompt_cache_key":"\u0007"}`:                           "",
	} {
		if _, got := codexBody([]byte(in)); got != want {
			t.Errorf("%.40s: key %q, want %q", in, got, want)
		}
	}
}

func TestCodexSessionIDOnlyOnCodexAttemptAcrossFailover(t *testing.T) {
	h := newHarness(t)
	rec := &headerRecorder{}
	rec.install(h)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a", "b")
	h.start()
	resp := h.post("/v1/responses", clientKey, cacheKeyBody, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	sent := rec.all()
	if len(sent) != 2 {
		t.Fatalf("upstream requests %d", len(sent))
	}
	if _, vals := sessionIDs(sent[0]); len(vals) != 1 || vals[0] != "k1" {
		t.Fatalf("codex attempt session_id %q", vals)
	}
	if keys, _ := sessionIDs(sent[1]); len(keys) != 0 {
		t.Fatalf("non-codex attempt session_id %q", keys)
	}
}

func TestCodexTurnStateForwardedBothWays(t *testing.T) {
	h := newHarness(t)
	var got string
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Codex-Turn-State")
		w.Header().Set("X-Codex-Turn-State", "ts-out")
		okJSON(w, r)
	})
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, map[string]string{"X-Codex-Turn-State": "ts-in"})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got != "ts-in" {
		t.Fatalf("upstream X-Codex-Turn-State %q", got)
	}
	if v := resp.Header.Get("X-Codex-Turn-State"); v != "ts-out" {
		t.Fatalf("client X-Codex-Turn-State %q", v)
	}
}

// codexLiveStream mimics the Codex backend's stream: no Content-Type, a large
// instructions echo in response.created, and the current usage shape.
func codexLiveStream() string {
	instr := strings.Repeat("x", 300<<10)
	return "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","instructions":"` + instr + `","tools":[]}}` + "\n\n" +
		"event: response.in_progress\n" +
		`data: {"type":"response.in_progress","response":{"id":"r1","instructions":"` + instr + `"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","instructions":"` + instr + `","usage":` +
		`{"input_tokens":3521,"input_tokens_details":{"cached_tokens":3328,"cache_write_tokens":0},"output_tokens":5,` +
		`"output_tokens_details":{"reasoning_tokens":2},"total_tokens":3526,"attribution":{"source":"x","n":[1,2]}}}}` + "\n\n"
}

// noContentType suppresses net/http's Content-Type sniffing on w.
func noContentType(w http.ResponseWriter) { w.Header()["Content-Type"] = nil }

func TestUsageCapturedFromSSEWithoutContentType(t *testing.T) {
	stream := codexLiveStream()
	want := core.Usage{InputTokens: 3521, CachedInputTokens: 3328, OutputTokens: 5, ReasoningTokens: 2}
	for _, tc := range []struct {
		name  string
		steps []int // chunk sizes cycled over the stream
	}{
		{"whole", []int{len(stream)}},
		{"split_prefix", []int{1, 2, 3, 7, 4096}},
		{"small", []int{13, 997, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, _ *http.Request) {
				noContentType(w)
				f := w.(http.Flusher)
				for i, s := 0, 0; i < len(stream); s++ {
					n := min(tc.steps[s%len(tc.steps)], len(stream)-i)
					_, _ = io.WriteString(w, stream[i:i+n])
					f.Flush()
					i += n
				}
			})
			singleRoute(h, "a")
			h.start()
			resp := h.post("/v1/responses", clientKey, `{"model":"gpt-x","input":"hi","stream":true}`, nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != stream {
				t.Fatalf("relayed body differs (len %d vs %d)", len(body), len(stream))
			}
			// Sniffed SSE is relayed as an event stream.
			if ct := resp.Header.Values("Content-Type"); len(ct) != 1 || ct[0] != "text/event-stream" {
				t.Fatalf("client got Content-Type %q", ct)
			}
			row := h.waitRows(1)[0]
			if !row.UsageKnown || row.Usage != want || row.Error != "" {
				t.Fatalf("row usage %+v known=%v err=%q", row.Usage, row.UsageKnown, row.Error)
			}
		})
	}
}

// A leading UTF-8 BOM (allowed by the SSE spec) does not hide an untyped
// stream: it is still sniffed as SSE, its usage parses, and the relayed bytes
// keep the BOM.
func TestUsageCapturedFromSSEWithBOM(t *testing.T) {
	stream := "\xEF\xBB\xBF" + codexLiveStream()
	for _, step := range []int{1, 2, 4096} {
		h := newHarness(t)
		h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, _ *http.Request) {
			noContentType(w)
			f := w.(http.Flusher)
			for i := 0; i < len(stream); i += step {
				_, _ = io.WriteString(w, stream[i:min(i+step, len(stream))])
				if i < 8 {
					f.Flush()
				}
			}
		})
		singleRoute(h, "a")
		h.start()
		resp := h.post("/v1/responses", clientKey, `{"model":"gpt-x","input":"hi","stream":true}`, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != stream {
			t.Fatalf("step %d: relayed body differs (len %d vs %d)", step, len(body), len(stream))
		}
		if ct := resp.Header.Values("Content-Type"); len(ct) != 1 || ct[0] != "text/event-stream" {
			t.Fatalf("step %d: client got Content-Type %q", step, ct)
		}
		row := h.waitRows(1)[0]
		if !row.UsageKnown || row.Usage.InputTokens != 3521 || row.Error != "" {
			t.Fatalf("step %d: row usage %+v known=%v err=%q", step, row.Usage, row.UsageKnown, row.Error)
		}
	}
}

func TestUsageCapturedFromJSONWithoutContentType(t *testing.T) {
	const payload = "\n  {\"id\":\"r1\",\"usage\":{\"input_tokens\":10,\"output_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":4},\"output_tokens_details\":{\"reasoning_tokens\":1}}}"
	h := newHarness(t)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, _ *http.Request) {
		noContentType(w)
		_, _ = io.WriteString(w, payload)
	})
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != payload {
		t.Fatalf("relayed body %q", body)
	}
	if ct, ok := resp.Header["Content-Type"]; ok {
		t.Fatalf("client got Content-Type %q", ct)
	}
	row := h.waitRows(1)[0]
	want := core.Usage{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 3, ReasoningTokens: 1}
	if !row.UsageKnown || row.Usage != want {
		t.Fatalf("row usage %+v known=%v", row.Usage, row.UsageKnown)
	}
}

func TestUsageCaptureSniffing(t *testing.T) {
	sse := "\r\n: keepalive\n" + "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n"
	json := " \t{\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}"
	want := core.Usage{InputTokens: 7, OutputTokens: 2}
	for _, step := range []int{1, 2, 3, 5, 64, 1 << 20} {
		for name, tc := range map[string]struct {
			body string
			sse  bool
		}{"sse": {sse, true}, "json": {json, false}} {
			c := newUsageCapture("")
			feed(c, tc.body, step)
			if u, ok := c.Result(); !ok || u != want {
				t.Fatalf("%s step %d: %+v %v", name, step, u, ok)
			}
			if c.sse != tc.sse {
				t.Fatalf("%s step %d: sse=%v", name, step, c.sse)
			}
		}
	}
	// A leading BOM is skipped, also when split across writes.
	for _, step := range []int{1, 2, 3, 1 << 20} {
		c := newUsageCapture("")
		feed(c, "\xEF\xBB\xBF"+sse, step)
		if u, ok := c.Result(); !ok || u != want || !c.sse {
			t.Fatalf("bom step %d: %+v %v sse=%v", step, u, ok, c.sse)
		}
	}
	for _, prefix := range []string{"event: x\n", "data: x\n", "id: 1\n", "retry: 5\n", ": c\n"} {
		c := newUsageCapture("")
		feed(c, prefix, 1)
		_, _ = c.Result()
		if !c.sse {
			t.Errorf("%q not sniffed as SSE", prefix)
		}
	}
	// An explicit Content-Type is never overridden by the body.
	c := newUsageCapture("application/json")
	feed(c, sse, 4)
	if _, ok := c.Result(); ok || c.sse {
		t.Fatal("explicit application/json parsed as SSE")
	}
	// Whitespace-only and empty bodies stay JSON (unknown usage, complete).
	for _, body := range []string{"", "   \n"} {
		c := newUsageCapture("")
		feed(c, body, 1)
		if _, ok := c.Result(); ok || c.sse {
			t.Fatalf("%q: sse=%v", body, c.sse)
		}
	}
}
