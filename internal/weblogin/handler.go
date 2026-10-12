package weblogin

import (
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Handler serves GET /auth/login, GET /auth/callback and POST /auth/logout.
// Mount it at "/auth/" behind the app's host guard.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+LoginPath, s.login)
	mux.HandleFunc("GET "+CallbackPath, s.callback)
	mux.HandleFunc("POST "+LogoutPath, s.logout)
	return mux
}

// Fixed failure codes shown to the browser and logged. Nothing else about a
// failure is ever reflected or logged.
const (
	codeProviderUnavailable = "provider_unavailable"
	codeBusy                = "busy"
	codeExchangeFailed      = "exchange_failed"
	codeTokenInvalid        = "token_invalid"
	codeLoginFailed         = "login_failed"
	codeLoginExpired        = "login_expired"
	codeLoginCSRF           = "login_csrf"
	codeBadRequest          = "bad_request"
	codeIdPError            = "idp_error"
	codeStaleAuth           = "stale_auth"
	codeNotProvisioned      = "not_provisioned"
	codeUserDisabled        = "user_disabled"
)

var codeStatus = map[string]int{
	codeProviderUnavailable: http.StatusServiceUnavailable,
	codeBusy:                http.StatusServiceUnavailable,
	codeExchangeFailed:      http.StatusBadGateway,
	codeTokenInvalid:        http.StatusBadGateway,
	codeLoginFailed:         http.StatusServiceUnavailable,
	codeLoginExpired:        http.StatusBadRequest,
	codeLoginCSRF:           http.StatusBadRequest,
	codeBadRequest:          http.StatusBadRequest,
	codeIdPError:            http.StatusBadRequest,
	codeStaleAuth:           http.StatusUnauthorized,
	codeNotProvisioned:      http.StatusForbidden,
	codeUserDisabled:        http.StatusForbidden,
	ReasonNotPermitted:      http.StatusForbidden,
	ReasonGroupOverage:      http.StatusForbidden,
	ReasonClaimMalformed:    http.StatusForbidden,
	"access_denied":         http.StatusForbidden,
	"login_required":        http.StatusBadRequest,
	"interaction_required":  http.StatusBadRequest,
	"consent_required":      http.StatusBadRequest,
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if canonicalHost(r.Host) != s.publicHost {
		// Land the binding cookie on the canonical host only.
		w.Header().Set("Location", s.cfg.PublicBaseURL+LoginPath)
		w.WriteHeader(http.StatusSeeOther)
		return
	}
	p, err := s.provider(r.Context())
	if err != nil {
		s.fail(w, codeProviderUnavailable)
		return
	}
	// The flow lives only in the browser's sealed cookie: the server holds
	// nothing for it until the callback, so anonymous logins exhaust nothing.
	f, sealed := s.sealer.newFlow(s.cfg.Clock.Now(), s.cfg.LoginTimeout)
	http.SetCookie(w, &http.Cookie{
		Name:     LoginCookieName,
		Value:    sealed,
		Path:     "/",
		MaxAge:   int(s.cfg.LoginTimeout.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	// max_age=0 and prompt=login both ask for a fresh IdP authentication,
	// so an old IdP session can never renew the 30-day login window.
	// Providers differ in which one they honour, so both are sent; either
	// way auth_time is checked on return and a stale one is refused.
	w.Header().Set("Location", p.oauth.AuthCodeURL(f.state,
		oauth2.S256ChallengeOption(f.verifier),
		oidc.Nonce(f.nonce),
		oauth2.SetAuthURLParam("max_age", "0"),
		oauth2.SetAuthURLParam("prompt", "login")))
	w.WriteHeader(http.StatusFound)
}

// fail writes the fixed plain-text failure page for code and logs the code.
// attrs must be fixed values only.
func (s *Service) fail(w http.ResponseWriter, code string, attrs ...any) {
	s.cfg.Logger.Warn("weblogin: login failed", append([]any{"code", code}, attrs...)...)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(codeStatus[code])
	w.Write([]byte("login failed: " + code + "\n"))
}

func secureHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
}

// canonicalHost lower-cases host and drops the default https port.
func canonicalHost(host string) string {
	host = strings.ToLower(host)
	if h, ok := strings.CutSuffix(host, ":443"); ok {
		return h
	}
	return host
}
