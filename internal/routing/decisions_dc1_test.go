package routing

import (
	"errors"
	"strings"
	"testing"
)

// TestDC1FixNonTextContentRejected (review DC-1): a decision descriptor is
// text-only, so non-text parts the chat classifier treats as audio/file, and
// image parts in instructions, criteria or an input member next to state, are
// ErrDecisionUnsupported rather than forwarded as text.
func TestDC1FixNonTextContentRejected(t *testing.T) {
	const img = `{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}`
	cases := []struct{ name, protocol, body string }{
		{"state input_audio", ProtocolDecisions,
			`{"model":"m","state":["clip:",{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}],"questions":{"q":{"type":"noul","instructions":"i"}}}`},
		{"state file", ProtocolDecisions,
			`{"model":"m","state":[{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,AAAA"}}],"questions":{"q":{"type":"noul","instructions":"i"}}}`},
		{"state input_file", ProtocolSystemOne,
			`{"model":"m","state":[{"type":"input_file","file_data":"data:application/pdf;base64,AAAA"}],"questions":{"q":{"type":"noul","instructions":"i"}}}`},
		{"image in instructions", ProtocolSystemOne,
			`{"model":"m","state":"s","questions":{"q":{"type":"noul","instructions":["look at",` + img + `]}}}`},
		{"image in choice criteria", ProtocolDecisions,
			`{"model":"m","state":"s","questions":{"q":{"type":"choice","instructions":"i","criteria":{"a":[` + img + `],"b":"x"}}}}`},
		{"image in input next to state", ProtocolDecisions,
			`{"model":"m","state":"s","input":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}],"questions":{"q":{"type":"noul","instructions":"i"}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := InferDecision(c.protocol, []byte(c.body), DecisionBounds{})
			if !errors.Is(err, ErrDecisionUnsupported) {
				t.Fatalf("InferDecision = %v, want ErrDecisionUnsupported (non-text content on a text-only decision route)", err)
			}
		})
	}
}

// TestT8aNonTextParts (R1.1): every non-text part type is rejected in state,
// instructions, criteria and any other top-level member, on both protocols;
// strings and non-"type" keys stay text, and other top-level members obey
// max_nesting like state.
func TestT8aNonTextParts(t *testing.T) {
	types := []string{"image_url", "input_image", "input_audio", "audio_url", "file", "input_file", "video_url", "video"}
	for _, typ := range types {
		part := `{"type":"` + typ + `"}`
		places := map[string]string{
			"state":        `{"model":"m","state":{"a":[` + part + `]},"questions":{"q":{"type":"noul","instructions":"i"}}}`,
			"instructions": `{"model":"m","state":"s","questions":{"q":{"type":"noul","instructions":[` + part + `]}}}`,
			"noul criteria": `{"model":"m","state":"s","questions":{"q":{"type":"noul","instructions":"i","criteria":{"true":` +
				part + `,"false":"no"}}}}`,
			"score criteria": `{"model":"m","state":"s","questions":{"q":{"type":"score","instructions":"i","criteria":["low",` + part + `]}}}`,
			"unknown member": `{"model":"m","state":"s","metadata":{"x":` + part + `},"questions":{"q":{"type":"noul","instructions":"i"}}}`,
		}
		for where, body := range places {
			for _, p := range []string{ProtocolSystemOne, ProtocolDecisions} {
				_, err := InferDecision(p, []byte(body), DecisionBounds{})
				if !errors.Is(err, ErrDecisionUnsupported) {
					t.Errorf("%s %s in %s: err = %v, want ErrDecisionUnsupported", p, typ, where, err)
				}
				if err != nil && strings.Contains(err.Error(), "metadata") {
					t.Errorf("error quotes a body-supplied member name: %v", err)
				}
			}
		}
		// The trace member is read on /v1/decisions and scanned too.
		body := `{"model":"m","state":"s","trace":` + part + `,"questions":{"q":{"type":"noul","instructions":"i"}}}`
		if _, err := InferDecision(ProtocolDecisions, []byte(body), DecisionBounds{}); !errors.Is(err, ErrDecisionUnsupported) {
			t.Errorf("trace %s: err = %v", typ, err)
		}
	}
	// Strings are never inspected (R1.3); a type under another key is text.
	for _, st := range []string{`"image_url"`, `["input_audio","file"]`, `{"kind":"video"}`, `{"file":"x"}`, `{"Type":"input_file"}`} {
		body := `{"model":"m","state":` + st + `,"questions":{"q":{"type":"noul","instructions":"type: image_url","criteria":{"true":"video","false":"file"}}},"note":"input_audio"}`
		if _, err := InferDecision(ProtocolDecisions, []byte(body), DecisionBounds{}); err != nil {
			t.Errorf("state %s: %v", st, err)
		}
	}
	deep := strings.Repeat("[", 3) + strings.Repeat("]", 3)
	body := `{"model":"m","state":"s","extra":` + deep + `,"questions":{"q":{"type":"noul"}}}`
	if _, err := InferDecision(ProtocolSystemOne, []byte(body), DecisionBounds{MaxNesting: 2}); !errors.Is(err, ErrDecisionBounds) {
		t.Errorf("deep unknown member: err = %v, want ErrDecisionBounds", err)
	}
}

// TestT8bInputMember (R1.2): a top-level input member is ErrDecisionUnsupported
// on both paths, with or without state, and the message names state.
func TestT8bInputMember(t *testing.T) {
	noul := `"questions":{"q":{"type":"noul","instructions":"i"}}`
	for _, body := range []string{
		`{"model":"m","input":"text",` + noul + `}`,
		`{"model":"m","state":"s","input":"text",` + noul + `}`,
		`{"model":"m","state":"s","input":null,` + noul + `}`,
	} {
		for _, p := range []string{ProtocolSystemOne, ProtocolDecisions} {
			_, err := InferDecision(p, []byte(body), DecisionBounds{})
			if !errors.Is(err, ErrDecisionUnsupported) || !strings.Contains(err.Error(), "state") {
				t.Errorf("%s %s: err = %v, want ErrDecisionUnsupported naming state", p, body, err)
			}
		}
	}
}
