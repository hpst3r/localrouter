package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hpst3r/localrouter/internal/core"
)

// Owned static keys.
//
// A role "user" static client is a key held in a configuration file, not in
// this store, so disabling its owner cannot delete it. The registry binds the
// SHA-256 digest of every configured owned key to its owner, permanently, and
// carries the revocation: disable, delete and an access-policy denial revoke
// every binding of the user in the same transaction (see revokeCredentials),
// and nothing ever clears a revocation. Re-registering a known digest (every
// generation build does) keeps its owner and revocation; a key that comes
// back after a reload, a restart or a restored file stays dead. The operator
// replaces a revoked key with a new one.

// StaticKeyState is the registry state of an owned static key, as returned by
// RegisterStaticKey.
type StaticKeyState string

const (
	// StaticKeyActive: bound to an existing owner and not revoked. It still
	// authenticates only while the owner is active with a fresh login.
	StaticKeyActive StaticKeyState = "active"
	// StaticKeyPending: bound to an owner id that is not provisioned. It
	// cannot authenticate until a user with exactly that id exists.
	StaticKeyPending StaticKeyState = "pending"
	// StaticKeyRevoked: revoked for good (or never bound, because its owner
	// is deleted). It will never authenticate.
	StaticKeyRevoked StaticKeyState = "revoked"
)

// RevokeOwnerRevoked is the revocation reason of a static key first seen
// while its owner's credentials stood revoked (disabled, or no allowed login
// since a disable or policy denial): it may be one the owner held before.
const RevokeOwnerRevoked = "owner_revoked"

// maxStaticKeyBytes bounds a static key handed to the registry.
const maxStaticKeyBytes = 4096

// ErrStaticKeyConflict is returned by RegisterStaticKey for a key already
// bound to a different owner. Bindings never move.
var ErrStaticKeyConflict = errors.New("identity: static key is bound to a different owner")

// validUserID reports whether s has the opaque user id grammar ("u_" + 26
// lowercase base32 characters).
func validUserID(s string) bool {
	return strings.HasPrefix(s, "u_") && canonicalID(strings.TrimPrefix(s, "u_"))
}

func validStaticKey(token string) bool {
	return token != "" && len(token) <= maxStaticKeyBytes
}

// RegisterStaticKey binds the owned static key token to owner (an opaque user
// id that need not exist yet) in one write transaction. Only the token's
// SHA-256 digest is stored. Call it for every configured owned key before the
// key is served, whether or not it has ever been used.
//
//   - A known digest keeps its binding and revocation: the result is
//     StaticKeyRevoked if it was ever revoked. A different owner is
//     ErrStaticKeyConflict.
//   - A new digest for an unknown owner is bound StaticKeyPending.
//   - A new digest for a deleted owner is not stored (StaticKeyRevoked).
//   - A new digest for a disabled owner, or an active owner with no allowed
//     login since their credentials were last revoked, is stored already
//     revoked (StaticKeyRevoked, reason RevokeOwnerRevoked), so a key the
//     owner held before the revocation never activates. Rotate it after the
//     owner's next allowed login.
//   - Otherwise it is bound StaticKeyActive.
func (s *Store) RegisterStaticKey(ctx context.Context, owner, token string) (StaticKeyState, error) {
	if !validUserID(owner) {
		return "", fmt.Errorf("%w: static key owner must be a user id", ErrInvalid)
	}
	if !validStaticKey(token) {
		return "", fmt.Errorf("%w: static key must be 1..%d bytes", ErrInvalid, maxStaticKeyBytes)
	}
	digest := tokenDigest(token)
	now := s.now()
	var state StaticKeyState
	err := s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			bound   string
			revoked sql.NullInt64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT user_id, revoked_at FROM static_keys WHERE digest = ?`, digest).Scan(&bound, &revoked)
		switch {
		case err == nil:
			if bound != owner {
				return ErrStaticKeyConflict
			}
			state = StaticKeyActive
			if revoked.Valid {
				state = StaticKeyRevoked
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("identity: register static key: %w", err)
		}
		var (
			status    string
			lastLogin sql.NullInt64
			revokedAt any // nil: live binding
		)
		err = tx.QueryRowContext(ctx, `SELECT status, last_login_at FROM users WHERE id = ?`, owner).Scan(&status, &lastLogin)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			state = StaticKeyPending
		case err != nil:
			return fmt.Errorf("identity: register static key: %w", err)
		case UserStatus(status) == StatusDeleted:
			state = StaticKeyRevoked
			return nil
		case UserStatus(status) != StatusActive || !lastLogin.Valid:
			state, revokedAt = StaticKeyRevoked, toMS(now)
		default:
			state = StaticKeyActive
		}
		reason := ""
		if revokedAt != nil {
			reason = RevokeOwnerRevoked
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO static_keys (digest, user_id, registered_at, revoked_at, revoke_reason) VALUES (?, ?, ?, ?, ?)`,
			digest, owner, toMS(now), revokedAt, reason); err != nil {
			return fmt.Errorf("identity: register static key: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return state, nil
}

// AuthenticateStaticKey authenticates an owned static key that the caller has
// already matched to a configured client owned by owner, with a fresh read on
// every call (no cache). It succeeds only if the token's digest is
// registered to owner, not revoked, and the owner exists, is active and has
// an allowed login within Options.LoginMaxAge. Owned static keys have no
// expiry of their own.
//
// The principal is Kind PrincipalStaticClient, Role RoleUser and UserID; the
// caller fills in the client. Every credential failure is
// core.ErrUnauthenticated; a store failure wraps core.ErrAuthUnavailable.
func (s *Store) AuthenticateStaticKey(ctx context.Context, owner, token string) (core.Principal, error) {
	if !validUserID(owner) || !validStaticKey(token) {
		return core.Principal{}, core.ErrUnauthenticated
	}
	digest := tokenDigest(token)
	var (
		found     bool
		bound     string
		revoked   sql.NullInt64
		status    sql.NullString
		lastLogin sql.NullInt64
	)
	err := s.read(ctx, func(ctx context.Context) error {
		err := s.db.QueryRowContext(ctx,
			`SELECT k.user_id, k.revoked_at, u.status, u.last_login_at
			 FROM static_keys k LEFT JOIN users u ON u.id = k.user_id WHERE k.digest = ?`, digest).
			Scan(&bound, &revoked, &status, &lastLogin)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return core.Principal{}, unavailable(err)
	}
	if !found || bound != owner || revoked.Valid || UserStatus(status.String) != StatusActive ||
		!s.loginFresh(lastLogin, s.now()) {
		return core.Principal{}, core.ErrUnauthenticated
	}
	return core.Principal{Kind: core.PrincipalStaticClient, Role: core.RoleUser, UserID: owner}, nil
}
