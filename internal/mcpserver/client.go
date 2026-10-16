package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Paths of the LocalRouter control API this adapter forwards to. They are
// compile-time constants: a tool argument can never choose a URL or a path.
const (
	pathStatus = "/control/v1/status"
	pathUsage  = "/control/v1/usage"
	pathAdmit  = "/control/v1/admit"
)

// upstream is a minimal HTTP client for one configured control base URL. It
// refuses redirects (net/http would re-send the Authorization header to the
// redirect target, leaking the client key to a host the operator never chose),
// never uses an environment proxy (see noProxyTransport), and caps how much of
// a body it will read.
type upstream struct {
	base     string
	key      string // never logged, never reflected into a result or error
	client   *http.Client
	maxBytes int64
}

// noProxyTransport returns an explicit transport that keeps the sane dial and
// TLS defaults of http.DefaultTransport but never consults the environment for
// a proxy. The adapter already refuses redirects so the bearer key is never
// re-sent to a host the operator did not choose; a nil Transport would inherit
// http.DefaultTransport's http.ProxyFromEnvironment and silently route that
// same key through HTTP_PROXY/HTTPS_PROXY. The global default transport is left
// untouched (it is cloned, not mutated).
func noProxyTransport() *http.Transport {
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		tr := def.Clone()
		tr.Proxy = nil
		return tr
	}
	return &http.Transport{Proxy: nil}
}

func newUpstream(base, key string, timeout time.Duration) *upstream {
	return &upstream{
		base: base,
		key:  key,
		client: &http.Client{
			Timeout:       timeout,
			Transport:     noProxyTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		maxBytes: MaxResponseBytes,
	}
}

// get performs a GET and returns the raw JSON object body.
func (u *upstream) get(ctx context.Context, path string, q url.Values) (json.RawMessage, *toolError) {
	return u.do(ctx, http.MethodGet, path, q, nil)
}

// post performs a POST of the given JSON body and returns the raw JSON object
// body.
func (u *upstream) post(ctx context.Context, path string, body []byte) (json.RawMessage, *toolError) {
	return u.do(ctx, http.MethodPost, path, nil, body)
}

func (u *upstream) do(ctx context.Context, method, path string, q url.Values, body []byte) (json.RawMessage, *toolError) {
	target := u.base + path
	if len(q) > 0 {
		// Only fixed keys whose values this package validates are ever added.
		target += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, &toolError{Code: CodeBadResponse, Message: "could not build the control API request"}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if u.key != "" {
		req.Header.Set("Authorization", "Bearer "+u.key)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, classifyTransport(err, u.base)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, u.maxBytes+1))
	if err != nil {
		// The cause can quote the request URL; keep the message fixed instead.
		return nil, &toolError{Code: CodeBadResponse, Message: "reading the control API response failed"}
	}
	if int64(len(raw)) > u.maxBytes {
		return nil, &toolError{Code: CodeBadResponse,
			Message: fmt.Sprintf("the control API response exceeded the %d byte cap", u.maxBytes)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode, raw)
	}
	return validateObject(raw)
}

// validateObject rejects a 200 response that is not a well-formed JSON object,
// so a malformed upstream body can never masquerade as a successful result.
func validateObject(raw []byte) (json.RawMessage, *toolError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &toolError{Code: CodeBadResponse, Message: "the control API returned an empty response body"}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, &toolError{Code: CodeBadResponse, Message: "the control API returned a body that is not a JSON object"}
	}
	return json.RawMessage(raw), nil
}

// statusError maps a non-200 control API status onto a stable code. The
// upstream message text is never copied into the result: only the two
// capability refusal type names, which are part of the control API contract,
// pass through.
func statusError(status int, body []byte) *toolError {
	switch {
	case status == http.StatusUnauthorized:
		return &toolError{Code: CodeAuthRequired, Message: "the control API requires a valid client key (pass --key-file)"}
	case status == http.StatusForbidden:
		return &toolError{Code: CodeForbidden, Message: "the control API refused this client key"}
	case status == http.StatusNotFound:
		return &toolError{Code: CodeNotFound, Message: "the control API reports the requested account or model is unknown"}
	case status == http.StatusMethodNotAllowed, status == http.StatusNotImplemented,
		status == http.StatusServiceUnavailable, status == http.StatusBadGateway,
		status == http.StatusGatewayTimeout:
		return &toolError{Code: CodeEndpointUnavailable,
			Message: fmt.Sprintf("the control API cannot serve this request now (HTTP %d)", status)}
	case status >= 300 && status < 400:
		return &toolError{Code: CodeBadResponse,
			Message: fmt.Sprintf("the control API returned a redirect (HTTP %d); redirects are not followed", status)}
	}
	var env struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil {
		switch ErrorCode(env.Error.Type) {
		case CodeCapabilityUnsupported:
			return &toolError{Code: CodeCapabilityUnsupported,
				Message: "the router refused the request: no configured account can serve the declared capabilities",
				Detail:  env.Error.Type}
		case CodeCapabilityRequirementsRequired:
			return &toolError{Code: CodeCapabilityRequirementsRequired,
				Message: "the router requires an explicit capability profile for this route",
				Detail:  env.Error.Type}
		}
	}
	return &toolError{Code: CodeBadResponse, Message: fmt.Sprintf("the control API returned HTTP %d", status)}
}

// classifyTransport maps a transport failure onto server_timeout or
// server_unreachable. Only the wrapped cause is used, never the url.Error
// string, so a URL can never be reflected back into a result.
func classifyTransport(err error, base string) *toolError {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() || errors.Is(ue.Err, context.DeadlineExceeded) {
			return &toolError{Code: CodeServerTimeout,
				Message: fmt.Sprintf("the control API at %s did not answer before the deadline", base)}
		}
		if errors.Is(ue.Err, context.Canceled) {
			return &toolError{Code: CodeServerTimeout, Message: "the control API request was cancelled"}
		}
		cause := "the connection failed"
		if ue.Err != nil {
			cause = ue.Err.Error()
		}
		return &toolError{Code: CodeServerUnreachable,
			Message: fmt.Sprintf("the control API is unreachable at %s: %s", base, cause)}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &toolError{Code: CodeServerTimeout,
			Message: fmt.Sprintf("the control API at %s did not answer before the deadline", base)}
	}
	return &toolError{Code: CodeServerUnreachable,
		Message: fmt.Sprintf("the control API is unreachable at %s", base)}
}
