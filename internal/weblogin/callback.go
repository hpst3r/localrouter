package weblogin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// idClaims are the ID-token claims read beyond what go-oidc checks.
type idClaims struct {
	AuthTime          *json.Number `json:"auth_time"`
	Azp               *string      `json:"azp"`
	Iat               *json.Number `json:"iat"`
	Tid               any          `json:"tid"`
	Email             any          `json:"email"`
	Name              any          `json:"name"`
	PreferredUsername any          `json:"preferred_username"`
}

func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	clearLoginCookie(w)
	if canonicalHost(r.Host) != s.publicHost {
		s.fail(w, codeBadRequest)
		return
	}
	q := r.URL.Query()
	state, okState := single(q, "state")
	if idpErr, ok := single(q, "error"); ok || q.Has("error") {
		// Never reflect error_description or error_uri. The login cookie is
		// cleared above, which ends the flow in this browser.
		s.fail(w, idpErrorCode(idpErr))
		return
	}
	code, okCode := single(q, "code")
	if !okState || !b64Token.MatchString(state) || !okCode || !validCode(code) {
		s.fail(w, codeBadRequest)
		return
	}
	// Login CSRF / session swap: the browser finishing the flow must be the
	// one that started it, so the flow comes only from its sealed cookie.
	cookies := r.CookiesNamed(LoginCookieName)
	if len(cookies) != 1 {
		s.fail(w, codeLoginCSRF)
		return
	}
	f, ok := s.sealer.open(cookies[0].Value)
	if !ok {
		s.fail(w, codeLoginCSRF)
		return
	}
	now := s.cfg.Clock.Now()
	if now.Before(f.created) || now.After(f.expires) || f.expires.Sub(f.created) != s.cfg.LoginTimeout {
		s.fail(w, codeLoginExpired)
		return
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(f.state)) != 1 {
		s.fail(w, codeLoginCSRF)
		return
	}
	p, err := s.provider(r.Context())
	if err != nil {
		s.fail(w, codeProviderUnavailable)
		return
	}
	login, ok := s.redeem(r.Context(), w, p, code, f)
	if !ok {
		return
	}
	sess, err := s.cfg.Hooks.CompleteLogin(r.Context(), login, s.cfg.Policy.JIT)
	switch {
	case errors.Is(err, ErrNotProvisioned):
		s.fail(w, codeNotProvisioned)
		return
	case errors.Is(err, ErrUserDisabled):
		s.fail(w, codeUserDisabled)
		return
	case err != nil:
		// The adapter's error may carry store details: log nothing of it.
		s.fail(w, codeLoginFailed)
		return
	}
	now = s.cfg.Clock.Now()
	if !validSessionSecret(sess.Secret) || !sess.ExpiresAt.After(now) {
		s.fail(w, codeLoginFailed)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sess.Secret,
		Path:     "/",
		MaxAge:   max(1, int(sess.ExpiresAt.Sub(now)/time.Second)),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Location", LandingPath)
	w.WriteHeader(http.StatusSeeOther)
}

// redeem exchanges code for flow f at most once per state, within the
// exchange concurrency bound. A replay or a concurrent callback for the same
// state is refused before the token endpoint. On failure it has already
// written the response.
func (s *Service) redeem(ctx context.Context, w http.ResponseWriter, p *provider, code string, f flow) (VerifiedLogin, bool) {
	if !s.acquireExchange(ctx) {
		s.fail(w, codeBusy)
		return VerifiedLogin{}, false
	}
	defer func() { <-s.exchanges }()
	switch s.redeemed.begin(f.state, f.expires, s.cfg.Clock.Now()) {
	case redeemReplay:
		s.fail(w, codeLoginExpired)
		return VerifiedLogin{}, false
	case redeemFull:
		s.fail(w, codeBusy)
		return VerifiedLogin{}, false
	}
	verified := false
	defer func() { s.redeemed.finish(f.state, verified) }()
	login, verified, ok := s.exchange(ctx, w, p, code, f)
	return login, ok
}

// acquireExchange takes an exchange slot, waiting at most exchangeWait.
func (s *Service) acquireExchange(ctx context.Context) bool {
	t := time.NewTimer(s.exchangeWait)
	defer t.Stop()
	select {
	case s.exchanges <- struct{}{}:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// exchange redeems code, verifies the ID token and applies the policy. On
// failure it has already written the response. verified reports that the
// IdP returned an ID token signed for this flow's nonce, after which the
// state is spent whatever the outcome.
func (s *Service) exchange(ctx context.Context, w http.ResponseWriter, p *provider, code string, f flow) (login VerifiedLogin, verified, ok bool) {
	secret, err := s.cfg.ClientSecret()
	if err != nil {
		s.fail(w, codeExchangeFailed)
		return VerifiedLogin{}, false, false
	}
	oc := p.oauth
	oc.ClientSecret = secret
	reqCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Outbound.Timeout)
	defer cancel()
	ctx = oidc.ClientContext(ctx, s.client) // also the oauth2.HTTPClient key
	tok, err := oc.Exchange(ctx, code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		// RetrieveError.Error() embeds the raw response body: log a class.
		s.fail(w, codeExchangeFailed, "oauth_error", oauthErrorClass(err))
		return VerifiedLogin{}, false, false
	}
	raw, _ := tok.Extra("id_token").(string)
	if len(raw) > maxIDTokenLen {
		s.fail(w, codeTokenInvalid)
		return VerifiedLogin{}, false, false
	}
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		s.fail(w, codeTokenInvalid)
		return VerifiedLogin{}, false, false
	}
	var payload json.RawMessage
	var c idClaims
	if idt.Claims(&payload) != nil || json.Unmarshal(payload, &c) != nil {
		s.fail(w, codeTokenInvalid)
		return VerifiedLogin{}, false, false
	}
	// go-oidc verified signature, alg, iss, aud, exp and nbf; it leaves the
	// nonce to the caller.
	if idt.Nonce == "" || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.nonce)) != 1 {
		s.fail(w, codeTokenInvalid)
		return VerifiedLogin{}, false, false
	}
	// azp: required when there are several audiences, and when present it
	// must be this client.
	if (len(idt.Audience) > 1 && c.Azp == nil) || (c.Azp != nil && *c.Azp != s.cfg.ClientID) {
		s.fail(w, codeTokenInvalid)
		return VerifiedLogin{}, true, false
	}
	if !validSubject(idt.Subject) {
		s.fail(w, codeTokenInvalid)
		return VerifiedLogin{}, true, false
	}
	if tid, _ := c.Tid.(string); s.cfg.TenantID != "" && tid != s.cfg.TenantID {
		s.fail(w, codeTokenInvalid)
		return VerifiedLogin{}, true, false
	}
	now := s.cfg.Clock.Now()
	if iat, ok := unixTime(c.Iat); !ok || !inWindow(iat, f.created, now) {
		s.fail(w, codeTokenInvalid)
		return VerifiedLogin{}, true, false
	}
	// max_age=0 was requested: the IdP must report a fresh authentication.
	// A stale or missing auth_time never reaches the hooks, so it can never
	// renew the login window.
	authTime, ok := unixTime(c.AuthTime)
	if !ok || authTime.Before(f.created.Add(-maxClockSkew)) {
		s.fail(w, codeStaleAuth)
		return VerifiedLogin{}, true, false
	}
	if authTime.After(now.Add(maxClockSkew)) {
		s.fail(w, codeTokenInvalid)
		return VerifiedLogin{}, true, false
	}
	d := s.cfg.Policy.Evaluate(idt.Subject, payload)
	if !d.Allowed {
		if err := s.cfg.Hooks.LoginDenied(reqCtx, s.cfg.Issuer, idt.Subject, d.Reason); err != nil {
			s.cfg.Logger.Warn("weblogin: login denial hook failed")
		}
		s.fail(w, d.Reason)
		return VerifiedLogin{}, true, false
	}
	return VerifiedLogin{
		Issuer:      s.cfg.Issuer,
		Subject:     idt.Subject,
		Role:        d.Role,
		AuthTime:    authTime,
		Email:       sanitizeEmail(c.Email),
		DisplayName: sanitizeDisplay(c.Name, c.PreferredUsername),
	}, true, true
}

// maxIDTokenLen bounds the ID token before it is parsed.
const maxIDTokenLen = 64 << 10

// maxClockSkew tolerates IdP/router clock differences, matching go-oidc's
// nbf leeway.
const maxClockSkew = 5 * time.Minute

// inWindow reports whether t lies in [from-skew, to+skew].
func inWindow(t, from, to time.Time) bool {
	return !t.Before(from.Add(-maxClockSkew)) && !t.After(to.Add(maxClockSkew))
}

// unixTime decodes a NumericDate claim; absent is not ok.
func unixTime(n *json.Number) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f <= 0 || f > 1<<40 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// validSubject requires 1..255 bytes of valid UTF-8 without control
// characters.
func validSubject(sub string) bool {
	if sub == "" || len(sub) > 255 || !utf8.ValidString(sub) {
		return false
	}
	return strings.IndexFunc(sub, unicode.IsControl) < 0
}

// validSessionSecret accepts 16..256 base64url characters, so the cookie
// value never needs quoting and cannot inject attributes.
func validSessionSecret(v string) bool {
	if len(v) < 16 || len(v) > 256 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// oauthErrorClass maps a token-endpoint failure to a fixed log value.
func oauthErrorClass(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		switch re.ErrorCode {
		case "invalid_request", "invalid_client", "invalid_grant", "unauthorized_client",
			"unsupported_grant_type", "invalid_scope", "temporarily_unavailable", "server_error":
			return re.ErrorCode
		}
		return "other"
	}
	if c := errorClass(err); c != "unavailable" {
		return c
	}
	return "other"
}

var b64Token = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// single returns the only value of key; repeated keys are rejected.
func single(q url.Values, key string) (string, bool) {
	v := q[key]
	if len(v) != 1 {
		return "", false
	}
	return v[0], true
}

// validCode accepts 1..4096 bytes of printable ASCII.
func validCode(code string) bool {
	if code == "" || len(code) > 4096 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] <= ' ' || code[i] >= 0x7f {
			return false
		}
	}
	return true
}

// idpErrorCode maps an authorization error to a fixed code.
func idpErrorCode(e string) string {
	switch e {
	case "access_denied", "login_required", "interaction_required", "consent_required":
		return e
	}
	return codeIdPError
}

// Informational claims are display-only. They are bounded and stripped of
// control characters; an email that does not fit is dropped rather than cut.
const (
	maxEmailLen   = 254
	maxDisplayLen = 128
)

func infoString(v any) string {
	str, _ := v.(string)
	str = strings.ToValidUTF8(str, "")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, str)
}

func sanitizeEmail(v any) string {
	if e := infoString(v); len(e) <= maxEmailLen {
		return e
	}
	return ""
}

// sanitizeDisplay returns the first non-empty value cut to maxDisplayLen
// bytes on a rune boundary.
func sanitizeDisplay(vs ...any) string {
	for _, v := range vs {
		d := infoString(v)
		if d == "" {
			continue
		}
		for len(d) > maxDisplayLen {
			_, size := utf8.DecodeLastRuneInString(d)
			d = d[:len(d)-size]
		}
		return d
	}
	return ""
}

func clearLoginCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     LoginCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
