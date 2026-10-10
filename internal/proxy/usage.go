package proxy

import (
	"bytes"
	"encoding/json"
	"math"
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

	// Cost is the provider-reported cost in USD (OpenRouter usage.cost). It is
	// kept as raw JSON so that an unusable value (string, null, negative,
	// non-finite, out of range) is ignored without failing the decode and
	// losing otherwise-valid token counts. cost_details is deliberately not
	// declared: only usage.cost is ever considered.
	Cost json.RawMessage `json:"cost"`
}

// reportedCostUSD decodes the provider-reported cost strictly. A usable cost
// is a JSON number that is finite, non-negative and at most
// core.MaxReportedCostUSD (so ledger sums cannot overflow); an explicit zero
// is valid. Missing, null, non-numeric, negative, non-finite, or out-of-range
// values yield nil.
func (u *usageJSON) reportedCostUSD() *float64 {
	if u == nil {
		return nil
	}
	raw := bytes.TrimSpace(u.Cost)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > core.MaxReportedCostUSD {
		return nil
	}
	return &f
}

// toUsage converts a decoded usage object; ok is false if it has no token
// counts or any count is out of range (see outOfRange).
func (u *usageJSON) toUsage() (core.Usage, bool) {
	if u == nil || u.outOfRange() {
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
	// Cached input is a subset of input and reasoning a subset of output.
	out.CachedInputTokens = min(out.CachedInputTokens, out.InputTokens)
	out.ReasoningTokens = min(out.ReasoningTokens, out.OutputTokens)
	return out, true
}

// outOfRange reports whether any reported token count is negative or above
// core.MaxRecordTokens. Such usage is garbage (buggy or hostile upstream) and
// is recorded as unknown rather than stored.
func (u *usageJSON) outOfRange() bool {
	bad := func(p *int64) bool { return p != nil && (*p < 0 || *p > core.MaxRecordTokens) }
	ptrs := []*int64{u.InputTokens, u.OutputTokens, u.PromptTokens, u.CompletionTokens}
	if u.InputDetails != nil {
		ptrs = append(ptrs, &u.InputDetails.CachedTokens)
	}
	if u.OutputDetails != nil {
		ptrs = append(ptrs, &u.OutputDetails.ReasoningTokens)
	}
	if u.PromptDetails != nil {
		ptrs = append(ptrs, &u.PromptDetails.CachedTokens)
	}
	if u.CompletionDetails != nil {
		ptrs = append(ptrs, &u.CompletionDetails.ReasoningTokens)
	}
	for _, p := range ptrs {
		if bad(p) {
			return true
		}
	}
	return false
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
	// bad is set once any usage record had an out-of-range count; the
	// response's usage is then unknown whatever other records say.
	bad bool

	// reportedCost is the usable provider-reported cost in the latest
	// meaningful usage record. It is tracked independently of token
	// knowledge so a cost-only usage object still yields a value; nil means
	// that latest record did not carry a usable cost.
	reportedCost *float64

	// Protocol completion, tracked independently of usage and cost: whether
	// the stream's own terminal arrived whole. sawDone is a chat
	// "data: [DONE]"; sawResponseEnd is a Responses response.completed,
	// response.incomplete or response.failed event carrying its response
	// object. See Completed.
	sawDone        bool
	sawResponseEnd bool
}

// doneSentinel is the chat completions stream terminal's data payload.
var doneSentinel = []byte("[DONE]")

// isResponseEnd reports whether typ is a Responses terminal event type.
func isResponseEnd(typ string) bool {
	switch typ {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	}
	return false
}

// mayEndResponse is a cheap pre-filter for events that could be a Responses
// terminal, so usage-less events are only decoded when they might be one.
func mayEndResponse(event string, data []byte) bool {
	return isResponseEnd(event) ||
		bytes.Contains(data, []byte("response.completed")) ||
		bytes.Contains(data, []byte("response.incomplete")) ||
		bytes.Contains(data, []byte("response.failed"))
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
	if over || len(data) == 0 {
		return
	}
	if bytes.Equal(bytes.TrimSpace(data), doneSentinel) {
		c.sawDone = true
		return
	}
	if !bytes.Contains(data, []byte(`"usage"`)) && !mayEndResponse(event, data) {
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
	if isResponseEnd(typ) {
		if ev.Response != nil {
			// A terminal is complete without usage: some providers omit it.
			c.sawResponseEnd = true
			if u := ev.Response.Usage; u != nil {
				// The most recent usage-bearing record is authoritative for
				// cost: re-read it unconditionally so a stale intermediate
				// cost (e.g. 0 from continuous usage stats) cannot survive a
				// final record whose cost is unusable. Cost knowledge stays
				// independent of token knowledge, so a cost-only usage object
				// still yields a value.
				c.reportedCost = u.reportedCostUSD()
				c.bad = c.bad || u.outOfRange()
				if tok, ok := u.toUsage(); ok {
					c.usage, c.known = tok, true
				}
			}
		}
		return
	}
	// Chat completions: the final chunk carries top-level usage.
	if u := ev.Usage; u != nil {
		// Same authority rule as above: the latest usage-bearing record decides
		// the cost, and an unusable cost clears any earlier one.
		c.reportedCost = u.reportedCostUSD()
		c.bad = c.bad || u.outOfRange()
		if tok, ok := u.toUsage(); ok {
			c.usage, c.known = tok, true
		}
	}
}

// flush dispatches a final SSE event not terminated by a blank line. It is
// idempotent.
func (c *usageCapture) flush() {
	if len(c.line) > 0 && !c.skipLine {
		c.handleLine(bytes.TrimSuffix(c.line, []byte{'\r'}))
		c.line = c.line[:0]
	}
	if len(c.data) > 0 {
		c.dispatch()
	}
}

// Result returns the captured usage. For SSE it is the last usage-bearing
// event; for JSON bodies it is the top-level usage object. Usage says nothing
// about whether the stream finished; see Completed.
func (c *usageCapture) Result() (core.Usage, bool) {
	if c.sse {
		c.flush()
		if c.bad {
			return core.Usage{}, false
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
	if u := body.Usage; u != nil {
		if cost := u.reportedCostUSD(); cost != nil {
			c.reportedCost = cost
		}
	}
	return body.Usage.toUsage()
}

// ReportedCost returns the usable cost in the latest meaningful usage record,
// or nil if that record did not carry one. It is independent of token knowledge:
// a cost-only usage object still yields a value. Call it after Result so any
// final event buffered without a trailing blank line has been flushed.
func (c *usageCapture) ReportedCost() *float64 { return c.reportedCost }

// Completed reports whether the response reached its protocol's own terminal,
// independently of any usage or cost it carried. Call it after the body was
// read to a clean EOF. A non-stream body is framed by HTTP and needs no
// marker, so it is always complete. An event stream is complete only if its
// terminal arrived whole: "data: [DONE]" for chat completions, or a
// response.completed, response.incomplete or response.failed event with its
// response object for Responses (which never sends [DONE]). A chat chunk with
// finish_reason is not a terminal, and neither protocol accepts the other's.
// endpoint is the upstream path, "/chat/completions" or "/responses"; a
// stream on any other endpoint is never complete.
func (c *usageCapture) Completed(endpoint string) bool {
	if !c.sse {
		return true
	}
	c.flush()
	switch endpoint {
	case "/chat/completions":
		return c.sawDone
	case "/responses":
		return c.sawResponseEnd
	}
	return false
}
