package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/hpst3r/localrouter/internal/core"
)

// Typed-decision wire protocols, matching POST /v1/systemone (TypeSafe System
// One shape) and POST /v1/decisions (OpenRouter Decisions API shape). They are
// never translated into each other or into chat/responses.
const (
	ProtocolSystemOne = "systemone"
	ProtocolDecisions = "decisions"
)

// Question types a v1 decision descriptor may declare. box needs images and is
// rejected as unsupported.
const (
	QuestionNoul   = "noul"
	QuestionChoice = "choice"
	QuestionScore  = "score"
)

// ErrDecisionUnsupported reports a decision request that uses a feature v1
// does not route: non-text (image, audio, file, video) parts, assets, box
// questions, streaming, or an OpenAI-style input field. The caller answers 400 decision_unsupported.
var ErrDecisionUnsupported = errors.New("routing: decision feature unsupported")

// ErrDecisionBounds reports a decision request over a configured bound
// (DecisionBounds). The caller answers 400 decision_bounds_exceeded.
var ErrDecisionBounds = errors.New("routing: decision bounds exceeded")

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDecisionUnsupported, fmt.Sprintf(format, args...))
}

func overBound(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDecisionBounds, fmt.Sprintf(format, args...))
}

// DecisionBounds limits the work the classifier accepts. A zero field takes
// its DefaultDecisionBounds value.
type DecisionBounds struct {
	MaxQuestions         int
	MaxQuestionNameBytes int
	MaxChoiceOptions     int
	MaxScoreLevels       int
	// MaxNesting bounds the depth of each of state, instructions and criteria:
	// a scalar is depth 0, {} or [] is depth 1.
	MaxNesting int
}

// DefaultDecisionBounds returns the documented defaults: 64 questions, 128-byte
// names, 255 choice options, 10 score levels, nesting 64.
func DefaultDecisionBounds() DecisionBounds {
	return DecisionBounds{MaxQuestions: 64, MaxQuestionNameBytes: 128, MaxChoiceOptions: 255, MaxScoreLevels: 10, MaxNesting: 64}
}

func (b DecisionBounds) withDefaults() DecisionBounds {
	d := DefaultDecisionBounds()
	for _, f := range []struct{ v, def *int }{
		{&b.MaxQuestions, &d.MaxQuestions},
		{&b.MaxQuestionNameBytes, &d.MaxQuestionNameBytes},
		{&b.MaxChoiceOptions, &d.MaxChoiceOptions},
		{&b.MaxScoreLevels, &d.MaxScoreLevels},
		{&b.MaxNesting, &d.MaxNesting},
	} {
		if *f.v <= 0 {
			*f.v = *f.def
		}
	}
	return b
}

// DecisionRequirements is what a decision request needs from a candidate.
type DecisionRequirements struct {
	Protocol string // ProtocolSystemOne or ProtocolDecisions
	// QuestionTypes are the distinct question types asked, in order of first
	// appearance.
	QuestionTypes []string
}

// IsDecisionProtocol reports whether p is a typed-decision protocol.
func IsDecisionProtocol(p string) bool { return p == ProtocolSystemOne || p == ProtocolDecisions }

// RouteDecisionProtocol returns the decision protocol a route serves, or "" for
// a chat/responses or legacy route. Config guarantees a decision route is
// single-family, so the first decision protocol found is the route's.
func RouteDecisionProtocol(route core.Route) string {
	for _, s := range route.Upstreams {
		for _, p := range s.Protocols {
			if IsDecisionProtocol(p) {
				return p
			}
		}
	}
	return ""
}

var (
	systemOneMembers = []string{"model", "stream", "state", "questions", "assets", "input"}
	decisionsMembers = append(append([]string(nil), systemOneMembers...), "provider", "session_id", "trace", "user")
	questionMembers  = []string{"type", "instructions", "criteria"}
)

// InferDecision strictly classifies a typed-decision body for protocol
// (ProtocolSystemOne or ProtocolDecisions). Members are read by exact name; a
// duplicate or case variant of a member it reads is ErrMalformed, as are
// duplicate or case-fold-variant question and choice option names. Unknown
// members (top level and inside questions) are forwarded untouched but still
// scanned like state. A non-text part
// (nonTextTypes) in state, instructions, criteria or any other top-level or
// question member, assets, box questions, stream:true and an input member (with or
// without state) are ErrDecisionUnsupported; exceeding b is ErrDecisionBounds.
// Returned error text never quotes body content.
func InferDecision(protocol string, body []byte, b DecisionBounds) (DecisionRequirements, error) {
	b = b.withDefaults()
	reqs := DecisionRequirements{Protocol: protocol}
	members := systemOneMembers
	if protocol == ProtocolDecisions {
		members = decisionsMembers
	}
	root, err := object("request body", body, members...)
	if err != nil {
		return reqs, err
	}
	if raw, ok := root["stream"]; ok && !isNull(raw) {
		var s bool
		if json.Unmarshal(raw, &s) != nil {
			return reqs, malformed("stream must be a boolean")
		}
		if s {
			return reqs, unsupported("stream is not supported on decision endpoints")
		}
	}
	if _, ok := root["assets"]; ok {
		return reqs, unsupported("assets (images) are not supported")
	}
	if _, ok := root["input"]; ok {
		return reqs, unsupported("the OpenAI-style input field is not translated; send the content as state")
	}
	state, ok := root["state"]
	if !ok || isNull(state) {
		return reqs, malformed("state is required")
	}
	if c := state[0]; c != '"' && c != '{' && c != '[' {
		return reqs, malformed("state must be a string, object or array")
	}
	// Every top-level member but questions (checked per question below),
	// including unknown passthrough members, is scanned exactly like state.
	if err := scanMembers("request body", body, members, []string{"questions"}, "a top-level member", b.MaxNesting); err != nil {
		return reqs, err
	}

	qraw, ok := root["questions"]
	if !ok || isNull(qraw) {
		return reqs, malformed("questions is required")
	}
	_, questions, err := uniqueMembers("questions", qraw, b.MaxQuestions, b.MaxQuestionNameBytes)
	if err != nil {
		return reqs, err
	}
	if len(questions) == 0 {
		return reqs, malformed("questions must not be empty")
	}
	seen := map[string]bool{}
	for _, q := range questions {
		typ, err := question(q, b)
		if err != nil {
			return reqs, err
		}
		if !seen[typ] {
			seen[typ] = true
			reqs.QuestionTypes = append(reqs.QuestionTypes, typ)
		}
	}
	return reqs, nil
}

// question validates one question object and returns its type.
func question(raw json.RawMessage, b DecisionBounds) (string, error) {
	q, err := object("question", raw, questionMembers...)
	if err != nil {
		return "", err
	}
	t, ok := q["type"]
	var typ string
	if !ok || json.Unmarshal(t, &typ) != nil || typ == "" {
		return "", malformed("question type must be a non-empty string")
	}
	// Unknown question members are forwarded untouched but scanned like
	// instructions.
	if err := scanMembers("question", raw, nil, questionMembers, "a question member", b.MaxNesting); err != nil {
		return "", err
	}
	if ins, ok := q["instructions"]; ok {
		if err := textOnly("instructions", ins, b.MaxNesting); err != nil {
			return "", err
		}
	}
	crit, hasCrit := q["criteria"]
	if hasCrit {
		if err := textOnly("criteria", crit, b.MaxNesting); err != nil {
			return "", err
		}
	}
	switch typ {
	case QuestionNoul:
		if !hasCrit || isNull(crit) {
			return typ, nil
		}
		names, _, err := uniqueMembers("noul criteria", crit, 0, 0)
		if err != nil {
			return "", err
		}
		for _, n := range names {
			if n != "true" && n != "false" {
				return "", malformed("noul criteria may only have true and false members")
			}
		}
	case QuestionChoice:
		if !hasCrit || isNull(crit) {
			return "", malformed("choice criteria is required")
		}
		names, _, err := uniqueMembers("choice criteria", crit, b.MaxChoiceOptions, 0)
		if err != nil {
			return "", err
		}
		if len(names) == 0 {
			return "", malformed("choice criteria must have at least one option")
		}
	case QuestionScore:
		if !hasCrit || isNull(crit) {
			return "", malformed("score criteria is required")
		}
		var levels []json.RawMessage
		if json.Unmarshal(crit, &levels) != nil {
			return "", malformed("score criteria must be an array")
		}
		if len(levels) == 0 {
			return "", malformed("score criteria must have at least one level")
		}
		if len(levels) > b.MaxScoreLevels {
			return "", overBound("score criteria has more than %d levels", b.MaxScoreLevels)
		}
	case "box":
		return "", unsupported("box questions are not supported")
	default:
		return "", malformed("unknown question type")
	}
	return typ, nil
}

// uniqueMembers reads every member of the JSON object raw in order. A
// duplicate name, or a name that is a case-fold variant (Unicode simple
// folding, as strings.EqualFold) of an earlier one, is ErrMalformed. limit > 0
// bounds the member count and nameLimit > 0 bounds each name's bytes (an
// empty name is then also rejected); both are ErrDecisionBounds, checked while
// scanning so an oversized object is not read to the end.
func uniqueMembers(what string, raw []byte, limit, nameLimit int) ([]string, []json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, malformed("%s must be a JSON object", what)
	}
	var names []string
	var values []json.RawMessage
	folded := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, malformed("%s must be a JSON object", what)
		}
		name, _ := tok.(string)
		if limit > 0 && len(names) >= limit {
			return nil, nil, overBound("%s has more than %d members", what, limit)
		}
		if nameLimit > 0 {
			if name == "" {
				return nil, nil, malformed("%s names must not be empty", what)
			}
			if len(name) > nameLimit {
				return nil, nil, overBound("%s name longer than %d bytes", what, nameLimit)
			}
		}
		k := foldKey(name)
		if folded[k] {
			return nil, nil, malformed("%s has a duplicate or case-variant name", what)
		}
		folded[k] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, malformed("%s must be a JSON object", what)
		}
		names = append(names, name)
		values = append(values, v)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, nil, malformed("%s must be a JSON object", what)
	}
	return names, values, nil
}

// foldKey canonicalizes s so two strings share a key exactly when
// strings.EqualFold reports them equal: each rune maps to the smallest rune
// of its simple-folding orbit.
func foldKey(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			m = min(m, f)
		}
		sb.WriteRune(m)
	}
	return sb.String()
}

// nonTextTypes are the content-part type values that carry non-text input on a
// text-only decision route: the chat/responses image, audio and file part
// types plus audio_url, the video spellings, and the Anthropic-style and
// assistants-style image, audio and document types. Only an exact "type"
// member value counts; strings elsewhere are text and never inspected. An
// object with an image_url member (nonTextMember) is non-text even without a
// type.
var nonTextTypes = map[string]bool{
	"image_url": true, "input_image": true, "image": true, "image_file": true,
	"input_audio": true, "audio_url": true, "audio": true,
	"file": true, "input_file": true, "document": true, "input_document": true,
	"video_url": true, "video": true, "input_video": true,
}

// nonTextMember is the member name that marks an object as an image part
// whatever its value or type.
const nonTextMember = "image_url"

// textOnly scans raw (bounded by maxDepth) and fails with
// ErrDecisionUnsupported when it holds a non-text part.
func textOnly(what string, raw []byte, maxDepth int) error {
	nonText, err := scanValue(what, raw, maxDepth)
	if err != nil {
		return err
	}
	if nonText {
		return unsupported("non-text content in %s is not supported", what)
	}
	return nil
}

// scanMembers applies textOnly to every member of the JSON object raw except
// those named in skip. A member outside known is labelled generic so error
// text never quotes a body-supplied name. object has already validated raw.
func scanMembers(what string, raw []byte, known, skip []string, generic string, maxDepth int) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return malformed("%s must be a JSON object", what)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return malformed("%s must be a JSON object", what)
		}
		name, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return malformed("%s must be a JSON object", what)
		}
		if contains(skip, name) {
			continue
		}
		what := generic
		if contains(known, name) {
			what = name
		}
		if err := textOnly(what, v, maxDepth); err != nil {
			return err
		}
	}
	return nil
}

// scanValue walks one JSON value without recursion. It fails with
// ErrDecisionBounds when the value nests deeper than maxDepth, and reports
// whether any object in it has a "type" member naming a non-text part or an
// image_url member.
func scanValue(what string, raw []byte, maxDepth int) (nonText bool, err error) {
	type frame struct {
		obj, wantKey bool
		key          string
	}
	var stack []frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].obj {
			stack[n-1].wantKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nonText, nil
		}
		if err != nil {
			return false, malformed("%s is not valid JSON", what)
		}
		if n := len(stack); n > 0 && stack[n-1].obj && stack[n-1].wantKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:n-1]
				valueDone()
				continue
			}
			stack[n-1].key, _ = tok.(string)
			stack[n-1].wantKey = false
			if stack[n-1].key == nonTextMember {
				nonText = true
			}
			continue
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				stack = append(stack, frame{obj: v == '{', wantKey: v == '{'})
				if len(stack) > maxDepth {
					return false, overBound("%s nests deeper than %d levels", what, maxDepth)
				}
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
		case string:
			if n := len(stack); n > 0 && stack[n-1].obj && stack[n-1].key == "type" && nonTextTypes[v] {
				nonText = true
			}
			valueDone()
		default:
			valueDone()
		}
	}
}

// FilterDecision returns, in order, the candidates whose descriptor serves
// reqs: exactly the request's decision protocol, text input, and every asked
// question type. Like Filter it only removes entries.
func FilterDecision(route core.Route, candidates []string, reqs DecisionRequirements) []string {
	out := make([]string, 0, len(candidates))
	for _, id := range candidates {
		spec, ok := route.Upstreams[id]
		if !ok || len(spec.Protocols) != 1 || spec.Protocols[0] != reqs.Protocol || !modalitySupported(spec, ModalityText) {
			continue
		}
		all := true
		for _, q := range reqs.QuestionTypes {
			if !contains(spec.QuestionTypes, q) {
				all = false
				break
			}
		}
		if all {
			out = append(out, id)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
