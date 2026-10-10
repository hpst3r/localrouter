// Package routing implements the pure, request-derived capability requirements
// used by capability-aware routing, together with the candidate filter and the
// per-account upstream model resolution derived from those requirements.
//
// Everything here is pure: it reads no configuration, performs no I/O and never
// dereferences a URL. Detection is declarative — it routes a request by what the
// request declares, and is explicitly NOT a security boundary: it cannot stop a
// client from embedding pixels in a field it does not model.
package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hpst3r/localrouter/internal/core"
)

// Wire protocols, matching the two inference endpoints.
const (
	ProtocolResponses = "responses"
	ProtocolChat      = "chat"
)

// Input modalities. text is implicitly supported by every candidate; image is
// an extra a candidate must explicitly declare. audio and file are
// known-but-unsupported modalities: they are reported in Requirements.Modalities
// so that no candidate can satisfy them and the request fails closed at
// admission (400 capability_unsupported) rather than being silently downgraded
// to text-only. file is a declared input modality (a file part / input_file), not
// a malformed body, so it must not be a 400 invalid_request_error and v1 adds no
// per-account file capability to the config or the descriptor.
const (
	ModalityText  = "text"
	ModalityImage = "image"
	ModalityAudio = "audio"
	ModalityFile  = "file"
)

// Requirements is the capability profile a request needs from the account it is
// routed to. It is derived from the endpoint and the request body only.
type Requirements struct {
	Protocol string // ProtocolResponses or ProtocolChat
	// Modalities lists every input modality the request actually carries, in
	// order of first appearance. text is only listed when the request really
	// carries text (a string instruction, a message or text part); an
	// image-only request lists just [image] so a vision-only candidate can serve
	// it. A request with no modelled input at all defaults to [text].
	Modalities []string
	Tools      bool // request declares tools / legacy functions
	JSONSchema bool // request asks for structured output
	Stream     bool // request asks for a streaming response
}

// ErrMalformed reports that a structured request body could not be classified:
// a content part, content container or structured-output declaration had a
// shape or type that cannot be safely interpreted. A capability-constrained
// route must fail closed on it (400 invalid_request_error); an unconstrained
// (legacy) route ignores it and forwards the body unchanged.
var ErrMalformed = errors.New("routing: request body cannot be classified")

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// Member names the classifier reads, per object kind. Every object is read
// case-sensitively through object, which also fails closed on a duplicate or a
// case variant of any member named here (see object).
var (
	rootMembers = []string{
		"model", "stream", "tools", "functions", "tool_choice", "function_call",
		"response_format", "text", "instructions", "input", "messages",
		"previous_response_id", "conversation", "prompt",
	}
	itemMembers    = []string{"type", "role", "content", "output"}
	messageMembers = []string{"content"}
	typeMembers    = []string{"type"}
	textMembers    = []string{"format"}
)

// Infer derives the Requirements of a request from its protocol (ProtocolChat
// or ProtocolResponses) and its raw body. It never scans free-form prompt text
// and never dereferences a URL; only structural fields are inspected.
//
// Infer is called ONLY on a capability-constrained route (a route with
// per-candidate descriptors); the legacy, unconstrained path forwards the body
// byte-for-byte and never calls Infer, so its behaviour is unchanged. Because
// the constrained route has promised to honour the capability contract, a body
// it cannot classify must fail closed: a non-object, invalid-JSON or otherwise
// unclassifiable body returns ErrMalformed (the caller turns that into a 400
// invalid_request_error) rather than being silently treated as a text request.
func Infer(protocol string, body []byte) (Requirements, error) {
	reqs := Requirements{Protocol: protocol}

	root, err := object("request body", body, rootMembers...)
	if err != nil {
		return reqs, err
	}

	if raw, ok := root["stream"]; ok && !isNull(raw) {
		var stream bool
		if err := json.Unmarshal(raw, &stream); err != nil {
			return reqs, malformed("stream must be a boolean")
		}
		reqs.Stream = stream
	}

	tools, err := detectTools(root)
	if err != nil {
		return reqs, err
	}
	reqs.Tools = tools

	schema, err := detectJSONSchema(protocol, root)
	if err != nil {
		return reqs, err
	}
	reqs.JSONSchema = schema

	mods, err := detectModalities(protocol, root)
	if err != nil {
		return reqs, err
	}
	if len(mods) == 0 {
		// No modelled input: stay conservative and require plain text.
		mods = []string{ModalityText}
	}
	reqs.Modalities = mods

	return reqs, nil
}

// detectTools reports whether the request declares tool/function calling. It
// counts the modern top-level "tools" array, the legacy "functions" array, and
// the presence of "tool_choice" or the legacy "function_call". A malformed
// declaration (a non-array tools/functions that is present and not null) is an
// error.
//
// A selector of "none" is an explicit opt-out: on its own it asks for no tool
// support and must not force a tools-capable candidate. A non-empty declared
// tools/functions array still requires support regardless of the selector.
func detectTools(root map[string]json.RawMessage) (bool, error) {
	for _, key := range []string{"tools", "functions"} {
		raw, ok := root[key]
		if !ok || isNull(raw) {
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return false, malformed("%s must be an array", key)
		}
		if len(arr) > 0 {
			return true, nil
		}
	}
	for _, key := range []string{"tool_choice", "function_call"} {
		raw, ok := root[key]
		if !ok || isNull(raw) {
			continue
		}
		if isNoneSelector(raw) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// isNoneSelector reports whether a tool_choice/function_call value is the
// string "none" — the one selector spelling that demands no tool support.
func isNoneSelector(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && s == "none"
}

// detectJSONSchema reports whether the request asks for structured output:
// chat "response_format" and Responses "text.format". A malformed declaration
// (wrong shape, missing or unknown type) is an error rather than a silent
// "text only".
func detectJSONSchema(protocol string, root map[string]json.RawMessage) (bool, error) {
	if protocol == ProtocolResponses {
		raw, ok := root["text"]
		if !ok || isNull(raw) {
			return false, nil
		}
		txt, err := object("text", raw, textMembers...)
		if err != nil {
			return false, err
		}
		format, ok := txt["format"]
		if !ok || isNull(format) {
			return false, nil
		}
		return formatIsSchema("text.format", format)
	}

	raw, ok := root["response_format"]
	if !ok || isNull(raw) {
		return false, nil
	}
	return formatIsSchema("response_format", raw)
}

func formatIsSchema(what string, raw json.RawMessage) (bool, error) {
	typ, err := objectType(what, raw)
	if err != nil {
		return false, err
	}
	return schemaType(what+".type", typ)
}

// schemaType maps a declared structured-output type to the JSONSchema
// requirement. v1 models the requirement as a single boolean, so every
// structured-output declaration — json_schema and json_object alike — requires
// JSONSchema support. The plain "text" type selects unstructured text; an empty
// or unknown type is malformed and fails closed.
func schemaType(what, typ string) (bool, error) {
	switch typ {
	case "json_schema", "json_object":
		return true, nil
	case "text":
		return false, nil
	case "":
		return false, malformed("%s is required", what)
	default:
		return false, malformed("unknown %s %q", what, typ)
	}
}

// detectModalities returns every input modality the request actually carries, in
// order of first appearance. text is added only when the request really carries
// text: a string instruction, a message string content, or a text content part.
// An image-only request therefore yields just [image] and can reach a
// vision-only backend, while an input_image accompanied by a textual prompt
// yields [text, image] and needs both. A known-but-unsupported modality (audio,
// file) is reported so no candidate can satisfy it; an unknown content type or
// malformed content shape is an error.
func detectModalities(protocol string, root map[string]json.RawMessage) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(m string) {
		if seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}

	if protocol == ProtocolResponses {
		// instructions is an optional plain-text field: its presence contributes
		// text, and a non-string value is malformed.
		if raw, ok := root["instructions"]; ok && !isNull(raw) {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, malformed("instructions must be a string")
			}
			add(ModalityText)
		}
		// previous_response_id, conversation and a reusable prompt (whose
		// template and variables may hold images or files) name opaque
		// server-side context that cannot be classified from this request alone:
		// fail closed rather than guess text.
		for _, key := range []string{"previous_response_id", "conversation", "prompt"} {
			if raw, ok := root[key]; ok && !isNull(raw) {
				return nil, malformed("%s cannot be classified", key)
			}
		}
		raw, ok := root["input"]
		if !ok || isNull(raw) {
			return out, nil
		}
		// A bare string input is the text shortcut.
		var s string
		if json.Unmarshal(raw, &s) == nil {
			add(ModalityText)
			return out, nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, malformed("input must be a string or an array")
		}
		for _, item := range items {
			if err := responsesItem(item, add); err != nil {
				return nil, err
			}
		}
		return out, nil
	}

	raw, ok := root["messages"]
	if !ok || isNull(raw) {
		return out, nil
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, malformed("messages must be an array")
	}
	for _, m := range msgs {
		if err := chatMessage(m, add); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// responsesItem inspects one element of a Responses "input" array. Only the
// item types listed below are modelled: tool outputs (function_call_output,
// custom_tool_call_output, local_shell_call_output, computer_call_output) are
// classified from their output, the model-emitted calls and reasoning carry no
// modality and are ignored, a reference to prior server-side content
// (item_reference) cannot be classified and fails closed, and an unknown item
// type — including a typeless object that is not a {role, content} message —
// is rejected rather than silently ignored.
func responsesItem(raw json.RawMessage, add func(string)) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		add(ModalityText)
		return nil
	}
	item, err := object("input item", raw, itemMembers...)
	if err != nil {
		return err
	}
	typ, err := stringMember("input item", item, "type")
	if err != nil {
		return err
	}
	switch typ {
	case "input_text":
		add(ModalityText)
		return nil
	case "input_image":
		add(ModalityImage)
		return nil
	case "input_audio":
		add(ModalityAudio)
		return nil
	case "input_file":
		add(ModalityFile)
		return nil
	case "message":
		return contentParts("message.content", item["content"], responsesPart, add)
	case "":
		// A typeless object is only the easy-message shape: a role and content.
		role, err := stringMember("input item", item, "role")
		if err != nil {
			return err
		}
		if role == "" || len(item["content"]) == 0 || isNull(item["content"]) {
			return malformed("typeless input item must be a message with role and content")
		}
		return contentParts("message.content", item["content"], responsesPart, add)
	case "function_call", "custom_tool_call", "local_shell_call":
		// A model-emitted call's arguments / input / action are opaque values the
		// model produced; they are not a content payload and are deliberately NOT
		// scanned for modality. The item carries no modelled input, so the
		// request stays conservative text.
		return nil
	case "function_call_output", "custom_tool_call_output":
		return functionCallOutput(typ, item, add)
	case "local_shell_call_output":
		return localShellCallOutput(item, add)
	case "reasoning":
		// reasoning is a non-content item: it carries no input modality.
		return nil
	case "item_reference":
		return malformed("item_reference cannot be classified")
	case "computer_call_output":
		return computerCallOutput(item, add)
	default:
		return malformed("unknown input item type %q", typ)
	}
}

// functionCallOutput classifies the "output" of a function_call_output or
// custom_tool_call_output item. Per the Responses schema output is either a
// plain string (the tool result) or an array of typed input content parts. A
// string is a real result payload — unlike function_call.arguments it is not an
// opaque value the model emitted mid-call — so it counts as text; but it is
// still never scanned, so an image URL embedded in the result string does not
// become an image requirement. An array is classified part by part, so an
// image/audio/file part reports its modality and any part the classifier does
// not model (including a typeless object) fails closed. Any other shape — a
// bare object, a missing or null output — fails closed rather than being
// silently downgraded to text.
func functionCallOutput(what string, item map[string]json.RawMessage, add func(string)) error {
	out := item["output"]
	if len(out) == 0 || isNull(out) {
		return malformed("%s.output is required", what)
	}
	var s string
	if json.Unmarshal(out, &s) == nil {
		add(ModalityText)
		return nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(out, &parts); err != nil {
		return malformed("%s.output must be a string or an array", what)
	}
	for _, p := range parts {
		if err := responsesPart(p, add); err != nil {
			return err
		}
	}
	return nil
}

// localShellCallOutput classifies a local_shell_call_output item, whose output
// is the command's text output and must be a string.
func localShellCallOutput(item map[string]json.RawMessage, add func(string)) error {
	var s string
	if out := item["output"]; len(out) == 0 || json.Unmarshal(out, &s) != nil {
		return malformed("local_shell_call_output.output must be a string")
	}
	add(ModalityText)
	return nil
}

// computerCallOutput classifies the nested payload of a computer_call_output
// item. A computer_screenshot payload is an image; a plain string output is
// text; an unmodelled payload type (a hidden payload we cannot see into) fails
// closed instead of being silently ignored.
func computerCallOutput(item map[string]json.RawMessage, add func(string)) error {
	out := item["output"]
	if len(out) == 0 || isNull(out) {
		return malformed("computer_call_output.output is required")
	}
	var s string
	if json.Unmarshal(out, &s) == nil {
		add(ModalityText)
		return nil
	}
	typ, err := objectType("computer_call_output.output", out)
	if err != nil {
		return err
	}
	switch typ {
	case "computer_screenshot":
		add(ModalityImage)
		return nil
	case "text":
		add(ModalityText)
		return nil
	default:
		return malformed("unknown computer_call_output output type %q", typ)
	}
}

func responsesPart(raw json.RawMessage, add func(string)) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		add(ModalityText)
		return nil
	}
	typ, err := objectType("Responses content part", raw)
	if err != nil {
		return err
	}
	switch typ {
	case "input_text", "output_text", "text", "refusal":
		add(ModalityText)
		return nil
	case "input_image":
		add(ModalityImage)
		return nil
	case "input_audio":
		add(ModalityAudio)
		return nil
	case "input_file":
		add(ModalityFile)
		return nil
	default:
		return malformed("unknown Responses content type %q", typ)
	}
}

func chatMessage(raw json.RawMessage, add func(string)) error {
	m, err := object("message", raw, messageMembers...)
	if err != nil {
		return err
	}
	return contentParts("message.content", m["content"], chatPart, add)
}

func chatPart(raw json.RawMessage, add func(string)) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		add(ModalityText) // a string part is text
		return nil
	}
	typ, err := objectType("chat content part", raw)
	if err != nil {
		return err
	}
	switch typ {
	case "text", "refusal":
		add(ModalityText)
		return nil
	case "image_url":
		add(ModalityImage)
		return nil
	case "input_audio":
		add(ModalityAudio)
		return nil
	case "file":
		add(ModalityFile)
		return nil
	default:
		return malformed("unknown chat content type %q", typ)
	}
}

// contentParts classifies a "content" value that may be a string or an array of
// parts. A bare string is text.
func contentParts(what string, raw json.RawMessage, part func(json.RawMessage, func(string)) error, add func(string)) error {
	if len(raw) == 0 || isNull(raw) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		add(ModalityText)
		return nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return malformed("%s must be a string or an array", what)
	}
	for _, p := range parts {
		if err := part(p, add); err != nil {
			return err
		}
	}
	return nil
}

// Filter returns the subset of candidates that can satisfy reqs, in the same
// order. It only ever removes entries: it never reorders, adds or widens the
// set, so failover stays inside the operator's consent boundary.
//
// When the route has no per-candidate descriptors (nil or empty Upstreams) the
// route is unconstrained and candidates is returned unchanged, preserving the
// legacy behaviour exactly.
func Filter(route core.Route, candidates []string, reqs Requirements) []string {
	if len(route.Upstreams) == 0 {
		return append([]string(nil), candidates...)
	}
	out := make([]string, 0, len(candidates))
	for _, id := range candidates {
		if candidateSupports(route, id, reqs) {
			out = append(out, id)
		}
	}
	return out
}

// candidateSupports reports whether the candidate's descriptor satisfies every
// requirement the request has. A candidate omitted from Upstreams gets the zero
// spec: inherit the route's upstream_model, provider-derived protocol, text-only
// modalities and no optional features.
func candidateSupports(route core.Route, id string, reqs Requirements) bool {
	spec := route.Upstreams[id]
	if !protocolSupported(spec, reqs.Protocol) {
		return false
	}
	for _, m := range reqs.Modalities {
		if !modalitySupported(spec, m) {
			return false
		}
	}
	if reqs.Tools && !spec.Tools {
		return false
	}
	if reqs.JSONSchema && !spec.JSONSchema {
		return false
	}
	if reqs.Stream && !spec.Stream {
		return false
	}
	return true
}

// protocolSupported reports whether the candidate serves the request protocol.
// An absent Protocols list means the candidate's protocol is derived from its
// account's provider and accepts either endpoint — the Codex restriction is
// enforced by the proxy's provider-keyed filter, which runs before Filter, so
// Filter itself stays provider-agnostic. A declared list must contain the
// request's protocol.
func protocolSupported(spec core.UpstreamSpec, protocol string) bool {
	if len(spec.Protocols) == 0 {
		return true
	}
	for _, p := range spec.Protocols {
		if p == protocol {
			return true
		}
	}
	return false
}

// modalitySupported reports whether the candidate supports a modality. An empty
// InputModalities list means text-only, and a declared list is explicit:
// declaring only "image" is a vision-only candidate and never implies "text".
// A modality no descriptor can declare (audio, file) is therefore never
// supported and the request fails closed.
func modalitySupported(spec core.UpstreamSpec, modality string) bool {
	if len(spec.InputModalities) == 0 {
		return modality == ModalityText
	}
	for _, m := range spec.InputModalities {
		if m == modality {
			return true
		}
	}
	return false
}

// ResolveModel returns the upstream model name to send to accountID for a
// route: the candidate's descriptor wins, then the route's legacy
// upstream_model, and finally the client-facing model unchanged.
func ResolveModel(route core.Route, accountID, clientModel string) string {
	if spec, ok := route.Upstreams[accountID]; ok && spec.UpstreamModel != "" {
		return spec.UpstreamModel
	}
	if route.UpstreamModel != "" {
		return route.UpstreamModel
	}
	return clientModel
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// object decodes raw as exactly one JSON object and returns the members named
// in members, matched by exact name. Member names are case-sensitive (RFC 8259),
// which is how a map- or dict-based upstream reads the body; encoding/json
// struct decoding instead matches case-insensitively and lets the last
// duplicate win. So that the classifier and every upstream reader see the same
// value, an object holding a duplicate or a case variant (Unicode simple
// folding, as encoding/json uses) of a named member is ambiguous and fails
// closed. Other members are skipped unread, whatever they contain.
func object(what string, raw []byte, members ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, malformed("%s must be a JSON object", what)
	}
	out := make(map[string]json.RawMessage, len(members))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, malformed("%s must be a JSON object", what)
		}
		name, _ := tok.(string)
		member := ""
		for _, m := range members {
			if strings.EqualFold(name, m) {
				member = m
				break
			}
		}
		if member == "" {
			if err := dec.Decode(&skipValue{}); err != nil {
				return nil, malformed("%s must be a JSON object", what)
			}
			continue
		}
		if _, dup := out[member]; dup || name != member {
			return nil, malformed("%s has an ambiguous %q member", what, member)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, malformed("%s must be a JSON object", what)
		}
		out[member] = v
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, malformed("%s must be a JSON object", what)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, malformed("%s must be a single JSON object", what)
	}
	return out, nil
}

// skipValue discards a JSON value the classifier does not read.
type skipValue struct{}

func (skipValue) UnmarshalJSON([]byte) error { return nil }

// stringMember returns obj's string member key; absent or null reads as "".
func stringMember(what string, obj map[string]json.RawMessage, key string) (string, error) {
	raw, ok := obj[key]
	if !ok || isNull(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", malformed("%s.%s must be a string", what, key)
	}
	return s, nil
}

// objectType returns the "type" member of the JSON object raw.
func objectType(what string, raw []byte) (string, error) {
	obj, err := object(what, raw, typeMembers...)
	if err != nil {
		return "", err
	}
	return stringMember(what, obj, "type")
}
