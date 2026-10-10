package config

import (
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// capAccounts is a minimal valid config preamble reused by the capability
// routing tests: one codex account (responses-only) and two API-key accounts
// (chat + responses capable), plus a client.
const capAccounts = `
clients: [{name: a, class: interactive, key_file: k}]
accounts:
  - {id: cx, provider: codex}
  - {id: ol, provider: ollama, base_url: http://ol, api_key_env: OLKEY}
  - {id: oc, provider: openai_compat, base_url: http://oc, api_key_env: OCKEY}
`

func loadRoutes(t *testing.T, body string) (*Config, error) {
	t.Helper()
	return Load(write(t, capAccounts+body))
}

// TestLegacyRouteHasNoUpstreams pins the first unknown rule: a route with no
// upstreams map is unconstrained and its core.Route carries a nil map, exactly
// as before capability routing existed.
func TestLegacyRouteHasNoUpstreams(t *testing.T) {
	c, err := loadRoutes(t, `
routes: [{name: r, models: [m], upstream_model: backend-x, interactive: [cx]}]
`)
	if err != nil {
		t.Fatalf("legacy route must load: %v", err)
	}
	r := c.CoreRoutes()[0]
	if r.Upstreams != nil {
		t.Fatalf("legacy route Upstreams = %#v, want nil", r.Upstreams)
	}
	if r.UpstreamModel != "backend-x" {
		t.Fatalf("legacy upstream_model lost: %q", r.UpstreamModel)
	}
}

// TestUpstreamsValidOptIn covers a well-formed opted-in route: every candidate
// in interactive ∪ background has a descriptor, booleans survive, and an
// image-only (vision-only) candidate is accepted without text being implied.
func TestUpstreamsValidOptIn(t *testing.T) {
	c, err := loadRoutes(t, `
routes:
  - name: r
    models: [m]
    upstream_model: inherited
    interactive: [cx, ol]
    background: [oc]
    upstreams:
      cx:
        protocols: [responses]
        input_modalities: [text]
        tools: true
        stream: true
      ol:
        upstream_model: vision-1
        protocols: [chat, responses]
        input_modalities: [image]
        json_schema: true
      oc:
        protocols: [chat]
        input_modalities: [text, image]
`)
	if err != nil {
		t.Fatalf("valid upstreams rejected: %v", err)
	}
	r := c.CoreRoutes()[0]
	if len(r.Upstreams) != 3 {
		t.Fatalf("Upstreams = %#v", r.Upstreams)
	}
	cx := r.Upstreams["cx"]
	if cx.UpstreamModel != "" || len(cx.Protocols) != 1 || cx.Protocols[0] != "responses" ||
		len(cx.InputModalities) != 1 || cx.InputModalities[0] != "text" || !cx.Tools || !cx.Stream || cx.JSONSchema {
		t.Fatalf("cx spec = %#v", cx)
	}
	ol := r.Upstreams["ol"]
	if ol.UpstreamModel != "vision-1" || len(ol.InputModalities) != 1 || ol.InputModalities[0] != "image" || ol.Tools {
		t.Fatalf("ol spec = %#v (image-only must not imply text)", ol)
	}
	oc := r.Upstreams["oc"]
	if len(oc.Protocols) != 1 || oc.Protocols[0] != "chat" || len(oc.InputModalities) != 2 {
		t.Fatalf("oc spec = %#v", oc)
	}
}

// TestTextOnlyRouteNeedsNoImageCandidate pins that capability declarations do
// not impose a new demand: a text-only route with no image-capable candidate is
// valid (no arbitrary "require an image candidate" rule).
func TestTextOnlyRouteNeedsNoImageCandidate(t *testing.T) {
	_, err := loadRoutes(t, `
routes:
  - name: r
    models: [m]
    interactive: [cx, ol]
    upstreams:
      cx: {protocols: [responses], input_modalities: [text]}
      ol: {protocols: [chat], input_modalities: [text]}
`)
	if err != nil {
		t.Fatalf("text-only opted-in route must load: %v", err)
	}
}

// TestUpstreamsRequiresDescriptorForEveryCandidate: every candidate in the
// union of interactive and background must be described.
func TestUpstreamsRequiresDescriptorForEveryCandidate(t *testing.T) {
	_, err := loadRoutes(t, `
routes:
  - name: r
    models: [m]
    interactive: [cx, ol]
    background: [oc]
    upstreams:
      cx: {protocols: [responses], input_modalities: [text]}
      ol: {protocols: [responses], input_modalities: [text]}
`)
	if err == nil || !strings.Contains(err.Error(), "missing a descriptor for candidate \"oc\"") {
		t.Fatalf("want missing-descriptor error for background-only candidate, got %v", err)
	}
}

// TestUpstreamsRejectsUnknownKey: a key that is not a candidate is a typo, not
// a silent no-op.
func TestUpstreamsRejectsUnknownKey(t *testing.T) {
	_, err := loadRoutes(t, `
routes:
  - name: r
    models: [m]
    interactive: [cx]
    upstreams:
      cx: {protocols: [responses], input_modalities: [text]}
      typo: {protocols: [responses], input_modalities: [text]}
`)
	if err == nil || !strings.Contains(err.Error(), "unknown candidate account \"typo\"") {
		t.Fatalf("want unknown-key error, got %v", err)
	}
}

func TestUpstreamsProtocolErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		want string
	}{
		{"empty", `{input_modalities: [text]}`, "protocols must be a non-empty"},
		{"invalid", `{protocols: [bogus], input_modalities: [text]}`, "invalid protocol \"bogus\""},
		{"duplicate", `{protocols: [chat, chat], input_modalities: [text]}`, "duplicate protocol \"chat\""},
		{"codex chat", `{protocols: [responses, chat], input_modalities: [text]}`, "codex accounts cannot serve chat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadRoutes(t, `
routes:
  - name: r
    models: [m]
    interactive: [cx]
    upstreams:
      cx: `+tc.spec+`
`)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestUpstreamsModalityErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		want string
	}{
		{"empty", `{protocols: [responses]}`, "input_modalities must be a non-empty"},
		{"invalid", `{protocols: [responses], input_modalities: [audio]}`, "invalid input modality \"audio\""},
		{"duplicate", `{protocols: [responses], input_modalities: [text, text]}`, "duplicate input modality \"text\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadRoutes(t, `
routes:
  - name: r
    models: [m]
    interactive: [ol]
    upstreams:
      ol: `+tc.spec+`
`)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// TestUpstreamsNonCodexChatAllowed: chat on a non-codex provider is legal.
func TestUpstreamsNonCodexChatAllowed(t *testing.T) {
	_, err := loadRoutes(t, `
routes:
  - name: r
    models: [m]
    interactive: [ol]
    upstreams:
      ol: {protocols: [chat], input_modalities: [text]}
`)
	if err != nil {
		t.Fatalf("chat on ollama must be allowed: %v", err)
	}
}

// TestUpstreamsWhitespaceTrimmed: surrounding whitespace on the new string
// fields is trimmed and the normalized values reach core.Route.
func TestUpstreamsWhitespaceTrimmed(t *testing.T) {
	c, err := loadRoutes(t, `
routes:
  - name: r
    models: [m]
    interactive: [ol]
    upstreams:
      ol:
        upstream_model: "  backend-9  "
        protocols: [" chat "]
        input_modalities: [" text "]
`)
	if err != nil {
		t.Fatalf("whitespace-padded values must normalize: %v", err)
	}
	s := c.CoreRoutes()[0].Upstreams["ol"]
	if s.UpstreamModel != "backend-9" || s.Protocols[0] != "chat" || s.InputModalities[0] != "text" {
		t.Fatalf("not normalized: %#v", s)
	}
}

// TestUpstreamsDeepCopy: CoreRoutes must return freshly allocated maps and
// slices so a caller can never mutate the loaded config (or another call's
// result) through the projection.
func TestUpstreamsDeepCopy(t *testing.T) {
	c, err := loadRoutes(t, `
routes:
  - name: r
    models: [m]
    interactive: [cx, ol]
    upstreams:
      cx: {protocols: [responses], input_modalities: [text]}
      ol: {protocols: [chat], input_modalities: [text]}
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r1 := c.CoreRoutes()[0]
	// Mutate through the projection: slices, map entries and route slices.
	r1.Interactive[0] = "MUT"
	r1.Models[0] = "MUT"
	spec := r1.Upstreams["cx"]
	spec.Protocols[0] = "MUT"
	r1.Upstreams["cx"] = spec
	delete(r1.Upstreams, "ol")

	// The loaded config is untouched.
	if c.Routes[0].Upstreams["cx"].Protocols[0] != "responses" {
		t.Fatalf("config descriptor aliased: %#v", c.Routes[0].Upstreams["cx"])
	}
	if _, ok := c.Routes[0].Upstreams["ol"]; !ok {
		t.Fatal("config descriptor map aliased")
	}
	if c.Routes[0].Interactive[0] != "cx" || c.Routes[0].Models[0] != "m" {
		t.Fatal("config slice aliased")
	}

	// A fresh projection is pristine.
	r2 := c.CoreRoutes()[0]
	if r2.Interactive[0] != "cx" || r2.Models[0] != "m" ||
		r2.Upstreams["cx"].Protocols[0] != "responses" || len(r2.Upstreams) != 2 {
		t.Fatalf("projection aliased across calls: %#v", r2)
	}
}

// TestUpstreamsUnknownFieldRejected: KnownFields(true) makes the new descriptor
// strict too.
func TestUpstreamsUnknownFieldRejected(t *testing.T) {
	_, err := loadRoutes(t, `
routes:
  - name: r
    models: [m]
    interactive: [ol]
    upstreams:
      ol: {protocols: [chat], input_modalities: [text], vision: true}
`)
	if err == nil {
		t.Fatal("unknown upstreams field must be rejected")
	}
}

// TestRequestRecordUpstreamModelAdditive guards the additive ledger field: it
// is optional on the wire and does not displace the client-facing Model.
func TestRequestRecordUpstreamModelAdditive(t *testing.T) {
	r := core.RequestRecord{Model: "gpt-alias", UpstreamModel: "backend-9"}
	if r.Model != "gpt-alias" || r.UpstreamModel != "backend-9" {
		t.Fatalf("RequestRecord.UpstreamModel not additive: %#v", r)
	}
}
