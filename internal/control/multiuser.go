package control

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hpst3r/localrouter/internal/authz"
	"github.com/hpst3r/localrouter/internal/core"
)

// Browser-session wire names shared with internal/weblogin (which sets the
// cookie and owns /auth/*). They are duplicated here rather than imported so
// the control server does not depend on the OIDC client stack; a test pins
// them to weblogin's exported constants.
const (
	sessionCookieName = "__Host-lr_session"
	csrfHeader        = "X-LocalRouter-CSRF"
)

// Generic multi-user error messages. They never name a user, key, token or
// backend error.
const (
	msgAuthRequired    = "client key required"
	msgAuthInvalid     = "invalid client key"
	msgAuthUnavailable = "authentication unavailable"
	msgForbidden       = "forbidden"
)

// multiUser reports whether the server runs in multi-user (identity) mode.
// Either flag enables it, so a partially wired caller fails closed.
func (s *Server) multiUser() bool { return s.opts.MultiUser || s.deps.MultiUser }

// requestAuth is what an authenticated multi-user request carries in its
// context: the principal and, for data reads, the scope the handler must
// apply. Handlers never derive a scope from the URL.
type requestAuth struct {
	principal core.Principal
	scope     *core.DataScope
	// session is the session cookie value of a /ui/v1 request ("" for
	// bearer requests). It is used only to derive the CSRF token and must
	// never be logged or returned.
	session string
}

type authCtxKey struct{}

func withAuth(r *http.Request, a requestAuth) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authCtxKey{}, a))
}

// authFrom returns the multi-user auth of r; ok is false on legacy requests.
func authFrom(ctx context.Context) (requestAuth, bool) {
	a, ok := ctx.Value(authCtxKey{}).(requestAuth)
	return a, ok
}

// scopeFrom returns the data scope a multi-user read must apply, or nil for a
// legacy request (which keeps the exact legacy unscoped behavior).
func scopeFrom(ctx context.Context) *core.DataScope {
	a, ok := authFrom(ctx)
	if !ok || a.scope == nil {
		return nil
	}
	sc := *a.scope
	return &sc
}

// scopeMode selects how a bearer route derives its data scope.
type scopeMode int

const (
	scopeNone scopeMode = iota // no data scope (status, admit, ingest, ...)
	scopeRead                  // authz.ReadScope: owners see own rows, services all
)

// bearer authenticates a multi-user /control/v1 request with
// AuthenticatePrincipal only (never cookies, never the legacy Authenticate),
// authorizes action and attaches the principal and scope. A missing seam or a
// backend failure is 503, a bad credential 401, a forbidden action 403.
func (s *Server) bearerAuth(action authz.Action, mode scopeMode, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authenticateBearer(w, r)
		if !ok {
			return
		}
		if !authz.Allow(p, action) {
			writeError(w, http.StatusForbidden, msgForbidden)
			return
		}
		a := requestAuth{principal: p}
		if mode == scopeRead {
			sc, err := authz.ReadScope(p)
			if err != nil {
				writeError(w, http.StatusForbidden, msgForbidden)
				return
			}
			a.scope = &sc
		}
		next(w, withAuth(r, a))
	})
}

// authenticateBearer runs AuthenticatePrincipal and writes the failure
// response itself. Errors are logged by class only: never the token or the
// backend error text.
func (s *Server) authenticateBearer(w http.ResponseWriter, r *http.Request) (core.Principal, bool) {
	if s.deps.AuthenticatePrincipal == nil {
		writeError(w, http.StatusServiceUnavailable, msgAuthUnavailable)
		return core.Principal{}, false
	}
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, msgAuthRequired)
		return core.Principal{}, false
	}
	p, err := s.deps.AuthenticatePrincipal(r.Context(), token)
	switch {
	case err == nil:
		return p, true
	case errors.Is(err, core.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, msgAuthInvalid)
	default:
		slog.Warn("control: bearer authentication unavailable")
		writeError(w, http.StatusServiceUnavailable, msgAuthUnavailable)
	}
	return core.Principal{}, false
}

// multiUserHandler serves the multi-user route table. Every /control/v1
// route is authenticated by AuthenticatePrincipal and authorized by authz
// regardless of Options.RequireAuth; the legacy widget redirects to /ui/.
func (s *Server) multiUserHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /{$}", s.widgetRedirect)
	mux.Handle("GET /control/v1/status", s.bearerAuth(authz.ActionStatus, scopeNone, s.status))
	mux.Handle("GET /control/v1/usage", s.bearerAuth(authz.ActionUsageRead, scopeRead, s.usage))
	mux.Handle("GET /control/v1/analytics", s.bearerAuth(authz.ActionUsageRead, scopeRead, s.analytics))
	mux.Handle("GET /control/v1/analytics/dimensions", s.bearerAuth(authz.ActionUsageRead, scopeRead, s.analyticsDimensions))
	mux.Handle("GET /control/v1/diagnostics", s.bearerAuth(authz.ActionDiagnostics, scopeNone, s.diagnostics))
	mux.Handle("GET /control/v1/budgets", s.bearerAuth(authz.ActionBudgetsGlobal, scopeNone, s.budgets))
	mux.Handle("POST /control/v1/admit", s.bearerAuth(authz.ActionAdmit, scopeNone, s.admit))
	mux.Handle("POST /control/v1/ingest", s.bearerAuth(authz.ActionIngest, scopeNone, s.ingest))

	mux.Handle("GET /ui/v1/me", s.sessionAuth(authz.ActionSelf, sessionNoScope, s.me))
	mux.Handle("GET /ui/v1/me/keys", s.sessionAuth(authz.ActionKeysOwn, sessionNoScope, s.meKeys))
	mux.Handle("POST /ui/v1/me/keys", s.sessionAuth(authz.ActionKeysOwn, sessionNoScope, s.meCreateKey))
	mux.Handle("DELETE /ui/v1/me/keys/{id}", s.sessionAuth(authz.ActionKeysOwn, sessionNoScope, s.meRevokeKey))
	mux.Handle("GET /ui/v1/me/usage", s.sessionAuth(authz.ActionUsageOwn, sessionOwn, s.usage))
	mux.Handle("GET /ui/v1/me/analytics", s.sessionAuth(authz.ActionUsageOwn, sessionOwn, s.analytics))
	mux.Handle("GET /ui/v1/me/analytics/dimensions", s.sessionAuth(authz.ActionUsageOwn, sessionOwn, s.analyticsDimensions))
	mux.Handle("GET /ui/v1/me/dimensions", s.sessionAuth(authz.ActionUsageOwn, sessionOwn, s.analyticsDimensions))
	mux.Handle("GET /ui/v1/me/budget", s.sessionAuth(authz.ActionBudgetOwn, sessionNoScope, s.meBudget))

	mux.Handle("GET /ui/v1/admin/users", s.sessionAuth(authz.ActionAdminUsers, sessionNoScope, s.adminUsers))
	for _, op := range []string{"disable", "enable", "delete"} {
		mux.Handle("POST /ui/v1/admin/users/{id}/"+op, s.sessionAuth(authz.ActionAdminUsers, sessionNoScope, s.adminUserAction(op)))
	}
	mux.Handle("GET /ui/v1/admin/users/{id}/keys", s.sessionAuth(authz.ActionAdminUsers, sessionNoScope, s.adminUserKeys))
	mux.Handle("DELETE /ui/v1/admin/users/{uid}/keys/{kid}", s.sessionAuth(authz.ActionAdminUsers, sessionNoScope, s.adminRevokeKey))
	mux.Handle("GET /ui/v1/admin/audit", s.sessionAuth(authz.ActionAdminAudit, sessionNoScope, s.adminAudit))
	mux.Handle("GET /ui/v1/admin/status", s.sessionAuth(authz.ActionStatus, sessionNoScope, s.status))
	mux.Handle("GET /ui/v1/admin/usage", s.sessionAuth(authz.ActionUsageGlobal, sessionGlobal, s.usage))
	mux.Handle("GET /ui/v1/admin/analytics", s.sessionAuth(authz.ActionUsageGlobal, sessionGlobal, s.analytics))
	mux.Handle("GET /ui/v1/admin/analytics/dimensions", s.sessionAuth(authz.ActionUsageGlobal, sessionGlobal, s.analyticsDimensions))
	mux.Handle("GET /ui/v1/admin/dimensions", s.sessionAuth(authz.ActionUsageGlobal, sessionGlobal, s.analyticsDimensions))
	mux.Handle("GET /ui/v1/admin/diagnostics", s.sessionAuth(authz.ActionDiagnostics, sessionNoScope, s.diagnostics))
	mux.Handle("GET /ui/v1/admin/budgets", s.sessionAuth(authz.ActionBudgetsGlobal, sessionNoScope, s.budgets))
	return mux
}

// widgetRedirect replaces the legacy single-user widget (which prompts for a
// bearer key) with the browser-session UI in multi-user mode.
func (s *Server) widgetRedirect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// scopedGroups adds the owner dimensions of a scoped read to base. "key" is open to
// every scope (an owner sees only their own key ids, because the owner
// predicate is applied before grouping); "user" is a global-reader dimension.
func scopedGroups(base []string, scope core.DataScope) []string {
	out := append([]string(nil), base...)
	out = append(out, "key")
	if scope.AllUsers {
		out = append(out, "user")
	}
	return out
}

// scopedSummary reads a usage summary restricted to scope. A ledger without
// scoped reads is unavailable in multi-user mode: there is no unscoped
// fallback.
func (s *Server) scopedSummary(ctx context.Context, since time.Time, group string, scope core.DataScope) ([]core.UsageRow, error) {
	sl, ok := s.deps.Ledger.(core.ScopedLedger)
	if !ok {
		return nil, errScopeUnsupported
	}
	return sl.SummaryScoped(ctx, since, group, scope)
}

var errScopeUnsupported = errors.New("control: ledger does not support scoped reads")
