package weblogin

import (
	"errors"
	"net/http"
)

// logout is local only: it revokes the LocalRouter session and clears the
// cookie. The IdP session is not ended (no RP-initiated logout).
//
// It is a cookie-authenticated unsafe request, so it requires the canonical
// host, an Origin exactly equal to PublicBaseURL, Sec-Fetch-Site (when sent)
// of same-origin, and the session's CSRF token, which Hooks.Logout checks.
func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if canonicalHost(r.Host) != s.publicHost || r.Header.Get("Origin") != s.cfg.PublicBaseURL ||
		len(r.Header.Values("Origin")) != 1 {
		s.logoutFail(w, http.StatusForbidden, "cross_site")
		return
	}
	if site := r.Header.Values("Sec-Fetch-Site"); len(site) > 1 || (len(site) == 1 && site[0] != "same-origin") {
		s.logoutFail(w, http.StatusForbidden, "cross_site")
		return
	}
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		err := s.cfg.Hooks.Logout(r.Context(), c.Value, r.Header.Get(CSRFHeader))
		switch {
		case errors.Is(err, ErrCSRF):
			s.logoutFail(w, http.StatusForbidden, "csrf")
			return
		case err != nil:
			s.logoutFail(w, http.StatusServiceUnavailable, "logout_failed")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) logoutFail(w http.ResponseWriter, status int, code string) {
	s.cfg.Logger.Warn("weblogin: logout failed", "code", code)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte("logout failed: " + code + "\n"))
}
