// Package authz is LocalRouter's multi-user authorization policy: which
// authenticated principal may perform which control action, and which data
// scope a read is restricted to. It decides only from the typed
// core.Principal fields (Kind, Role, UserID, KeyID, Client.Ingest) — never
// from a client name, key prefix or any other string pattern — and denies
// everything it does not positively recognize.
//
// It is consulted only in multi-user (identity) mode. Legacy single-user mode
// never calls it, so a core.RoleLegacy principal is denied every action here:
// a legacy principal reaching this package is a wiring error that must fail
// closed.
package authz

import (
	"errors"

	"github.com/hpst3r/localrouter/internal/core"
)

// Action is one authorizable control operation.
type Action string

const (
	ActionUsageOwn      Action = "usage.read.own"
	ActionUsageGlobal   Action = "usage.read.global"
	ActionUsageRead     Action = "usage.read"
	ActionStatus        Action = "status.read"
	ActionDiagnostics   Action = "diagnostics.read"
	ActionBudgetsGlobal Action = "budgets.read.global"
	ActionBudgetOwn     Action = "budget.read.own"
	ActionAdmit         Action = "admit"
	ActionIngest        Action = "ingest"
	ActionKeysOwn       Action = "keys.manage.own"
	ActionAdminUsers    Action = "admin.users"
	ActionAdminAudit    Action = "admin.audit"
	ActionSelf          Action = "self.read" // session profile and CSRF token
)

// ErrForbidden is returned when a well-formed principal may not read the
// requested scope.
var ErrForbidden = errors.New("authz: forbidden")

// class is the recognized shape of a principal. Anything that does not match
// one of the shapes exactly is classNone and is denied everything.
type class int

const (
	classNone         class = iota
	classUserBearer         // user API key or user-owned static client
	classService            // static service client
	classUserSession        // browser session of a user
	classAdminSession       // browser session of an admin
)

// classify maps p to its shape from typed fields only. Admin is
// session-only: a bearer principal claiming RoleAdmin is malformed.
func classify(p core.Principal) class {
	switch p.Kind {
	case core.PrincipalUserKey:
		if p.Role == core.RoleUser && p.UserID != "" && p.KeyID != "" && !p.Client.Ingest {
			return classUserBearer
		}
	case core.PrincipalStaticClient:
		switch {
		case p.KeyID != "":
		case p.Role == core.RoleUser && p.UserID != "" && !p.Client.Ingest:
			return classUserBearer
		case p.Role == core.RoleService && p.UserID == "":
			return classService
		}
	case core.PrincipalSession:
		if p.UserID == "" || p.KeyID != "" || p.Client != (core.Client{}) {
			return classNone
		}
		switch p.Role {
		case core.RoleUser:
			return classUserSession
		case core.RoleAdmin:
			return classAdminSession
		}
	}
	return classNone
}

// policy lists, per action, the principal shapes allowed to perform it.
var policy = map[Action][]class{
	ActionUsageOwn:      {classUserBearer, classUserSession, classAdminSession},
	ActionUsageGlobal:   {classService, classAdminSession},
	ActionUsageRead:     {classUserBearer, classService}, // scope from ReadScope
	ActionStatus:        {classService, classAdminSession},
	ActionDiagnostics:   {classService, classAdminSession},
	ActionBudgetsGlobal: {classService, classAdminSession},
	ActionBudgetOwn:     {classUserBearer, classUserSession, classAdminSession},
	ActionAdmit:         {classUserBearer, classService},
	ActionIngest:        {classService}, // and Client.Ingest, checked in Allow
	ActionKeysOwn:       {classUserSession, classAdminSession},
	ActionAdminUsers:    {classAdminSession},
	ActionAdminAudit:    {classAdminSession},
	ActionSelf:          {classUserSession, classAdminSession},
}

// Allow reports whether p may perform a. Unknown actions and malformed
// principals are denied. Ingest additionally requires the static client's
// explicit Ingest permission.
func Allow(p core.Principal, a Action) bool {
	c := classify(p)
	if c == classNone {
		return false
	}
	for _, ok := range policy[a] {
		if ok == c {
			return a != ActionIngest || p.Client.Ingest
		}
	}
	return false
}

// Scope returns p's own data scope: DataScope{UserID} for every principal
// that belongs to a user (user keys, user-owned static clients and sessions,
// admin sessions included), and DataScope{AllUsers: true} for a service
// client, which owns no data of its own and is a global reader. A malformed
// principal is ErrForbidden.
func Scope(p core.Principal) (core.DataScope, error) {
	switch classify(p) {
	case classUserBearer, classUserSession, classAdminSession:
		return core.DataScope{UserID: p.UserID}, nil
	case classService:
		return core.DataScope{AllUsers: true}, nil
	}
	return core.DataScope{}, ErrForbidden
}

// GlobalScope returns DataScope{AllUsers: true} iff p may read every user's
// data (ActionUsageGlobal); otherwise ErrForbidden.
func GlobalScope(p core.Principal) (core.DataScope, error) {
	if !Allow(p, ActionUsageGlobal) {
		return core.DataScope{}, ErrForbidden
	}
	return core.DataScope{AllUsers: true}, nil
}

// ReadScope is the scope of a bearer (/control/v1) usage read: all users for
// a service client, the owner's own rows for a user bearer. Sessions are
// refused (ErrForbidden): they never authenticate the bearer API, so an admin
// session can never be turned into a global bearer read.
func ReadScope(p core.Principal) (core.DataScope, error) {
	switch classify(p) {
	case classService:
		return core.DataScope{AllUsers: true}, nil
	case classUserBearer:
		return core.DataScope{UserID: p.UserID}, nil
	}
	return core.DataScope{}, ErrForbidden
}
