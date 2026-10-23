package routing

import (
	"errors"
	"os"
	"testing"
)

// TestReviewDC1NonTextContentForwardedAsText: a decision descriptor is
// text-only (input_modalities [text]) and SPEC A3 rejects images, yet
// InferDecision only recognises image_url/input_image parts inside state. The
// non-text parts the chat classifier itself treats as audio/file, and image
// parts placed in instructions, criteria or (on /v1/decisions) an input
// member sent next to state, all classify as plain text and are forwarded.
func TestReviewDC1NonTextContentForwardedAsText(t *testing.T) {
	if os.Getenv("LR_REVIEW") == "" {
		t.Skip("BUG DC-1: non-text parts (audio/file, images outside state) pass the text-only decision classifier")
	}
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
