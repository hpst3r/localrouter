package identity

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// MinKeyTTL is the shortest accepted API key lifetime.
const MinKeyTTL = time.Hour

// User API key grammar: "lrk_" + 26-char lowercase base32 key id + "_" +
// 43-char unpadded base64url secret (32 random bytes).
const (
	keyPrefix   = "lrk_"
	keyIDPrefix = "k_"
	keyTokenLen = len(keyPrefix) + idChars + 1 + secretChars
)

// IsUserKeyToken reports whether s has the exact user API key grammar
// (canonical encodings, exact length). It is the dispatch test between the
// identity store and static client keys; it does not authenticate.
func IsUserKeyToken(s string) bool {
	_, ok := parseKeyToken(s)
	return ok
}

// parseKeyToken returns the stored key id ("k_" + id part) of a token with
// the exact grammar.
func parseKeyToken(s string) (string, bool) {
	if len(s) != keyTokenLen || !strings.HasPrefix(s, keyPrefix) || s[len(keyPrefix)+idChars] != '_' {
		return "", false
	}
	idPart := s[len(keyPrefix) : len(keyPrefix)+idChars]
	secret := s[len(keyPrefix)+idChars+1:]
	if !canonicalID(idPart) || !canonicalSecret(secret) {
		return "", false
	}
	return keyIDPrefix + idPart, true
}

func canonicalID(s string) bool {
	b, err := idEncoding.DecodeString(s)
	return err == nil && len(b) == idRandomBytes && idEncoding.EncodeToString(b) == s
}

func canonicalSecret(s string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == secretRandomBytes && base64.RawURLEncoding.EncodeToString(b) == s
}

func tokenDigest(token string) []byte {
	d := sha256.Sum256([]byte(token))
	return d[:]
}

// validKeyName reports whether name is a bounded, printable, non-blank label.
func validKeyName(name string) bool {
	return validText(name, MaxKeyNameBytes) && strings.TrimSpace(name) != ""
}

// CreateKey creates an API key for userID and returns its token exactly once.
// ttl 0 means Options.KeyMaxTTL; otherwise it must be in [MinKeyTTL,
// KeyMaxTTL]. The user must be active with an allowed login within
// Options.LoginMaxAge (ErrStaleLogin), and may hold at most
// Options.MaxKeysPerUser unrevoked, unexpired keys (ErrKeyLimit). Only the
// SHA-256 digest of the token is stored. Key creation is a self-service
// action: the caller must have authenticated userID by session.
func (s *Store) CreateKey(ctx context.Context, userID, name string, ttl time.Duration) (NewAPIKey, error) {
	if !validKeyName(name) {
		return NewAPIKey{}, fmt.Errorf("%w: key name must be 1..%d bytes of printable UTF-8", ErrInvalid, MaxKeyNameBytes)
	}
	if ttl == 0 {
		ttl = s.opts.KeyMaxTTL
	}
	if ttl < MinKeyTTL || ttl > s.opts.KeyMaxTTL {
		return NewAPIKey{}, fmt.Errorf("%w: key ttl must be in [%s, %s]", ErrInvalid, MinKeyTTL, s.opts.KeyMaxTTL)
	}
	idRaw, err := s.random(idRandomBytes)
	if err != nil {
		return NewAPIKey{}, err
	}
	secretRaw, err := s.random(secretRandomBytes)
	if err != nil {
		return NewAPIKey{}, err
	}
	idPart := idEncoding.EncodeToString(idRaw)
	token := keyPrefix + idPart + "_" + base64.RawURLEncoding.EncodeToString(secretRaw)
	now := s.now()
	k := APIKey{
		ID:        keyIDPrefix + idPart,
		UserID:    userID,
		Name:      name,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	err = s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := s.liveUser(ctx, tx, userID, now); err != nil {
			return err
		}
		var live int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM api_keys WHERE user_id = ? AND revoked_at IS NULL AND MIN(expires_at, created_at + ?) > ?`,
			userID, s.opts.KeyMaxTTL.Milliseconds(), toMS(now)).Scan(&live); err != nil {
			return fmt.Errorf("identity: create key: %w", err)
		}
		if live >= s.opts.MaxKeysPerUser {
			return ErrKeyLimit
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO api_keys (id, user_id, name, token_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
			k.ID, userID, name, tokenDigest(token), toMS(k.CreatedAt), toMS(k.ExpiresAt)); err != nil {
			return fmt.Errorf("identity: create key: %w", err)
		}
		return audit(ctx, tx, now, AuditKeyCreated, Actor{Kind: ActorUser, UserID: userID}, userID, k.ID, OutcomeOK, "")
	})
	if err != nil {
		return NewAPIKey{}, err
	}
	return NewAPIKey{APIKey: k, Token: token}, nil
}

// keyExpiry is a key's effective expiry in unix ms: the stored expiry, but
// never later than creation + the current Options.KeyMaxTTL, so tightening
// the ceiling also bounds keys issued under a longer one.
func (s *Store) keyExpiry(createdMS, expiresMS int64) int64 {
	return min(expiresMS, createdMS+s.opts.KeyMaxTTL.Milliseconds())
}

// liveUser loads userID inside tx and requires it to be active with an
// allowed login within Options.LoginMaxAge. It returns the user's role.
func (s *Store) liveUser(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (core.Role, error) {
	var (
		status, role string
		lastLogin    sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, `SELECT status, role, last_login_at FROM users WHERE id = ?`, userID).
		Scan(&status, &role, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("identity: load user: %w", err)
	}
	switch UserStatus(status) {
	case StatusActive:
	case StatusDisabled:
		return "", ErrUserDisabled
	default:
		return "", ErrUserDeleted
	}
	if !s.loginFresh(lastLogin, now) {
		return "", ErrStaleLogin
	}
	return core.Role(role), nil
}

// loginFresh reports whether a last allowed login is within LoginMaxAge.
func (s *Store) loginFresh(lastLogin sql.NullInt64, now time.Time) bool {
	return lastLogin.Valid && now.Sub(fromMS(lastLogin.Int64)) <= s.opts.LoginMaxAge
}

// ListKeys returns userID's keys (metadata only), newest first, at most
// MaxListLimit. ExpiresAt is the effective expiry under the current
// Options.KeyMaxTTL (see keyExpiry).
func (s *Store) ListKeys(ctx context.Context, userID string) ([]APIKey, error) {
	var out []APIKey
	err := s.read(ctx, func(ctx context.Context) error {
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, user_id, name, created_at, expires_at, revoked_at, revoke_reason
			 FROM api_keys WHERE user_id = ? ORDER BY created_at DESC, id LIMIT ?`, userID, MaxListLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				k                APIKey
				created, expires int64
				revoked          sql.NullInt64
			)
			if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &created, &expires, &revoked, &k.RevokeReason); err != nil {
				return err
			}
			k.CreatedAt, k.ExpiresAt, k.RevokedAt = fromMS(created), fromMS(s.keyExpiry(created, expires)), fromNullMS(revoked)
			out = append(out, k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("identity: list keys: %w", err)
	}
	return out, nil
}

// RevokeKey permanently revokes keyID if it is owned by userID; a key that
// does not exist or belongs to someone else is ErrNotFound (no existence
// oracle). Revoking an already revoked key is a no-op that keeps the first
// reason. The reason code follows the actor kind. An ActorUser may act only
// on their own keys; an ActorAdmin must be an active admin.
func (s *Store) RevokeKey(ctx context.Context, a Actor, userID, keyID string) error {
	if err := validActor(a); err != nil {
		return err
	}
	if a.Kind == ActorUser && a.UserID != userID {
		return fmt.Errorf("%w: a user may only revoke their own keys", ErrInvalid)
	}
	now := s.now()
	return s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := checkAdminActor(ctx, tx, a); err != nil {
			return err
		}
		var revoked sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT revoked_at FROM api_keys WHERE id = ? AND user_id = ?`, keyID, userID).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("identity: revoke key: %w", err)
		}
		if revoked.Valid {
			return nil
		}
		reason := actorRevokeReason(a)
		if _, err := tx.ExecContext(ctx,
			`UPDATE api_keys SET revoked_at = ?, revoke_reason = ? WHERE id = ? AND revoked_at IS NULL`,
			toMS(now), reason, keyID); err != nil {
			return fmt.Errorf("identity: revoke key: %w", err)
		}
		return audit(ctx, tx, now, AuditKeyRevoked, a, userID, keyID, OutcomeOK, reason)
	})
}

func actorRevokeReason(a Actor) string {
	switch a.Kind {
	case ActorUser:
		return RevokeUser
	case ActorAdmin:
		return RevokeAdmin
	case ActorCLI:
		return RevokeCLI
	default:
		return RevokeSystem
	}
}

// validActor checks an actor's shape: user/admin actors carry a user id,
// system/cli actors do not.
func validActor(a Actor) error {
	switch a.Kind {
	case ActorUser, ActorAdmin:
		if a.UserID == "" {
			return fmt.Errorf("%w: actor requires a user id", ErrInvalid)
		}
	case ActorSystem, ActorCLI:
		if a.UserID != "" {
			return fmt.Errorf("%w: system/cli actor has no user id", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown actor kind", ErrInvalid)
	}
	return nil
}

// checkAdminActor requires an ActorAdmin to be an active admin right now.
func checkAdminActor(ctx context.Context, tx *sql.Tx, a Actor) error {
	if a.Kind != ActorAdmin {
		return nil
	}
	var status, role string
	err := tx.QueryRowContext(ctx, `SELECT status, role FROM users WHERE id = ?`, a.UserID).Scan(&status, &role)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (status != string(StatusActive) || role != string(core.RoleAdmin))) {
		return fmt.Errorf("%w: actor is not an active admin", ErrInvalid)
	}
	if err != nil {
		return fmt.Errorf("identity: load actor: %w", err)
	}
	return nil
}

// dummyDigest is compared against when the key id is unknown, so a miss does
// the same hashing and comparison work as a hit.
var dummyDigest = make([]byte, sha256.Size)

// AuthenticateKey authenticates a bearer user API key with a fresh read on
// every call (no cache). The token's key id only selects the row; the
// SHA-256 digest of the whole token is compared in constant time. It
// succeeds only if the key is unrevoked and unexpired (see keyExpiry), its
// owner is active and the owner's last allowed login is within
// Options.LoginMaxAge.
//
// The principal is Kind PrincipalUserKey, Role RoleUser (also for admins:
// admin powers are browser-session only), Client{Class: ClassInteractive}
// (no name, host or ingest), UserID and KeyID. Every credential failure is
// core.ErrUnauthenticated; a store failure (closed, IO, context) wraps
// core.ErrAuthUnavailable and must fail closed. Errors never contain the
// token.
func (s *Store) AuthenticateKey(ctx context.Context, token string) (core.Principal, error) {
	keyID, ok := parseKeyToken(token)
	if !ok {
		return core.Principal{}, core.ErrUnauthenticated
	}
	presented := tokenDigest(token)
	var (
		found     bool
		stored    []byte
		userID    string
		created   int64
		expires   int64
		revoked   sql.NullInt64
		status    string
		lastLogin sql.NullInt64
	)
	err := s.read(ctx, func(ctx context.Context) error {
		err := s.db.QueryRowContext(ctx,
			`SELECT k.token_hash, k.user_id, k.created_at, k.expires_at, k.revoked_at, u.status, u.last_login_at
			 FROM api_keys k JOIN users u ON u.id = k.user_id WHERE k.id = ?`, keyID).
			Scan(&stored, &userID, &created, &expires, &revoked, &status, &lastLogin)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return core.Principal{}, unavailable(err)
	}
	if !found {
		stored = dummyDigest
	}
	match := subtle.ConstantTimeCompare(presented, stored) == 1
	now := s.now()
	if !found || !match || revoked.Valid || toMS(now) >= s.keyExpiry(created, expires) ||
		UserStatus(status) != StatusActive || !s.loginFresh(lastLogin, now) {
		return core.Principal{}, core.ErrUnauthenticated
	}
	return core.Principal{
		Kind:   core.PrincipalUserKey,
		Role:   core.RoleUser,
		Client: core.Client{Class: core.ClassInteractive},
		UserID: userID,
		KeyID:  keyID,
	}, nil
}

// unavailable wraps a store failure on an authentication path.
func unavailable(err error) error {
	return fmt.Errorf("%w: identity: %w", core.ErrAuthUnavailable, err)
}
