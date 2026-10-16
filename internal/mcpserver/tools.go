package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// usageGroups are the summary dimensions GET /control/v1/usage accepts.
var usageGroups = []string{"account", "model", "class", "client", "host", "route", "task", "agent"}

// admissionClasses are the workload classes the control API admits for.
var admissionClasses = []string{"background", "interactive"}

const (
	defaultSince = "24h"
	maxSinceDays = 90
	defaultLimit = 20
	maxLimit     = 100
)

func boolPtr(b bool) *bool { return &b }

// readOnlyAnnotations is applied to every tool: the adapter only reads, has no
// side effects, and calling it twice with the same arguments is a no-op on the
// router. destructiveHint is explicitly false.
func readOnlyAnnotations(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		IdempotentHint:  true,
		DestructiveHint: boolPtr(false),
		OpenWorldHint:   boolPtr(false),
	}
}

// errorEnvelopeSchema describes the structured failure shape every tool can
// return (isError=true): a stable code plus a bounded message.
const errorEnvelopeSchema = `{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "code":{"type":"string"},
    "message":{"type":"string"},
    "detail":{"type":"string"}
  },
  "required":["code","message"]
}`

// resultMetaSchemas are the retrieval/source blocks shared by every result.
const resultMetaSchemas = `"retrieval":{"type":"object","additionalProperties":false,"description":"When and how this result was produced.","properties":{
      "retrieved_at":{"type":"string"},
      "truncated":{"type":"boolean"},
      "dropped_items":{"type":"integer","minimum":0},
      "max_result_chars":{"type":"integer","minimum":1}},
    "required":["retrieved_at","truncated","dropped_items","max_result_chars"]},
  "source":{"type":"object","additionalProperties":false,"description":"The control endpoint this result came from and the schema version it reported.","properties":{
      "endpoint":{"type":"string"},
      "upstream_schema_version":{"type":["integer","null"],"minimum":1}},
    "required":["endpoint","upstream_schema_version"]},
  "error":` + errorEnvelopeSchema

// quotaStatusOutputSchema describes a quota_status result or an error envelope.
// Upstream account and client objects are passed through verbatim, so their
// element schemas permit whatever fields the control API reported.
var quotaStatusOutputSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    ` + resultMetaSchemas + `,
    "now":{"type":"string","description":"The control API's own clock, verbatim."},
    "accounts":{"type":"array","description":"Upstream account objects, verbatim.","items":{"type":"object","additionalProperties":true}},
    "clients":{"type":"array","description":"Upstream client objects, verbatim.","items":{"type":"object","additionalProperties":true}},
    "counts":{"type":"object","additionalProperties":false,"properties":{
      "accounts_returned":{"type":"integer","minimum":0},
      "accounts_total":{"type":"integer","minimum":0},
      "clients_returned":{"type":"integer","minimum":0},
      "clients_total":{"type":"integer","minimum":0}},
      "required":["accounts_returned","accounts_total","clients_returned","clients_total"]}
  },
  "anyOf":[{"required":["retrieval","source","counts","accounts","clients"]},{"required":["error"]}]
}`)

var usageSummaryOutputSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    ` + resultMetaSchemas + `,
    "since":{"type":"string","description":"The window start the control API reported, verbatim."},
    "group":{"type":"string","enum":["account","model","class","client","host","route","task","agent"]},
    "rows":{"type":"array","description":"Ledger rows, verbatim; unknown fields are preserved.","items":{"type":"object","additionalProperties":true}},
    "counts":{"type":"object","additionalProperties":false,"properties":{
      "rows_returned":{"type":"integer","minimum":0},
      "rows_total":{"type":"integer","minimum":0},
      "limit_applied":{"type":"integer","minimum":1},
      "rows_truncated":{"type":"boolean"}},
      "required":["rows_returned","rows_total","limit_applied","rows_truncated"]},
    "cost_note":{"type":"string","description":"How to read cost_usd; not actual spend."}
  },
  "anyOf":[{"required":["retrieval","source","counts","rows","group","cost_note"]},{"required":["error"]}]
}`)

var checkAdmissionOutputSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    ` + resultMetaSchemas + `,
    "decision":{"type":"string","enum":["allow","deny"]},
    "admitted":{"type":"boolean"},
    "account_id":{"type":"string"},
    "reason":{"type":"string"},
    "budget":{"type":"object","additionalProperties":true,"description":"Advisory spend estimate, verbatim."}
  },
  "anyOf":[{"required":["retrieval","source","decision","admitted"]},{"required":["error"]}]
}`)

func (s *Server) registerTools() {
	s.mcp.AddTool(&mcp.Tool{
		Name:        "quota_status",
		Title:       "LocalRouter quota status",
		Description: "Report the router's per-account quota status: freshness, window usage, reserves, admissibility, cooldown and any per-account error, exactly as GET /control/v1/status returns them. Read-only; never fetches upstream quota or changes policy.",
		Annotations: readOnlyAnnotations("LocalRouter quota status"),
		InputSchema: json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "account":{"type":"string","description":"Optional exact account ID. When given, only that account is reported (and the call fails with not_found if no account has that ID). Omit to report every account."}
  }
}`),
		OutputSchema: quotaStatusOutputSchema,
	}, s.handleQuotaStatus)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "usage_summary",
		Title:       "LocalRouter usage summary",
		Description: "Aggregate recorded request usage over a look-back window, grouped by one dimension, exactly as GET /control/v1/usage returns it. Rows keep the ledger's own fields, including unpriced / unknown-usage counts. The control API usage endpoint supports no account filter, so this tool exposes none.",
		Annotations: readOnlyAnnotations("LocalRouter usage summary"),
		InputSchema: json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "since":{"type":"string","description":"Look-back window, either a Go duration (24h, 168h) or whole days (7d). Must be positive and at most 90d. Default 24h."},
    "group":{"type":"string","enum":["account","model","class","client","host","route","task","agent"],"description":"Summary dimension. Default model."},
    "limit":{"type":"integer","minimum":1,"maximum":100,"description":"Maximum rows to return, 1-100. Default 20."}
  }
}`),
		OutputSchema: usageSummaryOutputSchema,
	}, s.handleUsageSummary)

	s.mcp.AddTool(&mcp.Tool{
		Name:        "check_admission",
		Title:       "LocalRouter admission dry run",
		Description: "Ask whether the router's policy would admit one request of a given class on one account, or on the route for one model, without taking a lease or reserving anything. A deny decision is a normal answer, not a failure; only a transport, authentication or malformed-response problem is reported as a tool error.",
		Annotations: readOnlyAnnotations("LocalRouter admission dry run"),
		// oneOf enforces exactly one of account/model at the schema level. The
		// handler still validates it at runtime with the same invalid_arguments
		// result, so error semantics are unchanged.
		InputSchema: json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "account":{"type":"string","description":"Account ID to dry-run. Exactly one of account or model is required."},
    "model":{"type":"string","description":"Model whose configured route to dry-run. Exactly one of account or model is required."},
    "class":{"type":"string","enum":["background","interactive"],"description":"Workload class to admit for. Default background."}
  },
  "oneOf":[
    {"required":["account"]},
    {"required":["model"]}
  ]
}`),
		OutputSchema: checkAdmissionOutputSchema,
	}, s.handleCheckAdmission)
}

// ---- quota_status --------------------------------------------------------

func (s *Server) handleQuotaStatus(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, te := decodeArgs(req.Params.Arguments, "account")
	if te != nil {
		return s.errResult(te), nil
	}
	account, te := args.str("account", false)
	if te != nil {
		return s.errResult(te), nil
	}
	rctx, cancel := s.requestContext(ctx)
	defer cancel()
	raw, te := s.up.get(rctx, pathStatus, nil)
	if te != nil {
		return s.errResult(te), nil
	}
	upstreamSchema, doc, te := decodeStatus(raw)
	if te != nil {
		return s.errResult(te), nil
	}
	accounts := doc.accounts
	if account != "" {
		kept := make([]json.RawMessage, 0, 1)
		for _, a := range accounts {
			if accountID(a) == account {
				kept = append(kept, a)
			}
		}
		if len(kept) == 0 {
			return s.errResult(&toolError{Code: CodeNotFound,
				Message: fmt.Sprintf("the control API reports no account with ID %q", account)}), nil
		}
		accounts = kept
	}
	res := statusResult{
		Source:   sourceInfo{Endpoint: pathStatus, UpstreamSchemaVersion: upstreamSchema},
		Now:      doc.now,
		Accounts: accounts,
		Clients:  doc.clients,
	}
	if te := s.fitStatus(&res); te != nil {
		return s.errResult(te), nil
	}
	return s.finish(res)
}

// statusParts is the structurally validated GET /control/v1/status document,
// with every array element still verbatim so freshness, windows, reserves and
// error fields reach the caller untouched.
type statusParts struct {
	now      json.RawMessage
	accounts []json.RawMessage
	clients  []json.RawMessage
}

func decodeStatus(raw json.RawMessage) (*int, statusParts, *toolError) {
	var probe struct {
		SchemaVersion json.RawMessage `json:"schema_version"`
		Now           json.RawMessage `json:"now"`
		Accounts      json.RawMessage `json:"accounts"`
		Clients       json.RawMessage `json:"clients"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, statusParts{}, &toolError{Code: CodeBadResponse,
			Message: "the control API status response is not a JSON object"}
	}
	if probe.Accounts == nil {
		return nil, statusParts{}, &toolError{Code: CodeBadResponse,
			Message: "the control API status response has no accounts array"}
	}
	schema, te := decodeSchemaVersion(probe.SchemaVersion)
	if te != nil {
		return nil, statusParts{}, te
	}
	if probe.Now != nil {
		if err := decodeString(probe.Now); err != nil {
			return nil, statusParts{}, &toolError{Code: CodeBadResponse,
				Message: "the control API status now field is not a string"}
		}
	}
	accounts, err := decodeObjectArray(probe.Accounts)
	if err != nil {
		return nil, statusParts{}, &toolError{Code: CodeBadResponse,
			Message: "the control API status accounts field is not an array of objects"}
	}
	clients := []json.RawMessage{}
	if probe.Clients != nil {
		clients, err = decodeObjectArray(probe.Clients)
		if err != nil {
			return nil, statusParts{}, &toolError{Code: CodeBadResponse,
				Message: "the control API status clients field is not an array of objects"}
		}
	}
	return schema, statusParts{now: probe.Now, accounts: accounts, clients: clients}, nil
}

// decodeSchemaVersion reads a document's own schema_version. It returns null
// (nil) when the field is absent or explicitly null, and a bad_response error
// when it is present but not a positive integer. A JSON string is never coerced
// into a number, and the adapter never substitutes its own version for a
// missing one.
func decodeSchemaVersion(raw json.RawMessage) (*int, *toolError) {
	if raw == nil {
		return nil, nil
	}
	t := bytes.TrimSpace(raw)
	if string(t) == "null" {
		return nil, nil
	}
	if len(t) == 0 || (t[0] != '-' && (t[0] < '0' || t[0] > '9')) {
		return nil, &toolError{Code: CodeBadResponse,
			Message: "the control API reports a non-numeric schema_version"}
	}
	var n json.Number
	if err := json.Unmarshal(t, &n); err != nil {
		return nil, &toolError{Code: CodeBadResponse,
			Message: "the control API reports a non-numeric schema_version"}
	}
	i, err := n.Int64()
	if err != nil || i <= 0 {
		return nil, &toolError{Code: CodeBadResponse,
			Message: "the control API reports a non-positive schema_version"}
	}
	v := int(i)
	return &v, nil
}

// decodeString rejects a value that is not a JSON string (notably a number or a
// bare null), so a wrong upstream type fails the call rather than reaching the
// caller as a stringified field.
func decodeString(raw json.RawMessage) error {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '"' {
		return errors.New("not a JSON string")
	}
	var s string
	return json.Unmarshal(t, &s)
}

// decodeObjectArray parses a JSON array whose elements must each be a JSON
// object. An explicit null array, a null element or any non-object element
// fails the whole call rather than surfacing as a successful result with a
// broken row. (A bare json.Unmarshal into a map would happily accept null.)
func decodeObjectArray(raw json.RawMessage) ([]json.RawMessage, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '[' {
		return nil, errors.New("not a JSON array")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(t, &items); err != nil {
		return nil, err
	}
	if items == nil {
		return []json.RawMessage{}, nil
	}
	for _, it := range items {
		e := bytes.TrimSpace(it)
		if len(e) == 0 || e[0] != '{' {
			return nil, errors.New("array element is not a JSON object")
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(e, &m); err != nil || m == nil {
			return nil, errors.New("array element is not a JSON object")
		}
	}
	return items, nil
}

// accountID extracts the "id" field of one account element.
func accountID(raw json.RawMessage) string {
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return ""
	}
	return a.ID
}

// ---- usage_summary -------------------------------------------------------

func (s *Server) handleUsageSummary(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, te := decodeArgs(req.Params.Arguments, "since", "group", "limit")
	if te != nil {
		return s.errResult(te), nil
	}
	since, te := args.str("since", false)
	if te != nil {
		return s.errResult(te), nil
	}
	if since == "" {
		since = defaultSince
	}
	if te := validateSince(since); te != nil {
		return s.errResult(te), nil
	}
	group, ok, te := args.enum("group", usageGroups)
	if te != nil {
		return s.errResult(te), nil
	}
	if !ok {
		group = "model"
	}
	limit, ok, te := args.integer("limit")
	if te != nil {
		return s.errResult(te), nil
	}
	if !ok {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return s.errResult(invalidArg("argument %q must be between 1 and %d", "limit", maxLimit)), nil
	}

	q := url.Values{}
	q.Set("since", since)
	q.Set("group", group)
	rctx, cancel := s.requestContext(ctx)
	defer cancel()
	raw, te := s.up.get(rctx, pathUsage, q)
	if te != nil {
		return s.errResult(te), nil
	}
	upstreamSchema, sinceRaw, rows, te := decodeUsage(raw)
	if te != nil {
		return s.errResult(te), nil
	}
	res := usageResult{
		Source:   sourceInfo{Endpoint: pathUsage, UpstreamSchemaVersion: upstreamSchema},
		Since:    sinceRaw,
		Group:    group,
		Rows:     rows,
		CostNote: costNote,
		Counts:   usageCounts{LimitApplied: limit},
	}
	total := len(rows)
	if total > limit {
		res.Rows = rows[:limit]
	}
	if te := s.fitRows(&res, total); te != nil {
		return s.errResult(te), nil
	}
	return s.finish(res)
}

func decodeUsage(raw json.RawMessage) (*int, json.RawMessage, []json.RawMessage, *toolError) {
	var probe struct {
		SchemaVersion json.RawMessage `json:"schema_version"`
		Since         json.RawMessage `json:"since"`
		Rows          json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, nil, nil, &toolError{Code: CodeBadResponse,
			Message: "the control API usage response is not a JSON object"}
	}
	if probe.Rows == nil {
		return nil, nil, nil, &toolError{Code: CodeBadResponse,
			Message: "the control API usage response has no rows array"}
	}
	schema, te := decodeSchemaVersion(probe.SchemaVersion)
	if te != nil {
		return nil, nil, nil, te
	}
	if probe.Since != nil {
		if err := decodeString(probe.Since); err != nil {
			return nil, nil, nil, &toolError{Code: CodeBadResponse,
				Message: "the control API usage since field is not a string"}
		}
	}
	rows, err := decodeObjectArray(probe.Rows)
	if err != nil {
		return nil, nil, nil, &toolError{Code: CodeBadResponse,
			Message: "the control API usage rows field is not an array of objects"}
	}
	return schema, probe.Since, rows, nil
}

// validateSince accepts what the control API accepts (a Go duration or whole
// days), tightened to the 90 day bound this tool documents.
func validateSince(v string) *toolError {
	v = strings.TrimSpace(v)
	var d time.Duration
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 {
			return invalidArg("argument %q must be a positive number of days, e.g. 7d", "since")
		}
		if days > maxSinceDays {
			return invalidArg("argument %q must be at most %dd", "since", maxSinceDays)
		}
		return nil
	}
	var err error
	d, err = time.ParseDuration(v)
	if err != nil || d <= 0 {
		return invalidArg("argument %q must be a positive duration like 24h or 7d", "since")
	}
	if d > maxSinceDays*24*time.Hour {
		return invalidArg("argument %q must be at most %dd", "since", maxSinceDays)
	}
	return nil
}

// ---- check_admission -----------------------------------------------------

func (s *Server) handleCheckAdmission(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, te := decodeArgs(req.Params.Arguments, "account", "model", "class")
	if te != nil {
		return s.errResult(te), nil
	}
	account, te := args.str("account", false)
	if te != nil {
		return s.errResult(te), nil
	}
	model, te := args.str("model", false)
	if te != nil {
		return s.errResult(te), nil
	}
	if (account == "") == (model == "") {
		return s.errResult(invalidArg("exactly one of %q or %q is required", "account", "model")), nil
	}
	class, ok, te := args.enum("class", admissionClasses)
	if te != nil {
		return s.errResult(te), nil
	}
	if !ok {
		class = "background"
	}
	body := map[string]string{"class": class}
	if account != "" {
		body["account"] = account
	} else {
		body["model"] = model
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return s.errResult(&toolError{Code: CodeBadResponse, Message: "the request could not be encoded"}), nil
	}
	rctx, cancel := s.requestContext(ctx)
	defer cancel()
	raw, te := s.up.post(rctx, pathAdmit, payload)
	if te != nil {
		return s.errResult(te), nil
	}
	var doc struct {
		SchemaVersion json.RawMessage `json:"schema_version"`
		Decision      string          `json:"decision"`
		AccountID     string          `json:"account_id"`
		Reason        string          `json:"reason"`
		Budget        json.RawMessage `json:"budget"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return s.errResult(&toolError{Code: CodeBadResponse,
			Message: "the control API admission response is not an object"}), nil
	}
	if doc.Decision != "allow" && doc.Decision != "deny" {
		return s.errResult(&toolError{Code: CodeBadResponse,
			Message: "the control API admission response carries no allow/deny decision"}), nil
	}
	upstreamSchema, te := decodeSchemaVersion(doc.SchemaVersion)
	if te != nil {
		return s.errResult(te), nil
	}
	res := admissionResult{
		Source:    sourceInfo{Endpoint: pathAdmit, UpstreamSchemaVersion: upstreamSchema},
		Decision:  doc.Decision,
		Admitted:  doc.Decision == "allow",
		AccountID: doc.AccountID,
		Reason:    doc.Reason,
	}
	budget := bytes.TrimSpace(doc.Budget)
	if len(budget) > 0 && budget[0] == '{' {
		res.Budget = doc.Budget
	}
	if te := s.fitAdmission(&res); te != nil {
		return s.errResult(te), nil
	}
	return s.finish(res)
}

// fitAdmission stamps the retrieval metadata and then bounds the whole
// serialized result. An admission document that cannot fit is reported as a
// bounded error: the decision and reason are never silently truncated.
func (s *Server) fitAdmission(res *admissionResult) *toolError {
	res.Retrieval = s.retrievalAt(s.now().Format(time.RFC3339Nano), 0)
	r, te := newResult(res)
	if te != nil {
		return te
	}
	if resultBytes(r) <= s.cfg.MaxResultChars {
		return nil
	}
	return &toolError{Code: CodeBadResponse,
		Message: fmt.Sprintf("the control API admission response does not fit in the %d character result budget", s.cfg.MaxResultChars)}
}
