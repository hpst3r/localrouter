package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// capRoute installs one route whose candidates are described by specs. A
// candidate absent from specs gets the zero descriptor (text-only, no optional
// features).
func capRoute(h *harness, specs map[string]core.UpstreamSpec, ids ...string) {
	h.routes = []core.Route{{
		Name:        "cap",
		Models:      []string{"gpt-x"},
		Upstreams:   specs,
		Interactive: ids,
		Background:  ids,
	}}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// imageBody is a Responses request that declares an image input by shape, not by
// a URL in prompt text.
const imageBody = `{"model":"gpt-x","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`

const textBody = `{"model":"gpt-x","stream":false,"input":"hello"}`

// An image request must never fail over to a candidate that cannot accept
// images: the text-only candidate is filtered out before admission, so the
// retryable 429 from the only image-capable account is relayed instead of being
// answered (incorrectly) by a text-only backend.
func TestCapabilityImageFailoverNeverTextOnly(t *testing.T) {
	h := newHarness(t)
	var visionHits, textHits atomic.Int32
	h.upstream("vision", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		visionHits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"vision busy"}}`)
	})
	h.upstream("text", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		textHits.Add(1)
		okJSON(w, r)
	})
	capRoute(h,
		map[string]core.UpstreamSpec{
			"vision": {Protocols: []string{"responses"}, InputModalities: []string{"text", "image"}, Tools: true, JSONSchema: true, Stream: true},
			"text":   {}, // zero descriptor: text only
		},
		"vision", "text")
	h.start()

	resp := h.post("/v1/responses", clientKey, imageBody, nil)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "vision busy") {
		t.Fatalf("status %d body %s (want the buffered 429 relayed)", resp.StatusCode, body)
	}
	if textHits.Load() != 0 {
		t.Fatalf("text-only account was used for an image request (%d hits)", textHits.Load())
	}
	if visionHits.Load() != 1 {
		t.Fatalf("vision hits %d, want 1", visionHits.Load())
	}
	rows := h.waitRows(1)
	if len(rows) != 1 || rows[0].AccountID != "vision" || rows[0].Status != 429 {
		t.Fatalf("rows %+v", rows)
	}
	if len(h.policy.leases) != 1 || h.policy.leases[0].id != "vision" {
		t.Fatalf("leases %d, want exactly the vision lease", len(h.policy.leases))
	}
}

// A capability-constrained request no candidate can serve is rejected with 400
// capability_unsupported before any lease or upstream call, and never silently
// downgraded to a text-only attempt.
func TestCapabilityUnsupportedRejectedBeforeUpstream(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("text", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		okJSON(w, r)
	})
	capRoute(h, map[string]core.UpstreamSpec{"text": {}}, "text")
	h.start()

	resp := h.post("/v1/responses", clientKey, imageBody, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	e := decodeErr(t, resp)
	if e.Error.Type != "capability_unsupported" {
		t.Fatalf("error type %q, want capability_unsupported", e.Error.Type)
	}
	if strings.Contains(e.Error.Message, "text") {
		t.Fatalf("error message names a candidate: %q", e.Error.Message)
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream was called %d times", hits.Load())
	}
	if len(h.policy.leases) != 0 {
		t.Fatalf("a lease was taken for an unservable request")
	}
	if rows := h.ledger.all(); len(rows) != 0 {
		t.Fatalf("ledger rows recorded: %+v", rows)
	}
}

// tools, json_schema and stream are each a hard requirement: a candidate that
// does not declare them is excluded and the request fails closed.
func TestCapabilityToolsJSONSchemaStreamMismatch(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		body  string
		specs map[string]core.UpstreamSpec
	}{
		{
			name: "tools",
			path: "/v1/responses",
			body: `{"model":"gpt-x","input":"hi","tools":[{"type":"function","function":{"name":"f"}}]}`,
			specs: map[string]core.UpstreamSpec{
				"a": {},             // supports neither
				"b": {Tools: false}, // explicitly no tools
			},
		},
		{
			name: "json_schema_chat",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{}}}}`,
			specs: map[string]core.UpstreamSpec{
				"a": {},
				"b": {JSONSchema: false},
			},
		},
		{
			name: "stream",
			path: "/v1/responses",
			body: `{"model":"gpt-x","input":"hi","stream":true}`,
			specs: map[string]core.UpstreamSpec{
				"a": {},
				"b": {Stream: false},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var hits atomic.Int32
			h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
			h.upstream("b", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
			capRoute(h, tc.specs, "a", "b")
			h.start()

			resp := h.post(tc.path, clientKey, tc.body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", resp.StatusCode)
			}
			if e := decodeErr(t, resp); e.Error.Type != "capability_unsupported" {
				t.Fatalf("error type %q", e.Error.Type)
			}
			if hits.Load() != 0 || len(h.policy.leases) != 0 {
				t.Fatalf("hits %d leases %d, want 0 and 0", hits.Load(), len(h.policy.leases))
			}
		})
	}
}

// A candidate that does declare the required capability still serves the
// request; the mismatch cases above are about exclusion, not blanket rejection.
func TestCapabilityDeclaredFeaturesPass(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("capable", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		okJSON(w, r)
	})
	capRoute(h, map[string]core.UpstreamSpec{
		"capable": {Protocols: []string{"responses"}, InputModalities: []string{"text", "image"}, Tools: true, JSONSchema: true, Stream: true},
	}, "capable")
	h.start()

	body := `{"model":"gpt-x","stream":true,"tools":[{"type":"function","function":{"name":"f"}}],"text":{"format":{"type":"json_schema","name":"r","schema":{}}},"input":[{"type":"input_image","image_url":"https://example.com/a.png"}]}`
	resp := h.post("/v1/responses", clientKey, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
}

// Each attempt builds its own body from the untouched client body, so a failover
// to a different backend sends that backend's alias and never the previous
// attempt's rewrite.
func TestCapabilityPerBackendAliasRewritesBothAttempts(t *testing.T) {
	h := newHarness(t)
	bodyA := make(chan string, 1)
	bodyB := make(chan string, 1)
	h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyA <- string(b)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.upstream("b", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyB <- string(b)
		okJSON(w, r)
	})
	capRoute(h, map[string]core.UpstreamSpec{
		"a": {UpstreamModel: "alias-a", Protocols: []string{"responses"}},
		"b": {UpstreamModel: "alias-b", Protocols: []string{"responses"}},
	}, "a", "b")
	h.start()

	resp := h.post("/v1/responses", clientKey, textBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	modelOf := func(b string) string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			t.Fatalf("upstream body not JSON: %s", b)
		}
		return strings.Trim(string(m["model"]), `"`)
	}
	if got := modelOf(<-bodyA); got != "alias-a" {
		t.Fatalf("attempt a sent model %q, want alias-a", got)
	}
	if got := modelOf(<-bodyB); got != "alias-b" {
		t.Fatalf("attempt b sent model %q, want alias-b (no rewrite carryover)", got)
	}

	rows := h.waitRows(2)
	if len(rows) != 2 {
		t.Fatalf("rows %d", len(rows))
	}
	// The client-facing model stays the request's model; UpstreamModel records
	// what each attempt actually resolved.
	if rows[0].Model != "gpt-x" || rows[0].UpstreamModel != "alias-a" || rows[0].AccountID != "a" {
		t.Fatalf("row a %+v", rows[0])
	}
	if rows[1].Model != "gpt-x" || rows[1].UpstreamModel != "alias-b" || rows[1].AccountID != "b" {
		t.Fatalf("row b %+v", rows[1])
	}
	if rows[1].FailoverOf != rows[0].ID {
		t.Fatalf("failover chain not linked: %+v", rows)
	}
}

// A per-attempt rewrite must not leak in the other direction either: an attempt
// with no alias sends the client model verbatim even when a later attempt does
// rewrite it.
func TestCapabilityRewriteDoesNotLeakForward(t *testing.T) {
	h := newHarness(t)
	bodyA := make(chan string, 1)
	h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyA <- string(b)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.upstream("b", core.ProviderOllama, okJSON)
	capRoute(h, map[string]core.UpstreamSpec{
		"a": {},
		"b": {UpstreamModel: "alias-b"},
	}, "a", "b")
	h.start()

	resp := h.post("/v1/responses", clientKey, textBody, nil)
	resp.Body.Close()
	gotA := <-bodyA
	if !strings.Contains(gotA, `"model":"gpt-x"`) {
		t.Fatalf("attempt a body %s, want the client model", gotA)
	}
	rows := h.waitRows(2)
	if rows[0].UpstreamModel != "gpt-x" || rows[1].UpstreamModel != "alias-b" {
		t.Fatalf("rows %+v", rows)
	}
}

// Failover still follows the operator's candidate order: the filter only removes
// candidates, so a text-capable request fails over vision -> text unchanged.
func TestCapabilityFilterKeepsCandidateOrder(t *testing.T) {
	h := newHarness(t)
	h.upstream("vision", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h.upstream("text", core.ProviderOllama, okJSON)
	capRoute(h, map[string]core.UpstreamSpec{
		"vision": {InputModalities: []string{"text", "image"}, Protocols: []string{"responses"}},
		"text":   {},
	}, "vision", "text")
	h.start()

	resp := h.post("/v1/responses", clientKey, textBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	rows := h.waitRows(2)
	if rows[0].AccountID != "vision" || rows[1].AccountID != "text" {
		t.Fatalf("failover order %s -> %s", rows[0].AccountID, rows[1].AccountID)
	}
}

// A constrained route fails closed on a body it cannot classify: a content
// part it does not model is 400 invalid_request_error, not a silent text-only
// attempt.
func TestCapabilityMalformedConstrainedBodyRejected(t *testing.T) {
	malformed := []struct {
		name     string
		endpoint string
		body     string
	}{
		{"chat unknown part type", "/v1/chat/completions",
			`{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"https://example.com/v.mp4"}}]}]}`},
		{"chat messages not array", "/v1/chat/completions",
			`{"model":"gpt-x","messages":{"role":"user"}}`},
		{"chat unknown response_format", "/v1/chat/completions",
			`{"model":"gpt-x","response_format":{"type":"mystery"}}`},
		{"responses unknown content type", "/v1/responses",
			`{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"video_url","video_url":{"url":"https://example.com/v.mp4"}}]}]}`},
		{"responses content not parts", "/v1/responses",
			`{"model":"gpt-x","input":[{"type":"message","role":"user","content":{"nested":true}}]}`},
		{"responses input not string or array", "/v1/responses",
			`{"model":"gpt-x","input":{"type":"message"}}`},
		{"responses unknown format type", "/v1/responses",
			`{"model":"gpt-x","text":{"format":{"type":"mystery"}}}`},
		{"responses previous_response_id", "/v1/responses",
			`{"model":"gpt-x","input":"hi","previous_response_id":"resp_1"}`},
		{"responses item_reference", "/v1/responses",
			`{"model":"gpt-x","input":[{"type":"item_reference","id":"msg_1"}]}`},
		{"responses unknown item type", "/v1/responses",
			`{"model":"gpt-x","input":[{"type":"mystery_item","foo":1}]}`},
		{"responses computer_call_output unknown payload", "/v1/responses",
			`{"model":"gpt-x","input":[{"type":"computer_call_output","call_id":"c1","output":{"type":"mystery_blob","data":"..."}}]}`},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var hits atomic.Int32
			h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				okJSON(w, r)
			})
			capRoute(h, map[string]core.UpstreamSpec{"a": {Stream: true, Tools: true, JSONSchema: true}}, "a")
			h.start()

			resp := h.post(tc.endpoint, clientKey, tc.body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d body %s, want 400", resp.StatusCode, readAll(t, resp))
			}
			if e := decodeErr(t, resp); e.Error.Type != "invalid_request_error" {
				t.Fatalf("error type %q, want invalid_request_error", e.Error.Type)
			}
			if hits.Load() != 0 || len(h.policy.leases) != 0 || len(h.ledger.all()) != 0 {
				t.Fatalf("malformed constrained body reached upstream/lease/ledger")
			}
		})
	}
}

// A route with no per-candidate descriptors stays legacy: its body is forwarded
// byte-for-byte (no capability inference at all), image content and all, so a
// body the constrained path would reject is not rejected here.
func TestLegacyRouteBypassesCapabilityInference(t *testing.T) {
	h := newHarness(t)
	bodyCh := make(chan string, 1)
	h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- string(b)
		okJSON(w, r)
	})
	singleRoute(h, "a")
	h.start()

	in := `{"model":"gpt-x",  "messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"https://example.com/v.mp4"}}]}]}`
	resp := h.post("/v1/chat/completions", clientKey, in, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if got := <-bodyCh; got != in {
		t.Fatalf("legacy body altered:\n got %q\nwant %q", got, in)
	}
}

// The legacy route-level upstream model keeps working off the same per-attempt
// path, and the ledger records the resolved upstream model for it.
func TestLegacyRouteUpstreamModelRecorded(t *testing.T) {
	h := newHarness(t)
	bodyCh := make(chan string, 1)
	h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- string(b)
		okJSON(w, r)
	})
	h.routes = []core.Route{{Name: "main", Models: []string{"gpt-x"}, UpstreamModel: "real-model", Interactive: []string{"a"}, Background: []string{"a"}}}
	h.start()

	resp := h.post("/v1/responses", clientKey, textBody, nil)
	resp.Body.Close()
	if got := <-bodyCh; !strings.Contains(got, `"model":"real-model"`) {
		t.Fatalf("legacy rewrite missing: %s", got)
	}
	rows := h.waitRows(1)
	if rows[0].Model != "gpt-x" || rows[0].UpstreamModel != "real-model" {
		t.Fatalf("row %+v", rows[0])
	}
}

// assertDenied proves a rejected request produced the expected stable error type
// and reached no upstream, took no lease and wrote no ledger row — a deny with
// zero hits.
func assertDenied(t *testing.T, h *harness, resp *http.Response, hits *atomic.Int32, wantType string) {
	t.Helper()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d body %s, want 400", resp.StatusCode, readAll(t, resp))
	}
	if e := decodeErr(t, resp); e.Error.Type != wantType {
		t.Fatalf("error type %q, want %q", e.Error.Type, wantType)
	}
	if hits.Load() != 0 || len(h.policy.leases) != 0 || len(h.ledger.all()) != 0 {
		t.Fatalf("denied request reached upstream/lease/ledger: hits=%d leases=%d rows=%d",
			hits.Load(), len(h.policy.leases), len(h.ledger.all()))
	}
}

// An image-only request declares image and is not forced to also require text,
// so it reaches a vision-only candidate while a text-only candidate is filtered
// out — never silently downgraded to a text-only backend.
func TestCapabilityImageOnlyReachesVisionOnlyBackend(t *testing.T) {
	h := newHarness(t)
	var visionHits, textHits atomic.Int32
	h.upstream("visiononly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		visionHits.Add(1)
		okJSON(w, r)
	})
	h.upstream("textonly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		textHits.Add(1)
		okJSON(w, r)
	})
	capRoute(h, map[string]core.UpstreamSpec{
		"visiononly": {InputModalities: []string{"image"}},
		"textonly":   {},
	}, "visiononly", "textonly")
	h.start()

	body := `{"model":"gpt-x","stream":false,"input":[{"type":"input_image","image_url":"https://example.com/a.png"}]}`
	resp := h.post("/v1/responses", clientKey, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if visionHits.Load() != 1 || textHits.Load() != 0 {
		t.Fatalf("vision hits %d text hits %d, want 1 and 0", visionHits.Load(), textHits.Load())
	}
	rows := h.waitRows(1)
	if len(rows) != 1 || rows[0].AccountID != "visiononly" {
		t.Fatalf("rows %+v", rows)
	}
}

// An input_image paired with a textual prompt requires both modalities: neither
// an image-only nor a text-only candidate can serve it, and only a candidate
// that declares both does.
func TestCapabilityTextPlusImageRequiresBoth(t *testing.T) {
	const bothBody = `{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`

	t.Run("no candidate serves both", func(t *testing.T) {
		h := newHarness(t)
		var hits atomic.Int32
		h.upstream("imgonly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
		h.upstream("txtonly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
		capRoute(h, map[string]core.UpstreamSpec{
			"imgonly": {InputModalities: []string{"image"}},
			"txtonly": {},
		}, "imgonly", "txtonly")
		h.start()
		assertDenied(t, h, h.post("/v1/responses", clientKey, bothBody, nil), &hits, "capability_unsupported")
	})

	t.Run("both-capable candidate serves", func(t *testing.T) {
		h := newHarness(t)
		var hits atomic.Int32
		h.upstream("both", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
		capRoute(h, map[string]core.UpstreamSpec{
			"both": {InputModalities: []string{"text", "image"}},
		}, "both")
		h.start()

		resp := h.post("/v1/responses", clientKey, bothBody, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
		}
		resp.Body.Close()
		if hits.Load() != 1 {
			t.Fatalf("hits %d, want 1", hits.Load())
		}
	})
}

// In v1 the structured-output requirement is a single boolean, so json_object
// requires JSONSchema support exactly like json_schema does: a candidate that
// does not declare it is excluded and a capable one serves.
func TestCapabilityJSONObjectRequiresJSONSchema(t *testing.T) {
	h := newHarness(t)
	var aHits, bHits atomic.Int32
	h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { aHits.Add(1); okJSON(w, r) })
	h.upstream("b", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { bHits.Add(1); okJSON(w, r) })
	capRoute(h, map[string]core.UpstreamSpec{
		"a": {},
		"b": {JSONSchema: true},
	}, "a", "b")
	h.start()

	body := `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`
	resp := h.post("/v1/chat/completions", clientKey, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if aHits.Load() != 0 || bHits.Load() != 1 {
		t.Fatalf("json_object routed to a=%d b=%d, want 0 and 1", aHits.Load(), bHits.Load())
	}
}

// A bare "none" tool selector opts out: it does not force tool support, so a
// candidate that declares no tools serves it. A declared tools array is still
// authoritative and requires tool support even alongside "none".
func TestCapabilityToolChoiceNoneDoesNotRequireTools(t *testing.T) {
	t.Run("none selector needs no tools support", func(t *testing.T) {
		h := newHarness(t)
		var hits atomic.Int32
		h.upstream("plain", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
		capRoute(h, map[string]core.UpstreamSpec{"plain": {}}, "plain")
		h.start()

		resp := h.post("/v1/responses", clientKey, `{"model":"gpt-x","input":"hi","tool_choice":"none"}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
		}
		resp.Body.Close()
		if hits.Load() != 1 {
			t.Fatalf("hits %d, want 1", hits.Load())
		}
	})

	t.Run("declared tools still require tools", func(t *testing.T) {
		h := newHarness(t)
		var hits atomic.Int32
		h.upstream("plain", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
		capRoute(h, map[string]core.UpstreamSpec{"plain": {}}, "plain")
		h.start()

		body := `{"model":"gpt-x","input":"hi","tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"none"}`
		assertDenied(t, h, h.post("/v1/responses", clientKey, body, nil), &hits, "capability_unsupported")
	})
}

// Opaque prior context (previous_response_id, conversation, a reusable prompt,
// item_reference) cannot be classified, so a constrained route rejects it as invalid_request_error rather
// than guessing text-only, with no upstream, lease or ledger side effect.
func TestCapabilityOpaqueContextRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"previous_response_id", `{"model":"gpt-x","input":"hi","previous_response_id":"resp_1"}`},
		{"item_reference", `{"model":"gpt-x","input":[{"type":"item_reference","id":"msg_1"}]}`},
		{"prompt variables image", `{"model":"gpt-x","prompt":{"id":"pmpt_1","variables":{"img":{"type":"input_image","image_url":"https://x/y.png"}}},"input":"describe"}`},
		{"prompt variables file", `{"model":"gpt-x","prompt":{"id":"pmpt_1","variables":{"doc":{"type":"input_file","file_id":"file_1"}}}}`},
		{"conversation string", `{"model":"gpt-x","conversation":"conv_1","input":"continue"}`},
		{"conversation object", `{"model":"gpt-x","conversation":{"id":"conv_1"},"input":"continue"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var hits atomic.Int32
			h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
			capRoute(h, map[string]core.UpstreamSpec{
				"a": {Stream: true, Tools: true, JSONSchema: true},
			}, "a")
			h.start()
			assertDenied(t, h, h.post("/v1/responses", clientKey, tc.body, nil), &hits, "invalid_request_error")
		})
	}
}

// A computer_call_output carries its image nested inside output: the classifier
// must detect that screenshot so the request reaches an image-capable backend.
func TestCapabilityComputerCallOutputNestedImage(t *testing.T) {
	h := newHarness(t)
	var visionHits, textHits atomic.Int32
	h.upstream("visiononly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		visionHits.Add(1)
		okJSON(w, r)
	})
	h.upstream("textonly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		textHits.Add(1)
		okJSON(w, r)
	})
	capRoute(h, map[string]core.UpstreamSpec{
		"visiononly": {InputModalities: []string{"image"}},
		"textonly":   {},
	}, "visiononly", "textonly")
	h.start()

	body := `{"model":"gpt-x","input":[{"type":"computer_call_output","call_id":"c1","output":{"type":"computer_screenshot","image_url":"https://example.com/s.png"}}]}`
	resp := h.post("/v1/responses", clientKey, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if visionHits.Load() != 1 || textHits.Load() != 0 {
		t.Fatalf("vision hits %d text hits %d, want 1 and 0", visionHits.Load(), textHits.Load())
	}
}

// An unmodelled nested payload inside computer_call_output may hide an image, so
// it fails closed as invalid_request_error instead of being ignored.
func TestCapabilityComputerCallOutputUnknownPayloadFailsClosed(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
	capRoute(h, map[string]core.UpstreamSpec{
		"a": {InputModalities: []string{"text", "image"}, Tools: true, JSONSchema: true, Stream: true},
	}, "a")
	h.start()

	body := `{"model":"gpt-x","input":[{"type":"computer_call_output","call_id":"c1","output":{"type":"mystery_blob","data":"..."}}]}`
	assertDenied(t, h, h.post("/v1/responses", clientKey, body, nil), &hits, "invalid_request_error")
}

// file is a known-but-unsupported input modality: it is reported (so no
// candidate can satisfy it) and the request is 400 capability_unsupported, not
// the malformed invalid_request_error.
func TestCapabilityFileInputIsUnsupportedNotMalformed(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{"chat file part", "/v1/chat/completions",
			`{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"f1"}}]}]}`},
		{"responses input_file", "/v1/responses",
			`{"model":"gpt-x","input":[{"type":"input_file","file_id":"file_1"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var hits atomic.Int32
			h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
			capRoute(h, map[string]core.UpstreamSpec{
				"a": {Stream: true, Tools: true, JSONSchema: true},
			}, "a")
			h.start()
			assertDenied(t, h, h.post(tc.path, clientKey, tc.body, nil), &hits, "capability_unsupported")
		})
	}
}

// A body that is not a JSON object cannot be classified on a constrained route;
// it is 400 invalid_request_error with no upstream call, lease or ledger row.
func TestCapabilityInvalidJSONBodyRejected(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
	capRoute(h, map[string]core.UpstreamSpec{"a": {}}, "a")
	h.start()

	assertDenied(t, h, h.post("/v1/responses", clientKey, `[1,2,3]`, nil), &hits, "invalid_request_error")
}

// A function_call_output whose output is an array of typed content parts
// declares the modalities it holds: an input_image part makes it an image
// request, so a text-only backend must never serve it. The vision-only
// candidate serves and the text-only one is filtered out before admission.
func TestCapabilityFunctionCallOutputImageNotRoutedToTextOnlyBackend(t *testing.T) {
	h := newHarness(t)
	var visionHits, textHits atomic.Int32
	h.upstream("visiononly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		visionHits.Add(1)
		okJSON(w, r)
	})
	h.upstream("textonly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		textHits.Add(1)
		okJSON(w, r)
	})
	capRoute(h, map[string]core.UpstreamSpec{
		"visiononly": {InputModalities: []string{"image"}},
		"textonly":   {},
	}, "visiononly", "textonly")
	h.start()

	body := `{"model":"gpt-x","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"https://example.com/s.png"}]}]}`
	resp := h.post("/v1/responses", clientKey, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if visionHits.Load() != 1 || textHits.Load() != 0 {
		t.Fatalf("vision hits %d text hits %d, want 1 and 0", visionHits.Load(), textHits.Load())
	}
	rows := h.waitRows(1)
	if len(rows) != 1 || rows[0].AccountID != "visiononly" {
		t.Fatalf("rows %+v", rows)
	}
}

// A function_call_output whose output is a plain string is text, and that
// string is not scanned for embedded content: a text-only backend serves it
// even when the result string contains an image URL, and the vision-only
// candidate (which does not declare text) is filtered out.
func TestCapabilityFunctionCallOutputStringOutputIsText(t *testing.T) {
	h := newHarness(t)
	var textHits, visionHits atomic.Int32
	h.upstream("textonly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		textHits.Add(1)
		okJSON(w, r)
	})
	h.upstream("visiononly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		visionHits.Add(1)
		okJSON(w, r)
	})
	capRoute(h, map[string]core.UpstreamSpec{
		"textonly":   {},
		"visiononly": {InputModalities: []string{"image"}},
	}, "textonly", "visiononly")
	h.start()

	body := `{"model":"gpt-x","input":[{"type":"function_call_output","call_id":"c1","output":"{\"note\":\"see https://example.com/a.png\"}"}]}`
	resp := h.post("/v1/responses", clientKey, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if textHits.Load() != 1 || visionHits.Load() != 0 {
		t.Fatalf("text hits %d vision hits %d, want 1 and 0", textHits.Load(), visionHits.Load())
	}
	rows := h.waitRows(1)
	if len(rows) != 1 || rows[0].AccountID != "textonly" {
		t.Fatalf("rows %+v", rows)
	}
}

// A function_call_output whose output array holds a content part the classifier
// does not model may hide an image, so a constrained route fails closed as
// invalid_request_error with no upstream call, lease or ledger row.
func TestCapabilityFunctionCallOutputArrayUnknownRejectedNoHit(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
	capRoute(h, map[string]core.UpstreamSpec{
		"a": {InputModalities: []string{"text", "image"}, Stream: true, Tools: true, JSONSchema: true},
	}, "a")
	h.start()

	body := `{"model":"gpt-x","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"mystery_blob","data":"..."}]}]}`
	assertDenied(t, h, h.post("/v1/responses", clientKey, body, nil), &hits, "invalid_request_error")
}

// A case variant or duplicate of a member the classifier reads would let the
// router route on a different value than a case-sensitive upstream acts on, so
// a constrained route rejects it before any lease: the only candidate here is
// text-only with no structured output, and each body really carries an image,
// opaque context or a json_schema request under the exact member name.
func TestCapabilityAmbiguousMembersRejectedNoHit(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"chat image_url part with Type:text", "/v1/chat/completions",
			`{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"},"Type":"text"}]}]}`},
		{"responses input_image item with Type:input_text", "/v1/responses",
			`{"model":"gpt-x","input":[{"type":"input_image","image_url":"https://x/y.png","Type":"input_text"}]}`},
		{"responses message Content shadow", "/v1/responses",
			`{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}],"Content":"hi"}]}`},
		{"responses item_reference with Type:input_text", "/v1/responses",
			`{"model":"gpt-x","input":[{"type":"item_reference","id":"msg_1","Type":"input_text"}]}`},
		{"chat response_format json_schema with Type:text", "/v1/chat/completions",
			`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{}},"Type":"text"}}`},
		{"responses text.format json_schema with Format:null", "/v1/responses",
			`{"model":"gpt-x","input":"hi","text":{"format":{"type":"json_schema","name":"r","schema":{}},"Format":null}}`},
		{"chat duplicate part type", "/v1/chat/completions",
			`{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"text","type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`},
		{"chat Stream shadow", "/v1/chat/completions",
			`{"model":"gpt-x","stream":false,"Stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var hits atomic.Int32
			h.upstream("textonly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
			capRoute(h, map[string]core.UpstreamSpec{"textonly": {}}, "textonly")
			h.start()
			assertDenied(t, h, h.post(tc.path, clientKey, tc.body, nil), &hits, "invalid_request_error")
		})
	}
}

// A Codex-style multi-turn Responses history — assistant text and refusal,
// reasoning, function, custom (apply_patch) and local shell calls with their
// outputs — is classified as text and served by a text-only constrained
// candidate instead of failing on the first replayed tool call.
func TestCapabilityAgentHistoryServedAsText(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("textonly", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
	capRoute(h, map[string]core.UpstreamSpec{"textonly": {Tools: true}}, "textonly")
	h.start()

	body := `{"model":"gpt-x","tools":[{"type":"custom","name":"apply_patch"}],"input":[` +
		`{"role":"user","content":"fix the bug"},` +
		`{"type":"reasoning","id":"rs_1","summary":[]},` +
		`{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"contents"},` +
		`{"type":"local_shell_call","id":"ls_1","call_id":"c2","status":"completed","action":{"type":"exec","command":["go","test"],"env":{}}},` +
		`{"type":"local_shell_call_output","id":"c2","output":"ok"},` +
		`{"type":"custom_tool_call","call_id":"c3","name":"apply_patch","input":"*** Begin Patch"},` +
		`{"type":"custom_tool_call_output","call_id":"c3","output":"Done"},` +
		`{"type":"message","role":"assistant","id":"msg_1","status":"completed","content":[{"type":"output_text","text":"done","annotations":[]},{"type":"refusal","refusal":"no"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":"thanks"}]}]}`
	resp := h.post("/v1/responses", clientKey, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}

// A legacy route that rewrites the body (route upstream_model, or include_usage
// on a streaming chat request) needs the body to be one strict JSON object. A
// body the lenient head decode accepts but that carries trailing data must be
// the legacy 400 before any lease — not a per-candidate transport error that
// leases, fetches credentials and writes a ledger row for every candidate.
func TestLegacyRewriteRejectsTrailingDataBeforeLease(t *testing.T) {
	ids := []string{"a", "b", "c"}
	cases := []struct {
		name, path, body string
		route            core.Route
	}{
		{"route upstream_model", "/v1/responses", `{"model":"gpt-x","input":"hi"} x`,
			core.Route{Name: "main", Models: []string{"gpt-x"}, UpstreamModel: "real-model", Interactive: ids, Background: ids}},
		{"route upstream_model equal to client model", "/v1/responses", `{"model":"gpt-x","input":"hi"} x`,
			core.Route{Name: "main", Models: []string{"gpt-x"}, UpstreamModel: "gpt-x", Interactive: ids, Background: ids}},
		{"chat stream include_usage", "/v1/chat/completions", `{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}{"x":1}`,
			core.Route{Name: "main", Models: []string{"gpt-x"}, Interactive: ids, Background: ids}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var hits atomic.Int32
			for _, id := range ids {
				h.upstream(id, core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); okJSON(w, r) })
			}
			h.routes = []core.Route{tc.route}
			h.start()
			assertDenied(t, h, h.post(tc.path, clientKey, tc.body, nil), &hits, "invalid_request_error")
		})
	}

	// A legacy route that rewrites nothing refuses trailing data too (parseHead
	// accepts only a single JSON object), and still forwards a valid body
	// verbatim.
	h := newHarness(t)
	var hits atomic.Int32
	got := make(chan string, 1)
	h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		okJSON(w, r)
	})
	singleRoute(h, "a")
	h.start()
	assertDenied(t, h, h.post("/v1/responses", clientKey, `{"model":"gpt-x","input":"hi"} x`, nil), &hits, "invalid_request_error")
	in := `{"input": "hi",  "model":"gpt-x"}`
	resp := h.post("/v1/responses", clientKey, in, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no-rewrite status %d", resp.StatusCode)
	}
	if b := <-got; b != in {
		t.Fatalf("no-rewrite body altered: %q", b)
	}
}
