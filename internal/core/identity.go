package core

import (
	"context"
	"errors"
	"time"
)

// Role is the authorization role of a principal. Roles are decided by
// configuration (static clients) or by the identity store (users); no package
// infers a role from a name or a key format.
type Role string

const (
	// RoleLegacy is every principal when identity is disabled: exactly the
	// pre-identity single-user semantics.
	RoleLegacy Role = "legacy"
	// RoleAdmin is a human user whose last allowed login carried the
	// configured admin claim/subject. Admin powers are browser-session only.
	RoleAdmin Role = "admin"
	// RoleUser is a human user (session or user API key).
	RoleUser Role = "user"
	// RoleService is a static client explicitly configured as a service
	// (agents, automation). Never admin.
	RoleService Role = "service"
)

// PrincipalKind is the credential that authenticated a request.
type PrincipalKind string

const (
	PrincipalStaticClient PrincipalKind = "static_client" // clients[] key file
	PrincipalUserKey      PrincipalKind = "user_key"      // identity-store API key
	PrincipalSession      PrincipalKind = "session"       // browser session (never inference)
)

// Principal is the authenticated caller of one request. It carries only
// opaque identifiers — never a name, email, IdP claim, token or secret.
type Principal struct {
	Kind PrincipalKind
	Role Role
	// Client is the static client for PrincipalStaticClient. For user keys and
	// sessions Client.Name and Client.Host are "" and Client.Ingest is false.
	Client Client
	// UserID is the opaque internal id of the owning user ("" for unowned
	// static clients).
	UserID string
	// KeyID is the opaque API key id (PrincipalUserKey only).
	KeyID string
}

// DataScope selects whose ledger/budget data a read may see. It is valid iff
// exactly one of AllUsers or a non-empty UserID is set; neither and both are
// denied with ErrInvalidScope.
type DataScope struct {
	AllUsers bool
	UserID   string
}

// Validate reports ErrInvalidScope unless exactly one of AllUsers or a
// non-empty UserID is set.
func (s DataScope) Validate() error {
	if s.AllUsers == (s.UserID != "") {
		return ErrInvalidScope
	}
	return nil
}

// Authentication and scoping outcomes. Authenticators must not distinguish
// unknown, revoked, expired or disabled credentials to callers: all are
// ErrUnauthenticated. ErrAuthUnavailable means the authentication backend
// failed and the request must fail closed (503), never fall back.
var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrAuthUnavailable = errors.New("authentication unavailable")
	ErrInvalidScope    = errors.New("invalid data scope")
)

// BearerAuthenticator authenticates an Authorization: Bearer token (static
// client key or user API key). It never consults cookies and never returns a
// PrincipalSession.
type BearerAuthenticator func(ctx context.Context, bearer string) (Principal, error)

// ScopedLedger is implemented by ledgers that can restrict summaries to a
// DataScope. An invalid scope is an error (ErrInvalidScope), never "all".
type ScopedLedger interface {
	SummaryScoped(ctx context.Context, since time.Time, group string, scope DataScope) ([]UsageRow, error)
}
