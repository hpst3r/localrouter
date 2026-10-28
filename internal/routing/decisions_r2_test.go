package routing

import (
	"errors"
	"strings"
	"testing"
)

// TestR21WidenedNonTextParts (R2.1, FDC-L1): the Anthropic-style and other
// image, audio, video and document part types are non-text, as is any object
// with an image_url member even without a type, wherever the scan runs.
func TestR21WidenedNonTextParts(t *testing.T) {
	parts := []string{
		`{"type":"image","source":{"type":"base64","data":"AAAA"}}`,
		`{"type":"image_file","image_file":{"file_id":"f"}}`,
		`{"type":"input_video"}`,
		`{"type":"audio"}`,
		`{"type":"document"}`,
		`{"type":"input_document"}`,
		`{"image_url":{"url":"data:image/png;base64,AAAA"}}`,
		`{"image_url":null}`,
	}
	for _, part := range parts {
		for where, body := range map[string]string{
			"state":        `{"model":"m","state":[` + part + `],"questions":{"q":{"type":"noul","instructions":"i"}}}`,
			"instructions": `{"model":"m","state":"s","questions":{"q":{"type":"noul","instructions":[` + part + `]}}}`,
			"unknown":      `{"model":"m","state":"s","extra":{"x":` + part + `},"questions":{"q":{"type":"noul","instructions":"i"}}}`,
		} {
			for _, p := range []string{ProtocolSystemOne, ProtocolDecisions} {
				if _, err := InferDecision(p, []byte(body), DecisionBounds{}); !errors.Is(err, ErrDecisionUnsupported) {
					t.Errorf("%s %s in %s: err = %v, want ErrDecisionUnsupported", p, part, where, err)
				}
			}
		}
	}
	// The member name alone counts; the same word as a value or under another
	// key stays text.
	for _, st := range []string{`"image_url"`, `{"url":"image_url"}`, `{"kind":"document"}`, `{"Image_URL":"x"}`} {
		body := `{"model":"m","state":` + st + `,"questions":{"q":{"type":"noul","instructions":"image"}}}`
		if _, err := InferDecision(ProtocolDecisions, []byte(body), DecisionBounds{}); err != nil {
			t.Errorf("state %s: %v", st, err)
		}
	}
}

// TestR22UnknownQuestionMembersScanned (R2.2, FDC-L2): unknown members inside a
// question get the same non-text scan and max_nesting bound as instructions,
// and error text never quotes the body-supplied member name.
func TestR22UnknownQuestionMembersScanned(t *testing.T) {
	body := `{"model":"m","state":"s","questions":{"q":{"type":"noul","instructions":"x","imgzz":{"type":"image_url"}}}}`
	_, err := InferDecision(ProtocolSystemOne, []byte(body), DecisionBounds{})
	if !errors.Is(err, ErrDecisionUnsupported) {
		t.Fatalf("image in unknown question member: err = %v, want ErrDecisionUnsupported", err)
	}
	if strings.Contains(err.Error(), "imgzz") {
		t.Fatalf("error quotes a body-supplied member name: %v", err)
	}
	deep := `{"model":"m","state":"s","questions":{"q":{"type":"noul","extrazz":[[["x"]]]}}}`
	_, err = InferDecision(ProtocolDecisions, []byte(deep), DecisionBounds{MaxNesting: 2})
	if !errors.Is(err, ErrDecisionBounds) {
		t.Fatalf("deep unknown question member: err = %v, want ErrDecisionBounds", err)
	}
	if strings.Contains(err.Error(), "extrazz") {
		t.Fatalf("error quotes a body-supplied member name: %v", err)
	}
	// A text-only unknown question member is still forwarded untouched.
	ok := `{"model":"m","state":"s","questions":{"q":{"type":"noul","instructions":"x","note":{"a":["b"]}}}}`
	if _, err := InferDecision(ProtocolDecisions, []byte(ok), DecisionBounds{MaxNesting: 2}); err != nil {
		t.Fatalf("text unknown question member: %v", err)
	}
}
