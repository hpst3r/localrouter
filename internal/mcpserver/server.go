package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ControlSchemaVersion is the control API document version this adapter was
// written against. It is only ever reported as the endpoint's *own* reported
// version (see sourceInfo): a missing or malformed upstream version is surfaced
// as null, never silently replaced with this constant.
const ControlSchemaVersion = 1

// Server is the MCP adapter. It holds one configured control endpoint and no
// router state whatsoever: no config, no database, no ledger, no provider
// credentials.
type Server struct {
	cfg Config
	up  *upstream
	mcp *mcp.Server
	now func() time.Time
}

// New validates cfg, reads the client key file (the only file this program
// reads) and builds the three-tool MCP server.
func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	key, err := readKeyFile(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg,
		up:  newUpstream(cfg.URL, key, cfg.Timeout),
		now: func() time.Time { return time.Now().UTC() },
	}
	s.mcp = mcp.NewServer(&mcp.Implementation{
		Name:        ImplementationName,
		Title:       "LocalRouter control API",
		Version:     ImplementationVersion,
		Description: "Read-only access to a running LocalRouter's quota status, usage summary and admission dry run.",
	}, nil)
	s.registerTools()
	return s, nil
}

// MCPServer exposes the underlying SDK server so a host (or a test) can connect
// it to any transport.
func (s *Server) MCPServer() *mcp.Server { return s.mcp }

// Run serves the MCP protocol on stdio until ctx is cancelled. Nothing but
// JSON-RPC is written to stdout; diagnostics go to stderr by the caller.
func (s *Server) Run(ctx context.Context) error {
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// requestContext bounds one tool call by the configured timeout.
func (s *Server) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.Timeout)
}

// ---- result shapes -------------------------------------------------------

// retrievalInfo records when and how a result was produced. schema_version is
// the control document version the adapter understands; source carries the
// version the endpoint itself reported.
type retrievalInfo struct {
	RetrievedAt    string `json:"retrieved_at"`
	Truncated      bool   `json:"truncated"`
	DroppedItems   int    `json:"dropped_items"`
	MaxResultChars int    `json:"max_result_chars"`
}

// sourceInfo names the endpoint a result came from and the schema version the
// endpoint itself reported. UpstreamSchemaVersion is null when the endpoint
// reports no (or an unusable) version: the adapter never invents one.
type sourceInfo struct {
	Endpoint              string `json:"endpoint"`
	UpstreamSchemaVersion *int   `json:"upstream_schema_version"`
}

type statusCounts struct {
	AccountsReturned int `json:"accounts_returned"`
	AccountsTotal    int `json:"accounts_total"`
	ClientsReturned  int `json:"clients_returned"`
	ClientsTotal     int `json:"clients_total"`
}

type usageCounts struct {
	RowsReturned  int  `json:"rows_returned"`
	RowsTotal     int  `json:"rows_total"`
	LimitApplied  int  `json:"limit_applied"`
	RowsTruncated bool `json:"rows_truncated"`
}

// statusResult mirrors GET /control/v1/status with every upstream field kept
// verbatim (now, accounts, clients) and the bounded arrays reported honestly.
type statusResult struct {
	Retrieval retrievalInfo     `json:"retrieval"`
	Source    sourceInfo        `json:"source"`
	Now       json.RawMessage   `json:"now,omitempty"`
	Accounts  []json.RawMessage `json:"accounts"`
	Clients   []json.RawMessage `json:"clients"`
	Counts    statusCounts      `json:"counts"`
}

// usageResult mirrors GET /control/v1/usage. Rows are carried verbatim, so the
// ledger's own cost_usd / unpriced_requests / unknown_usage_requests fields are
// neither dropped nor re-derived.
type usageResult struct {
	Retrieval retrievalInfo     `json:"retrieval"`
	Source    sourceInfo        `json:"source"`
	Since     json.RawMessage   `json:"since,omitempty"`
	Group     string            `json:"group"`
	Rows      []json.RawMessage `json:"rows"`
	Counts    usageCounts       `json:"counts"`
	CostNote  string            `json:"cost_note"`
}

// costNote is attached to every usage summary. cost_usd is a ledger amount, not
// an invoice; the control API does not expose the per-row pricing basis, and a
// grouped total may combine provider-reported and estimated costs. Only a
// subscription account's API-equivalent figure is categorically not money
// spent. Requests with no known price are counted in unpriced_requests;
// requests whose token usage was not metered are counted, separately, in
// unknown_usage_requests.
const costNote = "cost_usd values are LocalRouter ledger amounts, priced from the basis recorded for " +
	"each account; they are not invoices and not actual spend. The control API does not expose the " +
	"per-row pricing basis, and a grouped total may mix provider-reported and estimated costs. For a " +
	"subscription account the API-equivalent figure is not money spent. Requests with no known price " +
	"are counted in unpriced_requests; requests whose token usage was not metered are counted " +
	"separately in unknown_usage_requests."

// admissionResult mirrors POST /control/v1/admit. A deny is a valid decision,
// not a tool failure: it is reported with isError=false.
type admissionResult struct {
	Retrieval retrievalInfo   `json:"retrieval"`
	Source    sourceInfo      `json:"source"`
	Decision  string          `json:"decision"`
	Admitted  bool            `json:"admitted"`
	AccountID string          `json:"account_id,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Budget    json.RawMessage `json:"budget,omitempty"`
}

// ---- result helpers ------------------------------------------------------

func (s *Server) retrievalAt(at string, dropped int) retrievalInfo {
	return retrievalInfo{
		RetrievedAt:    at,
		Truncated:      dropped > 0,
		DroppedItems:   dropped,
		MaxResultChars: s.cfg.MaxResultChars,
	}
}

// buildResult wraps an already-encoded document exactly as it goes on the wire:
// the same bytes as the JSON text content and as the structured content.
func buildResult(doc []byte, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError:           isError,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(doc)}},
		StructuredContent: json.RawMessage(doc),
	}
}

// resultBytes is the exact size in bytes of the CallToolResult the host
// receives as the `result` member of the tools/call response: both copies of the
// document (the text copy JSON-escaped) plus the result's own metadata. It
// measures the result object only, excluding the JSON-RPC/transport envelope
// that carries it. The result budget is enforced against this, never against
// the bare document.
func resultBytes(res *mcp.CallToolResult) int {
	b, err := json.Marshal(res)
	if err != nil {
		return 1 << 30
	}
	return len(b)
}

func newResult(v any) (*mcp.CallToolResult, *toolError) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &toolError{Code: CodeBadResponse, Message: "the result could not be encoded"}
	}
	return buildResult(b, false), nil
}

// errResult builds a failed tool call whose serialized result also fits the
// result budget. A message can quote an unbounded caller-supplied string (an
// unknown argument name, an account id) or a long upstream field, so the whole
// result — both copies plus the envelope — is bounded, truncating the message
// rather than blowing the budget.
func (s *Server) errResult(te *toolError) *mcp.CallToolResult {
	render := func(msg, detail string) *mcp.CallToolResult {
		b, err := json.Marshal(errorContent{Error: errorBody{Code: te.Code, Message: msg, Detail: detail}})
		if err != nil {
			b = []byte(`{"error":{"code":"bad_response","message":"the error could not be encoded"}}`)
		}
		return buildResult(b, true)
	}
	if res := render(te.Message, te.Detail); resultBytes(res) <= s.cfg.MaxResultChars {
		return res
	}
	const marker = " [truncated to fit the result budget]"
	// Search over rune boundaries, never raw byte offsets: the largest prefix
	// that still fits is found by binary search on the rune slice, so the
	// truncated message is always valid UTF-8. A byte-index search that rounds a
	// midpoint down to a rune boundary can land it back on lo while hi == lo+1,
	// so lo never advances and the loop spins forever.
	runes := []rune(te.Message)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2 // strictly greater than lo, so lo always advances
		if resultBytes(render(string(runes[:mid])+marker, "")) <= s.cfg.MaxResultChars {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	res := render(string(runes[:lo])+marker, "")
	if resultBytes(res) > s.cfg.MaxResultChars {
		res = render(string(te.Code), "")
	}
	return res
}

// finish marshals a bounded result document, degrading to a bounded error if
// even the fully-truncated document somehow overflows.
func (s *Server) finish(v any) (*mcp.CallToolResult, error) {
	res, te := newResult(v)
	if te != nil {
		return s.errResult(te), nil
	}
	if resultBytes(res) > s.cfg.MaxResultChars {
		return s.errResult(&toolError{Code: CodeBadResponse,
			Message: fmt.Sprintf("the result does not fit in the %d character result budget", s.cfg.MaxResultChars)}), nil
	}
	return res, nil
}

// fitStatus bounds doc.Accounts then doc.Clients so that the serialized
// CallToolResult — including its retrieval metadata, counts and both document
// copies — fits the configured budget. Retrieval is stamped and the counts are
// final before each measurement, so the measured bytes are the emitted bytes.
func (s *Server) fitStatus(doc *statusResult) *toolError {
	allAccounts, allClients := doc.Accounts, doc.Clients
	doc.Counts.AccountsTotal = len(allAccounts)
	doc.Counts.ClientsTotal = len(allClients)
	at := s.now().Format(time.RFC3339Nano)
	n := len(allAccounts) + len(allClients)

	set := func(k int) {
		ka := k
		if ka > len(allAccounts) {
			ka = len(allAccounts)
		}
		kc := k - ka
		doc.Accounts = allAccounts[:ka]
		doc.Clients = allClients[:kc]
		doc.Counts.AccountsReturned = ka
		doc.Counts.ClientsReturned = kc
		dropped := (len(allAccounts) - ka) + (len(allClients) - kc)
		doc.Retrieval = s.retrievalAt(at, dropped)
	}
	fits := func(k int) bool {
		set(k)
		res, err := newResult(doc)
		return err == nil && resultBytes(res) <= s.cfg.MaxResultChars
	}
	if !fits(0) {
		return &toolError{Code: CodeBadResponse,
			Message: fmt.Sprintf("the control API status document does not fit in the %d character result budget", s.cfg.MaxResultChars)}
	}
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	set(lo)
	return nil
}

// fitRows bounds res.Rows so that the serialized CallToolResult fits the budget.
// The dropped count covers both the requested limit and the byte cap.
func (s *Server) fitRows(res *usageResult, total int) *toolError {
	all := res.Rows
	at := s.now().Format(time.RFC3339Nano)
	res.Counts.RowsTotal = total

	set := func(k int) {
		res.Rows = all[:k]
		dropped := total - k
		res.Counts.RowsReturned = k
		res.Counts.RowsTruncated = dropped > 0
		res.Retrieval = s.retrievalAt(at, dropped)
	}
	fits := func(k int) bool {
		set(k)
		r, err := newResult(res)
		return err == nil && resultBytes(r) <= s.cfg.MaxResultChars
	}
	if !fits(0) {
		return &toolError{Code: CodeBadResponse,
			Message: fmt.Sprintf("the control API usage document does not fit in the %d character result budget", s.cfg.MaxResultChars)}
	}
	lo, hi := 0, len(all)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	set(lo)
	return nil
}
