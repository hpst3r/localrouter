package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// wireLen is the exact size of the CallToolResult the server puts on the wire,
// reconstructed with the same SDK types and the same two-copy doc that
// production emits (JSON text content plus structured content). The result
// budget is enforced against this number, not against the bare document.
func wireLen(t *testing.T, doc string, isError bool) int {
	t.Helper()
	b, err := json.Marshal(&mcp.CallToolResult{
		IsError:           isError,
		Content:           []mcp.Content{&mcp.TextContent{Text: doc}},
		StructuredContent: json.RawMessage(doc),
	})
	if err != nil {
		t.Fatalf("marshal wire result: %v", err)
	}
	return len(b)
}

// textOf returns the single text content of a result.
func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("want one content item, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T", res.Content[0])
	}
	return tc.Text
}

func bigAccounts(n, pad int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"acct-%03d","provider":"codex","reserve":{"weekly":0.5},`+
			`"windows":[],"pad":%q,"stale":false}`, i, strings.Repeat("x", pad))
	}
	return b.String()
}

// ---- serialized-result budget --------------------------------------------

func TestResultBudgetBoundsTheSerializedCallToolResult(t *testing.T) {
	const budget = 8192
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, statusDoc(bigAccounts(80, 60)))
	}, Config{MaxResultChars: budget})

	res, doc := h.call("quota_status", map[string]any{})
	text := textOf(t, res)
	got := wireLen(t, text, res.IsError)
	if got > budget {
		t.Fatalf("serialized CallToolResult is %d bytes, over the %d budget (text doc %d bytes)", got, budget, len(text))
	}
	// The budget must actually bite: 80 fat accounts cannot fit.
	accounts, _ := doc["accounts"].([]any)
	if len(accounts) == 0 || len(accounts) >= 80 {
		t.Fatalf("accounts kept = %d, want a strict bounded prefix", len(accounts))
	}
	retrieval, _ := doc["retrieval"].(map[string]any)
	if retrieval["truncated"] != true || retrieval["dropped_items"].(float64) <= 0 {
		t.Fatalf("retrieval must report the truncation: %v", retrieval)
	}
	if retrieval["max_result_chars"] != float64(budget) {
		t.Fatalf("max_result_chars = %v, want %d", retrieval["max_result_chars"], budget)
	}
	counts, _ := doc["counts"].(map[string]any)
	if counts["accounts_total"] != float64(80) || counts["accounts_returned"] != float64(len(accounts)) {
		t.Fatalf("counts not honest: %v", counts)
	}
}

func TestResultBudgetAtMinimumStaysHonestOrErrors(t *testing.T) {
	const budget = MinMaxResultChars
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, statusDoc(bigAccounts(80, 60)))
	}, Config{MaxResultChars: budget})
	res, doc := h.call("quota_status", map[string]any{})
	text := textOf(t, res)
	if got := wireLen(t, text, res.IsError); got > budget {
		t.Fatalf("serialized result is %d bytes, over the %d minimum budget", got, budget)
	}
	if res.IsError {
		if h.errorCode(doc) != string(CodeBadResponse) {
			t.Fatalf("over-budget error code = %q, want bad_response", h.errorCode(doc))
		}
		return
	}
	retrieval, _ := doc["retrieval"].(map[string]any)
	if retrieval["truncated"] != true {
		t.Fatalf("a bounded result must be marked truncated: %v", retrieval)
	}
	counts, _ := doc["counts"].(map[string]any)
	accounts, _ := doc["accounts"].([]any)
	if counts["accounts_returned"] != float64(len(accounts)) || counts["accounts_total"] != float64(80) {
		t.Fatalf("counts not honest at minimum budget: %v", counts)
	}
}

func TestUsageSummaryByteCapSetsRowsTruncated(t *testing.T) {
	const budget = 4096
	var rows []string
	for i := 0; i < 40; i++ {
		rows = append(rows, fmt.Sprintf(`{"key":"model-%03d","requests":1,"cost_usd":null,"unpriced_requests":1,"unknown_usage_requests":0,"pad":%q}`,
			i, strings.Repeat("y", 120)))
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"schema_version":1,"since":"2026-10-15T12:00:00Z","group":"model","rows":[`+strings.Join(rows, ",")+`]}`)
	}, Config{MaxResultChars: budget})

	res, doc := h.call("usage_summary", map[string]any{"group": "model", "limit": 100})
	text := textOf(t, res)
	if got := wireLen(t, text, res.IsError); got > budget {
		t.Fatalf("serialized result is %d bytes, over the %d budget", got, budget)
	}
	got, _ := doc["rows"].([]any)
	if len(got) == 0 || len(got) >= 40 {
		t.Fatalf("rows kept = %d, want a strict bounded prefix", len(got))
	}
	counts, _ := doc["counts"].(map[string]any)
	if counts["rows_total"] != float64(40) {
		t.Fatalf("rows_total = %v, want the real total 40", counts["rows_total"])
	}
	if counts["rows_truncated"] != true {
		t.Fatalf("a byte-capped row set must set rows_truncated=true: %v", counts)
	}
	if counts["rows_returned"] != float64(len(got)) {
		t.Fatalf("rows_returned = %v, want %d", counts["rows_returned"], len(got))
	}
	retrieval, _ := doc["retrieval"].(map[string]any)
	if retrieval["dropped_items"] != float64(40-len(got)) {
		t.Fatalf("dropped_items = %v, want %d", retrieval["dropped_items"], 40-len(got))
	}
}

// ---- error results are bounded too --------------------------------------

func TestErrorResultsAreBoundedForHugeInputStrings(t *testing.T) {
	const budget = MinMaxResultChars
	huge := strings.Repeat("x", 8000)

	usageBody := `{"schema_version":1,"rows":[]}`
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, usageBody) }, Config{MaxResultChars: budget})

	t.Run("unknown argument", func(t *testing.T) {
		res, doc := h.rawCall("usage_summary", fmt.Sprintf(`{%q:1}`, huge))
		if !res.IsError || h.errorCode(doc) != string(CodeInvalidArguments) {
			t.Fatalf("isError=%v code=%q, want invalid_arguments", res.IsError, h.errorCode(doc))
		}
		if got := wireLen(t, textOf(t, res), true); got > budget {
			t.Fatalf("error result is %d bytes, over the %d budget", got, budget)
		}
	})

	t.Run("unknown account", func(t *testing.T) {
		hs := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, statusDoc(oneAccount)) }, Config{MaxResultChars: budget})
		res, doc := hs.call("quota_status", map[string]any{"account": huge})
		if !res.IsError || hs.errorCode(doc) != string(CodeNotFound) {
			t.Fatalf("isError=%v code=%q, want not_found", res.IsError, hs.errorCode(doc))
		}
		if got := wireLen(t, textOf(t, res), true); got > budget {
			t.Fatalf("error result is %d bytes, over the %d budget", got, budget)
		}
	})
}

func TestCheckAdmissionOverBudgetYieldsABoundedError(t *testing.T) {
	const budget = MinMaxResultChars
	hugeReason := strings.Repeat("r", 8000)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, fmt.Sprintf(`{"decision":"allow","account_id":"primary","reason":%q}`, hugeReason))
	}, Config{MaxResultChars: budget})

	res, doc := h.call("check_admission", map[string]any{"model": "gpt-5"})
	if !res.IsError {
		t.Fatalf("an admission result that cannot fit must be a bounded error, not a silently truncated decision: %v", doc)
	}
	if got := wireLen(t, textOf(t, res), true); got > budget {
		t.Fatalf("error result is %d bytes, over the %d budget", got, budget)
	}
	// A normal-sized decision still comes back as a decision.
	h2 := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"decision":"allow","account_id":"primary","reason":"admitted"}`)
	}, Config{MaxResultChars: budget})
	res2, doc2 := h2.call("check_admission", map[string]any{"model": "gpt-5"})
	if res2.IsError || doc2["decision"] != "allow" {
		t.Fatalf("a small admission result must still succeed: %v", doc2)
	}
}

// ---- output schemas ------------------------------------------------------

func toolSchema(t *testing.T, name string) (input, output map[string]any) {
	t.Helper()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, statusDoc("")) }, Config{})
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != name {
			continue
		}
		in, _ := tool.InputSchema.(map[string]any)
		out, _ := tool.OutputSchema.(map[string]any)
		return in, out
	}
	t.Fatalf("tool %s not found", name)
	return nil, nil
}

func TestToolsExposeOutputSchemasCoveringResultsAndErrorEnvelopes(t *testing.T) {
	for _, name := range []string{"quota_status", "usage_summary", "check_admission"} {
		t.Run(name, func(t *testing.T) {
			_, out := toolSchema(t, name)
			if out == nil {
				t.Fatalf("%s has no output schema", name)
			}
			if out["type"] != "object" {
				t.Fatalf("%s output schema type = %v, want object", name, out["type"])
			}
			props, _ := out["properties"].(map[string]any)
			if props == nil {
				t.Fatalf("%s output schema has no properties", name)
			}
			// Required: retrieval/source/counts on success, error on failure.
			anyOf, _ := out["anyOf"].([]any)
			var sawSuccess, sawError bool
			for _, alt := range anyOf {
				m, _ := alt.(map[string]any)
				req, _ := m["required"].([]any)
				for _, r := range req {
					switch r {
					case "retrieval":
						sawSuccess = true
					case "error":
						sawError = true
					}
				}
			}
			if !sawSuccess || !sawError {
				t.Fatalf("%s output schema must accept both a result and an error envelope: %v", name, out["anyOf"])
			}
			if _, ok := props["error"]; !ok {
				t.Fatalf("%s output schema must describe the error envelope", name)
			}
			for _, field := range []string{"retrieval", "source"} {
				obj, _ := props[field].(map[string]any)
				if obj == nil {
					t.Fatalf("%s output schema: %s not described", name, field)
				}
				if _, ok := obj["required"].([]any); !ok {
					t.Fatalf("%s output schema: %s must declare required fields", name, field)
				}
			}
			if name != "check_admission" {
				obj, _ := props["counts"].(map[string]any)
				if obj == nil {
					t.Fatalf("%s output schema: counts not described", name)
				}
				if _, ok := obj["required"].([]any); !ok {
					t.Fatalf("%s output schema: counts must declare required fields", name)
				}
			}
			// Upstream opaque objects must preserve unknown fields.
			assertOpaqueArray := func(field string) {
				arr, _ := props[field].(map[string]any)
				if arr == nil || arr["type"] != "array" {
					t.Fatalf("%s output schema: %s must be an array, got %v", name, field, arr)
				}
				items, _ := arr["items"].(map[string]any)
				if items == nil || items["type"] != "object" {
					t.Fatalf("%s output schema: %s items must be objects", name, field)
				}
				if items["additionalProperties"] != true {
					t.Fatalf("%s output schema: %s items must permit opaque upstream fields", name, field)
				}
			}
			switch name {
			case "quota_status":
				assertOpaqueArray("accounts")
				assertOpaqueArray("clients")
			case "usage_summary":
				assertOpaqueArray("rows")
			case "check_admission":
				budget, _ := props["budget"].(map[string]any)
				if budget == nil || budget["type"] != "object" || budget["additionalProperties"] != true {
					t.Fatalf("check_admission budget must be an opaque object: %v", budget)
				}
			}
		})
	}
}

func TestCheckAdmissionInputSchemaEncodesExactlyOneTarget(t *testing.T) {
	in, _ := toolSchema(t, "check_admission")
	if in == nil {
		t.Fatal("check_admission has no input schema")
	}
	oneOf, _ := in["oneOf"].([]any)
	if len(oneOf) != 2 {
		t.Fatalf("check_admission input schema must encode exactly-one(account, model), got oneOf=%v", in["oneOf"])
	}
	seen := map[string]bool{}
	for _, alt := range oneOf {
		m, _ := alt.(map[string]any)
		req, _ := m["required"].([]any)
		for _, r := range req {
			s, _ := r.(string)
			seen[s] = true
		}
	}
	if !seen["account"] || !seen["model"] {
		t.Fatalf("check_admission oneOf must require account and model alternatives, got %v", in["oneOf"])
	}
}

// ---- upstream strictness -------------------------------------------------

func TestQuotaStatusDoesNotInventSchemaVersion(t *testing.T) {
	noVersion := `{"now":"2026-10-16T12:00:00Z","accounts":[` + oneAccount + `],"clients":[]}`
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, noVersion) }, Config{})
	_, doc := h.call("quota_status", map[string]any{})
	source, _ := doc["source"].(map[string]any)
	if v, ok := source["upstream_schema_version"]; !ok || v != nil {
		t.Fatalf("a missing upstream schema_version must be reported as null, got %v", source)
	}

	// A reported version is preserved verbatim.
	h2 := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"schema_version":2,"now":"2026-10-16T12:00:00Z","accounts":[`+oneAccount+`],"clients":[]}`)
	}, Config{})
	_, doc2 := h2.call("quota_status", map[string]any{})
	source2, _ := doc2["source"].(map[string]any)
	if source2["upstream_schema_version"] != float64(2) {
		t.Fatalf("upstream_schema_version = %v, want 2", source2["upstream_schema_version"])
	}
}

func TestUpstreamMalformedEnvelopesRejected(t *testing.T) {
	cases := map[string]string{
		"status schema_version string": `{"schema_version":"1","now":"2026-10-16T12:00:00Z","accounts":[],"clients":[]}`,
		"status schema_version bool":   `{"schema_version":true,"now":"2026-10-16T12:00:00Z","accounts":[],"clients":[]}`,
		"status schema_version zero":   `{"schema_version":0,"now":"2026-10-16T12:00:00Z","accounts":[],"clients":[]}`,
		"status accounts null":         `{"schema_version":1,"now":"2026-10-16T12:00:00Z","accounts":null,"clients":[]}`,
		"status clients null":          `{"schema_version":1,"now":"2026-10-16T12:00:00Z","accounts":[],"clients":null}`,
		"status element null":          `{"schema_version":1,"now":"2026-10-16T12:00:00Z","accounts":[null],"clients":[]}`,
		"status now non-string":        `{"schema_version":1,"now":1697467200,"accounts":[],"clients":[]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, body) }, Config{})
			res, doc := h.call("quota_status", map[string]any{})
			if !res.IsError || h.errorCode(doc) != string(CodeBadResponse) {
				t.Fatalf("isError=%v code=%q, want bad_response for %s", res.IsError, h.errorCode(doc), name)
			}
		})
	}

	usageCases := map[string]string{
		"usage rows null":          `{"schema_version":1,"rows":null}`,
		"usage element null":       `{"schema_version":1,"rows":[null]}`,
		"usage since non-string":   `{"schema_version":1,"since":1697467200,"rows":[]}`,
		"usage schema_version str": `{"schema_version":"1","rows":[]}`,
	}
	for name, body := range usageCases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, body) }, Config{})
			res, doc := h.call("usage_summary", map[string]any{})
			if !res.IsError || h.errorCode(doc) != string(CodeBadResponse) {
				t.Fatalf("isError=%v code=%q, want bad_response for %s", res.IsError, h.errorCode(doc), name)
			}
		})
	}
}

// ---- cost labelling ------------------------------------------------------

func TestCostNoteDistinguishesLedgerBasisUnpricedAndUnmetered(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"schema_version":1,"since":"2026-10-15T12:00:00Z","group":"model","rows":[]}`)
	}, Config{})
	_, doc := h.call("usage_summary", map[string]any{})
	note, _ := doc["cost_note"].(string)
	if note == "" {
		t.Fatal("cost_note missing")
	}
	for _, want := range []string{"not actual spend", "unpriced_requests", "unknown_usage_requests"} {
		if !strings.Contains(note, want) {
			t.Errorf("cost_note must mention %q, got %q", want, note)
		}
	}
	// The blanket claim that the figures are API-equivalent/subscription
	// pricing is wrong: an aggregate has no exposed basis and may mix
	// provider-reported and estimated costs.
	if strings.Contains(note, "API-equivalent or subscription pricing basis") {
		t.Errorf("cost_note must not claim a single pricing basis: %q", note)
	}
}

// TestSuccessOutputSchemasRequireEveryAlwaysEmittedField pins the success
// alternative against what the tools actually emit: every field the adapter
// always produces must be required, while upstream-provenance fields the
// endpoint may omit (now, since) stay optional.
func TestSuccessOutputSchemasRequireEveryAlwaysEmittedField(t *testing.T) {
	want := map[string][]string{
		"quota_status":    {"retrieval", "source", "counts", "accounts", "clients"},
		"usage_summary":   {"retrieval", "source", "counts", "rows", "group", "cost_note"},
		"check_admission": {"retrieval", "source", "decision", "admitted"},
	}
	for name, fields := range want {
		t.Run(name, func(t *testing.T) {
			_, out := toolSchema(t, name)
			anyOf, _ := out["anyOf"].([]any)
			var success []any
			for _, alt := range anyOf {
				m, _ := alt.(map[string]any)
				req, _ := m["required"].([]any)
				for _, r := range req {
					if r == "retrieval" {
						success = req
					}
				}
			}
			if success == nil {
				t.Fatalf("%s: output schema has no success alternative", name)
			}
			have := map[string]bool{}
			for _, r := range success {
				s, _ := r.(string)
				have[s] = true
			}
			for _, f := range fields {
				if !have[f] {
					t.Errorf("%s: success schema must require %q, got %v", name, f, success)
				}
			}
			for _, f := range []string{"now", "since"} {
				if have[f] {
					t.Errorf("%s: %q is upstream provenance the endpoint may omit and must stay optional", name, f)
				}
			}
		})
	}
}
