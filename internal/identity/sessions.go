package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/hpst3r/localrouter/internal/core"
)

// Session token grammar: "lrs_" + 43-char unpadded base64url (32 random
// bytes).
const (
	sessionPrefix   = "lrs_"
	sessionTokenLen = len(sessionPrefix) + secretChars
	csrfContext     = "localrouter-csrf-v1"
)

func validSessionToken(s string) bool {
	return len(s) == sessionTokenLen && strings.HasPrefix(s, sessionPrefix) && canonicalSecret(s[len(sessionPrefix):])
}

// CSRFToken derives the CSRF token bound to a session token:
// base64url(HMAC-SHA256(key = session token, "localrouter-csrf-v1")). It is
// never stored; it is useless without the (HttpOnly) session token and
// cannot be used to recover it.
func CSRFToken(sessionToken string) string {
	m := hmac.New(sha256.New, []byte(sessionToken))
	m.Write([]byte(csrfContext))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// ValidCSRF reports, in constant time, whether presented is the CSRF token of
// sessionToken. A malformed session token is never valid. It does not
// authenticate the session itself: call AuthenticateSession as well.
func ValidCSRF(sessionToken, presented string) bool {
	if !validSessionToken(sessionToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(CSRFToken(sessionToken)), []byte(presented)) == 1
}

// CreateSession creates a browser session for userID right after a
// successful ResolveLogin: the user must be active and their last allowed
// login no older than Options.MaxAuthAge (ErrStaleLogin), so a session is
// never minted from an old login. It always returns a fresh random token
// (no fixation) and its CSRF token; only the token's SHA-256 is stored. The
// session expires SessionAbsoluteTTL after creation and SessionIdleTTL after
// its last (throttled) use, whichever is first.
func (s *Store) CreateSession(ctx context.Context, userID string) (NewSession, error) {
	raw, err := s.random(secretRandomBytes)
	if err != nil {
		return NewSession{}, err
	}
	token := sessionPrefix + base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	ns := NewSession{
		UserID:        userID,
		Token:         token,
		CSRF:          CSRFToken(token),
		ExpiresAt:     now.Add(s.opts.SessionAbsoluteTTL),
		IdleExpiresAt: now.Add(s.opts.SessionIdleTTL),
	}
	err = s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			status    string
			lastLogin sql.NullInt64
		)
		err := tx.QueryRowContext(ctx, `SELECT status, last_login_at FROM users WHERE id = ?`, userID).
			Scan(&status, &lastLogin)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("identity: create session: %w", err)
		}
		switch UserStatus(status) {
		case StatusActive:
		case StatusDisabled:
			return ErrUserDisabled
		default:
			return ErrUserDeleted
		}
		if !lastLogin.Valid || now.Sub(fromMS(lastLogin.Int64)) > s.opts.MaxAuthAge {
			return ErrStaleLogin
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, idle_expires_at, last_seen_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			tokenDigest(token), userID, toMS(now), toMS(ns.ExpiresAt), toMS(ns.IdleExpiresAt), toMS(now)); err != nil {
			return fmt.Errorf("identity: create session: %w", err)
		}
		return nil
	})
	if err != nil {
		return NewSession{}, err
	}
	return ns, nil
}

// AuthenticateSession authenticates a session token with a fresh read on
// every call (no cache). It succeeds only while the session exists, is
// before both its idle and absolute deadlines, and its user is active with
// an allowed login within Options.LoginMaxAge. The principal's Role is the
// user's role read now, so a demotion or disable applies to existing
// sessions. At most once per SessionTouchInterval the idle deadline slides
// to min(now + SessionIdleTTL, absolute).
//
// Credential failures are core.ErrUnauthenticated; store failures wrap
// core.ErrAuthUnavailable. The CSRF token is checked separately (ValidCSRF).
func (s *Store) AuthenticateSession(ctx context.Context, token string) (Session, error) {
	if !validSessionToken(token) {
		return Session{}, core.ErrUnauthenticated
	}
	digest := tokenDigest(token)
	var (
		found                        bool
		userID, status, role         string
		expires, idleExpires, seenAt int64
		lastLogin                    sql.NullInt64
	)
	err := s.read(ctx, func(ctx context.Context) error {
		err := s.db.QueryRowContext(ctx,
			`SELECT s.user_id, s.expires_at, s.idle_expires_at, s.last_seen_at, u.status, u.role, u.last_login_at
			 FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`, digest).
			Scan(&userID, &expires, &idleExpires, &seenAt, &status, &role, &lastLogin)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return Session{}, unavailable(err)
	}
	now := s.now()
	nowMS := toMS(now)
	if !found || nowMS >= expires || nowMS >= idleExpires ||
		UserStatus(status) != StatusActive || !s.loginFresh(lastLogin, now) {
		return Session{}, core.ErrUnauthenticated
	}
	if now.Sub(fromMS(seenAt)) >= SessionTouchInterval {
		idleExpires = min(toMS(now.Add(s.opts.SessionIdleTTL)), expires)
		var touched int64
		err := s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx,
				`UPDATE sessions SET last_seen_at = ?, idle_expires_at = ? WHERE token_hash = ?`,
				nowMS, idleExpires, digest)
			if err != nil {
				return err
			}
			touched, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return Session{}, unavailable(err)
		}
		if touched == 0 { // revoked between the read and the touch
			return Session{}, core.ErrUnauthenticated
		}
	}
	return Session{
		Principal:     core.Principal{Kind: core.PrincipalSession, Role: core.Role(role), UserID: userID},
		ExpiresAt:     fromMS(expires),
		IdleExpiresAt: fromMS(idleExpires),
	}, nil
}

// RevokeSession deletes one session (logout). It is idempotent: an unknown,
// already revoked or malformed token is not an error.
func (s *Store) RevokeSession(ctx context.Context, token string) error {
	if !validSessionToken(token) {
		return nil
	}
	return s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenDigest(token)); err != nil {
			return fmt.Errorf("identity: revoke session: %w", err)
		}
		return nil
	})
}

// RevokeUserSessions deletes every session of userID ("sign out
// everywhere"). An ActorUser may act only on themselves; an ActorAdmin must
// be an active admin.
func (s *Store) RevokeUserSessions(ctx context.Context, a Actor, userID string) error {
	if err := validActor(a); err != nil {
		return err
	}
	if a.Kind == ActorUser && a.UserID != userID {
		return fmt.Errorf("%w: a user may only revoke their own sessions", ErrInvalid)
	}
	now := s.now()
	return s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := checkAdminActor(ctx, tx, a); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("identity: revoke sessions: %w", err)
		}
		return audit(ctx, tx, now, AuditSessionsRevoked, a, userID, "", OutcomeOK, "")
	})
}

// PurgeExpiredSessions deletes sessions past their idle or absolute deadline
// and returns how many were removed. Expired sessions never authenticate
// regardless; this only bounds table growth.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int, error) {
	nowMS := toMS(s.now())
	var n int64
	err := s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE expires_at <= ? OR idle_expires_at <= ?`, nowMS, nowMS)
		if err != nil {
			return fmt.Errorf("identity: purge sessions: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}
