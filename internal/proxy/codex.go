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
// report the error. cacheKey is the body's prompt_cache_key when it is usable
// as a session_id header value (see sessionIDFromCacheKey), else "".
func codexBody(body []byte) (out []byte, cacheKey string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body, ""
	}
	cacheKey = sessionIDFromCacheKey(m["prompt_cache_key"])
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
		return body, cacheKey
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return body, cacheKey
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), cacheKey
}

// maxSessionIDBytes bounds a session_id synthesized from prompt_cache_key.
const maxSessionIDBytes = 256

// sessionIDFromCacheKey returns a raw prompt_cache_key value as a session_id
// header value. The Codex backend only caches prompts for requests carrying a
// session_id header, and clients that send prompt_cache_key to a non-ChatGPT
// base URL omit it. Anything other than a non-empty JSON string of at most
// maxSessionIDBytes printable ASCII bytes yields "": it is never truncated.
func sessionIDFromCacheKey(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || s == "" || len(s) > maxSessionIDBytes {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return ""
		}
	}
	return s
}
