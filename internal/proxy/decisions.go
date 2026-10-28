package proxy

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/routing"
)

// Typed-decision endpoints (docs/SPEC.md "Typed decisions"). The request
// endpoint strings double as the upstream path suffix for /systemone; the
// decisions upstream URL is derived by decisionsURL.
const (
	pathSystemOne = "/v1/systemone"
	pathDecisions = "/v1/decisions"

	endpointSystemOne = "/systemone"
	endpointDecisions = "/decisions"
)

// Error types for LocalRouter-originated decision rejections.
const (
	errTypeDecisionUnsupported = "decision_unsupported"
	errTypeDecisionBounds      = "decision_bounds_exceeded"
	errTypeCapability          = "capability_unsupported"
)

// registerDecisions mounts both decision paths when at least one route serves
// a decision protocol. With none, they fall through to the 404 handler exactly
// as before the feature existed.
func (p *Proxy) registerDecisions(mux *http.ServeMux) {
	for _, r := range p.deps.Routes {
		if routing.RouteDecisionProtocol(r) != "" {
			mux.HandleFunc(pathSystemOne, func(w http.ResponseWriter, r *http.Request) {
				p.serveDecision(w, r, endpointSystemOne, routing.ProtocolSystemOne)
			})
			mux.HandleFunc(pathDecisions, func(w http.ResponseWriter, r *http.Request) {
				p.serveDecision(w, r, endpointDecisions, routing.ProtocolDecisions)
			})
			return
		}
	}
}

func isDecisionEndpoint(endpoint string) bool {
	return endpoint == endpointSystemOne || endpoint == endpointDecisions
}

// decisionFinal reports the upstream statuses that are final on a decision
// endpoint: relayed as the answer with no failover and no account cooldown.
// 400/404/413/422 depend on the request or the pinned model, which every
// candidate shares; 529 is provider-wide overload, so replaying it on another
// key only multiplies load. Every other status keeps the OpenRouter rules.
func decisionFinal(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity, 529:
		return true
	}
	return false
}

// decisionsURL derives the Decisions API URL from an OpenRouter base_url by
// replacing its final /v1 path segment with /alpha/decisions
// (https://openrouter.ai/api/v1 -> https://openrouter.ai/api/alpha/decisions).
// Config rejects a decisions candidate whose base_url path does not end in /v1.
func decisionsURL(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1") + "/alpha/decisions"
}

// forwardDecisionHeaders adds the one extra client header a decision attempt
// may carry: OpenRouter's x-session-id observability grouping header.
func forwardDecisionHeaders(dst, src http.Header) {
	if v, ok := src[http.CanonicalHeaderKey("x-session-id")]; ok {
		dst[http.CanonicalHeaderKey("x-session-id")] = append([]string(nil), v...)
	}
}

func writeDecisionEndpointMismatch(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest,
		"localrouter: this model's route does not serve this endpoint", errTypeCapability)
}

// serveDecision handles POST /v1/systemone and /v1/decisions. The front half
// (method, client auth, class, concurrency limit, body bound, strict head
// parse, route lookup) matches serveInference. It then requires the route to
// serve exactly this decision protocol, rejects unsupported encodings and
// features, classifies the body strictly with bounds, filters candidates by
// question type, and forwards through the shared attempt/failover loop.
func (p *Proxy) serveDecision(w http.ResponseWriter, r *http.Request, endpoint, protocol string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "localrouter: method not allowed", "invalid_request_error")
		return
	}
	client, ok := p.authenticate(r)
	if !ok {
		writeUnauthorized(w)
		return
	}
	class := client.Class
	if class == core.ClassInteractive && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-LocalRouter-Class")), string(core.ClassBackground)) {
		class = core.ClassBackground
	}
	if p.deps.Limiter != nil {
		release, ok := p.deps.Limiter.Acquire(client.Name)
		if !ok {
			writeConcurrencyLimit(w, r)
			return
		}
		defer release()
	}
	// The body validated here must be exactly the body sent upstream, so no
	// compressed or multipart body is accepted (images/assets are out of v1).
	if ce := strings.TrimSpace(r.Header.Get("Content-Encoding")); ce != "" {
		writeError(w, http.StatusBadRequest, "localrouter: Content-Encoding is not supported on decision endpoints", errTypeDecisionUnsupported)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, "localrouter: decision requests must be application/json", errTypeDecisionUnsupported)
			return
		}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.opts.MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "localrouter: request body too large", "invalid_request_error")
			return
		}
		writeError(w, http.StatusBadRequest, "localrouter: could not read request body", "invalid_request_error")
		return
	}
	head, err := parseHead(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "localrouter: "+err.Error(), "invalid_request_error")
		return
	}
	route, ok := p.routes[head.Model]
	if !ok {
		writeError(w, http.StatusNotFound, "localrouter: unknown model", "model_not_found")
		return
	}
	if routing.RouteDecisionProtocol(route) != protocol {
		writeDecisionEndpointMismatch(w)
		return
	}
	candidates := route.Interactive
	if class == core.ClassBackground {
		candidates = route.Background
	}

	reqs, err := routing.InferDecision(protocol, body, p.opts.DecisionBounds)
	if err != nil {
		writeDecisionError(w, err)
		return
	}
	filtered := routing.FilterDecision(route, candidates, reqs)
	if len(filtered) == 0 {
		p.log.Info("decision filter excluded every candidate",
			"client", client.Name, "class", class, "model", head.Model, "protocol", protocol)
		writeError(w, http.StatusBadRequest,
			"localrouter: no account supports the question types this request asks", errTypeCapability)
		return
	}

	p.forward(w, r, &request{
		endpoint:   endpoint,
		client:     client,
		class:      class,
		model:      head.Model,
		route:      route,
		candidates: filtered,
		body:       body,
		header:     r.Header,
		session:    core.TruncateLabel(r.Header.Get("X-LocalRouter-Session")),
		task:       core.TruncateLabel(r.Header.Get("X-LocalRouter-Task")),
		agent:      core.TruncateLabel(r.Header.Get("X-LocalRouter-Agent")),
	})
}

// writeDecisionError maps a routing.InferDecision error to its 400 type. The
// classifier's messages name fields and bounds only, never body content.
func writeDecisionError(w http.ResponseWriter, err error) {
	typ, sentinel := "invalid_request_error", routing.ErrMalformed
	switch {
	case errors.Is(err, routing.ErrDecisionUnsupported):
		typ, sentinel = errTypeDecisionUnsupported, routing.ErrDecisionUnsupported
	case errors.Is(err, routing.ErrDecisionBounds):
		typ, sentinel = errTypeDecisionBounds, routing.ErrDecisionBounds
	}
	msg := strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
	writeError(w, http.StatusBadRequest, "localrouter: "+msg, typ)
}
