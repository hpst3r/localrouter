package proxy

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	// maxEventBytes bounds one buffered SSE event (or line).
	maxEventBytes = 4 << 20
	// maxJSONBytes bounds the buffered non-SSE body parsed for usage.
	maxJSONBytes = 16 << 20
)

// usageJSON accepts both Responses (input/output) and chat (prompt/completion)
// usage shapes.
type usageJSON struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	InputDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`

	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// toUsage converts a decoded usage object; ok is false if it has no token counts.
func (u *usageJSON) toUsage() (core.Usage, bool) {
	if u == nil {
		return core.Usage{}, false
	}
	var out core.Usage
	switch {
	case u.InputTokens != nil || u.OutputTokens != nil:
		out.InputTokens = deref(u.InputTokens)
		out.OutputTokens = deref(u.OutputTokens)
		if u.InputDetails != nil {
			out.CachedInputTokens = u.InputDetails.CachedTokens
		}
		if u.OutputDetails != nil {
			out.ReasoningTokens = u.OutputDetails.ReasoningTokens
		}
	case u.PromptTokens != nil || u.CompletionTokens != nil:
		out.InputTokens = deref(u.PromptTokens)
		out.OutputTokens = deref(u.CompletionTokens)
		if u.PromptDetails != nil {
			out.CachedInputTokens = u.PromptDetails.CachedTokens
		}
		if u.CompletionDetails != nil {
			out.ReasoningTokens = u.CompletionDetails.ReasoningTokens
		}
	default:
		return core.Usage{}, false
	}
	return out, true
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// usageCapture observes response bytes as they stream and extracts usage.
// It never retains more than maxEventBytes (SSE) or maxJSONBytes (JSON).
type usageCapture struct {
	sse bool

	// SSE state.
	line     []byte // partial line carried between writes
	skipLine bool   // current line exceeded the cap; drop until newline
	event    string
	data     []byte
	dataOver bool // current event exceeded the cap

	// JSON state.
	body     bytes.Buffer
	jsonOver bool

	usage core.Usage
	known bool
}

func newUsageCapture(contentType string) *usageCapture {
	return &usageCapture{sse: strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")}
}

// Write feeds response bytes. It never fails.
func (c *usageCapture) Write(p []byte) (int, error) {
	if !c.sse {
		if !c.jsonOver {
			if c.body.Len()+len(p) > maxJSONBytes {
				c.jsonOver = true
				c.body = bytes.Buffer{}
			} else {
				c.body.Write(p)
			}
		}
		return len(p), nil
	}
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !c.skipLine {
				if len(c.line)+len(p) > maxEventBytes {
					c.skipLine, c.line = true, c.line[:0]
				} else {
					c.line = append(c.line, p...)
				}
			}
			break
		}
		seg := p[:i]
		p = p[i+1:]
		if c.skipLine {
			c.skipLine = false
			c.dataOver = true
			continue
		}
		var full []byte
		if len(c.line) > 0 {
			full = append(c.line, seg...)
		} else {
			full = seg
		}
		c.handleLine(bytes.TrimSuffix(full, []byte{'\r'}))
		c.line = c.line[:0]
	}
	return n, nil
}

func (c *usageCapture) handleLine(line []byte) {
	if len(line) == 0 {
		c.dispatch()
		return
	}
	if line[0] == ':' {
		return
	}
	field, value := line, []byte(nil)
	if i := bytes.IndexByte(line, ':'); i >= 0 {
		field, value = line[:i], line[i+1:]
		value = bytes.TrimPrefix(value, []byte{' '})
	}
	switch string(field) {
	case "event":
		c.event = string(value)
	case "data":
		if c.dataOver {
			return
		}
		if len(c.data)+len(value)+1 > maxEventBytes {
			c.dataOver, c.data = true, c.data[:0]
			return
		}
		if len(c.data) > 0 {
			c.data = append(c.data, '\n')
		}
		c.data = append(c.data, value...)
	}
}

func (c *usageCapture) dispatch() {
	data, event, over := c.data, c.event, c.dataOver
	c.data, c.event, c.dataOver = c.data[:0], "", false
	if over || len(data) == 0 || !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Response *struct {
			Usage *usageJSON `json:"usage"`
		} `json:"response"`
		Usage *usageJSON `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	typ := ev.Type
	if typ == "" {
		typ = event
	}
	switch typ {
	case "response.completed", "response.incomplete", "response.failed":
		if ev.Response != nil {
			if u, ok := ev.Response.Usage.toUsage(); ok {
				c.usage, c.known = u, true
			}
		}
		return
	}
	// Chat completions: the final chunk carries top-level usage.
	if u, ok := ev.Usage.toUsage(); ok {
		c.usage, c.known = u, true
	}
}

// Result returns the captured usage. For SSE it is the last usage-bearing
// event; for JSON bodies it is the top-level usage object.
func (c *usageCapture) Result() (core.Usage, bool) {
	if c.sse {
		// Flush a final event not terminated by a blank line.
		if len(c.line) > 0 && !c.skipLine {
			c.handleLine(bytes.TrimSuffix(c.line, []byte{'\r'}))
			c.line = c.line[:0]
		}
		if len(c.data) > 0 {
			c.dispatch()
		}
		return c.usage, c.known
	}
	if c.jsonOver || c.body.Len() == 0 {
		return core.Usage{}, false
	}
	var body struct {
		Usage *usageJSON `json:"usage"`
	}
	if json.Unmarshal(c.body.Bytes(), &body) != nil {
		return core.Usage{}, false
	}
	return body.Usage.toUsage()
}
