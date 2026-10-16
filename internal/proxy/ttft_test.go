package proxy

// Performance capture (SPEC "Performance metrics → Capture", P1–P5): the stream
// loop records whether the response was SSE, the time to the first output
// event, and whether that event was reasoning.

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func sseData(data string) string { return "data: " + data + "\n\n" }

func sseEvent(name, data string) string { return "event: " + name + "\n" + sseData(data) }

// ttftStep is one upstream write: the fake clock advances by adv, then data is
// written and flushed.
type ttftStep struct {
	adv  time.Duration
	data string
}

// relayTTFT relays steps through the proxy one at a time: the upstream waits
// until the client has read each step's bytes (so the proxy has already
// handled them) before advancing the clock for the next step. tail advances
// the clock after the last step, just before the upstream returns.
func relayTTFT(t *testing.T, path, contentType string, steps []ttftStep, tail time.Duration) (core.RequestRecord, string) {
	t.Helper()
	h := newHarness(t)
	clk := &stepClock{t: testNow}
	h.clock = clk
	ack := make(chan struct{})
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		for _, s := range steps {
			clk.Add(s.adv)
			_, _ = io.WriteString(w, s.data)
			w.(http.Flusher).Flush()
			select {
			case <-ack:
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Second):
				return
			}
		}
		clk.Add(tail)
	})
	singleRoute(h, "a")
	h.start()

	body := `{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	if path == "/v1/responses" {
		body = `{"model":"gpt-x","stream":true,"input":"hi"}`
	}
	resp := h.post(path, clientKey, body, nil)
	defer resp.Body.Close()
	for i, s := range steps {
		buf := make([]byte, len(s.data))
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatalf("step %d: read: %v", i, err)
		}
		if string(buf) != s.data {
			t.Fatalf("step %d: relayed %q, want %q", i, buf, s.data)
		}
		select {
		case ack <- struct{}{}:
		case <-time.After(5 * time.Second):
			t.Fatalf("step %d: upstream did not wait for ack", i)
		}
	}
	_, _ = io.ReadAll(resp.Body)
	h.waitHandled(1)
	return h.waitRows(1)[0], h.logs.String()
}

func TestTTFTCapture(t *testing.T) {
	const ms = time.Millisecond
	role := sseData(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`)
	content := sseData(`{"choices":[{"index":0,"delta":{"content":"Hello"}}]}`)
	usage := sseData(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7}}`)
	done := sseData("[DONE]")
	completed := sseEvent("response.completed",
		`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":7}}}`)
	respPrelude := []ttftStep{
		{10 * ms, sseEvent("response.created", `{"type":"response.created","response":{"id":"r"}}`)},
		{10 * ms, sseEvent("response.in_progress", `{"type":"response.in_progress","response":{"id":"r"}}`)},
		{10 * ms, sseEvent("response.output_item.added", `{"type":"response.output_item.added","item":{"type":"message"}}`)},
		{10 * ms, sseEvent("response.content_part.added", `{"type":"response.content_part.added","part":{"type":"output_text","text":""}}`)},
	}
	withPrelude := func(rest ...ttftStep) []ttftStep { return append(append([]ttftStep(nil), respPrelude...), rest...) }

	none := int64(-1)
	for _, tc := range []struct {
		name          string
		path, ctype   string
		steps         []ttftStep
		tail          time.Duration
		wantTTFT      int64 // none = nil
		wantReasoning bool
		wantStreamed  bool
		wantLatency   int64
		wantErr       string
	}{
		{name: "P1 chat role then content", path: "/v1/chat/completions", ctype: "text/event-stream",
			steps:    []ttftStep{{10 * ms, role}, {40 * ms, content}, {100 * ms, usage + done}},
			wantTTFT: 50, wantStreamed: true, wantLatency: 150},
		{name: "P2 responses output_text after prelude", path: "/v1/responses", ctype: "text/event-stream",
			steps: withPrelude(
				ttftStep{25 * ms, sseEvent("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hi"}`)},
				ttftStep{5 * ms, completed}),
			wantTTFT: 65, wantStreamed: true, wantLatency: 70},
		{name: "P2 responses reasoning summary first", path: "/v1/responses", ctype: "text/event-stream",
			steps: withPrelude(
				ttftStep{3 * ms, sseEvent("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","delta":"Thinking"}`)},
				ttftStep{50 * ms, sseEvent("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hi"}`)},
				ttftStep{5 * ms, completed}),
			wantTTFT: 43, wantReasoning: true, wantStreamed: true, wantLatency: 98},
		{name: "P3 chat reasoning_content first", path: "/v1/chat/completions", ctype: "text/event-stream",
			steps: []ttftStep{{10 * ms, role},
				{20 * ms, sseData(`{"choices":[{"delta":{"reasoning_content":"hmm"}}]}`)},
				{30 * ms, content}, {1 * ms, usage + done}},
			wantTTFT: 30, wantReasoning: true, wantStreamed: true, wantLatency: 61},
		{name: "P3 chat tool_calls first", path: "/v1/chat/completions", ctype: "text/event-stream",
			steps: []ttftStep{{10 * ms, role},
				{15 * ms, sseData(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"f"}}]}}]}`)},
				{1 * ms, usage + done}},
			wantTTFT: 25, wantStreamed: true, wantLatency: 26},
		{name: "P3/P4 SSE usage and DONE only", path: "/v1/chat/completions", ctype: "text/event-stream",
			steps:    []ttftStep{{10 * ms, role}, {10 * ms, usage}, {10 * ms, done}},
			wantTTFT: none, wantStreamed: true, wantLatency: 30},
		{name: "P4 non-SSE JSON", path: "/v1/chat/completions", ctype: "application/json",
			steps:    []ttftStep{{10 * ms, `{"choices":[{"delta":{"content":"x"},"message":{"content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`}},
			wantTTFT: none, wantLatency: 10},
		{name: "P5 event split across reads", path: "/v1/chat/completions", ctype: "text/event-stream",
			steps: []ttftStep{{10 * ms, role}, {10 * ms, `data: {"choices":[{"delta":{"con`},
				{30 * ms, "tent\":\"Hi\"}}]}\n\n"}, {5 * ms, usage + done}},
			wantTTFT: 50, wantStreamed: true, wantLatency: 55},
		{name: "P5 final unterminated event", path: "/v1/chat/completions", ctype: "text/event-stream",
			steps:    []ttftStep{{10 * ms, role}, {20 * ms, `data: {"choices":[{"delta":{"content":"Hi"}}]}`}},
			tail:     7 * ms,
			wantTTFT: 37, wantStreamed: true, wantLatency: 37, wantErr: errUnterminatedStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, logs := relayTTFT(t, tc.path, tc.ctype, tc.steps, tc.tail)
			if rec.Streamed != tc.wantStreamed || rec.Error != tc.wantErr || rec.LatencyMS != tc.wantLatency {
				t.Fatalf("streamed=%v error=%q latency=%d, want %v %q %d",
					rec.Streamed, rec.Error, rec.LatencyMS, tc.wantStreamed, tc.wantErr, tc.wantLatency)
			}
			if tc.wantTTFT == none {
				if rec.TTFTMS != nil || rec.FirstTokenReasoning {
					t.Fatalf("ttft=%d reasoning=%v, want nil false", *rec.TTFTMS, rec.FirstTokenReasoning)
				}
				if strings.Contains(logs, "ttft_ms=") {
					t.Fatalf("log has ttft_ms without a first output: %s", logs)
				}
			} else {
				if rec.TTFTMS == nil || *rec.TTFTMS != tc.wantTTFT || rec.FirstTokenReasoning != tc.wantReasoning {
					t.Fatalf("ttft=%v reasoning=%v, want %d %v", rec.TTFTMS, rec.FirstTokenReasoning, tc.wantTTFT, tc.wantReasoning)
				}
				if *rec.TTFTMS > rec.LatencyMS {
					t.Fatalf("ttft %d > latency %d", *rec.TTFTMS, rec.LatencyMS)
				}
			}
			line := requestLogLine(t, logs)
			if want := "streamed=" + strconv.FormatBool(tc.wantStreamed); !strings.Contains(line, want) {
				t.Fatalf("request log %q lacks %q", line, want)
			}
			if tc.wantTTFT != none && !strings.Contains(line, "ttft_ms="+strconv.FormatInt(tc.wantTTFT, 10)) {
				t.Fatalf("request log %q lacks ttft_ms=%d", line, tc.wantTTFT)
			}
			if strings.Contains(logs, "Hello") || strings.Contains(logs, "hmm") || strings.Contains(logs, "Thinking") {
				t.Fatalf("log leaks event content: %s", logs)
			}
		})
	}
}

func requestLogLine(t *testing.T, logs string) string {
	t.Helper()
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "msg=request ") {
			return l
		}
	}
	t.Fatalf("no request log line in %s", logs)
	return ""
}

func TestFirstOutputDetection(t *testing.T) {
	type want struct{ seen, reasoning bool }
	for _, tc := range []struct {
		name   string
		stream string
		want   want
	}{
		// Responses API.
		{"output_text", sseData(`{"type":"response.output_text.delta","delta":"a"}`), want{true, false}},
		{"refusal", sseData(`{"type":"response.refusal.delta","delta":"no"}`), want{true, false}},
		{"function args", sseData(`{"type":"response.function_call_arguments.delta","delta":"{\"a\""}`), want{true, false}},
		{"reasoning summary", sseData(`{"type":"response.reasoning_summary_text.delta","delta":"t"}`), want{true, true}},
		{"reasoning text", sseData(`{"type":"response.reasoning_text.delta","delta":"t"}`), want{true, true}},
		{"type from event name", sseEvent("response.output_text.delta", `{"delta":"a"}`), want{true, false}},
		{"json type wins over event name", sseEvent("response.output_text.delta", `{"type":"response.output_text.done","delta":"a"}`), want{false, false}},
		{"empty string delta", sseData(`{"type":"response.output_text.delta","delta":""}`), want{false, false}},
		{"null delta", sseData(`{"type":"response.output_text.delta","delta":null}`), want{false, false}},
		{"empty array delta", sseData(`{"type":"response.output_text.delta","delta":[ ]}`), want{false, false}},
		{"empty object delta", sseData(`{"type":"response.output_text.delta","delta":{}}`), want{false, false}},
		{"missing delta", sseData(`{"type":"response.output_text.delta"}`), want{false, false}},
		{"not a delta type", sseData(`{"type":"response.output_text.done","text":"delta"}`), want{false, false}},
		{"created", sseData(`{"type":"response.created","response":{"id":"delta"}}`), want{false, false}},
		{"output_item.added", sseData(`{"type":"response.output_item.added","item":{"delta":"x"}}`), want{false, false}},
		// Chat completions.
		{"content", sseData(`{"choices":[{"delta":{"content":"a"}}]}`), want{true, false}},
		{"role only", sseData(`{"choices":[{"delta":{"role":"assistant","content":""}}]}`), want{false, false}},
		{"null content", sseData(`{"choices":[{"delta":{"content":null}}]}`), want{false, false}},
		{"chat refusal", sseData(`{"choices":[{"delta":{"refusal":"no"}}]}`), want{true, false}},
		{"reasoning_content", sseData(`{"choices":[{"delta":{"reasoning_content":"r"}}]}`), want{true, true}},
		{"reasoning", sseData(`{"choices":[{"delta":{"reasoning":"r","content":""}}]}`), want{true, true}},
		{"reasoning and content", sseData(`{"choices":[{"delta":{"reasoning":"r","content":"c"}}]}`), want{true, false}},
		{"reasoning in one choice, content in another", sseData(`{"choices":[{"delta":{"reasoning":"r"}},{"delta":{"content":"c"}}]}`), want{true, false}},
		{"tool_calls", sseData(`{"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`), want{true, false}},
		{"empty tool_calls", sseData(`{"choices":[{"delta":{"tool_calls":[]}}]}`), want{false, false}},
		{"function_call", sseData(`{"choices":[{"delta":{"function_call":{"name":"f"}}}]}`), want{true, false}},
		{"null function_call", sseData(`{"choices":[{"delta":{"function_call":null}}]}`), want{false, false}},
		{"empty delta with usage", sseData(`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":1}}`), want{false, false}},
		{"done", sseData("[DONE]"), want{false, false}},
		{"comment", ": delta content\n\n", want{false, false}},
		{"invalid json", sseData(`{"choices":[{"delta":{"content":"a"`), want{false, false}},
		{"unterminated final event", `data: {"choices":[{"delta":{"content":"a"}}]}`, want{true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newUsageCapture("text/event-stream")
			_, _ = c.Write([]byte(tc.stream))
			_, _ = c.Result() // flushes a final unterminated event
			if seen, reasoning := c.FirstOutput(); (want{seen, reasoning}) != tc.want {
				t.Fatalf("FirstOutput() = %v %v, want %+v", seen, reasoning, tc.want)
			}
		})
	}

	c := newUsageCapture("application/json")
	_, _ = c.Write([]byte(`{"choices":[{"delta":{"content":"a"}}]}`))
	_, _ = c.Result()
	if seen, _ := c.FirstOutput(); seen {
		t.Fatal("non-SSE capture reported a first output")
	}
}

// The first reasoning event is kept even when visible output follows.
func TestFirstOutputIsSticky(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	_, _ = c.Write([]byte(sseData(`{"choices":[{"delta":{"reasoning":"r"}}]}`) +
		sseData(`{"choices":[{"delta":{"content":"c"}}]}`)))
	if seen, reasoning := c.FirstOutput(); !seen || !reasoning {
		t.Fatalf("FirstOutput() = %v %v, want true true", seen, reasoning)
	}
}

// After the first output the detector never decodes again, however long the
// stream, and usage capture is unaffected.
func TestFirstOutputStopsDecoding(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	_, _ = c.Write([]byte(sseData(`{"choices":[{"delta":{"role":"assistant","content":""}}]}`) +
		sseData(`{"choices":[{"delta":{"content":"a"}}]}`)))
	if seen, _ := c.FirstOutput(); !seen || c.firstDecodes != 2 {
		t.Fatalf("seen=%v decodes=%d, want true 2", seen, c.firstDecodes)
	}
	chunk := []byte(sseData(`{"choices":[{"delta":{"content":"more"}}]}`))
	for range 10_000 {
		_, _ = c.Write(chunk)
	}
	_, _ = c.Write([]byte(sseData(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7}}`) + sseData("[DONE]")))
	if c.firstDecodes != 2 {
		t.Fatalf("decodes = %d after first output, want 2", c.firstDecodes)
	}
	if u, ok := c.Result(); !ok || u.InputTokens != 5 || u.OutputTokens != 7 || !c.Completed("/chat/completions") {
		t.Fatalf("usage %+v %v completed=%v", u, ok, c.Completed("/chat/completions"))
	}
}
