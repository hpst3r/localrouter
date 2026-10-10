package proxy

import (
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

func feed(c *usageCapture, s string, step int) {
	for i := 0; i < len(s); i += step {
		_, _ = c.Write([]byte(s[i:min(i+step, len(s))]))
	}
}

func TestSSECaptureChunkings(t *testing.T) {
	stream := "event: response.completed\r\n" +
		"data: {\"type\":\"response.completed\",\r\n" +
		"data:\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":2," +
		"\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\r\n\r\n"
	want := core.Usage{InputTokens: 7, CachedInputTokens: 3, OutputTokens: 2, ReasoningTokens: 1}
	for _, step := range []int{1, 2, 3, 5, 64, len(stream)} {
		c := newUsageCapture("text/event-stream")
		feed(c, stream, step)
		if u, ok := c.Result(); !ok || u != want {
			t.Fatalf("step %d: %+v %v", step, u, ok)
		}
	}
}

func TestSSEIgnoresNonTerminalUsage(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	feed(c, "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", 10)
	if _, ok := c.Result(); ok {
		t.Fatal("in_progress usage must not count")
	}
}

func TestSSEIncompleteAndUnterminated(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	feed(c, "data: {\"type\":\"response.incomplete\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":5}}}", 4)
	if u, ok := c.Result(); !ok || u.InputTokens != 4 || u.OutputTokens != 5 {
		t.Fatalf("%+v %v", u, ok)
	}
}

func TestSSEOversizedEventDropped(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	big := "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1},\"x\":\"" + strings.Repeat("a", maxEventBytes) + "\"}\n\n"
	feed(c, big, 1<<16)
	if _, ok := c.Result(); ok {
		t.Fatal("oversized event should be dropped")
	}
	if cap(c.line) > 2*maxEventBytes || cap(c.data) > 2*maxEventBytes {
		t.Fatal("buffers grew beyond cap")
	}
	// Parser recovers for the next event.
	feed(c, "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\n", 7)
	if u, ok := c.Result(); !ok || u.InputTokens != 2 {
		t.Fatalf("%+v %v", u, ok)
	}
}

func TestJSONCapture(t *testing.T) {
	c := newUsageCapture("application/json")
	feed(c, `{"usage":{"input_tokens":3,"output_tokens":4}}`, 3)
	if u, ok := c.Result(); !ok || u.InputTokens != 3 || u.OutputTokens != 4 {
		t.Fatalf("%+v %v", u, ok)
	}
	c = newUsageCapture("application/json")
	feed(c, `{"error":{"message":"x"}}`, 100)
	if _, ok := c.Result(); ok {
		t.Fatal("no usage expected")
	}
}

func TestRewriteBodyNoop(t *testing.T) {
	in := []byte(`{"model":"m",  "x":1}`)
	out, err := rewriteBody(in, "", false)
	if err != nil || string(out) != string(in) {
		t.Fatalf("%s %v", out, err)
	}
	out, err = rewriteBody([]byte(`{"model":"m","stream_options":null}`), "", true)
	if err != nil || string(out) != `{"model":"m","stream_options":{"include_usage":true}}` {
		t.Fatalf("%s %v", out, err)
	}
}

// Protocol completion is tracked separately from usage: a stream is complete
// only when its protocol's own terminal arrived, whatever usage or cost it
// carried. Chat completions end with data: [DONE]; Responses ends with a
// response.completed, response.incomplete or response.failed event carrying
// its response object. A non-stream JSON body is framed by HTTP and needs no
// marker.
func TestCaptureCompletion(t *testing.T) {
	const (
		chat = "/chat/completions"
		resp = "/responses"
	)
	usageChunk := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"cost\":0}}\n\n"
	finishChunk := "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"cost\":0.001}}\n\n"
	for _, tc := range []struct {
		name     string
		ct       string
		endpoint string
		body     string
		want     bool
	}{
		{"chat usage then clean EOF", "text/event-stream", chat, usageChunk, false},
		{"chat finish_reason without DONE", "text/event-stream", chat, usageChunk + finishChunk, false},
		{"chat DONE", "text/event-stream", chat, usageChunk + finishChunk + "data: [DONE]\n\n", true},
		{"chat DONE without usage", "text/event-stream", chat, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n", true},
		{"chat DONE no space crlf", "text/event-stream", chat, usageChunk + "data:[DONE]\r\n\r\n", true},
		{"chat DONE unterminated final line", "text/event-stream", chat, usageChunk + "data: [DONE]", true},
		{"chat truncated DONE", "text/event-stream", chat, usageChunk + "data: [DO", false},
		{"chat response.completed is not chat terminal", "text/event-stream", chat,
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", false},
		{"chat empty stream", "text/event-stream", chat, "", false},
		{"responses completed", "text/event-stream", resp,
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", true},
		{"responses completed without usage", "text/event-stream", resp,
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n", true},
		{"responses incomplete", "text/event-stream", resp,
			"data: {\"type\":\"response.incomplete\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":5}}}\n\n", true},
		{"responses failed", "text/event-stream", resp,
			"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n", true},
		{"responses type from event line", "text/event-stream", resp,
			"event: response.completed\ndata: {\"response\":{\"status\":\"completed\"}}\n\n", true},
		{"responses in_progress then EOF", "text/event-stream", resp,
			"data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", false},
		{"responses deltas then EOF", "text/event-stream", resp,
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"response.completed\"}\n\n", false},
		{"responses completed without response object", "text/event-stream", resp,
			"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n", false},
		{"responses truncated completed", "text/event-stream", resp,
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tok", false},
		{"responses DONE is not responses terminal", "text/event-stream", resp, "data: [DONE]\n\n", false},
		{"json chat body", "application/json", chat, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`, true},
		{"json responses body without usage", "application/json", resp, `{"id":"r1"}`, true},
		{"empty json body", "application/json", resp, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, step := range []int{1, 3, 64, max(len(tc.body), 1)} {
				c := newUsageCapture(tc.ct)
				feed(c, tc.body, step)
				_, _ = c.Result()
				if got := c.Completed(tc.endpoint); got != tc.want {
					t.Fatalf("step %d: Completed(%s) = %v, want %v", step, tc.endpoint, got, tc.want)
				}
			}
		})
	}
}

// A terminal event too large to buffer was never seen whole, so it is not
// trusted as completion.
func TestCaptureCompletionOversizedTerminalNotTrusted(t *testing.T) {
	c := newUsageCapture("text/event-stream")
	big := "data: {\"type\":\"response.completed\",\"response\":{\"x\":\"" + strings.Repeat("a", maxEventBytes) + "\"}}\n\n"
	feed(c, big, 1<<16)
	_, _ = c.Result()
	if c.Completed("/responses") {
		t.Fatal("an oversized, dropped terminal event must not count as completion")
	}
}
