package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
)

// Token prefixes reserved by the identity store. A bearer starting with one
// of them is an identity credential or nothing: it never reaches the static
// key table, so a malformed user key cannot fall through to a static match.
const (
	userKeyPrefix = "lrk_"
	sessionPrefix = "lrs_"
)

// userKeyClientPrefix starts every user API key id, which is also the key's
// client name. Static clients may not use it in multi-user mode, or one would
// share a user key's per-client budget, concurrency and ledger client label.
const userKeyClientPrefix = "k_"

// staticPrincipal is one configured static client in multi-user mode: its
// client attributes, its explicit role (RoleService or RoleUser) and, for
// RoleUser, the opaque id of the owning identity user.
type staticPrincipal struct {
	client core.Client
	role   core.Role
	owner  string
}

// bearerAuth is the multi-user core.BearerAuthenticator of one runtime
// generation. It never consults cookies and never yields a session or admin
// principal.
//
//   - A token with the exact user-key grammar is authenticated by the identity
//     store only (fresh read per request: revoked, expired, disabled or stale
//     owners are denied on the next request).
//   - Any other token with a reserved identity prefix is rejected.
//   - Everything else is looked up in this generation's static key table and
//     must map to an explicit service or user role. A user-role key must also
//     be registered to its owner in the identity store (registerOwnedKeys)
//     and not revoked, and its owner must be active with an allowed login
//     within the store's window, all read fresh on every request.
type bearerAuth struct {
	store   *identity.Store
	lookup  func(bearer string) (string, bool)
	statics map[string]staticPrincipal
}

// errNoIdentityStore is returned (wrapping ErrAuthUnavailable) when a
// credential needs the identity store and none is open.
var errNoIdentityStore = fmt.Errorf("%w: identity store not open", core.ErrAuthUnavailable)

// Authenticate implements core.BearerAuthenticator.
func (b *bearerAuth) Authenticate(ctx context.Context, bearer string) (core.Principal, error) {
	if bearer == "" {
		return core.Principal{}, core.ErrUnauthenticated
	}
	if identity.IsUserKeyToken(bearer) {
		if b.store == nil {
			return core.Principal{}, errNoIdentityStore
		}
		p, err := b.store.AuthenticateKey(ctx, bearer)
		if err != nil {
			return core.Principal{}, err
		}
		// The key id is the stable, opaque client identity of a user key: it
		// keys per-client limits and budgets and labels ledger rows. It is
		// never parsed for a role, and a user key is never ingest-capable.
		p.Client = core.Client{Name: p.KeyID, Class: core.ClassInteractive}
		return p, nil
	}
	if strings.HasPrefix(bearer, userKeyPrefix) || strings.HasPrefix(bearer, sessionPrefix) {
		return core.Principal{}, core.ErrUnauthenticated
	}
	if b.lookup == nil {
		return core.Principal{}, core.ErrUnauthenticated
	}
	name, ok := b.lookup(bearer)
	if !ok {
		return core.Principal{}, core.ErrUnauthenticated
	}
	sp, ok := b.statics[name]
	if !ok {
		return core.Principal{}, core.ErrUnauthenticated
	}
	switch sp.role {
	case core.RoleService:
		return core.Principal{Kind: core.PrincipalStaticClient, Role: core.RoleService, Client: sp.client}, nil
	case core.RoleUser:
		if sp.owner == "" {
			return core.Principal{}, core.ErrUnauthenticated
		}
		if b.store == nil {
			return core.Principal{}, errNoIdentityStore
		}
		p, err := b.store.AuthenticateStaticKey(ctx, sp.owner, bearer)
		if err != nil {
			return core.Principal{}, err
		}
		p.Client = sp.client
		p.Client.Ingest = false
		return p, nil
	default:
		return core.Principal{}, core.ErrUnauthenticated
	}
}
