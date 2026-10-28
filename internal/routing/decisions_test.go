package routing

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// tutorialBody is the OpenRouter Jev tutorial request (design/or-jev-tutorial.md
// lines 46-78): one noul, one choice and one score question.
const tutorialBody = `{"model":"typesafe/jev-1.13","state":{"customer_tier":"enterprise","ticket":"blank screen"},
"questions":{
 "is_bug":{"type":"noul","instructions":"Is it a defect?","criteria":{"true":"broken","false":"question"}},
 "team":{"type":"choice","instructions":"Which team?","criteria":{"payments":"p","frontend":"f","account":"a"}},
 "urgency":{"type":"score","instructions":"How urgent?","criteria":["later","this week","now"]}}}`

func inferBoth(t *testing.T, body string) map[string]error {
	t.Helper()
	out := map[string]error{}
	for _, p := range []string{ProtocolSystemOne, ProtocolDecisions} {
		_, err := InferDecision(p, []byte(body), DecisionBounds{})
		out[p] = err
	}
	return out
}

func TestInferDecisionTutorialBody(t *testing.T) {
	for _, p := range []string{ProtocolSystemOne, ProtocolDecisions} {
		reqs, err := InferDecision(p, []byte(tutorialBody), DecisionBounds{})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		want := []string{QuestionNoul, QuestionChoice, QuestionScore}
		if reqs.Protocol != p || !reflect.DeepEqual(reqs.QuestionTypes, want) {
			t.Fatalf("%s: reqs = %#v", p, reqs)
		}
	}
	// Optional Decisions members and unknown question members pass through.
	body := `{"model":"m","state":"s","provider":{"sort":"price"},"session_id":"x","trace":{},"user":"u",
"questions":{"q":{"type":"noul","instructions":"i","extra":{"k":1}}}}`
	if _, err := InferDecision(ProtocolDecisions, []byte(body), DecisionBounds{}); err != nil {
		t.Fatalf("decisions optional members: %v", err)
	}
	// string and array state are both valid.
	for _, st := range []string{`"text"`, `["a",{"b":1}]`} {
		body := `{"model":"m","state":` + st + `,"questions":{"q":{"type":"noul","instructions":"i"}}}`
		if _, err := InferDecision(ProtocolSystemOne, []byte(body), DecisionBounds{}); err != nil {
			t.Fatalf("state %s: %v", st, err)
		}
	}
}

// TestT6StrictJSON: every ambiguous or malformed body is ErrMalformed.
func TestT6StrictJSON(t *testing.T) {
	q := func(questions string) string {
		return `{"model":"m","state":"s","questions":` + questions + `}`
	}
	cases := map[string]string{
		"duplicate question name":    q(`{"a":{"type":"noul","instructions":"i"},"a":{"type":"noul","instructions":"i"}}`),
		"case-variant question":      q(`{"Billing":{"type":"noul","instructions":"i"},"billing":{"type":"noul","instructions":"i"}}`),
		"fold-variant question":      q(`{"ſ":{"type":"noul","instructions":"i"},"S":{"type":"noul","instructions":"i"}}`),
		"duplicate questions":        `{"model":"m","state":"s","questions":{"a":{"type":"noul"}},"questions":{"b":{"type":"noul"}}}`,
		"Questions variant":          `{"model":"m","state":"s","Questions":{"a":{"type":"noul"}}}`,
		"State variant":              `{"model":"m","State":"s","state":"s","questions":{"a":{"type":"noul"}}}`,
		"Type variant":               q(`{"a":{"Type":"noul","instructions":"i"}}`),
		"Criteria variant":           q(`{"a":{"type":"choice","instructions":"i","criteria":{"x":"y"},"Criteria":{}}}`),
		"duplicate choice options":   q(`{"a":{"type":"choice","instructions":"i","criteria":{"x":"1","x":"2"}}}`),
		"case-variant choice":        q(`{"a":{"type":"choice","instructions":"i","criteria":{"Yes":"1","yes":"2"}}}`),
		"noul True variant":          q(`{"a":{"type":"noul","instructions":"i","criteria":{"True":"y","false":"n"}}}`),
		"noul extra criterion":       q(`{"a":{"type":"noul","instructions":"i","criteria":{"true":"y","maybe":"n"}}}`),
		"non-object questions":       q(`["a"]`),
		"empty questions":            q(`{}`),
		"missing questions":          `{"model":"m","state":"s"}`,
		"missing state":              `{"model":"m","questions":{"a":{"type":"noul"}}}`,
		"number state":               `{"model":"m","state":1,"questions":{"a":{"type":"noul"}}}`,
		"empty question name":        q(`{"":{"type":"noul","instructions":"i"}}`),
		"non-object question":        q(`{"a":"noul"}`),
		"missing type":               q(`{"a":{"instructions":"i"}}`),
		"unknown type":               q(`{"a":{"type":"rank","instructions":"i"}}`),
		"choice without criteria":    q(`{"a":{"type":"choice","instructions":"i"}}`),
		"choice empty criteria":      q(`{"a":{"type":"choice","instructions":"i","criteria":{}}}`),
		"choice array criteria":      q(`{"a":{"type":"choice","instructions":"i","criteria":["x"]}}`),
		"score without criteria":     q(`{"a":{"type":"score","instructions":"i"}}`),
		"score empty criteria":       q(`{"a":{"type":"score","instructions":"i","criteria":[]}}`),
		"score object criteria":      q(`{"a":{"type":"score","instructions":"i","criteria":{"0":"x"}}}`),
		"non-object body":            `[1]`,
		"trailing data":              q(`{"a":{"type":"noul","instructions":"i"}}`) + `{}`,
		"non-boolean stream":         `{"model":"m","stream":"yes","state":"s","questions":{"a":{"type":"noul"}}}`,
		"duplicate type in question": q(`{"a":{"type":"noul","type":"score","instructions":"i"}}`),
	}
	for name, body := range cases {
		for p, err := range inferBoth(t, body) {
			if !errors.Is(err, ErrMalformed) {
				t.Errorf("%s (%s): err = %v, want ErrMalformed", name, p, err)
			}
			if err != nil && strings.Contains(err.Error(), "Billing") {
				t.Errorf("%s: error quotes body content: %v", name, err)
			}
		}
	}
}

// TestT7Bounds: one over each bound is ErrDecisionBounds; exactly at each bound
// is accepted.
func TestT7Bounds(t *testing.T) {
	b := DefaultDecisionBounds()
	questions := func(n int) string {
		var parts []string
		for i := 0; i < n; i++ {
			parts = append(parts, fmt.Sprintf(`"q%d":{"type":"noul","instructions":"i"}`, i))
		}
		return `{"model":"m","state":"s","questions":{` + strings.Join(parts, ",") + `}}`
	}
	choice := func(n int) string {
		var opts []string
		for i := 0; i < n; i++ {
			opts = append(opts, fmt.Sprintf(`"o%d":"x"`, i))
		}
		return `{"model":"m","state":"s","questions":{"c":{"type":"choice","instructions":"i","criteria":{` + strings.Join(opts, ",") + `}}}}`
	}
	score := func(n int) string {
		levels := strings.TrimSuffix(strings.Repeat(`"l",`, n), ",")
		return `{"model":"m","state":"s","questions":{"s":{"type":"score","instructions":"i","criteria":[` + levels + `]}}}`
	}
	name := func(n int) string {
		return `{"model":"m","state":"s","questions":{"` + strings.Repeat("n", n) + `":{"type":"noul","instructions":"i"}}}`
	}
	nest := func(field string, depth int) string {
		v := strings.Repeat("[", depth) + strings.Repeat("]", depth)
		switch field {
		case "state":
			return `{"model":"m","state":` + v + `,"questions":{"q":{"type":"noul","instructions":"i"}}}`
		case "instructions":
			return `{"model":"m","state":"s","questions":{"q":{"type":"noul","instructions":` + v + `}}}`
		}
		// criteria: one choice option whose value nests depth-1 below the object.
		inner := strings.Repeat("[", depth-1) + strings.Repeat("]", depth-1)
		return `{"model":"m","state":"s","questions":{"q":{"type":"choice","instructions":"i","criteria":{"o":` + inner + `}}}}`
	}
	cases := []struct {
		what      string
		at, above string
	}{
		{"questions", questions(b.MaxQuestions), questions(b.MaxQuestions + 1)},
		{"choice options", choice(b.MaxChoiceOptions), choice(b.MaxChoiceOptions + 1)},
		{"score levels", score(b.MaxScoreLevels), score(b.MaxScoreLevels + 1)},
		{"name bytes", name(b.MaxQuestionNameBytes), name(b.MaxQuestionNameBytes + 1)},
		{"state nesting", nest("state", b.MaxNesting), nest("state", b.MaxNesting+1)},
		{"instructions nesting", nest("instructions", b.MaxNesting), nest("instructions", b.MaxNesting+1)},
		{"criteria nesting", nest("criteria", b.MaxNesting), nest("criteria", b.MaxNesting+1)},
	}
	for _, c := range cases {
		for p, err := range inferBoth(t, c.at) {
			if err != nil {
				t.Errorf("%s at limit (%s): %v", c.what, p, err)
			}
		}
		for p, err := range inferBoth(t, c.above) {
			if !errors.Is(err, ErrDecisionBounds) {
				t.Errorf("%s above limit (%s): err = %v, want ErrDecisionBounds", c.what, p, err)
			}
		}
	}
	// Configured bounds apply instead of the defaults.
	small := DecisionBounds{MaxQuestions: 2}
	if _, err := InferDecision(ProtocolSystemOne, []byte(questions(3)), small); !errors.Is(err, ErrDecisionBounds) {
		t.Fatalf("configured max_questions: %v", err)
	}
	if _, err := InferDecision(ProtocolSystemOne, []byte(questions(2)), small); err != nil {
		t.Fatalf("configured max_questions at limit: %v", err)
	}
}

// TestT8UnsupportedFeatures: images, assets, box, stream:true and an
// OpenAI-style input body are ErrDecisionUnsupported.
func TestT8UnsupportedFeatures(t *testing.T) {
	noul := `"questions":{"q":{"type":"noul","instructions":"i"}}`
	cases := map[string]string{
		"box":             `{"model":"m","state":"s","questions":{"p":{"type":"box","instructions":"the product"}}}`,
		"assets":          `{"model":"m","state":"s","assets":{},` + noul + `}`,
		"null assets":     `{"model":"m","state":"s","assets":null,` + noul + `}`,
		"image_url state": `{"model":"m","state":["photo:",{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}],` + noul + `}`,
		"nested image":    `{"model":"m","state":{"a":{"b":[{"type":"input_image","image_url":"x"}]}},` + noul + `}`,
		"stream true":     `{"model":"m","stream":true,"state":"s",` + noul + `}`,
		"input not state": `{"model":"m","input":"text",` + noul + `}`,
	}
	for name, body := range cases {
		p := ProtocolSystemOne
		if name == "input not state" {
			p = ProtocolDecisions
		}
		_, err := InferDecision(p, []byte(body), DecisionBounds{})
		if !errors.Is(err, ErrDecisionUnsupported) {
			t.Errorf("%s: err = %v, want ErrDecisionUnsupported", name, err)
		}
		if name == "input not state" && (err == nil || !strings.Contains(err.Error(), "state")) {
			t.Errorf("input error must name state: %v", err)
		}
	}
	// A "type":"image_url" string that is a value under another key is not an
	// image part. (An object key named image_url is, since Amendment R2.1.)
	for _, st := range []string{`{"kind":"image_url"}`, `["type","image_url"]`} {
		body := `{"model":"m","state":` + st + `,` + noul + `}`
		if _, err := InferDecision(ProtocolSystemOne, []byte(body), DecisionBounds{}); err != nil {
			t.Errorf("state %s: %v", st, err)
		}
	}
	for _, s := range []string{`false`, `null`} {
		body := `{"model":"m","stream":` + s + `,"state":"s",` + noul + `}`
		if _, err := InferDecision(ProtocolSystemOne, []byte(body), DecisionBounds{}); err != nil {
			t.Errorf("stream %s: %v", s, err)
		}
	}
}

// TestT9FilterDecision: question-type filtering removes candidates only and
// keeps the operator's order; protocol must match exactly.
func TestT9FilterDecision(t *testing.T) {
	spec := func(proto string, qt ...string) core.UpstreamSpec {
		return core.UpstreamSpec{UpstreamModel: "typesafe/jev-1.13", Protocols: []string{proto}, InputModalities: []string{"text"}, QuestionTypes: qt}
	}
	route := core.Route{Upstreams: map[string]core.UpstreamSpec{
		"a": spec(ProtocolSystemOne, QuestionNoul, QuestionChoice),
		"b": spec(ProtocolSystemOne, QuestionNoul, QuestionChoice, QuestionScore),
		"c": spec(ProtocolSystemOne, QuestionScore),
	}}
	cands := []string{"c", "a", "b"}
	got := FilterDecision(route, cands, DecisionRequirements{Protocol: ProtocolSystemOne, QuestionTypes: []string{QuestionScore}})
	if !reflect.DeepEqual(got, []string{"c", "b"}) {
		t.Fatalf("score filter = %v", got)
	}
	got = FilterDecision(route, cands, DecisionRequirements{Protocol: ProtocolSystemOne, QuestionTypes: []string{QuestionNoul, QuestionScore}})
	if !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("noul+score filter = %v", got)
	}
	if got := FilterDecision(route, cands, DecisionRequirements{Protocol: ProtocolDecisions, QuestionTypes: []string{QuestionNoul}}); len(got) != 0 {
		t.Fatalf("wrong protocol kept %v", got)
	}
	if RouteDecisionProtocol(route) != ProtocolSystemOne {
		t.Fatal("RouteDecisionProtocol")
	}
	if RouteDecisionProtocol(core.Route{Upstreams: map[string]core.UpstreamSpec{"x": {Protocols: []string{"chat"}}}}) != "" {
		t.Fatal("chat route reported as decision route")
	}
}

func TestFoldKeyMatchesEqualFold(t *testing.T) {
	pairs := [][2]string{{"Billing", "billing"}, {"ſ", "s"}, {"K", "k"}, {"Σσς", "ΣΣΣ"}, {"a", "b"}, {"ab", "a"}}
	for _, p := range pairs {
		if (foldKey(p[0]) == foldKey(p[1])) != strings.EqualFold(p[0], p[1]) {
			t.Errorf("foldKey disagrees with EqualFold on %q/%q", p[0], p[1])
		}
	}
}
