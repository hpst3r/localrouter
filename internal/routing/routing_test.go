package routing

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestInferStream(t *testing.T) {
	reqs, err := Infer(ProtocolResponses, []byte(`{"model":"gpt-x","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if !reqs.Stream {
		t.Fatalf("Stream = false, want true")
	}
	if reqs.Protocol != ProtocolResponses {
		t.Fatalf("Protocol = %q", reqs.Protocol)
	}
	if !reflect.DeepEqual(reqs.Modalities, []string{ModalityText}) {
		t.Fatalf("Modalities = %v, want [text]", reqs.Modalities)
	}

	// streaming is a default-off requirement: an absent stream field means false.
	reqs, err = Infer(ProtocolResponses, []byte(`{"model":"gpt-x","input":"hi"}`))
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if reqs.Stream {
		t.Fatalf("Stream = true for an absent stream field, want false")
	}

	if _, err := Infer(ProtocolResponses, []byte(`{"model":"gpt-x","stream":"yes"}`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("non-boolean stream: err = %v, want ErrMalformed", err)
	}
}

func TestInferTools(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
		bad  bool
	}{
		{"modern tools", `{"model":"m","tools":[{"type":"function","function":{"name":"f"}}]}`, true, false},
		{"empty tools", `{"model":"m","tools":[]}`, false, false},
		{"legacy functions", `{"model":"m","functions":[{"name":"f"}]}`, true, false},
		{"tool_choice auto", `{"model":"m","tool_choice":"auto"}`, true, false},
		{"tool_choice required", `{"model":"m","tool_choice":"required"}`, true, false},
		{"tool_choice named object", `{"model":"m","tool_choice":{"type":"function","function":{"name":"f"}}}`, true, false},
		{"legacy function_call", `{"model":"m","function_call":{"name":"f"}}`, true, false},
		// "none" is an explicit opt-out: a bare selector of "none" demands no
		// tool support, on either the modern or legacy spelling.
		{"tool_choice none alone", `{"model":"m","tool_choice":"none"}`, false, false},
		{"function_call none alone", `{"model":"m","function_call":"none"}`, false, false},
		// A declared tools array is authoritative: it still requires support even
		// when the selector says "none".
		{"declared tools with none", `{"model":"m","tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"none"}`, true, false},
		// Empty tools with a selector still declares no tools, but the selector
		// itself (non-"none") asks for tool support.
		{"none", `{"model":"m"}`, false, false},
		{"bad tools", `{"model":"m","tools":{}}`, false, true},
		{"bad functions", `{"model":"m","functions":"nope"}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs, err := Infer(ProtocolChat, []byte(tc.body))
			if tc.bad {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("err = %v, want ErrMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Infer: %v", err)
			}
			if reqs.Tools != tc.want {
				t.Fatalf("Tools = %v, want %v", reqs.Tools, tc.want)
			}
		})
	}
}

func TestInferJSONSchema(t *testing.T) {
	chat := []struct {
		name string
		body string
		want bool
		bad  bool
	}{
		{"json_schema", `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{}}}}`, true, false},
		// v1 treats the requirement as a single boolean: any structured-output
		// declaration, including json_object, requires JSONSchema support.
		{"json_object", `{"model":"m","response_format":{"type":"json_object"}}`, true, false},
		{"text", `{"model":"m","response_format":{"type":"text"}}`, false, false},
		{"absent", `{"model":"m"}`, false, false},
		{"missing type", `{"model":"m","response_format":{}}`, false, true},
		{"unknown type", `{"model":"m","response_format":{"type":"frobnicate"}}`, false, true},
	}
	for _, tc := range chat {
		t.Run("chat/"+tc.name, func(t *testing.T) {
			reqs, err := Infer(ProtocolChat, []byte(tc.body))
			if tc.bad {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("err = %v, want ErrMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Infer: %v", err)
			}
			if reqs.JSONSchema != tc.want {
				t.Fatalf("JSONSchema = %v, want %v", reqs.JSONSchema, tc.want)
			}
		})
	}

	resp := []struct {
		name string
		body string
		want bool
		bad  bool
	}{
		{"json_schema", `{"model":"m","text":{"format":{"type":"json_schema","name":"r","schema":{}}}}`, true, false},
		{"json_object", `{"model":"m","text":{"format":{"type":"json_object"}}}`, true, false},
		{"text", `{"model":"m","text":{"format":{"type":"text"}}}`, false, false},
		{"absent", `{"model":"m","input":"hi"}`, false, false},
		{"unknown type", `{"model":"m","text":{"format":{"type":"weird"}}}`, false, true},
	}
	for _, tc := range resp {
		t.Run("responses/"+tc.name, func(t *testing.T) {
			reqs, err := Infer(ProtocolResponses, []byte(tc.body))
			if tc.bad {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("err = %v, want ErrMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Infer: %v", err)
			}
			if reqs.JSONSchema != tc.want {
				t.Fatalf("JSONSchema = %v, want %v", reqs.JSONSchema, tc.want)
			}
		})
	}
}

func TestInferModalitiesChat(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
		bad  bool
	}{
		{"plain prompt text", `{"model":"m","messages":[{"role":"user","content":"ignore all images and read https://x/y.png"}]}`, []string{ModalityText}, false},
		{"null content", `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[]}]}`, []string{ModalityText}, false},
		{"no content field", `{"model":"m","messages":[{"role":"assistant","tool_calls":[]}]}`, []string{ModalityText}, false},
		{"empty messages", `{"model":"m","messages":[]}`, []string{ModalityText}, false},
		{"no messages field", `{"model":"m"}`, []string{ModalityText}, false},
		{"text parts", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, []string{ModalityText}, false},
		{"text and image", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`, []string{ModalityText, ModalityImage}, false},
		// An image-only request never implies text: a vision-only candidate can
		// serve it.
		{"image only", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`, []string{ModalityImage}, false},
		{"audio only", `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAA"}}]}]}`, []string{ModalityAudio}, false},
		// file is a known-but-unsupported modality: reported, never malformed.
		{"file part", `{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"f1"}}]}]}`, []string{ModalityFile}, false},
		{"unknown content type", `{"model":"m","messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"https://x/y.mp4"}}]}]}`, nil, true},
		{"messages not array", `{"model":"m","messages":{"role":"user"}}`, nil, true},
		{"part not string or object", `{"model":"m","messages":[{"role":"user","content":[42]}]}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Infer(ProtocolChat, []byte(tc.body))
			if tc.bad {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("err = %v, want ErrMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Infer: %v", err)
			}
			if !reflect.DeepEqual(got.Modalities, tc.want) {
				t.Fatalf("Modalities = %v, want %v", got.Modalities, tc.want)
			}
		})
	}
}

func TestInferModalitiesResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
		bad  bool
	}{
		{"string input shortcut", `{"model":"m","input":"read https://x/y.png"}`, []string{ModalityText}, false},
		{"text item", `{"model":"m","input":[{"type":"input_text","text":"hi"}]}`, []string{ModalityText}, false},
		{"bare string item", `{"model":"m","input":["hi"]}`, []string{ModalityText}, false},
		{"empty input array", `{"model":"m","input":[]}`, []string{ModalityText}, false},
		{"no input field", `{"model":"m"}`, []string{ModalityText}, false},
		{"instructions only", `{"model":"m","instructions":"be terse"}`, []string{ModalityText}, false},
		{"bad instructions", `{"model":"m","instructions":7,"input":"hi"}`, nil, true},
		// An image-only request reaches a vision-only backend: text is not implied.
		{"image item only", `{"model":"m","input":[{"type":"input_image","image_url":"https://x/y.png"}]}`, []string{ModalityImage}, false},
		{"audio item", `{"model":"m","input":[{"type":"input_audio","input_audio":{"data":"AAA"}}]}`, []string{ModalityAudio}, false},
		{"file item", `{"model":"m","input":[{"type":"input_file","file_id":"file_1"}]}`, []string{ModalityFile}, false},
		{"nested message text", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, []string{ModalityText}, false},
		{"nested message image", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`, []string{ModalityImage}, false},
		// An input_image with a textual prompt requires both modalities.
		{"nested message image and text", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"https://x/y.png"}]}]}`, []string{ModalityText, ModalityImage}, false},
		{"instructions and image", `{"model":"m","instructions":"describe","input":[{"type":"input_image","image_url":"https://x/y.png"}]}`, []string{ModalityText, ModalityImage}, false},
		{"plain message string content", `{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`, []string{ModalityText}, false},
		{"function_call alone is conservative text", `{"model":"m","input":[{"type":"function_call","name":"f","arguments":"{}"}]}`, []string{ModalityText}, false},
		{"function_call_output string output is text", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":"ok"}]}`, []string{ModalityText}, false},
		{"reasoning ignored", `{"model":"m","input":[{"type":"reasoning","summary":[]}]}`, []string{ModalityText}, false},
		// computer_call_output carries a nested screenshot image.
		{"computer_call_output screenshot", `{"model":"m","input":[{"type":"computer_call_output","call_id":"c1","output":{"type":"computer_screenshot","image_url":"https://x/y.png"}}]}`, []string{ModalityImage}, false},
		{"computer_call_output string output", `{"model":"m","input":[{"type":"computer_call_output","call_id":"c1","output":"done"}]}`, []string{ModalityText}, false},
		// An unmodelled nested payload fails closed instead of being ignored.
		{"computer_call_output unknown payload", `{"model":"m","input":[{"type":"computer_call_output","call_id":"c1","output":{"type":"mystery_blob","data":"..."}}]}`, nil, true},
		{"computer_call_output missing output", `{"model":"m","input":[{"type":"computer_call_output","call_id":"c1"}]}`, nil, true},
		// Opaque prior context cannot be classified, so a constrained route rejects
		// it rather than guessing text.
		{"item_reference opaque", `{"model":"m","input":[{"type":"item_reference","id":"msg_1"}]}`, nil, true},
		{"previous_response_id opaque", `{"model":"m","input":"hi","previous_response_id":"resp_1"}`, nil, true},
		{"unknown item type", `{"model":"m","input":[{"type":"mystery_item","foo":1}]}`, nil, true},
		{"unknown item content", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_video","video_url":"x"}]}]}`, nil, true},
		{"content not string or array", `{"model":"m","input":[{"type":"message","content":{"type":"input_text"}}]}`, nil, true},
		{"input not string or array", `{"model":"m","input":{"foo":1}}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Infer(ProtocolResponses, []byte(tc.body))
			if tc.bad {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("err = %v, want ErrMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Infer: %v", err)
			}
			if !reflect.DeepEqual(got.Modalities, tc.want) {
				t.Fatalf("Modalities = %v, want %v", got.Modalities, tc.want)
			}
		})
	}
}

// TestInferFunctionCallOutput covers the function_call_output item, whose
// "output" field is the function result. Per the Responses schema it is either
// a plain string (a JSON-encoded result) or an array of typed input content
// parts (text, image, file — and the audio part this package models). A string
// counts as text and is deliberately NOT scanned for embedded content; a typed
// part array reports every modality it holds; any other shape — an unmodelled
// or typeless content part, a bare object, or a missing/absent output — fails
// closed rather than being silently downgraded to text.
func TestInferFunctionCallOutput(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
		bad  bool
	}{
		{"string output is text", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":"ok"}]}`, []string{ModalityText}, false},
		{"string output alongside text", `{"model":"m","input":[{"type":"input_text","text":"hi"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`, []string{ModalityText}, false},
		// The string is an opaque result, never scanned: a URL or image-looking
		// text inside it does not become an image requirement.
		{"string output is not scanned", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":"{\"note\":\"see https://x/y.png\"}"}]}`, []string{ModalityText}, false},
		{"array text part", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"ok"}]}]}`, []string{ModalityText}, false},
		{"array image part is image only", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`, []string{ModalityImage}, false},
		{"array text and image parts", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"see this"},{"type":"input_image","image_url":"https://x/y.png"}]}]}`, []string{ModalityText, ModalityImage}, false},
		{"array audio part", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_audio","input_audio":{"data":"AAA"}}]}]}`, []string{ModalityAudio}, false},
		{"array file part", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_file","file_id":"file_1"}]}]}`, []string{ModalityFile}, false},
		{"array unknown content type", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"mystery_blob","data":"..."}]}]}`, nil, true},
		{"array typeless object", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[{"data":"..."}]}]}`, nil, true},
		{"bare object output", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":{"type":"mystery_blob"}}]}`, nil, true},
		{"missing output", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1"}]}`, nil, true},
		{"null output", `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":null}]}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Infer(ProtocolResponses, []byte(tc.body))
			if tc.bad {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("err = %v, want ErrMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Infer: %v", err)
			}
			if !reflect.DeepEqual(got.Modalities, tc.want) {
				t.Fatalf("Modalities = %v, want %v", got.Modalities, tc.want)
			}
		})
	}
}

// TestInferNonObjectRejected proves Infer fails closed on a body it cannot
// classify. Infer is only ever called on a capability-constrained route (the
// legacy path never calls it), so a non-object or invalid-JSON body must be an
// error rather than being silently treated as text-only.
func TestInferNonObjectRejected(t *testing.T) {
	for _, body := range []string{`not json`, `null`, `[1,2]`, `"str"`, ``, `123`, `true`} {
		if _, err := Infer(ProtocolChat, []byte(body)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("Infer(%q): err = %v, want ErrMalformed", body, err)
		}
	}
}

// TestInferNoURLDeref proves Infer classifies by declaration only: a prompt that
// merely mentions an image URL, or a data: URL in a text part, never turns into
// an image requirement.
func TestInferNoURLDeref(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"see https://example.com/a.png and https://example.com/b.jpg"}]}`
	reqs, err := Infer(ProtocolChat, []byte(body))
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if !reflect.DeepEqual(reqs.Modalities, []string{ModalityText}) {
		t.Fatalf("Modalities = %v, want text only", reqs.Modalities)
	}
}

func TestFilter(t *testing.T) {
	route := core.Route{
		Upstreams: map[string]core.UpstreamSpec{
			"vision": {Protocols: []string{ProtocolResponses}, InputModalities: []string{"text", "image"}, Tools: true, JSONSchema: true, Stream: true},
			"tools":  {Protocols: []string{ProtocolChat, ProtocolResponses}, Tools: true},
			"plain":  {},
			"chatvy": {Protocols: []string{ProtocolChat}},
			"all":    {Protocols: []string{ProtocolChat}, InputModalities: []string{"text"}, Tools: true, JSONSchema: true, Stream: true},
		},
	}
	candidates := []string{"vision", "plain", "tools", "chatvy", "all"}

	cases := []struct {
		name string
		reqs Requirements
		want []string
	}{
		{"chat text only", Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityText}},
			[]string{"plain", "tools", "chatvy", "all"}},
		{"image needs the image modality", Requirements{Protocol: ProtocolResponses, Modalities: []string{"text", "image"}},
			[]string{"vision"}},
		{"tools", Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityText}, Tools: true},
			[]string{"tools", "all"}},
		{"json schema", Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityText}, JSONSchema: true},
			[]string{"all"}},
		{"stream", Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityText}, Stream: true},
			[]string{"all"}},
		{"protocol mismatch", Requirements{Protocol: ProtocolResponses, Modalities: []string{ModalityText}},
			[]string{"vision", "plain", "tools"}},
		{"audio is unsatisfiable", Requirements{Protocol: ProtocolChat, Modalities: []string{"text", "audio"}},
			nil},
		{"file is unsatisfiable", Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityFile}},
			nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Filter(route, candidates, tc.reqs)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Filter = %v, want %v", got, tc.want)
			}
		})
	}
}

// A candidate that declares only "image" is vision-only: text is never implied,
// so the descriptor list is authoritative rather than additive.
func TestFilterVisionOnlyDoesNotImplyText(t *testing.T) {
	route := core.Route{Upstreams: map[string]core.UpstreamSpec{
		"visiononly":   {InputModalities: []string{"image"}},
		"imageandtext": {InputModalities: []string{"text", "image"}},
	}}
	got := Filter(route, []string{"visiononly", "imageandtext"},
		Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityText}})
	if !reflect.DeepEqual(got, []string{"imageandtext"}) {
		t.Fatalf("text request: Filter = %v, want [imageandtext]", got)
	}
	// An image-only request is served by a vision-only candidate.
	got = Filter(route, []string{"visiononly", "imageandtext"},
		Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityImage}})
	if !reflect.DeepEqual(got, []string{"visiononly", "imageandtext"}) {
		t.Fatalf("image-only request: Filter = %v, want [visiononly imageandtext]", got)
	}
	got = Filter(route, []string{"visiononly", "imageandtext"},
		Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityText, ModalityImage}})
	if !reflect.DeepEqual(got, []string{"imageandtext"}) {
		t.Fatalf("text+image request: Filter = %v, want [imageandtext]", got)
	}
}

// TestFilterPreservesOrder proves Filter only ever removes: the surviving order
// is the candidate order, so failover stays inside the operator's consent list.
func TestFilterPreservesOrder(t *testing.T) {
	route := core.Route{Upstreams: map[string]core.UpstreamSpec{}}
	for _, id := range []string{"a", "b", "c"} {
		route.Upstreams[id] = core.UpstreamSpec{Tools: true}
	}
	got := Filter(route, []string{"c", "a", "b"}, Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityText}, Tools: true})
	if !reflect.DeepEqual(got, []string{"c", "a", "b"}) {
		t.Fatalf("Filter reordered candidates: %v", got)
	}
}

// TestFilterLegacyPassthrough proves a route with no per-candidate descriptors is
// unconstrained: every candidate survives and no requirement can exclude one.
func TestFilterLegacyPassthrough(t *testing.T) {
	candidates := []string{"a", "b", "c"}
	reqs := Requirements{Protocol: ProtocolChat, Modalities: []string{"text", "image"}, Tools: true, JSONSchema: true, Stream: true}
	for _, route := range []core.Route{{}, {Upstreams: nil}, {Upstreams: map[string]core.UpstreamSpec{}}} {
		got := Filter(route, candidates, reqs)
		if !reflect.DeepEqual(got, candidates) {
			t.Fatalf("Filter(legacy) = %v, want %v", got, candidates)
		}
	}
}

func TestFilterReturnsACopy(t *testing.T) {
	candidates := []string{"a", "b"}
	got := Filter(core.Route{}, candidates, Requirements{})
	got[0] = "mutated"
	if candidates[0] != "a" {
		t.Fatalf("Filter aliased the caller's slice")
	}
}

func TestResolveModel(t *testing.T) {
	route := core.Route{
		UpstreamModel: "legacy-route-model",
		Upstreams: map[string]core.UpstreamSpec{
			"alias": {UpstreamModel: "backend-alias"},
			"bare":  {},
		},
	}
	cases := []struct {
		name    string
		account string
		client  string
		want    string
	}{
		{"descriptor alias wins", "alias", "client-model", "backend-alias"},
		{"descriptor without alias falls back to route", "bare", "client-model", "legacy-route-model"},
		{"unknown account falls back to route", "other", "client-model", "legacy-route-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveModel(route, tc.account, tc.client); got != tc.want {
				t.Fatalf("ResolveModel = %q, want %q", got, tc.want)
			}
		})
	}

	// No route-level upstream model: the client-facing model is passed through.
	bare := core.Route{Upstreams: map[string]core.UpstreamSpec{"bare": {}}}
	if got := ResolveModel(bare, "bare", "client-model"); got != "client-model" {
		t.Fatalf("ResolveModel = %q, want client-model", got)
	}
	if got := ResolveModel(core.Route{}, "x", "client-model"); got != "client-model" {
		t.Fatalf("ResolveModel(legacy) = %q, want client-model", got)
	}
}

// TestInferRejectsAmbiguousMembers covers member names the classifier reads.
// encoding/json struct decoding matches names case-insensitively and lets the
// last duplicate win, while a map/dict-based upstream reads the exact name. A
// case variant or exact duplicate of a modelled member therefore lets the
// router classify a different value than the upstream acts on, so it must
// fail closed instead of routing an image or structured-output request to a
// text-only candidate.
func TestInferRejectsAmbiguousMembers(t *testing.T) {
	cases := []struct {
		name, protocol, body string
	}{
		// Case-variant shadows (each was a runtime-confirmed bypass).
		{"chat part Type shadow", ProtocolChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"},"Type":"text"}]}]}`},
		{"chat message Content shadow", ProtocolChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}],"Content":"hi"}]}`},
		{"responses message Content shadow", ProtocolResponses, `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}],"Content":"hi"}]}`},
		{"responses item Type shadow", ProtocolResponses, `{"model":"m","input":[{"type":"input_image","image_url":"https://x/y.png","Type":"input_text"}]}`},
		{"responses part TYPE shadow", ProtocolResponses, `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/y.png","TYPE":"input_text"}]}]}`},
		{"function_call_output Output shadow", ProtocolResponses, `{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"https://x/y.png"}],"Output":"ok"}]}`},
		{"computer_call_output nested Type shadow", ProtocolResponses, `{"model":"m","input":[{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","image_url":"https://x/s.png","Type":"text"}}]}`},
		{"item_reference hidden by Type", ProtocolResponses, `{"model":"m","input":[{"type":"item_reference","id":"msg_1","Type":"input_text"}]}`},
		{"chat response_format Type shadow", ProtocolChat, `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{}},"Type":"text"}}`},
		{"responses text Format shadow", ProtocolResponses, `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"r","schema":{}},"Format":null}}`},
		{"root Messages shadow", ProtocolChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}],"Messages":[{"role":"user","content":"hi"}]}`},
		{"root Stream shadow", ProtocolChat, `{"model":"m","stream":false,"Stream":true}`},
		{"root Model shadow", ProtocolChat, `{"model":"m","Model":"other"}`},
		// U+017F (long s) folds to "s" under the Unicode simple folding
		// encoding/json uses, so a struct decoder reads it as "messages".
		{"unicode fold shadow", ProtocolChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}],"meſſages":[{"role":"user","content":"hi"}]}`},
		// Exact duplicates: first-wins and last-wins readers disagree.
		{"chat part dup type image first", ProtocolChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"},"type":"text"}]}]}`},
		{"chat part dup type text first", ProtocolChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`},
		{"root dup messages", ProtocolChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}],"messages":[{"role":"user","content":"hi"}]}`},
		{"root dup input", ProtocolResponses, `{"model":"m","input":[{"type":"input_image","image_url":"https://x/y.png"}],"input":"hi"}`},
		{"dup response_format type", ProtocolChat, `{"model":"m","response_format":{"type":"json_schema","type":"text"}}`},
		{"escaped dup type", ProtocolResponses, `{"model":"m","input":[{"type":"input_image","image_url":"https://x/y.png","type":"input_text"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs, err := Infer(tc.protocol, []byte(tc.body))
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("err = %v (reqs %+v), want ErrMalformed", err, reqs)
			}
		})
	}
}

// TestInferIgnoresUnmodelledMembers proves the ambiguity rule is scoped to the
// members the classifier reads: case variants or duplicates of anything else,
// and anything nested inside a value the classifier does not descend into
// (a tool's JSON schema, a structured-output schema), are left alone.
func TestInferIgnoresUnmodelledMembers(t *testing.T) {
	cases := []struct {
		name, protocol, body string
		want                 Requirements
	}{
		{"root dup unmodelled", ProtocolChat, `{"model":"m","temperature":1,"temperature":0,"Temperature":2,"messages":[{"role":"user","content":"hi"}]}`,
			Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityText}}},
		{"part unmodelled variants", ProtocolChat, `{"model":"m","messages":[{"role":"user","Role":"x","content":[{"type":"text","text":"a","Text":"b"}]}]}`,
			Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityText}}},
		{"schema internals not inspected", ProtocolChat, `{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"Type":{"type":"string"},"type":{"type":"string"}}}}}],"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object","Type":"x","type":"object"}}}}`,
			Requirements{Protocol: ProtocolChat, Modalities: []string{ModalityText}, Tools: true, JSONSchema: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Infer(tc.protocol, []byte(tc.body))
			if err != nil {
				t.Fatalf("Infer: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Infer = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestInferResponsesOpaqueContext pins that every Responses field naming
// server-side context the request does not carry — previous_response_id,
// conversation and a reusable prompt (whose template and variables may hold
// images or files) — fails closed rather than being classified as text.
func TestInferResponsesOpaqueContext(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","prompt":{"id":"pmpt_1","variables":{"img":{"type":"input_image","image_url":"https://x/y.png"}}},"input":"describe"}`,
		`{"model":"m","prompt":{"id":"pmpt_1","variables":{"doc":{"type":"input_file","file_id":"file_1"}}}}`,
		`{"model":"m","prompt":{"id":"pmpt_1"}}`,
		`{"model":"m","prompt":"pmpt_1"}`,
		`{"model":"m","conversation":"conv_1","input":"continue"}`,
		`{"model":"m","conversation":{"id":"conv_1"},"input":"continue"}`,
		`{"model":"m","previous_response_id":"resp_1","input":"continue"}`,
	} {
		if reqs, err := Infer(ProtocolResponses, []byte(body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("Infer(%s) = %+v, %v; want ErrMalformed", body, reqs, err)
		}
	}
	// An explicit null is the absence of context, as for previous_response_id.
	reqs, err := Infer(ProtocolResponses, []byte(`{"model":"m","prompt":null,"conversation":null,"previous_response_id":null,"input":"hi"}`))
	if err != nil || !reflect.DeepEqual(reqs.Modalities, []string{ModalityText}) {
		t.Fatalf("null context: %+v, %v", reqs, err)
	}
}

// TestInferTypelessItems pins the Responses easy-message shape: a typeless
// input object is only a message when it carries a role and content. Any other
// typeless object is unknown and fails closed instead of defaulting to text.
func TestInferTypelessItems(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
		bad  bool
	}{
		{"easy message string", `{"model":"m","input":[{"role":"user","content":"hi"}]}`, []string{ModalityText}, false},
		{"easy message image", `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`, []string{ModalityImage}, false},
		{"null type easy message", `{"model":"m","input":[{"type":null,"role":"user","content":"hi"}]}`, []string{ModalityText}, false},
		{"unknown typeless object", `{"model":"m","input":[{"foo":1}]}`, nil, true},
		{"typeless image without content", `{"model":"m","input":[{"role":"user","image_url":"https://x/y.png"}]}`, nil, true},
		{"typeless null content", `{"model":"m","input":[{"role":"user","content":null}]}`, nil, true},
		{"typeless content without role", `{"model":"m","input":[{"content":"hi"}]}`, nil, true},
		{"non-string type", `{"model":"m","input":[{"type":7,"role":"user","content":"hi"}]}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Infer(ProtocolResponses, []byte(tc.body))
			if tc.bad {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("err = %v (reqs %+v), want ErrMalformed", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Infer: %v", err)
			}
			if !reflect.DeepEqual(got.Modalities, tc.want) {
				t.Fatalf("Modalities = %v, want %v", got.Modalities, tc.want)
			}
		})
	}
}

// TestInferAgentHistory covers replayed agent history whose shape is pinned by
// the openai-go v3.52.0 param types: assistant refusal parts (chat and
// Responses), custom tool calls and their outputs, and local shell calls and
// their outputs. Calls are model-emitted, like function_call, and carry no
// input modality; outputs are classified like function_call_output. Items whose
// payload can hold images or other server-side results (web search, image
// generation, code interpreter, MCP) are still unknown and fail closed.
func TestInferAgentHistory(t *testing.T) {
	cases := []struct {
		name, protocol, body string
		want                 []string
		bad                  bool
	}{
		{"chat assistant refusal", ProtocolChat, `{"model":"m","messages":[{"role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}`, []string{ModalityText}, false},
		{"responses assistant refusal", ProtocolResponses, `{"model":"m","input":[{"type":"message","role":"assistant","id":"msg_1","status":"completed","content":[{"type":"output_text","text":"a","annotations":[]},{"type":"refusal","refusal":"no"}]}]}`, []string{ModalityText}, false},
		{"custom tool round trip", ProtocolResponses, `{"model":"m","input":[{"type":"custom_tool_call","call_id":"c","name":"apply_patch","input":"*** Begin Patch"},{"type":"custom_tool_call_output","call_id":"c","output":"ok"}]}`, []string{ModalityText}, false},
		{"custom tool call alone", ProtocolResponses, `{"model":"m","input":[{"type":"custom_tool_call","call_id":"c","name":"f","input":"see https://x/y.png"}]}`, []string{ModalityText}, false},
		{"custom tool output image", ProtocolResponses, `{"model":"m","input":[{"type":"custom_tool_call_output","call_id":"c","output":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`, []string{ModalityImage}, false},
		{"custom tool output unknown part", ProtocolResponses, `{"model":"m","input":[{"type":"custom_tool_call_output","call_id":"c","output":[{"type":"mystery"}]}]}`, nil, true},
		{"custom tool output missing", ProtocolResponses, `{"model":"m","input":[{"type":"custom_tool_call_output","call_id":"c"}]}`, nil, true},
		{"local shell round trip", ProtocolResponses, `{"model":"m","input":[{"type":"local_shell_call","id":"ls_1","call_id":"c","status":"completed","action":{"type":"exec","command":["ls"],"env":{}}},{"type":"local_shell_call_output","id":"c","output":"a.txt"}]}`, []string{ModalityText}, false},
		{"local shell output not string", ProtocolResponses, `{"model":"m","input":[{"type":"local_shell_call_output","id":"c","output":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`, nil, true},
		{"local shell output missing", ProtocolResponses, `{"model":"m","input":[{"type":"local_shell_call_output","id":"c"}]}`, nil, true},
		{"image_generation_call fails closed", ProtocolResponses, `{"model":"m","input":[{"type":"image_generation_call","id":"ig_1","status":"completed","result":"iVBORw0"}]}`, nil, true},
		{"web_search_call fails closed", ProtocolResponses, `{"model":"m","input":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"q"}}]}`, nil, true},
		{"mcp_call fails closed", ProtocolResponses, `{"model":"m","input":[{"type":"mcp_call","id":"m_1","name":"f","server_label":"s","arguments":"{}"}]}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Infer(tc.protocol, []byte(tc.body))
			if tc.bad {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("err = %v (reqs %+v), want ErrMalformed", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Infer: %v", err)
			}
			if !reflect.DeepEqual(got.Modalities, tc.want) {
				t.Fatalf("Modalities = %v, want %v", got.Modalities, tc.want)
			}
		})
	}
}
