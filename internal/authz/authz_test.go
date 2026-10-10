package authz

import (
	"errors"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

var (
	userKey = core.Principal{Kind: core.PrincipalUserKey, Role: core.RoleUser,
		Client: core.Client{Name: "k_aaaa", Class: core.ClassInteractive}, UserID: "u_alice", KeyID: "k_aaaa"}
	userStatic = core.Principal{Kind: core.PrincipalStaticClient, Role: core.RoleUser,
		Client: core.Client{Name: "laptop", Class: core.ClassInteractive}, UserID: "u_alice"}
	service = core.Principal{Kind: core.PrincipalStaticClient, Role: core.RoleService,
		Client: core.Client{Name: "agent", Class: core.ClassBackground}}
	ingestService = core.Principal{Kind: core.PrincipalStaticClient, Role: core.RoleService,
		Client: core.Client{Name: "agent", Class: core.ClassBackground, Ingest: true}}
	userSession  = core.Principal{Kind: core.PrincipalSession, Role: core.RoleUser, UserID: "u_alice"}
	adminSession = core.Principal{Kind: core.PrincipalSession, Role: core.RoleAdmin, UserID: "u_root"}
)

func TestUserKeyReadsOwnUsageOnly(t *testing.T) {
	if !Allow(userKey, ActionUsageOwn) {
		t.Fatal("user key must read its own usage")
	}
	for _, a := range []Action{ActionUsageGlobal, ActionStatus, ActionDiagnostics, ActionBudgetsGlobal,
		ActionIngest, ActionKeysOwn, ActionAdminUsers, ActionAdminAudit} {
		if Allow(userKey, a) {
			t.Errorf("user key allowed %s", a)
		}
	}
}

func TestMatrix(t *testing.T) {
	type row struct {
		name string
		p    core.Principal
		a    Action
		want bool
	}
	rows := []row{
		{"user key admit", userKey, ActionAdmit, true},
		{"user static admit", userStatic, ActionAdmit, true},
		{"user static own usage", userStatic, ActionUsageOwn, true},
		{"user static status", userStatic, ActionStatus, false},
		{"user static ingest even with flag", withIngest(userStatic), ActionIngest, false},
		{"user static keys", userStatic, ActionKeysOwn, false},

		{"service global usage", service, ActionUsageGlobal, true},
		{"service status", service, ActionStatus, true},
		{"service diagnostics", service, ActionDiagnostics, true},
		{"service budgets", service, ActionBudgetsGlobal, true},
		{"service admit", service, ActionAdmit, true},
		{"service own usage (no owner)", service, ActionUsageOwn, false},
		{"service ingest without flag", service, ActionIngest, false},
		{"service ingest with flag", ingestService, ActionIngest, true},
		{"service admin users", ingestService, ActionAdminUsers, false},
		{"service keys", ingestService, ActionKeysOwn, false},
		{"service own budget", service, ActionBudgetOwn, false},

		{"user session own usage", userSession, ActionUsageOwn, true},
		{"user session own keys", userSession, ActionKeysOwn, true},
		{"user session own budget", userSession, ActionBudgetOwn, true},
		{"user session global", userSession, ActionUsageGlobal, false},
		{"user session admin", userSession, ActionAdminUsers, false},
		{"user session admit (bearer only)", userSession, ActionAdmit, false},
		{"user session ingest", userSession, ActionIngest, false},

		{"admin session global", adminSession, ActionUsageGlobal, true},
		{"admin session status", adminSession, ActionStatus, true},
		{"admin session diagnostics", adminSession, ActionDiagnostics, true},
		{"admin session budgets", adminSession, ActionBudgetsGlobal, true},
		{"admin session users", adminSession, ActionAdminUsers, true},
		{"admin session audit", adminSession, ActionAdminAudit, true},
		{"admin session own keys", adminSession, ActionKeysOwn, true},
		{"admin session own usage", adminSession, ActionUsageOwn, true},
		{"admin session ingest", adminSession, ActionIngest, false},
		{"admin session admit", adminSession, ActionAdmit, false},

		{"user key own budget", userKey, ActionBudgetOwn, true},
		{"unknown action", adminSession, Action("admin.everything"), false},
	}
	for _, r := range rows {
		if got := Allow(r.p, r.a); got != r.want {
			t.Errorf("%s: Allow = %v, want %v", r.name, got, r.want)
		}
	}
}

func withIngest(p core.Principal) core.Principal {
	p.Client.Ingest = true
	return p
}

func TestMalformedPrincipalsDeniedEverything(t *testing.T) {
	bad := map[string]core.Principal{
		"zero":                {},
		"legacy":              {Kind: core.PrincipalStaticClient, Role: core.RoleLegacy, Client: core.Client{Name: "x", Ingest: true}},
		"admin bearer key":    {Kind: core.PrincipalUserKey, Role: core.RoleAdmin, UserID: "u", KeyID: "k"},
		"admin static":        {Kind: core.PrincipalStaticClient, Role: core.RoleAdmin, Client: core.Client{Name: "x"}, UserID: "u"},
		"user key no owner":   {Kind: core.PrincipalUserKey, Role: core.RoleUser, KeyID: "k"},
		"user key no key id":  {Kind: core.PrincipalUserKey, Role: core.RoleUser, UserID: "u"},
		"user key ingest":     {Kind: core.PrincipalUserKey, Role: core.RoleUser, UserID: "u", KeyID: "k", Client: core.Client{Ingest: true}},
		"user static no own":  {Kind: core.PrincipalStaticClient, Role: core.RoleUser, Client: core.Client{Name: "x"}},
		"session no user":     {Kind: core.PrincipalSession, Role: core.RoleAdmin},
		"session service":     {Kind: core.PrincipalSession, Role: core.RoleService, UserID: "u"},
		"session with key":    {Kind: core.PrincipalSession, Role: core.RoleAdmin, UserID: "u", KeyID: "k"},
		"service with owner":  {Kind: core.PrincipalStaticClient, Role: core.RoleService, Client: core.Client{Name: "x"}, UserID: "u"},
		"service as user key": {Kind: core.PrincipalUserKey, Role: core.RoleService, UserID: "u", KeyID: "k"},
		"unknown kind":        {Kind: "cookie", Role: core.RoleUser, UserID: "u"},
	}
	all := []Action{ActionSelf, ActionUsageOwn, ActionUsageRead, ActionUsageGlobal, ActionStatus, ActionDiagnostics, ActionBudgetsGlobal,
		ActionBudgetOwn, ActionAdmit, ActionIngest, ActionKeysOwn, ActionAdminUsers, ActionAdminAudit}
	for name, p := range bad {
		for _, a := range all {
			if Allow(p, a) {
				t.Errorf("%s: allowed %s", name, a)
			}
		}
		if _, err := Scope(p); err == nil {
			t.Errorf("%s: Scope succeeded", name)
		}
		if _, err := GlobalScope(p); err == nil {
			t.Errorf("%s: GlobalScope succeeded", name)
		}
	}
}

func TestScope(t *testing.T) {
	cases := []struct {
		name string
		p    core.Principal
		want core.DataScope
	}{
		{"user key", userKey, core.DataScope{UserID: "u_alice"}},
		{"user static", userStatic, core.DataScope{UserID: "u_alice"}},
		{"user session", userSession, core.DataScope{UserID: "u_alice"}},
		{"admin session own", adminSession, core.DataScope{UserID: "u_root"}},
		{"service", service, core.DataScope{AllUsers: true}},
	}
	for _, c := range cases {
		got, err := Scope(c.p)
		if err != nil || got != c.want {
			t.Errorf("%s: Scope = %+v, %v; want %+v", c.name, got, err, c.want)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%s: scope invalid: %v", c.name, err)
		}
	}
}

func TestGlobalScope(t *testing.T) {
	for _, p := range []core.Principal{service, adminSession} {
		got, err := GlobalScope(p)
		if err != nil || got != (core.DataScope{AllUsers: true}) {
			t.Errorf("%+v: GlobalScope = %+v, %v", p, got, err)
		}
	}
	for _, p := range []core.Principal{userKey, userStatic, userSession} {
		if _, err := GlobalScope(p); !errors.Is(err, ErrForbidden) {
			t.Errorf("%+v: GlobalScope err = %v, want ErrForbidden", p, err)
		}
	}
}

func TestReadScopeChoosesGlobalOnlyForGlobalReaders(t *testing.T) {
	// ReadScope is the single helper /control/v1 read endpoints use: global
	// readers see all users, owners see only themselves.
	for _, c := range []struct {
		p    core.Principal
		want core.DataScope
	}{
		{service, core.DataScope{AllUsers: true}},
		{userKey, core.DataScope{UserID: "u_alice"}},
		{userStatic, core.DataScope{UserID: "u_alice"}},
	} {
		got, err := ReadScope(c.p)
		if err != nil || got != c.want {
			t.Errorf("%+v: ReadScope = %+v, %v; want %+v", c.p, got, err, c.want)
		}
	}
	// A session never reaches the bearer API; ReadScope refuses it so an
	// admin session cannot be turned into a global bearer read by mistake.
	if _, err := ReadScope(adminSession); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin session ReadScope err = %v", err)
	}
}

func TestUsageReadIsBearerOwnOrGlobal(t *testing.T) {
	// ActionUsageRead gates the bearer usage/analytics routes; ReadScope then
	// picks own vs all rows. Sessions use the explicit own/global actions.
	for _, c := range []struct {
		name string
		p    core.Principal
		want bool
	}{
		{"user key", userKey, true},
		{"user static", userStatic, true},
		{"service", service, true},
		{"user session", userSession, false},
		{"admin session", adminSession, false},
	} {
		if got := Allow(c.p, ActionUsageRead); got != c.want {
			t.Errorf("%s: Allow(usage.read) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSelfProfileIsSessionOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		p    core.Principal
		want bool
	}{
		{"user session", userSession, true},
		{"admin session", adminSession, true},
		{"user key", userKey, false},
		{"service", ingestService, false},
	} {
		if got := Allow(c.p, ActionSelf); got != c.want {
			t.Errorf("%s: Allow(self) = %v, want %v", c.name, got, c.want)
		}
	}
}
