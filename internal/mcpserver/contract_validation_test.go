package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file is an independent, execution-level check of the adapter's
// *advertised* output contract. For every tool it asks a live MCP session what
// OutputSchema the server itself advertises, compiles that exact schema with
// github.com/google/jsonschema-go and validates the structuredContent the real
// server actually emitted over the wire. The schema is not copied into the test
// and the document is not hand-written to match: both come from production
// code, so a result that contradicts its own advertised schema fails here.
//
// The JSON text copy is deliberately NOT what is validated: MCP hosts read
// structuredContent, and the two copies are separately asserted equal inside
// harness.call. Validating the text alone would miss a mismatch between the
// advertised schema and the structured payload hosts actually consume.

// advertisedOutputResolved compiles the OutputSchema a live ListTools call
// advertises for one tool. Marshaling the SDK's decoded schema back to JSON and
// re-parsing it with jsonschema-go means the test cannot drift from the schema
// the server ships.
func advertisedOutputResolved(t *testing.T, session *mcp.ClientSession, tool string) *jsonschema.Resolved {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tl := range res.Tools {
		if tl.Name != tool {
			continue
		}
		if tl.OutputSchema == nil {
			t.Fatalf("%s: no output schema advertised", tool)
		}
		raw, err := json.Marshal(tl.OutputSchema)
		if err != nil {
			t.Fatalf("%s: advertised output schema does not marshal: %v", tool, err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: advertised output schema is not a JSON Schema: %v\n%s", tool, err, raw)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatalf("%s: advertised output schema does not resolve: %v", tool, err)
		}
		return resolved
	}
	t.Fatalf("tool %s is not advertised", tool)
	return nil
}

// conformResult validates the structuredContent the server emitted against the
// tool's advertised output schema. It fails when the validator itself errors
// (an unusable schema) or when the structured content does not validate.
func conformResult(t *testing.T, schema *jsonschema.Resolved, tool string, res *mcp.CallToolResult, doc map[string]any) {
	t.Helper()
	if res.StructuredContent == nil {
		t.Fatalf("%s: result carries no structuredContent to validate", tool)
	}
	if err := schema.Validate(res.StructuredContent); err != nil {
		b, _ := json.Marshal(doc)
		t.Fatalf("%s: structuredContent violates the advertised output schema: %v\ndocument: %s", tool, err, b)
	}
}

// jsonBody serves a fixed body as application/json.
func jsonBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { jsonResp(w, body) }
}

// A fat, realistic account whose upstream object carries unknown fields, an
// explicit upstream error and a stale flag. The adapter passes these through
// verbatim, so the advertised schema must accept them.
const contractFatAccount = `{"id":"primary","provider":"codex","healthy":false,"cooldown_until":"2026-10-16T13:00:00Z",` +
	`"inflight":3,"reserve":{"weekly":0.4,"daily":null},"windows":[` +
	`{"kind":"weekly","used_frac":0.5,"remaining_frac":0.5,"reset_at":"2026-10-20T00:00:00Z","window_seconds":604800,"rolled":false},` +
	`{"kind":"5h","used_frac":0.1,"remaining_frac":0.9,"reset_at":null,"window_seconds":18000,"rolled":true}],` +
	`"background_admissible":false,"interactive_admissible":true,"snapshot_age_s":412,"stale":true,` +
	`"error":{"code":"quota_fetch_failed","message":"upstream quota probe timed out"},` +
	`"vendor_extension":{"unexpected":true}}`

const contractFatStatus = `{"schema_version":1,"now":"2026-10-16T12:00:00Z","accounts":[` + contractFatAccount + `],` +
	`"clients":[{"name":"codex","class":"background","version":"0.9.1","extra":{"x":1}}]}`

const contractUsageRows = `{"schema_version":1,"since":"2026-10-15T12:00:00Z","group":"model","rows":[` +
	`{"key":"gpt-5","requests":9,"input_tokens":100,"cached_input_tokens":10,"cache_creation_input_tokens":5,` +
	`"output_tokens":40,"reasoning_tokens":7,"cost_usd":null,"unknown_usage_requests":3,"unpriced_requests":4,` +
	`"vendor_extension":{"kept":true}}]}`

// ---- success results -----------------------------------------------------

func TestContractOutputSchemasAcceptAllRealSuccessShapes(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		handler http.HandlerFunc
		args    any
	}{
		{
			name:    "quota_status/stale_error_and_unknown_upstream_fields",
			tool:    "quota_status",
			handler: jsonBody(contractFatStatus),
			args:    map[string]any{},
		},
		{
			name:    "quota_status/no_upstream_schema_version",
			tool:    "quota_status",
			handler: jsonBody(`{"now":"2026-10-16T12:00:00Z","accounts":[` + oneAccount + `],"clients":[]}`),
			args:    map[string]any{},
		},
		{
			name:    "quota_status/explicit_null_upstream_schema_version",
			tool:    "quota_status",
			handler: jsonBody(`{"schema_version":null,"now":"2026-10-16T12:00:00Z","accounts":[],"clients":[]}`),
			args:    map[string]any{},
		},
		{
			name:    "quota_status/empty_arrays_and_no_now",
			tool:    "quota_status",
			handler: jsonBody(`{"schema_version":1,"accounts":[],"clients":[]}`),
			args:    map[string]any{},
		},
		{
			name:    "usage_summary/unpriced_and_unmetered_rows",
			tool:    "usage_summary",
			handler: jsonBody(contractUsageRows),
			args:    map[string]any{},
		},
		{
			name:    "usage_summary/no_upstream_schema_version_or_since",
			tool:    "usage_summary",
			handler: jsonBody(`{"rows":[{"key":"gpt-5","unpriced_requests":1}]}`),
			args:    map[string]any{},
		},
		{
			name:    "check_admission/deny_is_a_decision",
			tool:    "check_admission",
			handler: jsonBody(`{"decision":"deny","account_id":"","reason":"reserve"}`),
			args:    map[string]any{"model": "gpt-5"},
		},
		{
			name:    "check_admission/allow_with_budget_object",
			tool:    "check_admission",
			handler: jsonBody(`{"decision":"allow","account_id":"primary","reason":"admitted","budget":{"estimated_usd":0.5,"note":"advisory","vendor":{"k":"v"}}}`),
			args:    map[string]any{"account": "primary", "class": "interactive"},
		},
		{
			name:    "check_admission/allow_with_upstream_schema_version",
			tool:    "check_admission",
			handler: jsonBody(`{"schema_version":2,"decision":"allow","account_id":"primary","reason":"admitted"}`),
			args:    map[string]any{"model": "gpt-5"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.handler, Config{})
			schema := advertisedOutputResolved(t, h.session, tc.tool)
			res, doc := h.call(tc.tool, tc.args)
			if res.IsError {
				t.Fatalf("%s: unexpected error result: %v", tc.tool, doc)
			}
			conformResult(t, schema, tc.tool, res, doc)
		})
	}
}

// ---- error results -------------------------------------------------------

func TestContractOutputSchemasAcceptAllRealErrorShapes(t *testing.T) {
	hugeReason := strings.Repeat("r", 8000)
	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		jsonResp(w, statusDoc(""))
	}
	capRefusal := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		jsonResp(w, `{"error":{"message":"localrouter: no account supports the capabilities this request needs","type":"capability_unsupported"}}`)
	}
	unauthorized := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		jsonResp(w, `{"error":{"message":"invalid client key"}}`)
	}
	unavailable := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		jsonResp(w, `{"error":{"message":"ledger unavailable"}}`)
	}

	cases := []struct {
		name    string
		tool    string
		cfg     Config
		handler http.HandlerFunc
		raw     string
		args    any
		code    ErrorCode
	}{
		{name: "invalid_arguments/unknown_field", tool: "usage_summary",
			handler: jsonBody(`{"schema_version":1,"rows":[]}`),
			raw:     `{"account":"primary"}`, code: CodeInvalidArguments},
		{name: "invalid_arguments/no_admission_target", tool: "check_admission",
			handler: jsonBody(`{"decision":"allow","account_id":"primary","reason":"admitted"}`),
			raw:     `{}`, code: CodeInvalidArguments},
		{name: "not_found/unknown_account", tool: "quota_status",
			handler: jsonBody(statusDoc(oneAccount)),
			args:    map[string]any{"account": "ghost"}, code: CodeNotFound},
		{name: "bad_response/malformed_upstream_body", tool: "quota_status",
			handler: jsonBody(`not json at all`),
			args:    map[string]any{}, code: CodeBadResponse},
		{name: "bad_response/upstream_schema_version_not_numeric", tool: "quota_status",
			handler: jsonBody(`{"schema_version":"1","accounts":[]}`),
			args:    map[string]any{}, code: CodeBadResponse},
		{name: "bad_response/no_allow_deny_decision", tool: "check_admission",
			handler: jsonBody(`{"verdict":"maybe"}`),
			args:    map[string]any{"model": "gpt-5"}, code: CodeBadResponse},
		{name: "endpoint_unavailable/ledger", tool: "usage_summary",
			handler: unavailable,
			args:    map[string]any{}, code: CodeEndpointUnavailable},
		{name: "auth_required", tool: "quota_status",
			handler: unauthorized,
			args:    map[string]any{}, code: CodeAuthRequired},
		{name: "capability_unsupported", tool: "check_admission",
			handler: capRefusal,
			args:    map[string]any{"model": "gpt-5"}, code: CodeCapabilityUnsupported},
		{name: "server_timeout", tool: "quota_status",
			cfg:     Config{Timeout: 80 * time.Millisecond},
			handler: slow,
			args:    map[string]any{}, code: CodeServerTimeout},
		{name: "bad_response/admission_over_result_budget", tool: "check_admission",
			cfg:     Config{MaxResultChars: MinMaxResultChars},
			handler: jsonBody(fmt.Sprintf(`{"decision":"allow","account_id":"primary","reason":%q}`, hugeReason)),
			args:    map[string]any{"model": "gpt-5"}, code: CodeBadResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.handler, tc.cfg)
			schema := advertisedOutputResolved(t, h.session, tc.tool)
			var res *mcp.CallToolResult
			var doc map[string]any
			if tc.raw != "" {
				res, doc = h.rawCall(tc.tool, tc.raw)
			} else {
				res, doc = h.call(tc.tool, tc.args)
			}
			if !res.IsError {
				t.Fatalf("%s: want an error result, got success: %v", tc.tool, doc)
			}
			if got := h.errorCode(doc); got != string(tc.code) {
				t.Fatalf("%s: error code = %q, want %q (doc %v)", tc.tool, got, tc.code, doc)
			}
			conformResult(t, schema, tc.tool, res, doc)
		})
	}
}

// ---- truncation and byte bounds -----------------------------------------

func TestContractOutputSchemasHoldUnderTruncationAndByteBounds(t *testing.T) {
	t.Run("quota_status/array_truncated_to_budget", func(t *testing.T) {
		const budget = 8192
		h := newHarness(t, jsonBody(statusDoc(bigAccounts(80, 60))), Config{MaxResultChars: budget})
		schema := advertisedOutputResolved(t, h.session, "quota_status")
		res, doc := h.call("quota_status", map[string]any{})
		if res.IsError {
			t.Fatalf("unexpected error result: %v", doc)
		}
		conformResult(t, schema, "quota_status", res, doc)
		retrieval, _ := doc["retrieval"].(map[string]any)
		if retrieval["truncated"] != true {
			t.Fatalf("a bounded result must report truncation: %v", retrieval)
		}
		if retrieval["max_result_chars"] != float64(budget) {
			t.Fatalf("max_result_chars = %v, want %d", retrieval["max_result_chars"], budget)
		}
	})

	t.Run("usage_summary/rows_truncated_to_byte_cap", func(t *testing.T) {
		const budget = 4096
		var rows []string
		for i := 0; i < 40; i++ {
			rows = append(rows, fmt.Sprintf(`{"key":"model-%03d","requests":1,"cost_usd":null,"unpriced_requests":1,"unknown_usage_requests":0,"pad":%q}`,
				i, strings.Repeat("y", 120)))
		}
		h := newHarness(t, jsonBody(`{"schema_version":1,"since":"2026-10-15T12:00:00Z","group":"model","rows":[`+strings.Join(rows, ",")+`]}`),
			Config{MaxResultChars: budget})
		schema := advertisedOutputResolved(t, h.session, "usage_summary")
		res, doc := h.call("usage_summary", map[string]any{"group": "model", "limit": 100})
		if res.IsError {
			t.Fatalf("unexpected error result: %v", doc)
		}
		conformResult(t, schema, "usage_summary", res, doc)
		counts, _ := doc["counts"].(map[string]any)
		if counts["rows_truncated"] != true {
			t.Fatalf("a byte-capped row set must set rows_truncated=true: %v", counts)
		}
	})

	t.Run("quota_status/bounded_error_result", func(t *testing.T) {
		h := newHarness(t, jsonBody(statusDoc(oneAccount)), Config{MaxResultChars: MinMaxResultChars})
		schema := advertisedOutputResolved(t, h.session, "quota_status")
		res, doc := h.call("quota_status", map[string]any{"account": strings.Repeat("x", 8000)})
		if !res.IsError {
			t.Fatalf("oversized unknown account must be a bounded error: %v", doc)
		}
		conformResult(t, schema, "quota_status", res, doc)
	})
}

// ---- the validator is really enforcing the schema ------------------------

func TestContractOutputSchemasRejectTamperedContent(t *testing.T) {
	// A schema that accepted everything would make the tests above worthless.
	// Deleting a required block, or breaking an enum, must be rejected by the
	// very same validator path the conformance tests use.

	clone := func(t *testing.T, doc map[string]any) map[string]any {
		t.Helper()
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("quota_status/missing_counts", func(t *testing.T) {
		h := newHarness(t, jsonBody(statusDoc(oneAccount)), Config{})
		schema := advertisedOutputResolved(t, h.session, "quota_status")
		res, doc := h.call("quota_status", map[string]any{})
		conformResult(t, schema, "quota_status", res, doc) // real output is accepted

		tampered := clone(t, doc)
		delete(tampered, "counts")
		if err := schema.Validate(tampered); err == nil {
			t.Fatal("the advertised schema accepted a success document with no counts block")
		}
	})

	t.Run("check_admission/decision_out_of_enum", func(t *testing.T) {
		h := newHarness(t, jsonBody(`{"decision":"deny","reason":"reserve"}`), Config{})
		schema := advertisedOutputResolved(t, h.session, "check_admission")
		res, doc := h.call("check_admission", map[string]any{"model": "gpt-5"})
		conformResult(t, schema, "check_admission", res, doc)

		tampered := clone(t, doc)
		tampered["decision"] = "maybe"
		if err := schema.Validate(tampered); err == nil {
			t.Fatal("the advertised schema accepted an out-of-enum decision")
		}
	})

	t.Run("usage_summary/error_envelope_missing_code", func(t *testing.T) {
		h := newHarness(t, jsonBody(`{"schema_version":1,"rows":[]}`), Config{})
		schema := advertisedOutputResolved(t, h.session, "usage_summary")
		res, doc := h.call("usage_summary", map[string]any{})
		conformResult(t, schema, "usage_summary", res, doc)

		tampered := clone(t, doc)
		tampered["rows"] = "not-an-array"
		if err := schema.Validate(tampered); err == nil {
			t.Fatal("the advertised schema accepted a non-array rows field")
		}
	})
}

// ---- adapter import boundary --------------------------------------------

// adapterModulePath is the router's own module. The thin adapter must not
// depend on any of its packages, only on the MCP SDK and the standard library.
const adapterModulePath = "github.com/hpst3r/localrouter"

// adapterDeniedSegments are the router subsystems the read-only adapter must
// never reach into. Matching is per path *segment*, so the module path
// github.com/hpst3r/localrouter (whose last segment "localrouter" merely
// contains the substring "router") is not a false positive.
var adapterDeniedSegments = []string{"app", "ledger", "auth", "config", "router", "provider"}

// adapterDeniedImports are concrete non-stdlib dependencies of the router itself
// (its SQL ledger driver, YAML config loader and OAuth stack) that a thin,
// read-only control-API adapter has no business importing, plus the test-only
// httptest helper.
var adapterDeniedImports = []string{
	"database/sql",
	"modernc.org/sqlite",
	"gopkg.in/yaml.v3",
	"golang.org/x/oauth2",
	"net/http/httptest",
}

func TestContractAdapterImportBoundary(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	pkg, ok := pkgs["mcpserver"]
	if !ok {
		t.Fatalf("package mcpserver not found in the parsed directory")
	}
	if len(pkg.Files) == 0 {
		t.Fatal("no production source was parsed")
	}

	osImporters := map[string]bool{}
	imported := map[string]bool{}
	for filename, file := range pkg.Files {
		base := filepath.Base(filename)
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unreadable import path %s", base, spec.Path.Value)
			}
			imported[path] = true
			for _, seg := range strings.Split(path, "/") {
				for _, denied := range adapterDeniedSegments {
					if seg == denied {
						t.Errorf("%s imports %q: the adapter must not depend on the %q subsystem", base, path, denied)
					}
				}
			}
			for _, denied := range adapterDeniedImports {
				if path == denied || strings.HasPrefix(path, denied+"/") {
					t.Errorf("%s imports %q: the adapter must not depend on the router's %q stack", base, path, denied)
				}
			}
			if path == adapterModulePath || strings.HasPrefix(path, adapterModulePath+"/") {
				t.Errorf("%s imports %q: the adapter must be independent of the router's own packages", base, path)
			}
			if path == "os" {
				osImporters[base] = true
			}
		}
	}

	// The only filesystem access the adapter is allowed is reading the client
	// key file, which lives in config.go. Any other production file importing
	// os would widen that surface.
	if !osImporters["config.go"] {
		t.Error("config.go must import os to read the client key file")
	}
	for base := range osImporters {
		if base != "config.go" {
			t.Errorf("%s imports os: filesystem access must stay confined to the key-file reader in config.go", base)
		}
	}

	// A minimal positive assertion that the scan saw the real production
	// dependencies, so a parse that silently found nothing cannot pass.
	if !imported["net/http"] || !imported["github.com/modelcontextprotocol/go-sdk/mcp"] {
		t.Errorf("expected the control-API HTTP client and MCP SDK imports, saw %v", imported)
	}
}
