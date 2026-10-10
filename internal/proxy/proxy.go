package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/routing"
)

const (
	pathResponses = "/v1/responses"
	pathChat      = "/v1/chat/completions"
	pathModels    = "/v1/models"

	defaultMaxBodyBytes = 32 << 20
	defaultMaxFailovers = 2

	defaultResponseHeaderTimeout = 180 * time.Second
	defaultStreamIdleTimeout     = 300 * time.Second
	dialTimeout                  = 15 * time.Second
	tlsHandshakeTimeout          = 15 * time.Second
)

// Deps are the collaborators the proxy needs. All interface fields are required
// except Clock and Logger, which default to core.SystemClock and slog.Default().
type Deps struct {
	Accounts     map[string]core.Account
	Routes       []core.Route
	Creds        core.CredentialSource
	Quota        core.QuotaSource
	Policy       core.Policy
	Ledger       core.Ledger
	Authenticate func(bearer string) (core.Client, bool)
	Clock        core.Clock
	Logger       *slog.Logger
	// Limiter bounds concurrent inference requests. It is consulted after
	// authentication and before the request body is read, and the slot is held
	// until the response (including any stream and every failover attempt) has
	// fully ended. Nil means unlimited.
	Limiter Limiter
	// Budget gates every upstream attempt against the client's and account's
	// configured spend ceilings. It is optional: nil admits every attempt and
	// settles nothing, exactly as before this seam existed.
	//
	// Reserve is called once per upstream attempt, after the credential is
	// resolved and immediately before the send (so a credential failure never
	// holds budget), and the matching Settle is called once on every path that
	// follows a successful Reserve. A Reserve error is terminal: the attempt is
	// not sent, does not fail over, and is not settled.
	Budget Budget
}

// Budget gates one upstream attempt against the budget store. It is satisfied by
// internal/budget.Gate; the proxy depends on the behaviour rather than the
// concrete type so it never constructs the store or knows the money arithmetic.
//
// Implementations must be safe for concurrent use and must not retain rec.
type Budget interface {
	// Reserve claims this attempt's fixed reservation. An error wrapping
	// budget.ErrExceeded denies the attempt because the client is out of
	// budget; any other error means the budget store is unavailable. Either
	// way the attempt must not be sent and must not be settled.
	Reserve(ctx context.Context, rec core.RequestRecord) error
	// Settle books the attempt's resolved cost once its outcome is known. It
	// must be called only for an attempt Reserve admitted.
	Settle(ctx context.Context, rec core.RequestRecord) error
	// SettleIncomplete settles an admitted attempt whose response was cut
	// short mid-relay (client disconnect, idle timeout, upstream read error).
	// Any cost observed so far is only a lower bound, so it must be booked as
	// unknown and charged at no less than the hold — never as a final cost.
	SettleIncomplete(ctx context.Context, rec core.RequestRecord) error
}

// Limiter bounds how many inference requests may be active at once. It is
// satisfied by internal/connlim.Controller; the proxy depends on the behaviour
// rather than the concrete type so the limit lives at the inbound edge.
type Limiter interface {
	// Acquire reserves a slot for client, reporting false when the limit is
	// saturated. The returned release must be called exactly once however the
	// request ends.
	Acquire(client string) (release func(), ok bool)
}

// Options tune proxy behaviour.
type Options struct {
	// MaxFailovers is the number of additional accounts tried after a
	// retryable upstream failure. 0 means the default (2); negative disables.
	MaxFailovers int
	// HTTPClient performs upstream requests. It must not set an overall
	// Timeout (responses stream). Nil uses a client that does not follow
	// redirects, with 15s dial and TLS handshake timeouts and
	// ResponseHeaderTimeout.
	HTTPClient *http.Client
	// MaxBodyBytes limits the client request body. 0 means 32 MiB.
	MaxBodyBytes int64
	// ResponseHeaderTimeout bounds the wait for upstream response headers
	// when HTTPClient is nil. 0 means 180s; negative disables.
	ResponseHeaderTimeout time.Duration
	// StreamIdleTimeout aborts a response body relay when no upstream bytes
	// arrive for this long. 0 means 300s; negative disables.
	StreamIdleTimeout time.Duration
}

// Proxy is the inference HTTP surface. Create it with New.
type Proxy struct {
	deps   Deps
	opts   Options
	routes map[string]core.Route // by client-facing model name
	models []string              // sorted
	log    *slog.Logger
	clock  core.Clock

	invMu          sync.Mutex
	lastInvalidate map[string]time.Time // by account; see allowInvalidate
}

// New builds a Proxy from its dependencies and options.
func New(deps Deps, opts Options) *Proxy {
	if opts.MaxFailovers == 0 {
		opts.MaxFailovers = defaultMaxFailovers
	} else if opts.MaxFailovers < 0 {
		opts.MaxFailovers = 0
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	if opts.ResponseHeaderTimeout == 0 {
		opts.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	} else if opts.ResponseHeaderTimeout < 0 {
		opts.ResponseHeaderTimeout = 0
	}
	if opts.StreamIdleTimeout == 0 {
		opts.StreamIdleTimeout = defaultStreamIdleTimeout
	} else if opts.StreamIdleTimeout < 0 {
		opts.StreamIdleTimeout = 0
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = defaultHTTPClient(opts.ResponseHeaderTimeout)
	}
	p := &Proxy{deps: deps, opts: opts, routes: map[string]core.Route{}, log: deps.Logger, clock: deps.Clock,
		lastInvalidate: map[string]time.Time{}}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.clock == nil {
		p.clock = core.SystemClock{}
	}
	for _, r := range deps.Routes {
		for _, m := range r.Models {
			if _, dup := p.routes[m]; !dup {
				p.models = append(p.models, m)
			}
			p.routes[m] = r
		}
	}
	sort.Strings(p.models)
	return p
}

// defaultHTTPClient clones http.DefaultTransport with bounded dial, TLS and
// response-header waits. It has no overall Timeout because responses stream.
func defaultHTTPClient(responseHeaderTimeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = tlsHandshakeTimeout
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Handler returns the HTTP handler for /v1/responses, /v1/chat/completions
// and /v1/models. Other paths return 404.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathResponses, func(w http.ResponseWriter, r *http.Request) { p.serveInference(w, r, "/responses") })
	mux.HandleFunc(pathChat, func(w http.ResponseWriter, r *http.Request) { p.serveInference(w, r, "/chat/completions") })
	mux.HandleFunc(pathModels, p.serveModels)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "localrouter: not found", "invalid_request_error")
	})
	return mux
}

func (p *Proxy) authenticate(r *http.Request) (core.Client, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) || p.deps.Authenticate == nil {
		return core.Client{}, false
	}
	return p.deps.Authenticate(strings.TrimSpace(h[len(prefix):]))
}

func (p *Proxy) serveModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "localrouter: method not allowed", "invalid_request_error")
		return
	}
	if _, ok := p.authenticate(r); !ok {
		writeUnauthorized(w)
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}
	for _, m := range p.models {
		out.Data = append(out.Data, model{ID: m, Object: "model", OwnedBy: "localrouter"})
	}
	writeJSON(w, http.StatusOK, out)
}

// request is one parsed downstream inference request.
type request struct {
	endpoint   string // "/responses" or "/chat/completions"
	client     core.Client
	class      core.Class
	model      string
	stream     bool // client asked for a streaming response
	route      core.Route
	candidates []string
	// body is the client body exactly as read. It is never mutated: every
	// attempt builds its own copy with its own model rewrite (and Codex
	// normalisation) in (*Proxy).send, so no rewrite can leak between attempts.
	body    []byte
	header  http.Header // original client headers
	session string
	task    string
	agent   string
}

func (p *Proxy) serveInference(w http.ResponseWriter, r *http.Request, endpoint string) {
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

	// Concurrency admission happens after authentication (unknown clients
	// cannot burn slots) and before the body is read (a rejected request does
	// no work). The slot is held until forward returns, i.e. for the whole
	// response including streams, aborts and every failover attempt.
	if p.deps.Limiter != nil {
		release, ok := p.deps.Limiter.Acquire(client.Name)
		if !ok {
			writeConcurrencyLimit(w, r)
			return
		}
		defer release()
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
	candidates := route.Interactive
	if class == core.ClassBackground {
		candidates = route.Background
	}
	if endpoint == "/chat/completions" {
		// Codex accounts only serve /responses; drop them before admission.
		var keep []string
		for _, id := range candidates {
			if p.deps.Accounts[id].Provider != core.ProviderCodex {
				keep = append(keep, id)
			}
		}
		if len(keep) == 0 && len(candidates) > 0 {
			writeError(w, http.StatusBadRequest, "codex accounts only serve /v1/responses", "invalid_request_error")
			return
		}
		candidates = keep
	}
	// parseHead accepted only a single JSON object without duplicate top-level
	// keys, so the per-attempt rewriteBody in (*Proxy).send cannot fail on it
	// after a lease is taken.

	// Capability-aware routing. A route with no per-candidate descriptors is
	// unconstrained: it keeps the legacy body and byte semantics exactly. A route
	// that declares candidate capabilities fails closed, in this order, before
	// any lease or upstream call:
	//
	//   - a body that is a JSON object but whose content cannot be classified
	//     (unknown or malformed content part, malformed structured-output
	//     declaration) is a 400 invalid_request_error — never a silent downgrade
	//     to text-only;
	//   - a request no candidate can serve is a 400 capability_unsupported. The
	//     filter only removes candidates, so the surviving order is the operator's
	//     consent order and failover stays inside it.
	if len(route.Upstreams) > 0 {
		reqs, err := routing.Infer(protocolFor(endpoint), body)
		if err != nil {
			writeError(w, http.StatusBadRequest,
				"localrouter: request body cannot be classified for this route",
				"invalid_request_error")
			return
		}
		filtered := routing.Filter(route, candidates, reqs)
		if len(filtered) > 0 {
			candidates = filtered
		} else if len(candidates) > 0 {
			p.log.Info("capability filter excluded every candidate",
				"client", client.Name, "class", class, "model", head.Model, "protocol", reqs.Protocol)
			writeError(w, http.StatusBadRequest,
				"localrouter: no account supports the capabilities this request needs",
				"capability_unsupported")
			return
		}
	}

	p.forward(w, r, &request{
		endpoint:   endpoint,
		client:     client,
		class:      class,
		model:      head.Model,
		stream:     head.Stream,
		route:      route,
		candidates: candidates,
		body:       body,
		header:     r.Header,
		session:    core.TruncateLabel(r.Header.Get("X-LocalRouter-Session")),
		task:       core.TruncateLabel(r.Header.Get("X-LocalRouter-Task")),
		agent:      core.TruncateLabel(r.Header.Get("X-LocalRouter-Agent")),
	})
}

// requestHead is the routing-relevant part of a request body.
type requestHead struct {
	Model  string
	Stream bool
}

var errBodyShape = errors.New("request body must be a single JSON object with a model")

// parseHead reads "model" and "stream" from body with the exact-key,
// case-sensitive semantics the upstream uses, and rejects any body the
// upstream could read differently: anything but a single UTF-8 JSON object,
// duplicate top-level keys, or a top-level key that matches "model"/"stream"
// only case-insensitively (encoding/json struct decoding would match it).
// model must be a non-empty string and stream, if present, a boolean or null
// (false). Nested values are only checked for syntax. A body that passes is unambiguous, so
// it can be forwarded unchanged.
func parseHead(body []byte) (requestHead, error) {
	var h requestHead
	if !utf8.Valid(body) {
		return h, errors.New("request body must be UTF-8 JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return h, errBodyShape
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return h, errBodyShape
		}
		key, _ := tok.(string)
		if seen[key] {
			return h, fmt.Errorf("duplicate key %q in request body", core.TruncateLabel(key))
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return h, errBodyShape
		}
		switch {
		case key == "model":
			if raw[0] != '"' || json.Unmarshal(raw, &h.Model) != nil {
				return h, errors.New("model must be a string")
			}
		case key == "stream":
			// null is an omitted optional (SDKs send it) and means false.
			if (raw[0] != 't' && raw[0] != 'f' && raw[0] != 'n') || json.Unmarshal(raw, &h.Stream) != nil {
				return h, errors.New("stream must be a boolean or null")
			}
		case strings.EqualFold(key, "model"), strings.EqualFold(key, "stream"):
			return h, fmt.Errorf("ambiguous key %q in request body", core.TruncateLabel(key))
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return h, errBodyShape
	}
	if _, err := dec.Token(); err != io.EOF {
		return h, errBodyShape
	}
	if h.Model == "" {
		return h, errBodyShape
	}
	return h, nil
}

// protocolFor maps an inference endpoint to the routing protocol name.
func protocolFor(endpoint string) string {
	if endpoint == "/chat/completions" {
		return routing.ProtocolChat
	}
	return routing.ProtocolResponses
}

// rewriteBody applies the only permitted body edits: the upstream model name
// and, for streaming chat completions, stream_options.include_usage=true.
// When no edit is needed the body is returned unchanged. body must have
// passed parseHead (no duplicate keys), so the map round trip loses nothing.
func rewriteBody(body []byte, upstreamModel string, forceUsage bool) ([]byte, error) {
	if upstreamModel == "" && !forceUsage {
		return body, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if upstreamModel != "" {
		m, err := marshalNoEscape(upstreamModel)
		if err != nil {
			return nil, err
		}
		obj["model"] = m
	}
	if forceUsage {
		opts := map[string]json.RawMessage{}
		if raw, ok := obj["stream_options"]; ok {
			// A non-object stream_options is replaced.
			_ = json.Unmarshal(raw, &opts)
			if opts == nil {
				opts = map[string]json.RawMessage{}
			}
		}
		opts["include_usage"] = json.RawMessage("true")
		raw, err := marshalNoEscape(opts)
		if err != nil {
			return nil, err
		}
		obj["stream_options"] = raw
	}
	return marshalNoEscape(obj)
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type,omitempty"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	var b errorBody
	b.Error.Message, b.Error.Type = msg, typ
	writeJSON(w, status, b)
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="localrouter"`)
	writeError(w, http.StatusUnauthorized, "localrouter: invalid or missing client key", "invalid_request_error")
}

// concurrencyRetryAfter is the Retry-After hint for a saturated concurrency
// limit. It is short: a slot frees as soon as any in-flight request finishes.
const concurrencyRetryAfter = "1"

// writeConcurrencyLimit rejects a request refused by the concurrency limiter.
// The error names the concurrency limit explicitly (type and message) so it is
// never mistaken for a quota/admission rejection, and advertises Retry-After.
func writeConcurrencyLimit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", concurrencyRetryAfter)
	writeError(w, http.StatusTooManyRequests,
		"localrouter: concurrency limit reached; retry shortly",
		"concurrency_limit_exceeded")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
