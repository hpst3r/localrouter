package mcpserver

import (
	"bytes"
	"encoding/json"
	"strings"
)

// argBag is a strictly decoded tool-call argument object. Strictness matters:
// an unknown field is a rejected call rather than a silently ignored typo, a
// value of the wrong JSON type is rejected rather than coerced (a JSON number is
// never accepted where a string is expected, and neither is true/false), a
// duplicated key is rejected rather than last-wins, and an explicitly supplied
// null or blank value is rejected rather than defaulted.
type argBag struct {
	m map[string]json.RawMessage
}

// decodeArgs parses the raw tool-call arguments. Absent arguments (nil or empty)
// and an empty object are the empty bag. Every other value must be a single JSON
// object with no duplicate keys and no key outside allowed.
func decodeArgs(raw json.RawMessage, allowed ...string) (argBag, *toolError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return argBag{m: map[string]json.RawMessage{}}, nil
	}
	if string(trimmed) == "null" {
		// An explicitly supplied null is not the same as an omitted argument
		// object, so it is rejected rather than guessed at.
		return argBag{}, invalidArg("arguments must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return argBag{}, invalidArg("arguments must be a JSON object")
	}
	if dec.More() {
		return argBag{}, invalidArg("arguments must be a single JSON object")
	}
	if dup, ok := firstDuplicateKey(trimmed); ok {
		return argBag{}, invalidArg("duplicate argument %q", dup)
	}
	permitted := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		permitted[k] = true
	}
	for k := range m {
		if !permitted[k] {
			return argBag{}, invalidArg("unknown argument %q", k)
		}
	}
	return argBag{m: m}, nil
}

// firstDuplicateKey scans the top-level JSON object and reports the first key
// that appears more than once. encoding/json keeps only the last value for a
// duplicate key and says nothing about it, so a caller that appends or merges
// arguments could silently override an earlier pin; the argument gate therefore
// requires each key at most once. Only top-level keys are considered: an
// argument value is either a scalar or an unknown field, so a nested duplicate
// can never reach a tool.
func firstDuplicateKey(raw []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	depth := 0
	expectKey := false
	seen := map[string]bool{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false // EOF, or malformed (already rejected above)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				depth++
				if depth == 1 {
					expectKey = true
				}
			case '[':
				depth++
			case '}', ']':
				depth--
				if depth == 1 {
					expectKey = true
				}
			}
			continue
		}
		if depth != 1 {
			continue // inside a nested container
		}
		if expectKey {
			s, _ := tok.(string)
			if seen[s] {
				return s, true
			}
			seen[s] = true
			expectKey = false
			continue
		}
		expectKey = true // a scalar value just completed
	}
}

// isJSONNull reports whether a raw value is the JSON literal null.
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// has reports whether key was supplied at all, including an explicit null.
func (b argBag) has(key string) bool {
	_, ok := b.m[key]
	return ok
}

// present reports whether key was supplied with a non-null value.
func (b argBag) present(key string) bool {
	return b.has(key) && !isJSONNull(b.m[key])
}

// str returns a supplied string argument. A supplied null, a non-string JSON
// value or a blank/whitespace-only string is an error: never a coercion, never a
// silent default. A valid value is returned byte for byte — an account or model
// identifier is matched literally, so surrounding whitespace is rejected rather
// than normalized away.
func (b argBag) str(key string, required bool) (string, *toolError) {
	if !b.has(key) {
		if required {
			return "", invalidArg("argument %q is required", key)
		}
		return "", nil
	}
	if isJSONNull(b.m[key]) {
		return "", invalidArg("argument %q must not be null", key)
	}
	var s string
	if err := json.Unmarshal(b.m[key], &s); err != nil {
		return "", invalidArg("argument %q must be a JSON string", key)
	}
	if strings.TrimSpace(s) == "" {
		return "", invalidArg("argument %q must not be blank", key)
	}
	if s != strings.TrimSpace(s) {
		return "", invalidArg("argument %q must not have leading or trailing whitespace", key)
	}
	return s, nil
}

// integer returns a supplied whole-number argument. A JSON string, boolean,
// null, fractional or exponent value is rejected: no numeric or boolean
// coercion. json.Number would happily absorb a quoted "7", so the token is
// checked to be an unquoted number before it is parsed.
func (b argBag) integer(key string) (int, bool, *toolError) {
	if !b.has(key) {
		return 0, false, nil
	}
	if isJSONNull(b.m[key]) {
		return 0, false, invalidArg("argument %q must not be null", key)
	}
	token := bytes.TrimSpace(b.m[key])
	if len(token) == 0 || (token[0] != '-' && (token[0] < '0' || token[0] > '9')) {
		return 0, false, invalidArg("argument %q must be a JSON number", key)
	}
	var n json.Number
	if err := json.Unmarshal(b.m[key], &n); err != nil {
		return 0, false, invalidArg("argument %q must be a JSON number", key)
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false, invalidArg("argument %q must be a whole number", key)
	}
	if i > int64(maxInt) || i < int64(minInt) {
		return 0, false, invalidArg("argument %q is out of range", key)
	}
	return int(i), true, nil
}

const (
	maxInt = int(^uint(0) >> 1)
	minInt = -maxInt - 1
)

// enum returns a supplied argument that must be one of values. Absent is
// reported as (ok=false, nil) so the caller applies its default; a supplied
// value that is blank, null or not in values is an error.
func (b argBag) enum(key string, values []string) (string, bool, *toolError) {
	s, err := b.str(key, false)
	if err != nil {
		return "", false, err
	}
	if s == "" {
		return "", false, nil // absent: the caller applies its default
	}
	for _, v := range values {
		if s == v {
			return s, true, nil
		}
	}
	return "", false, invalidArg("argument %q must be one of %s", key, strings.Join(values, ", "))
}
