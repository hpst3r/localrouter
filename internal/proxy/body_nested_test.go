package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// postRefused posts body and requires the multi-user field-policy 400 naming
// field.
func postRefused(t *testing.T, h *harness, path, body, field string) {
	t.Helper()
	resp := h.post(path, userTokenA1, body, nil)
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("%s %s: status %d, want 400", path, body, resp.StatusCode)
	}
	e := decodeErr(t, resp)
	want := `localrouter: field "` + core.TruncateLabel(field) + `" is not allowed on this gateway`
	if e.Error.Type != "invalid_request_error" || e.Error.Message != want {
		t.Fatalf("%s %s: error %+v, want %q", path, body, e.Error, want)
	}
}

// postForwarded posts body and requires it to reach the upstream unchanged.
func postForwarded(t *testing.T, h *harness, bodies chan []byte, path, token, body string) {
	t.Helper()
	resp := h.post(path, token, body, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: status %d, want 200", path, body, resp.StatusCode)
	}
	select {
	case got := <-bodies:
		if string(got) != body {
			t.Fatalf("%s: forwarded %s, want unchanged %s", path, got, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s %s: no upstream body", path, body)
	}
}

// In multi-user mode, references to state stored on the shared upstream
// account are refused wherever they appear inside input, messages or tools,
// and OpenRouter's web_search_options (the paid web plugin) is refused at the
// top level. Nothing reaches the upstream, takes a lease or writes a ledger
// row.
func TestMultiUserRefusesNestedStateReferences(t *testing.T) {
	h, hits, _ := multiUserFieldHarness(t)
	both := []string{"/v1/responses", "/v1/chat/completions"}
	cases := []struct {
		paths       []string
		body, field string
	}{
		{both, `{"model":"gpt-x","messages":[],"web_search_options":{"search_context_size":"high"}}`, "web_search_options"},
		{both, `{"model":"gpt-x","Web_Search_Options":{}}`, "Web_Search_Options"},
		{both, `{"model":"gpt-x","web_search_options":null}`, "web_search_options"},

		// Responses input items.
		{both, `{"model":"gpt-x","input":[{"type":"item_reference","id":"msg_other_user"}]}`, "item_reference"},
		{both, `{"model":"gpt-x","input":[{"id":"msg_other","type":"item_reference"}]}`, "item_reference"},
		{both, `{"model":"gpt-x","input":[{"Type":"Item_Reference","id":"msg_other"}]}`, "item_reference"},
		{both, `{"model":"gpt-x","input":[{"type":"input_text","text":"a","type":"item_reference","id":"m"}]}`, "item_reference"},
		{both, `{"model":"gpt-x","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-other"}]}]}`, "file_id"},
		{both, `{"model":"gpt-x","input":[{"role":"user","content":[{"type":"input_image","detail":"auto","file_id":"file-img"}]}]}`, "file_id"},
		{both, `{"model":"gpt-x","input":[{"role":"user","content":[{"type":"input_file","File_ID":"file-other"}]}]}`, "File_ID"},
		{both, `{"model":"gpt-x","Input":[{"type":"item_reference","id":"m"}]}`, "item_reference"},

		// Chat messages content parts.
		{both, `{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-other"}}]}]}`, "file_id"},
		{both, `{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"input_file","file_id":"file-other"}]}]}`, "file_id"},
		{both, `{"model":"gpt-x","MESSAGES":[{"role":"user","content":[{"type":"file","file":{"FILE_ID":"f"}}]}]}`, "FILE_ID"},

		// Hosted tools bound to account resources.
		{both, `{"model":"gpt-x","input":"q","tools":[{"type":"file_search","vector_store_ids":["vs_other"]}]}`, "vector_store_ids"},
		{both, `{"model":"gpt-x","input":"q","tools":[{"type":"file_search","Vector_Store_IDs":["vs_other"]}]}`, "Vector_Store_IDs"},
		{both, `{"model":"gpt-x","input":"q","tools":[{"type":"code_interpreter","container":"cntr_other"}]}`, "container"},
		{both, `{"model":"gpt-x","input":"q","tools":[{"type":"code_interpreter","container":{"type":"auto","file_ids":["f"]}}]}`, "container"},
		{both, `{"model":"gpt-x","messages":[],"tools":[{"type":"code_interpreter","CONTAINER":"cntr_other"}]}`, "CONTAINER"},
		{both, `{"model":"gpt-x","messages":[],"Tools":[{"type":"file_search","vector_store_ids":[]}]}`, "vector_store_ids"},
	}
	for _, tc := range cases {
		for _, path := range tc.paths {
			postRefused(t, h, path, tc.body, tc.field)
		}
	}
	if n := hits(); n != 0 {
		t.Fatalf("upstream hits = %d, want 0", n)
	}
	h.policy.mu.Lock()
	leases := len(h.policy.leases)
	h.policy.mu.Unlock()
	if leases != 0 {
		t.Fatalf("leases = %d, want 0", leases)
	}
	if rows := h.ledger.all(); len(rows) != 0 {
		t.Fatalf("ledger rows = %d, want 0", len(rows))
	}
}

// Ordinary multimodal requests, and policy key names inside JSON schemas
// (structured output, function parameters) or other top-level values, are
// forwarded unchanged.
func TestMultiUserAllowsBenignNestedContent(t *testing.T) {
	h, hits, bodies := multiUserFieldHarness(t)
	cases := []struct{ path, body string }{
		{"/v1/responses", `{"model":"gpt-x","input":[{"role":"user","content":[{"type":"input_text","text":"what is this?"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`},
		{"/v1/chat/completions", `{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`},
		{"/v1/chat/completions", `{"model":"gpt-x","messages":[{"role":"user","content":"file_id container item_reference"}]}`},
		{"/v1/chat/completions", `{"model":"gpt-x","messages":[],"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object","properties":{"file_id":{"type":"string"},"container":{"type":"string"}}}}}}`},
		{"/v1/responses", `{"model":"gpt-x","input":"q","text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object","properties":{"file_id":{"type":"string"},"vector_store_ids":{"type":"array"}}}}}}`},
		{"/v1/responses", `{"model":"gpt-x","input":"q","metadata":{"file_id":"x","type":"item_reference"}}`},
		// Function parameters are JSON schemas, not references.
		{"/v1/chat/completions", `{"model":"gpt-x","messages":[],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"file_id":{"type":"string"},"container":{"type":"string"},"kind":{"type":"item_reference"}}}}}]}`},
		{"/v1/responses", `{"model":"gpt-x","input":"q","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"vector_store_ids":{"type":"array"}}}}]}`},
		// A tool call's arguments are a JSON string, not structure.
		{"/v1/responses", `{"model":"gpt-x","input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{\"file_id\":\"x\"}"}]}`},
	}
	for i, tc := range cases {
		postForwarded(t, h, bodies, tc.path, userTokenA1, tc.body)
		if n := hits(); n != int32(i+1) {
			t.Fatalf("%s: hits %d", tc.body, n)
		}
	}
}

// A function tool's parameters are skipped only as a schema: policy keys on
// the tool object itself, or beside the function definition, are refused.
func TestMultiUserRefusesStateReferencesBesideFunctionSchema(t *testing.T) {
	h, hits, _ := multiUserFieldHarness(t)
	postRefused(t, h, "/v1/responses",
		`{"model":"gpt-x","input":"q","tools":[{"type":"function","name":"f","parameters":{},"container":"c"}]}`, "container")
	postRefused(t, h, "/v1/chat/completions",
		`{"model":"gpt-x","messages":[],"tools":[{"type":"function","function":{"name":"f","parameters":{},"file_id":"x"}}]}`, "file_id")
	postRefused(t, h, "/v1/responses",
		`{"model":"gpt-x","input":[{"role":"user","content":[{"type":"input_file","parameters":{"file_id":"x"}}]}]}`, "file_id")
	// Only tools entries carry function schemas; input and messages items
	// shaped like tools are walked in full.
	postRefused(t, h, "/v1/responses",
		`{"model":"gpt-x","input":[{"type":"function","parameters":{"file_id":"x"}}]}`, "file_id")
	postRefused(t, h, "/v1/chat/completions",
		`{"model":"gpt-x","messages":[{"function":{"parameters":{"container":"c"}}}]}`, "container")
	if hits() != 0 {
		t.Fatal("upstream hit")
	}
}

// The nested walk is bounded: input/messages/tools nested deeper than
// maxBodyDepth are refused with a 400 before any upstream call.
func TestMultiUserRefusesDeeplyNestedBody(t *testing.T) {
	h, hits, bodies := multiUserFieldHarness(t)
	nest := func(n int) string {
		return `{"model":"gpt-x","input":` + strings.Repeat("[", n) + strings.Repeat("]", n) + `}`
	}
	// The top-level object is level 1, so input may open maxBodyDepth-1 levels.
	postForwarded(t, h, bodies, "/v1/responses", userTokenA1, nest(maxBodyDepth-1))
	resp := h.post("/v1/responses", userTokenA1, nest(maxBodyDepth), nil)
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if e := decodeErr(t, resp); e.Error.Type != "invalid_request_error" || !strings.Contains(e.Error.Message, "nested too deeply") {
		t.Fatalf("error %+v", e.Error)
	}
	if hits() != 1 {
		t.Fatalf("hits %d, want 1", hits())
	}
}

// Legacy single-user mode forwards nested references, web_search_options and
// deep nesting unchanged.
func TestLegacyForwardsNestedStateReferences(t *testing.T) {
	h := newHarness(t)
	_, bodies := countingUpstream(h, "a", core.ProviderOpenRouter)
	singleRoute(h, "a")
	h.start()
	for _, body := range []string{
		`{"model":"gpt-x","input":[{"type":"item_reference","id":"msg_1"},{"role":"user","content":[{"type":"input_file","file_id":"file-1"}]}],` +
			`"tools":[{"type":"file_search","vector_store_ids":["vs_1"]},{"type":"code_interpreter","container":"cntr_1"}]}`,
		`{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"f"}}]}],"web_search_options":{}}`,
		`{"model":"gpt-x","input":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}`,
	} {
		postForwarded(t, h, bodies, "/v1/responses", clientKey, body)
	}
}
