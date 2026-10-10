package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// maxAuthClockSkew is how far in the future a verified auth_time may be.
const maxAuthClockSkew = time.Minute

const userColumns = `id, status, role, display_name, email, created_at, updated_at, last_login_at`

// ResolveLogin maps a verified, policy-allowed login to a local user in one
// write transaction. The caller (the OIDC layer) has already verified the ID
// token and evaluated the access policy; l.Role is that policy's result.
//
//   - l.Issuer must equal the pinned issuer; l.Subject is matched byte-exact.
//   - l.AuthTime must be within Options.MaxAuthAge (ErrStaleLogin) and not in
//     the future, so an arbitrarily old IdP session never renews the
//     30-day login window.
//   - An existing active user gets the new role, profile and last login.
//   - An unknown subject is created only if l.Provision, with a fresh random
//     id; otherwise ErrNotProvisioned.
//
// Email and display name are informational and never used for matching.
func (s *Store) ResolveLogin(ctx context.Context, l Login) (User, error) {
	if l.Issuer != s.opts.Issuer {
		return User{}, fmt.Errorf("%w: login issuer is not the pinned issuer", ErrInvalid)
	}
	if !validText(l.Subject, MaxSubjectBytes) {
		return User{}, fmt.Errorf("%w: login subject must be 1..%d bytes of printable UTF-8", ErrInvalid, MaxSubjectBytes)
	}
	if l.Role != core.RoleUser && l.Role != core.RoleAdmin {
		return User{}, fmt.Errorf("%w: login role must be user or admin", ErrInvalid)
	}
	now := s.now()
	if l.AuthTime.IsZero() || l.AuthTime.After(now.Add(maxAuthClockSkew)) {
		return User{}, fmt.Errorf("%w: login auth time missing or in the future", ErrInvalid)
	}
	if now.Sub(l.AuthTime) > s.opts.MaxAuthAge {
		return User{}, ErrStaleLogin
	}
	authAt := l.AuthTime.UTC().Truncate(time.Millisecond)
	if authAt.After(now) {
		authAt = now
	}
	display := core.TruncateLabel(l.DisplayName)
	email := core.TruncateLabel(l.Email)

	var out User
	err := s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var tombstoned int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM subject_tombstones WHERE digest = ?`, subjectDigest(l.Issuer, l.Subject)).Scan(&tombstoned); err != nil {
			return fmt.Errorf("identity: resolve login: %w", err)
		}
		if tombstoned > 0 {
			if err := audit(ctx, tx, now, AuditUserLogin, Actor{Kind: ActorSystem}, "", "", OutcomeDenied, "deleted"); err != nil {
				return err
			}
			return &commitErr{ErrUserDeleted}
		}
		var userID string
		err := tx.QueryRowContext(ctx,
			`SELECT user_id FROM identities WHERE issuer = ? AND subject = ?`, l.Issuer, l.Subject).Scan(&userID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if !l.Provision {
				if err := audit(ctx, tx, now, AuditUserLogin, Actor{Kind: ActorSystem}, "", "", OutcomeDenied, "not_provisioned"); err != nil {
					return err
				}
				return &commitErr{ErrNotProvisioned}
			}
			id, err := s.newID("u_")
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO users (id, status, role, display_name, email, created_at, updated_at, last_login_at)
				 VALUES (?, 'active', ?, ?, ?, ?, ?, ?)`,
				id, string(l.Role), display, email, toMS(now), toMS(now), toMS(authAt)); err != nil {
				return fmt.Errorf("identity: provision: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO identities (issuer, subject, user_id, created_at) VALUES (?, ?, ?, ?)`,
				l.Issuer, l.Subject, id, toMS(now)); err != nil {
				return fmt.Errorf("identity: provision: %w", err)
			}
			if err := audit(ctx, tx, now, AuditUserProvisioned, Actor{Kind: ActorSystem}, id, "", OutcomeOK, string(l.Role)); err != nil {
				return err
			}
			userID = id
		case err != nil:
			return fmt.Errorf("identity: resolve login: %w", err)
		default:
			var oldRole, status string
			if err := tx.QueryRowContext(ctx, `SELECT role, status FROM users WHERE id = ?`, userID).Scan(&oldRole, &status); err != nil {
				return fmt.Errorf("identity: resolve login: %w", err)
			}
			if UserStatus(status) != StatusActive {
				denied, reason := ErrUserDisabled, "disabled"
				if UserStatus(status) == StatusDeleted {
					denied, reason = ErrUserDeleted, "deleted"
				}
				if err := audit(ctx, tx, now, AuditUserLogin, Actor{Kind: ActorSystem}, userID, "", OutcomeDenied, reason); err != nil {
					return err
				}
				return &commitErr{denied}
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET role = ?, display_name = ?, email = ?, updated_at = ?, last_login_at = ? WHERE id = ?`,
				string(l.Role), display, email, toMS(now), toMS(authAt), userID); err != nil {
				return fmt.Errorf("identity: resolve login: %w", err)
			}
			if oldRole != string(l.Role) {
				if err := audit(ctx, tx, now, AuditUserRoleChanged, Actor{Kind: ActorSystem}, userID, "", OutcomeOK, string(l.Role)); err != nil {
					return err
				}
			}
		}
		if err := audit(ctx, tx, now, AuditUserLogin, Actor{Kind: ActorSystem}, userID, "", OutcomeOK, ""); err != nil {
			return err
		}
		u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, userID))
		if err != nil {
			return fmt.Errorf("identity: resolve login: %w", err)
		}
		out = u
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return out, nil
}

// DenyLogin is called by the OIDC layer when a verified login is denied by
// the access policy (claim no longer allowed, group overage, malformed
// claim). For an existing active user it permanently revokes every key and
// session and clears the last allowed login — the only IdP→LocalRouter
// de-provisioning signal — while leaving the account active so a later
// allowed login works (old credentials stay dead). An unknown subject
// creates nothing. Both outcomes are audited.
func (s *Store) DenyLogin(ctx context.Context, issuer, subject string) error {
	if issuer != s.opts.Issuer {
		return fmt.Errorf("%w: login issuer is not the pinned issuer", ErrInvalid)
	}
	if !validText(subject, MaxSubjectBytes) {
		return fmt.Errorf("%w: login subject must be 1..%d bytes of printable UTF-8", ErrInvalid, MaxSubjectBytes)
	}
	now := s.now()
	return s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var userID, status string
		err := tx.QueryRowContext(ctx,
			`SELECT u.id, u.status FROM identities i JOIN users u ON u.id = i.user_id WHERE i.issuer = ? AND i.subject = ?`,
			issuer, subject).Scan(&userID, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return audit(ctx, tx, now, AuditUserLogin, Actor{Kind: ActorSystem}, "", "", OutcomeDenied, "policy")
		case err != nil:
			return fmt.Errorf("identity: deny login: %w", err)
		}
		if err := audit(ctx, tx, now, AuditUserLogin, Actor{Kind: ActorSystem}, userID, "", OutcomeDenied, "policy"); err != nil {
			return err
		}
		if UserStatus(status) != StatusActive {
			return nil // disable/delete already revoked everything
		}
		if err := revokeCredentials(ctx, tx, userID, RevokePolicyDenied, now); err != nil {
			return err
		}
		return audit(ctx, tx, now, AuditPolicyRevoked, Actor{Kind: ActorSystem}, userID, "", OutcomeOK, "policy")
	})
}

// User returns one user by id (any status). Unknown ids are ErrNotFound.
func (s *Store) User(ctx context.Context, id string) (User, error) {
	var u User
	err := s.read(ctx, func(ctx context.Context) error {
		var err error
		u, err = scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("identity: user: %w", err)
	}
	return u, nil
}

// ListUsers returns up to limit users (any status) with id > afterID in id
// order; pass the last id of a page to get the next. limit is clamped to
// [1, MaxListLimit].
func (s *Store) ListUsers(ctx context.Context, afterID string, limit int) ([]User, error) {
	limit = clampLimit(limit)
	var out []User
	err := s.read(ctx, func(ctx context.Context) error {
		rows, err := s.db.QueryContext(ctx,
			`SELECT `+userColumns+` FROM users WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUser(rows)
			if err != nil {
				return err
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("identity: list users: %w", err)
	}
	return out, nil
}

func clampLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	if limit > MaxListLimit {
		return MaxListLimit
	}
	return limit
}

type rowScanner interface{ Scan(dest ...any) error }

func scanUser(r rowScanner) (User, error) {
	var (
		u              User
		status, role   string
		display, email sql.NullString
		created, upd   int64
		lastLogin      sql.NullInt64
	)
	if err := r.Scan(&u.ID, &status, &role, &display, &email, &created, &upd, &lastLogin); err != nil {
		return User{}, err
	}
	u.Status = UserStatus(status)
	u.Role = core.Role(role)
	u.DisplayName = display.String
	u.Email = email.String
	u.CreatedAt = fromMS(created)
	u.UpdatedAt = fromMS(upd)
	u.LastLoginAt = fromNullMS(lastLogin)
	return u, nil
}

// audit appends one id-only audit row inside tx.
func audit(ctx context.Context, tx *sql.Tx, at time.Time, action string, a Actor, targetUser, targetKey, outcome, reason string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_events (at, action, actor_kind, actor_user_id, target_user_id, target_key_id, outcome, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		toMS(at), action, string(a.Kind), a.UserID, targetUser, targetKey, outcome, reason); err != nil {
		return fmt.Errorf("identity: audit: %w", err)
	}
	return nil
}

// statusActor validates an actor allowed to change a user's status: admin,
// cli or system, never the user themselves.
func statusActor(a Actor) error {
	if err := validActor(a); err != nil {
		return err
	}
	if a.Kind == ActorUser {
		return fmt.Errorf("%w: users cannot change account status", ErrInvalid)
	}
	return nil
}

// userStatus loads a user's status inside tx (ErrNotFound if unknown).
func userStatus(ctx context.Context, tx *sql.Tx, id string) (UserStatus, error) {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("identity: load user: %w", err)
	}
	return UserStatus(status), nil
}

// revokeCredentials permanently revokes every live key and owned static key
// binding of id with reason, deletes its sessions and clears its last allowed
// login, so nothing the user held before can authenticate again and new
// credentials require a fresh allowed login.
func revokeCredentials(ctx context.Context, tx *sql.Tx, id, reason string, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ?, revoke_reason = ? WHERE user_id = ? AND revoked_at IS NULL`,
		toMS(now), reason, id); err != nil {
		return fmt.Errorf("identity: revoke keys: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE static_keys SET revoked_at = ?, revoke_reason = ? WHERE user_id = ? AND revoked_at IS NULL`,
		toMS(now), reason, id); err != nil {
		return fmt.Errorf("identity: revoke static keys: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("identity: revoke sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET last_login_at = NULL, updated_at = ? WHERE id = ?`, toMS(now), id); err != nil {
		return fmt.Errorf("identity: revoke credentials: %w", err)
	}
	return nil
}

// DisableUser disables id in one transaction: status disabled, every key
// permanently revoked (reason user_disabled), every session deleted and the
// last allowed login cleared. In-flight requests finish; the next request
// with any of the user's credentials is denied, in this and every other
// *Store on the file. Disabling a disabled user is a no-op; a deleted user
// is ErrUserDeleted. Actor: admin (must be an active admin), cli or system.
func (s *Store) DisableUser(ctx context.Context, a Actor, id string) error {
	if err := statusActor(a); err != nil {
		return err
	}
	now := s.now()
	return s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := checkAdminActor(ctx, tx, a); err != nil {
			return err
		}
		st, err := userStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		switch st {
		case StatusDisabled:
			return nil
		case StatusDeleted:
			return ErrUserDeleted
		}
		if err := revokeCredentials(ctx, tx, id, RevokeUserDisabled, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET status = 'disabled', disabled_at = ?, updated_at = ? WHERE id = ?`,
			toMS(now), toMS(now), id); err != nil {
			return fmt.Errorf("identity: disable user: %w", err)
		}
		return audit(ctx, tx, now, AuditUserDisabled, a, id, "", OutcomeOK, "")
	})
}

// EnableUser re-activates a disabled user. It resurrects nothing: revoked
// keys stay revoked, sessions stay deleted and the last allowed login stays
// cleared, so the user must log in again before creating credentials.
// Enabling an active user is a no-op; a deleted user is ErrUserDeleted.
// Actor: admin (must be an active admin), cli or system.
func (s *Store) EnableUser(ctx context.Context, a Actor, id string) error {
	if err := statusActor(a); err != nil {
		return err
	}
	now := s.now()
	return s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := checkAdminActor(ctx, tx, a); err != nil {
			return err
		}
		st, err := userStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		switch st {
		case StatusActive:
			return nil
		case StatusDeleted:
			return ErrUserDeleted
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET status = 'active', disabled_at = NULL, updated_at = ? WHERE id = ?`,
			toMS(now), id); err != nil {
			return fmt.Errorf("identity: enable user: %w", err)
		}
		return audit(ctx, tx, now, AuditUserEnabled, a, id, "", OutcomeOK, "")
	})
}

// subjectDigest is the tombstone key of a deleted identity: SHA-256(issuer
// || 0x00 || subject). The raw subject is not retained.
func subjectDigest(issuer, subject string) []byte {
	h := sha256.New()
	h.Write([]byte(issuer))
	h.Write([]byte{0})
	h.Write([]byte(subject))
	return h.Sum(nil)
}

// DeleteUser deletes id in one transaction: status deleted, display name and
// email removed, every key revoked (reason user_deleted unless already
// revoked) with its name blanked and digest zeroed, sessions deleted, and
// the (issuer, subject) identity replaced by a digest tombstone so the
// subject can never be re-provisioned (ResolveLogin → ErrUserDeleted). The
// users row and key ids remain so historical ledger/budget rows stay
// attributable to an opaque id without personal data. Deleting again is
// ErrUserDeleted. Actor: admin (must be an active admin), cli or system.
//
// The deletion and revocation are committed, and effective everywhere, when
// DeleteUser returns nil. Removing the superseded page images that still
// hold the profile from identity.db and its WAL needs a TRUNCATE checkpoint,
// which cannot complete while any connection in any process holds an older
// read snapshot. DeleteUser makes one short attempt. If a reader blocks it,
// the scrub stays pending (ScrubPending) and is retried after later writes,
// on Ping and on Close of any *Store on the file, until a checkpoint
// completes. DeleteUser still returns nil then: the scrub being pending does
// not undo the deletion.
func (s *Store) DeleteUser(ctx context.Context, a Actor, id string) error {
	if err := statusActor(a); err != nil {
		return err
	}
	now := s.now()
	return s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := checkAdminActor(ctx, tx, a); err != nil {
			return err
		}
		st, err := userStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		if st == StatusDeleted {
			return ErrUserDeleted
		}
		if err := revokeCredentials(ctx, tx, id, RevokeUserDeleted, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE api_keys SET name = '', token_hash = zeroblob(32) WHERE user_id = ?`, id); err != nil {
			return fmt.Errorf("identity: delete user: %w", err)
		}
		// The deleted status keeps a re-registered key of this owner dead
		// (RegisterStaticKey stores nothing for a deleted owner).
		if _, err := tx.ExecContext(ctx, `DELETE FROM static_keys WHERE user_id = ?`, id); err != nil {
			return fmt.Errorf("identity: delete user: %w", err)
		}
		var issuer, subject string
		err = tx.QueryRowContext(ctx, `SELECT issuer, subject FROM identities WHERE user_id = ?`, id).Scan(&issuer, &subject)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("identity: delete user: %w", err)
		default:
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO subject_tombstones (digest, user_id, deleted_at) VALUES (?, ?, ?)`,
				subjectDigest(issuer, subject), id, toMS(now)); err != nil {
				return fmt.Errorf("identity: delete user: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE user_id = ?`, id); err != nil {
				return fmt.Errorf("identity: delete user: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET status = 'deleted', display_name = NULL, email = NULL, deleted_at = ?, updated_at = ? WHERE id = ?`,
			toMS(now), toMS(now), id); err != nil {
			return fmt.Errorf("identity: delete user: %w", err)
		}
		if err := markScrubPending(ctx, tx); err != nil {
			return err
		}
		return audit(ctx, tx, now, AuditUserDeleted, a, id, "", OutcomeOK, "")
	})
}
