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
