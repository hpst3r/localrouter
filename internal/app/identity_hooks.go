package app

import (
	"context"
	"errors"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

// errLoginFailed is the only error CompleteLogin returns besides the two
// weblogin outcome errors. Store errors (stale auth_time, invalid input, IO)
// are deliberately not wrapped, so no store detail reaches a log or page.
var errLoginFailed = errors.New("identity: login failed")

// loginHooks adapts weblogin's verified-login callbacks to the identity
// store. weblogin has already verified the ID token, fresh auth_time and the
// access policy; the store re-checks issuer, role, subject and auth_time age
// (MaxAuthAge) before it records the login, so a stale authentication never
// renews the 30-day login window.
type loginHooks struct {
	store *identity.Store
}

var _ weblogin.Hooks = loginHooks{}

// CompleteLogin resolves (or, with jit, provisions) the user and creates a
// fresh session whose token becomes the session cookie.
func (h loginHooks) CompleteLogin(ctx context.Context, v weblogin.VerifiedLogin, jit bool) (weblogin.Session, error) {
	role := core.Role(v.Role)
	if role != core.RoleUser && role != core.RoleAdmin {
		return weblogin.Session{}, errLoginFailed
	}
	u, err := h.store.ResolveLogin(ctx, identity.Login{
		Issuer:      v.Issuer,
		Subject:     v.Subject,
		Role:        role,
		AuthTime:    v.AuthTime,
		Email:       v.Email,
		DisplayName: v.DisplayName,
		Provision:   jit,
	})
	switch {
	case errors.Is(err, identity.ErrNotProvisioned):
		return weblogin.Session{}, weblogin.ErrNotProvisioned
	case errors.Is(err, identity.ErrUserDisabled), errors.Is(err, identity.ErrUserDeleted):
		return weblogin.Session{}, weblogin.ErrUserDisabled
	case err != nil:
		return weblogin.Session{}, errLoginFailed
	}
	ns, err := h.store.CreateSession(ctx, u.ID)
	switch {
	case errors.Is(err, identity.ErrUserDisabled), errors.Is(err, identity.ErrUserDeleted):
		return weblogin.Session{}, weblogin.ErrUserDisabled
	case err != nil:
		return weblogin.Session{}, errLoginFailed
	}
	return weblogin.Session{Secret: ns.Token, ExpiresAt: ns.ExpiresAt}, nil
}

// LoginDenied applies the access-policy denial to an existing user (all keys
// and sessions revoked, last login cleared). An unknown subject is never
// created. reason is not stored: the store audits a fixed code.
func (h loginHooks) LoginDenied(ctx context.Context, issuer, subject, _ string) error {
	return h.store.DenyLogin(ctx, issuer, subject)
}

// Logout revokes the session after checking the CSRF token derived from it.
// An unknown or already revoked session is not an error.
func (h loginHooks) Logout(ctx context.Context, secret, csrf string) error {
	if !identity.ValidCSRF(secret, csrf) {
		return weblogin.ErrCSRF
	}
	return h.store.RevokeSession(ctx, secret)
}
