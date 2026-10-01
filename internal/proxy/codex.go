package proxy

import (
	"bytes"
	"encoding/json"
)

// codexRejectedFields are Responses API body fields the ChatGPT Codex backend
// rejects with HTTP 400. Generic Responses clients (and Hermes when pointed at
// a non-chatgpt.com base URL) send them, so the proxy strips them per attempt
// when the selected upstream account is a Codex account.
var codexRejectedFields = []string{
	"max_output_tokens",
	"max_tokens",
	"max_completion_tokens",
	"metadata",
}

// codexBody returns body normalized for the Codex backend: rejected fields
// removed and "store" forced to false (the backend requires store=false).
// If body is not a JSON object it is returned unchanged; the upstream will
// report the error.
func codexBody(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	changed := false
	for _, k := range codexRejectedFields {
		if _, ok := m[k]; ok {
			delete(m, k)
			changed = true
		}
	}
	if v, ok := m["store"]; !ok || !bytes.Equal(bytes.TrimSpace(v), []byte("false")) {
		m["store"] = json.RawMessage("false")
		changed = true
	}
	if !changed {
		return body
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return body
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
