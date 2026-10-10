// Package identity is the durable multi-user identity store on SQLite
// (modernc.org/sqlite): users keyed by an exact (issuer, subject) pair, their
// LocalRouter API keys, browser sessions and an id-only audit trail. It owns
// a separate identity.db (never the ledger) and has no HTTP, OIDC library or
// claim parsing: ResolveLogin receives an identity and role that the OIDC
// layer has already verified and evaluated.
//
// Security model.
//
//   - User ids are opaque random values; email and display name are stored
//     for display only and are never a lookup or linking key.
//   - API keys ("lrk_<key id>_<secret>") and session tokens ("lrs_<secret>")
//     carry 256 random bits. Only SHA-256 digests are stored; the plaintext is
//     returned exactly once. The key id in a token selects a row but is not
//     trusted: the digest of the whole token is compared in constant time.
//   - CSRF tokens are derived from the session token (CSRFToken) and are
//     never stored.
//   - Every authentication is a fresh indexed read (no positive cache), so a
//     disable, delete, revocation or role change made through this or any
//     other *Store on the same file is effective on the next request.
//   - An API key authenticates only while: it is not revoked, not expired
//     (every key expires, at most Options.KeyMaxTTL ≤ 90 days after
//     creation; a ceiling tightened later also bounds existing keys), its
//     owner is active, and the owner's last allowed login is
//     within Options.LoginMaxAge (≤ 30 days). API keys always yield RoleUser:
//     admin powers are browser-session only.
//   - Owned static keys live in configuration files; the store keeps a
//     registry binding each one's SHA-256 digest to its owner for good (see
//     RegisterStaticKey), so it can be revoked like an API key.
//   - Disable, delete and an access-policy denial permanently revoke the
//     user's keys, owned static key bindings and sessions; enabling never
//     resurrects them. Deleting a user scrubs profile data, key
//     names/digests and owned static key digests and leaves only a digest
//     tombstone of (issuer, subject), which blocks re-provisioning.
//     Older on-disk copies of the scrubbed data go at the next completed
//     WAL checkpoint, which a concurrent reader can defer (see DeleteUser
//     and ScrubPending).
//   - The store never logs. Errors never contain tokens, subjects, emails,
//     display names or key names.
//
// Lifecycle. Open pins the configured issuer and client id on first
// initialization and fails closed on a later mismatch, and refuses a schema
// newer than this binary. Every query runs under the caller's context bounded
// by Options.OpTimeout. Close waits for in-flight operations of this *Store
// and is terminal. Cross-process exclusive ownership of the file is the
// application's responsibility (see the app wiring); concurrent *Store
// instances on one file are nevertheless consistent because every write is
// an IMMEDIATE transaction and no state is cached.
package identity

import (
	"errors"
	"io"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Policy ceilings fixed by the product decisions. Options may tighten them,
// never loosen them.
const (
	MaxKeyTTL          = 90 * 24 * time.Hour
	MaxLoginAge        = 30 * 24 * time.Hour
	MaxSessionIdle     = 8 * time.Hour
	MaxSessionAbsolute = 24 * time.Hour
	// MaxAuthAgeCeiling bounds how old a verified authentication (auth_time)
	// may be when it is accepted by ResolveLogin or turned into a session.
	MaxAuthAgeCeiling = time.Hour
	// MaxKeysPerUserCeiling bounds Options.MaxKeysPerUser.
	MaxKeysPerUserCeiling = 100

	// SessionTouchInterval throttles idle-expiry slides: a session's idle
	// deadline is rewritten at most once per interval.
	SessionTouchInterval = time.Minute

	// MaxKeyNameBytes bounds an API key label.
	MaxKeyNameBytes = 64
	// MaxSubjectBytes bounds an OIDC subject.
	MaxSubjectBytes = 255
	// MaxListLimit bounds one page of ListUsers / AuditEvents.
	MaxListLimit = 200
)

// Defaults applied for zero Options fields.
const (
	DefaultMaxKeysPerUser = 10
	DefaultMaxAuthAge     = 10 * time.Minute
	DefaultOpTimeout      = 5 * time.Second
)

// Options configures a Store. Issuer and ClientID are required and pinned in
// the database on first initialization. Zero durations take the ceiling
// (KeyMaxTTL, LoginMaxAge, SessionIdleTTL, SessionAbsoluteTTL) or the default
// (MaxAuthAge, OpTimeout); a value above its ceiling is an error.
type Options struct {
	Issuer   string
	ClientID string

	KeyMaxTTL          time.Duration // ≤ MaxKeyTTL
	LoginMaxAge        time.Duration // ≤ MaxLoginAge
	SessionIdleTTL     time.Duration // ≤ SessionAbsoluteTTL
	SessionAbsoluteTTL time.Duration // ≤ MaxSessionAbsolute
	MaxAuthAge         time.Duration // ≤ MaxAuthAgeCeiling
	MaxKeysPerUser     int           // 1..MaxKeysPerUserCeiling
	OpTimeout          time.Duration // per-operation bound on top of the caller's ctx

	// Clock and Rand are test seams; nil means the system clock and
	// crypto/rand.
	Clock core.Clock
	Rand  io.Reader
}

// UserStatus is a user's local lifecycle state.
type UserStatus string

const (
	StatusActive   UserStatus = "active"
	StatusDisabled UserStatus = "disabled"
	StatusDeleted  UserStatus = "deleted"
)

// User is one local user. DisplayName and Email are informational only and
// empty after deletion. LastLoginAt is the auth time of the last allowed
// login; zero after disable, delete or policy denial.
type User struct {
	ID          string
	Status      UserStatus
	Role        core.Role // RoleUser or RoleAdmin
	DisplayName string
	Email       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastLoginAt time.Time
}

// Login is a verified, policy-allowed browser login handed over by the OIDC
// layer. Issuer must equal the pinned issuer; Subject is the exact `sub`;
// Role is the access-policy result (RoleUser or RoleAdmin); AuthTime is the
// verified auth_time and must be within Options.MaxAuthAge. Provision permits
// just-in-time creation of an unknown subject (configured JIT).
type Login struct {
	Issuer      string
	Subject     string
	Role        core.Role
	AuthTime    time.Time
	Email       string
	DisplayName string
	Provision   bool
}

// ActorKind names who performed an administrative change (audit only).
type ActorKind string

const (
	ActorUser   ActorKind = "user"   // the user acting on their own resources
	ActorAdmin  ActorKind = "admin"  // an admin session
	ActorSystem ActorKind = "system" // the server itself (policy, expiry)
	ActorCLI    ActorKind = "cli"    // local break-glass command
)

// Actor identifies the performer of a change. UserID is required for
// ActorUser and ActorAdmin and must be empty otherwise.
type Actor struct {
	Kind   ActorKind
	UserID string
}

// Key revocation reasons (fixed codes).
const (
	RevokeUser         = "user"
	RevokeAdmin        = "admin"
	RevokeCLI          = "cli"
	RevokeSystem       = "system"
	RevokeUserDisabled = "user_disabled"
	RevokeUserDeleted  = "user_deleted"
	RevokePolicyDenied = "policy_denied"
)

// APIKey is API key metadata; it never carries secret material. Name is ""
// after the owner is deleted. ExpiresAt is the effective expiry: the expiry
// chosen at creation, capped at CreatedAt + the current Options.KeyMaxTTL.
type APIKey struct {
	ID           string
	UserID       string
	Name         string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	RevokedAt    time.Time // zero if not revoked
	RevokeReason string
}

// NewAPIKey is returned exactly once by CreateKey. Token is the full bearer
// secret; it is not stored and cannot be retrieved again.
type NewAPIKey struct {
	APIKey
	Token string
}

// NewSession is returned exactly once by CreateSession. Token is the session
// cookie value and CSRF the matching CSRF token (CSRFToken(Token)); neither
// is stored.
type NewSession struct {
	UserID        string
	Token         string
	CSRF          string
	ExpiresAt     time.Time // absolute
	IdleExpiresAt time.Time
}

// Session is an authenticated browser session. Principal.Role is the user's
// current role read on this request.
type Session struct {
	Principal     core.Principal
	ExpiresAt     time.Time
	IdleExpiresAt time.Time
}

// Audit actions and outcomes (fixed vocabulary; rows carry opaque ids only).
const (
	AuditUserProvisioned = "user.provisioned"
	AuditUserLogin       = "user.login"
	AuditUserRoleChanged = "user.role_changed"
	AuditUserDisabled    = "user.disabled"
	AuditUserEnabled     = "user.enabled"
	AuditUserDeleted     = "user.deleted"
	AuditKeyCreated      = "key.created"
	AuditKeyRevoked      = "key.revoked"
	AuditSessionsRevoked = "session.revoked_all"
	AuditPolicyRevoked   = "credentials.revoked_policy"

	OutcomeOK     = "ok"
	OutcomeDenied = "denied"
)

// AuditEvent is one audit row. Reason is a fixed code, never free text.
type AuditEvent struct {
	Seq          int64
	At           time.Time
	Action       string
	ActorKind    ActorKind
	ActorUserID  string
	TargetUserID string
	TargetKeyID  string
	Outcome      string
	Reason       string
}

// Errors. Authentication methods (AuthenticateKey, AuthenticateSession)
// return only core.ErrUnauthenticated or core.ErrAuthUnavailable (wrapped);
// the rest are for administrative and login paths.
var (
	ErrNotFound        = errors.New("identity: not found")
	ErrNotProvisioned  = errors.New("identity: user not provisioned")
	ErrUserDisabled    = errors.New("identity: user disabled")
	ErrUserDeleted     = errors.New("identity: user deleted")
	ErrStaleLogin      = errors.New("identity: login too old")
	ErrKeyLimit        = errors.New("identity: api key limit reached")
	ErrInvalid         = errors.New("identity: invalid argument")
	ErrBindingMismatch = errors.New("identity: database is bound to a different OIDC issuer or client")
	ErrClosed          = errors.New("identity: store closed")
)
