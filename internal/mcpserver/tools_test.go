package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testKey = "SUPERSECRETCLIENTKEY"

// harness wires the real SDK server and a real SDK client together over an
// in-memory transport, in front of a real httptest control API. Nothing is
// stubbed: tool schemas, argument decoding, HTTP framing and result encoding
// all go through production paths.
type harness struct {
	t        *testing.T
	srv      *Server
	session  *mcp.ClientSession
	upstream *httptest.Server
	mu       sync.Mutex
	seen     []seenRequest
}

type seenRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   string
}

func (h *harness) record(r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, seenRequest{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.RawQuery,
		auth:   r.Header.Get("Authorization"),
		body:   string(body),
	})
}

func (h *harness) requests() []seenRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]seenRequest, len(h.seen))
	copy(out, h.seen)
	return out
}

// newHarness starts the fake control API and an MCP session against it.
func newHarness(t *testing.T, handler http.HandlerFunc, cfg Config) *harness {
	t.Helper()
	h := &harness{t: t}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.record(r)
		handler(w, r)
	}))
	t.Cleanup(h.upstream.Close)
	if cfg.URL == "" {
		cfg.URL = h.upstream.URL
	}
	if cfg.MaxResultChars == 0 {
		cfg.MaxResultChars = DefaultMaxResultChars
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.srv = srv
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.MCPServer().Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-host", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	h.session = cs
	return h
}

// call invokes a tool with raw JSON arguments and returns the decoded text
// content plus the structured content, asserting they are equivalent.
func (h *harness) call(name string, args any) (*mcp.CallToolResult, map[string]any) {
	h.t.Helper()
	res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("CallTool %s: %v", name, err)
	}
	if len(res.Content) != 1 {
		h.t.Fatalf("CallTool %s: want exactly one content item, got %d", name, len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		h.t.Fatalf("CallTool %s: content is %T, want *mcp.TextContent", name, res.Content[0])
	}
	var fromText map[string]any
	if err := json.Unmarshal([]byte(text.Text), &fromText); err != nil {
		h.t.Fatalf("CallTool %s: text content is not a JSON object: %v (%s)", name, err, text.Text)
	}
	if res.StructuredContent != nil {
		structured, err := json.Marshal(res.StructuredContent)
		if err != nil {
			h.t.Fatalf("CallTool %s: structured content does not marshal: %v", name, err)
		}
		var fromStructured map[string]any
		if err := json.Unmarshal(structured, &fromStructured); err != nil {
			h.t.Fatalf("CallTool %s: structured content is not an object: %v", name, err)
		}
		if !reflect.DeepEqual(fromText, fromStructured) {
			h.t.Fatalf("CallTool %s: structured content and JSON text content differ\n text: %s\n structured: %s",
				name, text.Text, structured)
		}
	}
	return res, fromText
}

// rawCall invokes a tool with an exact argument string, bypassing any
// marshaling the host SDK would do.
func (h *harness) rawCall(name, args string) (*mcp.CallToolResult, map[string]any) {
	h.t.Helper()
	return h.call(name, json.RawMessage(args))
}

func (h *harness) errorCode(doc map[string]any) string {
	h.t.Helper()
	e, ok := doc["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := e["code"].(string)
	return code
}

func statusDoc(accounts string) string {
	return fmt.Sprintf(`{"schema_version":1,"now":"2026-10-16T12:00:00Z","accounts":[%s],"clients":[{"name":"codex","class":"background"}]}`, accounts)
}

const oneAccount = `{"id":"primary","provider":"codex","healthy":true,"cooldown_until":null,"inflight":2,` +
	`"reserve":{"weekly":0.25},"windows":[{"kind":"weekly","used_frac":0.5,"remaining_frac":0.5,"reset_at":"2026-10-20T00:00:00Z","window_seconds":604800,"rolled":false}],` +
	`"background_admissible":false,"interactive_admissible":true,"snapshot_age_s":12,"stale":false,"error":null}`

func jsonResp(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

// ---- protocol surface ----------------------------------------------------

func TestListToolsExposesThreeReadOnlyAnnotatedTools(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, statusDoc("")) }, Config{})
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := []string{"check_admission", "quota_status", "usage_summary"}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
		if tool.Annotations == nil {
			t.Fatalf("tool %s has no annotations", tool.Name)
		}
		if !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s: readOnlyHint = false, want true", tool.Name)
		}
		if !tool.Annotations.IdempotentHint {
			t.Errorf("tool %s: idempotentHint = false, want true", tool.Name)
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Errorf("tool %s: destructiveHint must be explicitly false", tool.Name)
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("tool %s: input schema is %T, want object", tool.Name, tool.InputSchema)
		}
		if schema["additionalProperties"] != false {
			t.Errorf("tool %s: input schema must set additionalProperties false, got %v", tool.Name, schema["additionalProperties"])
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tool names = %v, want %v", got, want)
	}
}

// ---- quota_status --------------------------------------------------------

func TestQuotaStatusForwardsBearerAndPreservesUpstreamFields(t *testing.T) {
	dir := t.TempDir()
	keyPath := dir + "/client.key"
	if err := writeTestKey(keyPath, testKey); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, statusDoc(oneAccount)) },
		Config{KeyFile: keyPath})

	res, doc := h.call("quota_status", map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected error result: %v", doc)
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	if reqs[0].method != http.MethodGet || reqs[0].path != "/control/v1/status" {
		t.Errorf("upstream request = %s %s, want GET /control/v1/status", reqs[0].method, reqs[0].path)
	}
	if reqs[0].auth != "Bearer "+testKey {
		t.Errorf("upstream Authorization = %q, want the configured bearer key", reqs[0].auth)
	}
	accounts, _ := doc["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v, want 1 entry", doc["accounts"])
	}
	acct, _ := accounts[0].(map[string]any)
	if acct["id"] != "primary" || acct["stale"] != false {
		t.Errorf("account identity/freshness lost: %v", acct)
	}
	if acct["background_admissible"] != false || acct["interactive_admissible"] != true {
		t.Errorf("admissibility lost: %v", acct)
	}
	windows, _ := acct["windows"].([]any)
	if len(windows) != 1 {
		t.Fatalf("windows lost: %v", acct["windows"])
	}
	win, _ := windows[0].(map[string]any)
	if win["kind"] != "weekly" || win["rolled"] != false || win["window_seconds"] != float64(604800) {
		t.Errorf("window detail lost: %v", win)
	}
	reserve, _ := acct["reserve"].(map[string]any)
	if reserve["weekly"] != 0.25 {
		t.Errorf("reserve lost: %v", acct["reserve"])
	}
	if doc["now"] != "2026-10-16T12:00:00Z" {
		t.Errorf("upstream now field lost: %v", doc["now"])
	}
	source, _ := doc["source"].(map[string]any)
	if source["endpoint"] != "/control/v1/status" || source["upstream_schema_version"] != float64(1) {
		t.Errorf("provenance lost: %v", doc["source"])
	}
	retrieval, _ := doc["retrieval"].(map[string]any)
	if retrieval["retrieved_at"] == "" || retrieval["truncated"] != false || retrieval["dropped_items"] != float64(0) {
		t.Errorf("retrieval metadata wrong: %v", retrieval)
	}
	if retrieval["max_result_chars"] != float64(DefaultMaxResultChars) {
		t.Errorf("max_result_chars not reported: %v", retrieval)
	}
	clients, _ := doc["clients"].([]any)
	if len(clients) != 1 {
		t.Errorf("clients lost: %v", doc["clients"])
	}
}

func TestQuotaStatusAccountFilterAndNotFound(t *testing.T) {
	other := strings.Replace(oneAccount, `"id":"primary"`, `"id":"secondary"`, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, statusDoc(oneAccount+","+other))
	}, Config{})

	_, doc := h.call("quota_status", map[string]any{"account": "secondary"})
	accounts, _ := doc["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("filtered accounts = %v, want exactly the requested one", doc["accounts"])
	}
	if acct, _ := accounts[0].(map[string]any); acct["id"] != "secondary" {
		t.Errorf("filtered account = %v, want secondary", acct)
	}
	counts, _ := doc["counts"].(map[string]any)
	if counts["accounts_total"] != float64(1) || counts["accounts_returned"] != float64(1) {
		t.Errorf("counts = %v, want one total and one returned", counts)
	}

	res, doc := h.call("quota_status", map[string]any{"account": "ghost"})
	if !res.IsError || h.errorCode(doc) != string(CodeNotFound) {
		t.Fatalf("unknown account: isError=%v code=%q, want not_found", res.IsError, h.errorCode(doc))
	}
}

func TestQuotaStatusUnknownArgumentRejected(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, statusDoc(oneAccount)) }, Config{})
	res, doc := h.rawCall("quota_status", `{"class":"interactive"}`)
	if !res.IsError || h.errorCode(doc) != string(CodeInvalidArguments) {
		t.Fatalf("isError=%v code=%q, want invalid_arguments", res.IsError, h.errorCode(doc))
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("a rejected call must not reach the control API; saw %d requests", n)
	}
}

func TestQuotaStatusAuthRequiredNeverReflectsKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := dir + "/client.key"
	if err := writeTestKey(keyPath, testKey); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		jsonResp(w, `{"error":{"message":"invalid client key `+testKey+`"}}`)
	}, Config{KeyFile: keyPath})

	res, doc := h.call("quota_status", map[string]any{})
	if !res.IsError || h.errorCode(doc) != string(CodeAuthRequired) {
		t.Fatalf("isError=%v code=%q, want auth_required", res.IsError, h.errorCode(doc))
	}
	raw, _ := json.Marshal(doc)
	if strings.Contains(string(raw), testKey) {
		t.Fatalf("the bearer key was reflected into the result: %s", raw)
	}
}

func TestQuotaStatusServerUnreachableAndTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	_ = ln.Close()
	t.Run("unreachable", func(t *testing.T) {
		srv, err := New(Config{URL: dead, Timeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		cs := connectInMemory(t, srv)
		res, doc := callRaw(t, cs, "quota_status", `{}`)
		if !res.IsError || codeOf(t, doc) != string(CodeServerUnreachable) {
			t.Fatalf("isError=%v code=%q, want server_unreachable", res.IsError, codeOf(t, doc))
		}
	})
	t.Run("timeout", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(400 * time.Millisecond)
			jsonResp(w, statusDoc(""))
		}, Config{Timeout: 80 * time.Millisecond})
		res, doc := h.call("quota_status", map[string]any{})
		if !res.IsError || h.errorCode(doc) != string(CodeServerTimeout) {
			t.Fatalf("isError=%v code=%q, want server_timeout", res.IsError, h.errorCode(doc))
		}
	})
}

func TestQuotaStatusRedirectIsNotFollowed(t *testing.T) {
	var redirected int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected++
		jsonResp(w, statusDoc(oneAccount))
	}))
	defer target.Close()
	dir := t.TempDir()
	keyPath := dir + "/client.key"
	if err := writeTestKey(keyPath, testKey); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/control/v1/status", http.StatusFound)
	}, Config{KeyFile: keyPath})

	res, doc := h.call("quota_status", map[string]any{})
	if !res.IsError || h.errorCode(doc) != string(CodeBadResponse) {
		t.Fatalf("isError=%v code=%q, want bad_response", res.IsError, h.errorCode(doc))
	}
	if redirected != 0 {
		t.Fatalf("the client key was sent to the redirect target %d times", redirected)
	}
}

func TestQuotaStatusMalformedBodiesRejected(t *testing.T) {
	cases := map[string]string{
		"not json":        `not json at all`,
		"json array":      `[{"id":"primary"}]`,
		"missing array":   `{"schema_version":1,"now":"2026-10-16T12:00:00Z"}`,
		"accounts scalar": `{"schema_version":1,"accounts":5}`,
		"account scalar":  `{"schema_version":1,"accounts":["primary"]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, body) }, Config{})
			res, doc := h.call("quota_status", map[string]any{})
			if !res.IsError || h.errorCode(doc) != string(CodeBadResponse) {
				t.Fatalf("isError=%v code=%q, want bad_response", res.IsError, h.errorCode(doc))
			}
		})
	}
}

func TestQuotaStatusResponseByteCap(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"schema_version":1,"accounts":[],"pad":"`)
		blob := strings.Repeat("x", 64<<10)
		for i := 0; i < 20; i++ {
			if _, err := io.WriteString(w, blob); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}`)
	}, Config{})
	res, doc := h.call("quota_status", map[string]any{})
	if !res.IsError || h.errorCode(doc) != string(CodeBadResponse) {
		t.Fatalf("isError=%v code=%q, want bad_response", res.IsError, h.errorCode(doc))
	}
	if msg, _ := doc["error"].(map[string]any); !strings.Contains(fmt.Sprint(msg["message"]), "byte cap") {
		t.Errorf("message should name the byte cap: %v", msg)
	}
}

func TestQuotaStatusResultBudgetBoundsArrays(t *testing.T) {
	// The budget bounds the whole serialized CallToolResult (both document
	// copies plus envelope), so it is sized here to leave room for a non-empty
	// prefix of the fat account objects below.
	const budget = 8192
	var accts []string
	for i := 0; i < 60; i++ {
		accts = append(accts, fmt.Sprintf(
			`{"id":"acct-%02d","provider":"codex","reserve":{"weekly":0.5},"windows":[{"kind":"weekly","used_frac":0.1,"remaining_frac":0.9,"window_seconds":604800,"rolled":false}],"stale":false}`,
			i))
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, statusDoc(strings.Join(accts, ",")))
	}, Config{MaxResultChars: budget})

	_, doc := h.call("quota_status", map[string]any{})
	retrieval, _ := doc["retrieval"].(map[string]any)
	if retrieval["truncated"] != true {
		t.Fatalf("truncated = %v, want true", retrieval["truncated"])
	}
	if dropped, _ := retrieval["dropped_items"].(float64); dropped <= 0 {
		t.Fatalf("dropped_items = %v, want > 0", retrieval["dropped_items"])
	}
	accounts, _ := doc["accounts"].([]any)
	counts, _ := doc["counts"].(map[string]any)
	if counts["accounts_total"] != float64(60) {
		t.Errorf("accounts_total = %v, want 60 (the real total)", counts["accounts_total"])
	}
	if counts["accounts_returned"] != float64(len(accounts)) {
		t.Errorf("accounts_returned = %v, want %d", counts["accounts_returned"], len(accounts))
	}
	if len(accounts) == 0 || len(accounts) >= 60 {
		t.Fatalf("bounded accounts = %d, want a strict non-empty prefix", len(accounts))
	}
	// Every surviving element must still be a complete object: the budget is
	// enforced over whole array elements, not by cutting encoded JSON.
	for _, a := range accounts {
		if _, ok := a.(map[string]any); !ok {
			t.Fatalf("bounded account element is not an object: %v", a)
		}
	}
	text, _ := json.Marshal(doc)
	if len(text) > budget {
		t.Fatalf("emitted document is %d chars, over the %d budget", len(text), budget)
	}
}

// ---- usage_summary -------------------------------------------------------

func TestUsageSummaryDefaultsAndQuery(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"schema_version":1,"since":"2026-10-15T12:00:00Z","group":"model","rows":[{"key":"gpt","requests":2,"input_tokens":10,"cached_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":5,"reasoning_tokens":0,"cost_usd":0.001,"unknown_usage_requests":0,"unpriced_requests":0}]}`)
	}, Config{})

	res, doc := h.call("usage_summary", map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected error: %v", doc)
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].method != http.MethodGet || reqs[0].path != "/control/v1/usage" {
		t.Fatalf("upstream request = %+v, want GET /control/v1/usage", reqs)
	}
	if reqs[0].query != "group=model&since=24h" {
		t.Errorf("upstream query = %q, want group=model&since=24h", reqs[0].query)
	}
	counts, _ := doc["counts"].(map[string]any)
	if counts["limit_applied"] != float64(20) || counts["rows_returned"] != float64(1) || counts["rows_total"] != float64(1) {
		t.Errorf("counts = %v, want limit 20 and one row", counts)
	}
}

func TestUsageSummaryArgumentValidation(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, `{"schema_version":1,"rows":[]}`) }, Config{})
	cases := map[string]string{
		"since negative duration": `{"since":"-1h"}`,
		"since zero days":         `{"since":"0d"}`,
		"since too large days":    `{"since":"91d"}`,
		"since too large hours":   `{"since":"2200h"}`,
		"since garbage":           `{"since":"yesterday"}`,
		"since wrong type":        `{"since":24}`,
		"group unknown":           `{"group":"provider"}`,
		"group wrong type":        `{"group":true}`,
		"limit as string":         `{"limit":"20"}`,
		"limit as bool":           `{"limit":true}`,
		"limit fractional":        `{"limit":2.5}`,
		"limit exponent":          `{"limit":1e2}`,
		"limit zero":              `{"limit":0}`,
		"limit above max":         `{"limit":101}`,
		"unknown field":           `{"account":"primary"}`,
		"nested argument object":  `{"extra":{"since":"1h"}}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res, doc := h.rawCall("usage_summary", args)
			if !res.IsError || h.errorCode(doc) != string(CodeInvalidArguments) {
				t.Fatalf("isError=%v code=%q, want invalid_arguments", res.IsError, h.errorCode(doc))
			}
		})
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("rejected calls must not reach the control API; saw %d requests", n)
	}
}

func TestUsageSummaryLimitTruncatesRowsHonestly(t *testing.T) {
	var rows []string
	for i := 0; i < 30; i++ {
		rows = append(rows, fmt.Sprintf(`{"key":"m%02d","requests":1,"input_tokens":%d,"output_tokens":1,"cost_usd":null,"unpriced_requests":1,"unknown_usage_requests":0,"vendor_extension":"kept"}`, i, i))
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"schema_version":1,"since":"2026-10-15T12:00:00Z","group":"model","rows":[`+strings.Join(rows, ",")+`]}`)
	}, Config{})

	_, doc := h.call("usage_summary", map[string]any{"group": "model", "limit": 5})
	got, _ := doc["rows"].([]any)
	if len(got) != 5 {
		t.Fatalf("rows = %d, want 5", len(got))
	}
	counts, _ := doc["counts"].(map[string]any)
	if counts["rows_total"] != float64(30) {
		t.Errorf("rows_total = %v, want 30", counts["rows_total"])
	}
	if counts["rows_truncated"] != true {
		t.Errorf("rows_truncated = %v, want true", counts["rows_truncated"])
	}
	retrieval, _ := doc["retrieval"].(map[string]any)
	if retrieval["dropped_items"] != float64(25) {
		t.Errorf("dropped_items = %v, want 25", retrieval["dropped_items"])
	}
	row, _ := got[0].(map[string]any)
	if row["cost_usd"] != nil {
		t.Errorf("a null cost must stay null, got %v", row["cost_usd"])
	}
	if row["unpriced_requests"] != float64(1) || row["unknown_usage_requests"] != float64(0) {
		t.Errorf("unpriced/unknown fields lost: %v", row)
	}
	if row["vendor_extension"] != "kept" {
		t.Errorf("unknown upstream fields must be preserved verbatim: %v", row)
	}
	if note, _ := doc["cost_note"].(string); !strings.Contains(note, "not actual spend") {
		t.Errorf("cost_note must say the figures are estimates, got %q", note)
	}
}

func TestUsageSummaryLedgerUnavailable(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		jsonResp(w, `{"error":{"message":"ledger unavailable"}}`)
	}, Config{})
	res, doc := h.call("usage_summary", map[string]any{})
	if !res.IsError || h.errorCode(doc) != string(CodeEndpointUnavailable) {
		t.Fatalf("isError=%v code=%q, want endpoint_unavailable", res.IsError, h.errorCode(doc))
	}
}

// ---- check_admission -----------------------------------------------------

func TestCheckAdmissionDenyIsAValidDecision(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"decision":"deny","account_id":"","reason":"reserve"}`)
	}, Config{})
	res, doc := h.call("check_admission", map[string]any{"model": "gpt-5"})
	if res.IsError {
		t.Fatalf("a deny decision must not be a tool error: %v", doc)
	}
	if doc["decision"] != "deny" || doc["admitted"] != false {
		t.Fatalf("decision = %v admitted = %v, want deny/false", doc["decision"], doc["admitted"])
	}
	if doc["reason"] != "reserve" {
		t.Errorf("reason lost: %v", doc["reason"])
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].method != http.MethodPost || reqs[0].path != "/control/v1/admit" {
		t.Fatalf("upstream request = %+v, want POST /control/v1/admit", reqs)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(reqs[0].body), &body); err != nil {
		t.Fatalf("request body is not JSON: %q", reqs[0].body)
	}
	if len(body) != 2 || body["class"] != "background" || body["model"] != "gpt-5" {
		t.Errorf("request body = %v, want exactly {class:background, model:gpt-5}", body)
	}
}

func TestCheckAdmissionAllowCarriesBudgetEstimate(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"decision":"allow","account_id":"primary","reason":"admitted","budget":{"estimated_usd":0.5,"note":"advisory"}}`)
	}, Config{})
	res, doc := h.call("check_admission", map[string]any{"account": "primary", "class": "interactive"})
	if res.IsError {
		t.Fatalf("unexpected error: %v", doc)
	}
	if doc["decision"] != "allow" || doc["admitted"] != true || doc["account_id"] != "primary" {
		t.Fatalf("decision document wrong: %v", doc)
	}
	budget, _ := doc["budget"].(map[string]any)
	if budget["estimated_usd"] != 0.5 || budget["note"] != "advisory" {
		t.Errorf("advisory budget estimate must be preserved: %v", doc["budget"])
	}
	reqs := h.requests()
	var body map[string]any
	_ = json.Unmarshal([]byte(reqs[0].body), &body)
	if body["class"] != "interactive" || body["account"] != "primary" {
		t.Errorf("request body = %v", body)
	}
}

func TestCheckAdmissionArgumentValidation(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"decision":"allow","account_id":"primary","reason":"admitted"}`)
	}, Config{})
	cases := map[string]string{
		"neither target":     `{}`,
		"both targets":       `{"account":"primary","model":"gpt-5"}`,
		"class unknown":      `{"account":"primary","class":"batch"}`,
		"class wrong type":   `{"account":"primary","class":1}`,
		"unknown field":      `{"account":"primary","mode":"x"}`,
		"account wrong type": `{"account":7}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res, doc := h.rawCall("check_admission", args)
			if !res.IsError || h.errorCode(doc) != string(CodeInvalidArguments) {
				t.Fatalf("isError=%v code=%q, want invalid_arguments", res.IsError, h.errorCode(doc))
			}
		})
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("rejected calls must not reach the control API; saw %d requests", n)
	}
}

func TestCheckAdmissionCapabilityRefusalIsStable(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		jsonResp(w, `{"error":{"message":"localrouter: no account supports the capabilities this request needs","type":"capability_unsupported"}}`)
	}, Config{})
	res, doc := h.call("check_admission", map[string]any{"model": "gpt-5"})
	if !res.IsError || h.errorCode(doc) != string(CodeCapabilityUnsupported) {
		t.Fatalf("isError=%v code=%q, want capability_unsupported", res.IsError, h.errorCode(doc))
	}
}

func TestCheckAdmissionBadResponseIsRejected(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"verdict":"maybe"}`)
	}, Config{})
	res, doc := h.call("check_admission", map[string]any{"model": "gpt-5"})
	if !res.IsError || h.errorCode(doc) != string(CodeBadResponse) {
		t.Fatalf("isError=%v code=%q, want bad_response", res.IsError, h.errorCode(doc))
	}
}

// ---- shared helpers ------------------------------------------------------

func connectInMemory(t *testing.T, srv *Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.MCPServer().Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-host", Version: "0.0.1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callRaw(t *testing.T, cs *mcp.ClientSession, name, args string) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T", res.Content[0])
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(text.Text), &doc); err != nil {
		t.Fatalf("text content is not an object: %v", err)
	}
	return res, doc
}

func codeOf(t *testing.T, doc map[string]any) string {
	t.Helper()
	e, _ := doc["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func writeTestKey(path, key string) error {
	return os.WriteFile(path, []byte(key+"\n"), 0o600)
}
