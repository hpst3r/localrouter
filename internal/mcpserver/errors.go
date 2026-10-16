package mcpserver

import "fmt"

// ErrorCode is a stable, machine-branchable failure code. Hosts branch on
// these values; the human-readable message may change.
type ErrorCode string

const (
	// CodeInvalidArguments: the tool call itself was rejected before any
	// request left the process (unknown field, wrong type, out-of-range value).
	CodeInvalidArguments ErrorCode = "invalid_arguments"
	// CodeAuthRequired: the control API rejected or did not receive a client
	// key (HTTP 401).
	CodeAuthRequired ErrorCode = "auth_required"
	// CodeForbidden: the control API refused the key (HTTP 403).
	CodeForbidden ErrorCode = "forbidden"
	// CodeServerUnreachable: the control API could not be reached at all.
	CodeServerUnreachable ErrorCode = "server_unreachable"
	// CodeServerTimeout: the request did not complete before its deadline.
	CodeServerTimeout ErrorCode = "server_timeout"
	// CodeBadResponse: the control API answered with a status or body the
	// adapter cannot interpret, including a redirect it refuses to follow and
	// a body that exceeds the response cap.
	CodeBadResponse ErrorCode = "bad_response"
	// CodeEndpointUnavailable: the endpoint exists but cannot serve the
	// request now (HTTP 503, e.g. an unavailable ledger).
	CodeEndpointUnavailable ErrorCode = "endpoint_unavailable"
	// CodeNotFound: the control API reports the named account or model is
	// unknown (HTTP 404).
	CodeNotFound ErrorCode = "not_found"
	// CodeCapabilityUnsupported and CodeCapabilityRequirementsRequired are the
	// control API's own stable capability refusal types, passed through
	// verbatim so a caller can tell a capability refusal from a transport
	// failure. They are contract names, not upstream message text.
	CodeCapabilityUnsupported          ErrorCode = "capability_unsupported"
	CodeCapabilityRequirementsRequired ErrorCode = "capability_requirements_required"
)

// toolError is a failure reported to the MCP client. Every field is either a
// fixed string or derived from a status code: no bearer key, no URL userinfo
// and no raw upstream error body is ever reflected.
type toolError struct {
	Code    ErrorCode
	Message string
	Detail  string
}

func (e *toolError) Error() string { return string(e.Code) + ": " + e.Message }

// errorContent is the structured shape of a failed tool call. The JSON text
// content and the structured content of an error result are byte-identical.
type errorContent struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Detail  string    `json:"detail,omitempty"`
}

func invalidArg(format string, args ...any) *toolError {
	return &toolError{Code: CodeInvalidArguments, Message: fmt.Sprintf(format, args...)}
}
