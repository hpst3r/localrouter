package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/authz"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
)

// maxSessionBody bounds every /ui/v1 request body.
const maxSessionBody = 16 << 10

// Session API error types (error.type) the UI branches on.
const (
	errTypeUnavailable    = "unavailable"
	errTypeSessionMissing = "session_required"
	errTypeForbidden      = "forbidden"
	errTypeCSRF           = "csrf"
	errTypeNotFound       = "not_found"
	errTypeInvalid        = "invalid_request"
	errTypeKeyLimit       = "key_limit"
	errTypeReauth         = "reauth_required"
	errTypeSelfAction     = "self_action"
	errTypeConflict       = "conflict"
)

// sessionScope selects how a session route derives its data scope.
type sessionScope int

const (
	sessionNoScope sessionScope = iota
	sessionOwn                  // authz.Scope: the session user's own rows
	sessionGlobal               // authz.GlobalScope: every user (admin only)
)

// normalizeOrigin returns the canonical origin ("https://host[:port]") of a
// configured https public base URL, or "" when it is not one. It mirrors
// weblogin's normalization (lower-case, default port dropped) so the exact
// Origin comparison agrees with the login handlers.
func normalizeOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "443" {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		return "https://" + host + ":" + port
	}
	return "https://" + host
}

// isUnsafe reports whether method may change state (everything but GET/HEAD).
func isUnsafe(method string) bool {
	return method != http.MethodGet && method != http.MethodHead
}

// sessionAuth authenticates a /ui/v1 request by its session cookie only and
// authorizes action. It never accepts an Authorization header (mixing bearer
// and session credentials is refused outright) and never falls back to any
// other credential. Unsafe methods additionally need the exact configured
// Origin, Sec-Fetch-Site absent or same-origin, and the session's CSRF token.
// The session and the user's current role are re-read on every request.
func (s *Server) sessionAuth(action authz.Action, mode sessionScope, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if _, present := r.Header["Authorization"]; present {
			writeErrorCode(w, http.StatusBadRequest, "authorization header not accepted on session routes", errTypeInvalid)
			return
		}
		if s.deps.Identity == nil {
			writeErrorCode(w, http.StatusServiceUnavailable, "identity unavailable", errTypeUnavailable)
			return
		}
		unsafe := isUnsafe(r.Method)
		if unsafe && !s.sameOrigin(w, r) {
			return
		}
		token, ok := sessionCookie(r)
		if !ok {
			writeErrorCode(w, http.StatusUnauthorized, "session required", errTypeSessionMissing)
			return
		}
		sess, err := s.deps.Identity.AuthenticateSession(r.Context(), token)
		if err != nil {
			if errors.Is(err, core.ErrUnauthenticated) {
				writeErrorCode(w, http.StatusUnauthorized, "session required", errTypeSessionMissing)
				return
			}
			slog.Warn("control: session authentication unavailable")
			writeErrorCode(w, http.StatusServiceUnavailable, "identity unavailable", errTypeUnavailable)
			return
		}
		if unsafe {
			vals := r.Header.Values(csrfHeader)
			if len(vals) != 1 || !identity.ValidCSRF(token, vals[0]) {
				writeErrorCode(w, http.StatusForbidden, "invalid csrf token", errTypeCSRF)
				return
			}
		}
		p := sess.Principal
		if !authz.Allow(p, action) {
			writeErrorCode(w, http.StatusForbidden, msgForbidden, errTypeForbidden)
			return
		}
		a := requestAuth{principal: p, session: token}
		switch mode {
		case sessionOwn:
			sc, err := authz.Scope(p)
			if err != nil {
				writeErrorCode(w, http.StatusForbidden, msgForbidden, errTypeForbidden)
				return
			}
			a.scope = &sc
		case sessionGlobal:
			sc, err := authz.GlobalScope(p)
			if err != nil {
				writeErrorCode(w, http.StatusForbidden, msgForbidden, errTypeForbidden)
				return
			}
			a.scope = &sc
		}
		next(w, withAuth(r, a))
	})
}

// sameOrigin enforces the browser-origin checks of an unsafe session request
// and writes the refusal itself. The expected origin is the configured
// PublicBaseURL only; Host and forwarded headers are never trusted.
func (s *Server) sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if s.origin == "" {
		writeErrorCode(w, http.StatusServiceUnavailable, "public origin not configured", errTypeUnavailable)
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != s.origin {
		writeErrorCode(w, http.StatusForbidden, "cross-origin request refused", errTypeForbidden)
		return false
	}
	switch site := r.Header.Values("Sec-Fetch-Site"); {
	case len(site) == 0:
	case len(site) == 1 && site[0] == "same-origin":
	default:
		writeErrorCode(w, http.StatusForbidden, "cross-site request refused", errTypeForbidden)
		return false
	}
	return true
}

// sessionCookie returns the single session cookie of r. Zero or several
// cookies of that name are refused.
func sessionCookie(r *http.Request) (string, bool) {
	var val string
	n := 0
	for _, c := range r.Cookies() {
		if c.Name == sessionCookieName {
			val = c.Value
			n++
		}
	}
	return val, n == 1 && val != ""
}

// decodeSessionJSON strictly decodes a bounded single JSON object into dst:
// Content-Type application/json, at most maxSessionBody bytes, only dst's
// own members spelled exactly (encoding/json alone would match them
// case-insensitively), no member repeated in any object (encoding/json alone
// would keep the last) and no trailing data. It writes the 400/413 itself and
// never echoes the body.
func decodeSessionJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeErrorCode(w, http.StatusUnsupportedMediaType, "content type must be application/json", errTypeInvalid)
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSessionBody))
	if err == nil {
		err = exactMembers(body, jsonMembers(dst))
	}
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		err = dec.Decode(dst)
		if err == nil {
			if err = dec.Decode(&struct{}{}); err == io.EOF {
				err = nil
			} else if err == nil {
				err = errors.New("trailing data")
			}
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErrorCode(w, http.StatusRequestEntityTooLarge, "request body too large", errTypeInvalid)
			return false
		}
		writeErrorCode(w, http.StatusBadRequest, "body must be a single JSON object with known fields", errTypeInvalid)
		return false
	}
	return true
}

var errBadMember = errors.New("unknown, case-variant or repeated member")

// jsonMembers returns the exact JSON member names of the struct dst points to.
func jsonMembers(dst any) map[string]bool {
	t := reflect.TypeOf(dst)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := map[string]bool{}
	if t == nil || t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = true
	}
	return out
}

// exactMembers checks that data is a JSON object whose members are all in
// members, spelled exactly, and that no object anywhere in data repeats a
// member name. Member names are compared after JSON unescaping. Value types
// and trailing data are left to the decoder that runs afterwards.
func exactMembers(data []byte, members map[string]bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return errBadMember
	}
	return scanObject(dec, members)
}

// scanObject consumes the rest of an object whose '{' was just read. A nil
// members map accepts any unique names (nested objects).
func scanObject(dec *json.Decoder, members map[string]bool) error {
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		name, _ := tok.(string)
		if seen[name] || (members != nil && !members[name]) {
			return errBadMember
		}
		seen[name] = true
		if err := scanValue(dec); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// scanValue consumes one JSON value, checking every object inside it.
func scanValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		return scanObject(dec, nil)
	case json.Delim('['):
		for dec.More() {
			if err := scanValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return nil
}

// requireNoBody accepts an action request with an empty body or exactly an
// empty JSON object.
func requireNoBody(w http.ResponseWriter, r *http.Request) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSessionBody))
	if err != nil {
		writeErrorCode(w, http.StatusRequestEntityTooLarge, "request body too large", errTypeInvalid)
		return false
	}
	if s := strings.TrimSpace(string(b)); s != "" && s != "{}" {
		writeErrorCode(w, http.StatusBadRequest, "request body must be empty", errTypeInvalid)
		return false
	}
	return true
}

// identityFailed maps an identity-store error on a session route to a
// generic response. Store errors never reach the client or the log text.
func identityFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identity.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, "not found", errTypeNotFound)
	case errors.Is(err, identity.ErrKeyLimit):
		writeErrorCode(w, http.StatusConflict, "api key limit reached", errTypeKeyLimit)
	case errors.Is(err, identity.ErrStaleLogin):
		writeErrorCode(w, http.StatusForbidden, "sign in again to continue", errTypeReauth)
	case errors.Is(err, identity.ErrUserDeleted), errors.Is(err, identity.ErrUserDisabled):
		writeErrorCode(w, http.StatusConflict, "user is not active", errTypeConflict)
	case errors.Is(err, identity.ErrInvalid):
		writeErrorCode(w, http.StatusBadRequest, "invalid request", errTypeInvalid)
	default:
		slog.Warn("control: identity store unavailable")
		writeErrorCode(w, http.StatusServiceUnavailable, "identity unavailable", errTypeUnavailable)
	}
}

// userDoc is the frozen session-API user object, extended with LastLoginAt:
// the auth time of the user's last allowed login, null while there is none
// (never logged in, or cleared by disable). It is served only on the user's
// own profile and the admin user list.
type userDoc struct {
	ID          string     `json:"id"`
	Role        string     `json:"role"`
	Status      string     `json:"status"`
	DisplayName string     `json:"display_name"`
	Email       string     `json:"email"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

func newUserDoc(u identity.User) userDoc {
	d := userDoc{ID: u.ID, Role: string(u.Role), Status: string(u.Status), DisplayName: u.DisplayName, Email: u.Email}
	if !u.LastLoginAt.IsZero() {
		t := u.LastLoginAt.UTC()
		d.LastLoginAt = &t
	}
	return d
}

// meDoc is the GET /ui/v1/me response.
type meDoc struct {
	User      userDoc `json:"user"`
	CSRFToken string  `json:"csrf_token"`
}

// me serves GET /ui/v1/me: the session user's profile (current role and
// status) and the session's CSRF token for unsafe requests.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	a, _ := authFrom(r.Context())
	u, err := s.deps.Identity.User(r.Context(), a.principal.UserID)
	if err != nil {
		identityFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, meDoc{User: newUserDoc(u), CSRFToken: identity.CSRFToken(a.session)})
}
