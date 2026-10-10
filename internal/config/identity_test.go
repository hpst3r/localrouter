package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
)

// wantErrs loads body and requires an error mentioning every want.
func wantErrs(t *testing.T, body string, want ...string) {
	t.Helper()
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatalf("expected error mentioning %q, got nil", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("missing %q in %v", w, err)
		}
	}
}

// The identity-only fields are new, so no existing config sets them; without
// the identity block they would be silently ignored, so they are rejected.
func TestLegacyRejectsIdentityOnlyFields(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"client role": {`
clients: [{name: a, class: interactive, key_file: k, role: service}]
`, "clients[a].role requires the identity block"},
		"client owner": {`
clients: [{name: a, class: interactive, key_file: k, owner: u_aaaaaaaaaaaaaaaaaaaaaaaaaa}]
`, "clients[a].owner requires the identity block"},
		"per-user concurrency": {`
clients: [{name: a, class: interactive, key_file: k}]
limits: {max_concurrent_per_user: 2}
`, "limits.max_concurrent_per_user requires the identity block"},
		"user budgets": {`
clients: [{name: a, class: interactive, key_file: k}]
budgets:
  reserve_usd: "0.1"
  clients: {a: {daily_usd: "1"}}
  users: {daily_usd: "1"}
`, "budgets.users requires the identity block"},
	} {
		t.Run(name, func(t *testing.T) { wantErrs(t, tc.body, tc.want) })
	}
}

// identityBase is a minimal valid multi-user config: loopback public origin,
// no static clients (user keys are dynamic), control auth on.
const identityBase = `
control: {require_auth: true}
identity:
  public_base_url: https://localhost:8787
  oidc:
    issuer: https://idp.example.test/realms/lr
    client_id: localrouter
    client_secret_file: oidc.secret
  access:
    claim: groups
    user_values: [lr-users]
    admin_values: [lr-admins]
`

func TestIdentityMinimalLoadsWithDefaults(t *testing.T) {
	p := write(t, identityBase)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	id := c.Identity
	if id == nil {
		t.Fatal("identity block not loaded")
	}
	if len(c.Clients) != 0 {
		t.Fatalf("clients = %+v", c.Clients)
	}
	if want := filepath.Join(filepath.Dir(p), "oidc.secret"); id.OIDC.ClientSecretFile != want {
		t.Errorf("client_secret_file = %q, want %q", id.OIDC.ClientSecretFile, want)
	}
	for name, got := range map[string]struct{ got, want time.Duration }{
		"keys.max_ttl":              {id.Keys.MaxTTL.D(), 90 * 24 * time.Hour},
		"keys.require_login_within": {id.Keys.RequireLoginWithin.D(), 30 * 24 * time.Hour},
		"session.idle_ttl":          {id.Session.IdleTTL.D(), 8 * time.Hour},
		"session.absolute_ttl":      {id.Session.AbsoluteTTL.D(), 24 * time.Hour},
		"oidc.http_timeout":         {id.OIDC.HTTPTimeout.D(), 10 * time.Second},
	} {
		if got.got != got.want {
			t.Errorf("%s = %s, want %s", name, got.got, got.want)
		}
	}
	if id.Keys.MaxPerUser != 10 {
		t.Errorf("keys.max_per_user = %d, want 10", id.Keys.MaxPerUser)
	}
	if id.OIDC.ClientAuth != "client_secret_basic" {
		t.Errorf("oidc.client_auth = %q", id.OIDC.ClientAuth)
	}
	if !c.MultiUser() {
		t.Error("MultiUser() = false with identity block")
	}
}

// Without the identity block a client is still required; a nil block is
// single-user.
func TestEmptyClientsOnlyWithIdentity(t *testing.T) {
	wantErrs(t, "listen: 127.0.0.1:8787\n", "at least one client is required")
	var c Config
	if c.MultiUser() {
		t.Fatal("MultiUser() = true without identity block")
	}
}

// validOwner has the identity store's opaque user ID grammar.
const validOwner = "u_abcdefghijklmnopqrstuvwxyz"

func TestIdentityStaticClientRoles(t *testing.T) {
	c, err := Load(write(t, identityBase+`
clients:
  - {name: agent, class: background, key_file: a.key, role: service, ingest: true}
  - {name: laptop, class: interactive, key_file: l.key, role: user, owner: `+validOwner+`}
`))
	if err != nil {
		t.Fatalf("valid service/user clients rejected: %v", err)
	}
	if c.Clients[0].Role != "service" || c.Clients[1].Owner != validOwner {
		t.Fatalf("roles not loaded: %+v", c.Clients)
	}
	for name, tc := range map[string]struct{ client, want string }{
		"missing role":     {`{name: a, class: interactive, key_file: k}`, "clients[a].role must be service or user"},
		"static admin":     {`{name: a, class: interactive, key_file: k, role: admin}`, "clients[a].role admin is not allowed"},
		"legacy role":      {`{name: a, class: interactive, key_file: k, role: legacy}`, "clients[a].role must be service or user"},
		"unknown role":     {`{name: a, class: interactive, key_file: k, role: Service}`, "clients[a].role must be service or user"},
		"user no owner":    {`{name: a, class: interactive, key_file: k, role: user}`, "clients[a].owner is required for role user"},
		"user bad owner":   {`{name: a, class: interactive, key_file: k, role: user, owner: alice@example.com}`, "clients[a].owner must be an identity user ID"},
		"user short owner": {`{name: a, class: interactive, key_file: k, role: user, owner: u_abc}`, "clients[a].owner must be an identity user ID"},
		"user upper owner": {`{name: a, class: interactive, key_file: k, role: user, owner: u_ABCDEFGHIJKLMNOPQRSTUVWXYZ}`, "clients[a].owner must be an identity user ID"},
		"user ingest":      {`{name: a, class: interactive, key_file: k, role: user, owner: ` + validOwner + `, ingest: true}`, "clients[a].ingest is not allowed for role user"},
		"service owner":    {`{name: a, class: interactive, key_file: k, role: service, owner: ` + validOwner + `}`, "clients[a].owner applies only to role user"},
	} {
		t.Run(name, func(t *testing.T) {
			wantErrs(t, identityBase+"clients: ["+tc.client+"]\n", tc.want)
		})
	}
}

// withIdentity replaces the public_base_url line of identityBase and
// prepends top-level settings.
func withIdentity(top, publicURL string) string {
	return top + strings.Replace(identityBase, "https://localhost:8787", publicURL, 1)
}

// Multi-user mode exposes user data through the control API, so it can never
// run with control auth off, even on loopback.
func TestIdentityRequiresControlAuth(t *testing.T) {
	body := strings.Replace(identityBase, "control: {require_auth: true}", "", 1)
	wantErrs(t, body, "identity requires control.require_auth: true")
}

func TestIdentityPublicBaseURL(t *testing.T) {
	for name, u := range map[string]string{
		"empty":    `""`,
		"http":     "http://localhost:8787",
		"path":     "https://localhost:8787/lr",
		"query":    "https://localhost:8787/?x=1",
		"fragment": "https://localhost:8787/#x",
		"userinfo": "https://u:p@localhost:8787",
		"no host":  "https:///x",
		"relative": "localhost:8787",
	} {
		t.Run(name, func(t *testing.T) {
			wantErrs(t, withIdentity("", u), "identity.public_base_url")
		})
	}
	c, err := Load(write(t, withIdentity("", "HTTPS://LocalHost:443/")))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.Identity.PublicBaseURL; got != "https://localhost" {
		t.Fatalf("public_base_url normalized to %q, want https://localhost", got)
	}
}

// Browsers send the public host as Host; the app's HostGuard admits only
// loopback names unless allow_non_loopback lists it in allowed_hosts. A
// public origin the guard would refuse is a configuration error, not a
// runtime 403 on every login.
func TestIdentityPublicHostMustPassHostGuard(t *testing.T) {
	const exposed = "listen: 0.0.0.0:8787\nallow_non_loopback: true\n"
	for name, tc := range map[string]struct{ top, url string }{
		"loopback name":     {"", "https://localhost:8787"},
		"loopback v4":       {"", "https://127.0.0.1:8787"},
		"loopback v6":       {"", "https://[::1]:8787"},
		"listed name":       {exposed + "allowed_hosts: [Router.Example.COM.]\n", "https://router.example.com"},
		"listed ip":         {exposed + "allowed_hosts: [192.0.2.5]\n", "https://192.0.2.5:8443"},
		"listed among many": {exposed + "allowed_hosts: [a.example, router.example.com]\n", "https://router.example.com"},
	} {
		t.Run("ok/"+name, func(t *testing.T) {
			if _, err := Load(write(t, withIdentity(tc.top, tc.url))); err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
	for name, tc := range map[string]struct{ top, url string }{
		"loopback listen": {"", "https://router.example.com"},
		"not listed":      {exposed + "allowed_hosts: [other.example]\n", "https://router.example.com"},
		"no allowed list": {exposed, "https://router.example.com"},
		"ip not listed":   {exposed + "allowed_hosts: [192.0.2.6]\n", "https://192.0.2.5"},
	} {
		t.Run("reject/"+name, func(t *testing.T) {
			wantErrs(t, withIdentity(tc.top, tc.url), "identity.public_base_url host must be loopback or listed in allowed_hosts")
		})
	}
}

// withOIDC appends extra lines to identityBase's oidc block.
func withOIDC(extra string) string {
	return strings.Replace(identityBase, "    client_secret_file: oidc.secret\n",
		"    client_secret_file: oidc.secret\n"+extra, 1)
}

// replaceLine swaps one exact line of identityBase.
func replaceLine(old, new string) string {
	if !strings.Contains(identityBase, old) {
		panic("identityBase has no " + old)
	}
	return strings.Replace(identityBase, old, new, 1)
}

func TestIdentityOIDCValid(t *testing.T) {
	c, err := Load(write(t, withOIDC(`    client_auth: client_secret_post
    scopes: [openid, profile, groups]
    signing_algs: [RS256, ES256]
    tenant_id: lr
    allow_private_network: true
    extra_endpoint_hosts: [login.example.test]
    http_timeout: 5s
`)))
	if err != nil {
		t.Fatalf("valid oidc block rejected: %v", err)
	}
	o := c.Identity.OIDC
	if o.ClientAuth != "client_secret_post" || len(o.Scopes) != 3 || len(o.SigningAlgs) != 2 ||
		!o.AllowPrivateNetwork || o.HTTPTimeout.D() != 5*time.Second || o.TenantID != "lr" ||
		len(o.ExtraEndpointHosts) != 1 {
		t.Fatalf("oidc block not loaded: %+v", o)
	}
}

func TestIdentityOIDCInvalid(t *testing.T) {
	const iss = "    issuer: https://idp.example.test/realms/lr\n"
	const cid = "    client_id: localrouter\n"
	const sec = "    client_secret_file: oidc.secret\n"
	for name, tc := range map[string]struct{ body, want string }{
		"issuer missing":      {replaceLine(iss, ""), "identity.oidc.issuer"},
		"issuer http":         {replaceLine(iss, "    issuer: http://idp.example.test/realms/lr\n"), "identity.oidc.issuer"},
		"issuer query":        {replaceLine(iss, "    issuer: https://idp.example.test/?a=b\n"), "identity.oidc.issuer"},
		"issuer userinfo":     {replaceLine(iss, "    issuer: https://u@idp.example.test/\n"), "identity.oidc.issuer"},
		"issuer template":     {replaceLine(iss, "    issuer: \"https://login.example.test/{tenantid}/v2.0\"\n"), "identity.oidc.issuer"},
		"issuer entra common": {replaceLine(iss, "    issuer: https://login.microsoftonline.com/common/v2.0\n"), "identity"},
		"client id missing":   {replaceLine(cid, ""), "identity.oidc.client_id"},
		"client id space":     {replaceLine(cid, "    client_id: \"local router\"\n"), "identity.oidc.client_id"},
		"client id long":      {replaceLine(cid, "    client_id: "+strings.Repeat("x", 257)+"\n"), "identity.oidc.client_id"},
		"secret missing":      {replaceLine(sec, ""), "identity.oidc.client_secret_file is required"},
		"client auth":         {withOIDC("    client_auth: private_key_jwt\n"), "identity.oidc.client_auth"},
		"no openid":           {withOIDC("    scopes: [profile]\n"), "identity.oidc.scopes"},
		"offline access":      {withOIDC("    scopes: [openid, offline_access]\n"), "identity.oidc.scopes"},
		"bad scope token":     {withOIDC("    scopes: [openid, \"a b\"]\n"), "identity.oidc.scopes"},
		"duplicate scope":     {withOIDC("    scopes: [openid, openid]\n"), "identity.oidc.scopes"},
		"hs256":               {withOIDC("    signing_algs: [HS256]\n"), "identity.oidc.signing_algs"},
		"alg none":            {withOIDC("    signing_algs: [none]\n"), "identity.oidc.signing_algs"},
		"extra host url":      {withOIDC("    extra_endpoint_hosts: [\"https://x.example\"]\n"), "identity.oidc.extra_endpoint_hosts"},
		"extra host empty":    {withOIDC("    extra_endpoint_hosts: [\"\"]\n"), "identity.oidc.extra_endpoint_hosts"},
		"timeout negative":    {withOIDC("    http_timeout: -1s\n"), "identity.oidc.http_timeout"},
		"timeout too long":    {withOIDC("    http_timeout: 61s\n"), "identity.oidc.http_timeout"},
		"tenant not in iss":   {withOIDC("    tenant_id: other\n"), "identity.oidc.tenant_id"},
	} {
		t.Run(name, func(t *testing.T) { wantErrs(t, tc.body, tc.want) })
	}
}

// withAccess replaces identityBase's access block.
func withAccess(access string) string {
	i := strings.Index(identityBase, "  access:\n")
	return identityBase[:i] + "  access:\n" + access
}

func TestIdentityAccessPolicyValid(t *testing.T) {
	for name, access := range map[string]string{
		"user and admin values": "    claim: groups\n    user_values: [u]\n    admin_values: [a]\n",
		// The admin subject must still carry an allowed claim value to log
		// in; the subject only promotes that login to admin.
		"user values and admin subject": "    claim: roles\n    user_values: [u]\n    admin_subjects: [\"0b4c-11\"]\n",
		"admin values only":             "    claim: \"https://example.test/roles\"\n    admin_values: [a]\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(write(t, withAccess(access)))
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if c.Identity.Access.Claim == "" {
				t.Fatal("claim not loaded")
			}
		})
	}
}

func TestIdentityAccessPolicyInvalid(t *testing.T) {
	long := strings.Repeat("v", 257)
	for name, tc := range map[string]struct{ access, want string }{
		"no claim":             {"    user_values: [u]\n    admin_values: [a]\n", "identity.access.claim is required"},
		"subject only":         {"    claim: groups\n    admin_subjects: [s]\n", "identity.access requires at least one user_values or admin_values entry"},
		"subject only noclaim": {"    admin_subjects: [s]\n", "identity.access.claim is required"},
		"no admin mapping":     {"    claim: groups\n    user_values: [u]\n", "identity.access requires an admin mapping"},
		"reserved claim":       {"    claim: email\n    user_values: [u]\n    admin_values: [a]\n", "identity.access.claim"},
		"subject claim":        {"    claim: sub\n    user_values: [u]\n    admin_values: [a]\n", "identity.access.claim"},
		"bad claim chars":      {"    claim: \"gr oups\"\n    user_values: [u]\n    admin_values: [a]\n", "identity.access.claim"},
		"empty user value":     {"    claim: g\n    user_values: [\"\"]\n    admin_values: [a]\n", "identity.access.user_values"},
		"dup admin value":      {"    claim: g\n    user_values: [u]\n    admin_values: [a, a]\n", "identity.access.admin_values"},
		"long value":           {"    claim: g\n    user_values: [" + long + "]\n    admin_values: [a]\n", "identity.access.user_values"},
		"overlap":              {"    claim: g\n    user_values: [x]\n    admin_values: [x]\n", "identity.access: value listed in both user_values and admin_values"},
		"empty subject":        {"    claim: g\n    user_values: [u]\n    admin_subjects: [\"\"]\n", "identity.access.admin_subjects"},
		"dup subject":          {"    claim: g\n    user_values: [u]\n    admin_subjects: [s, s]\n", "identity.access.admin_subjects"},
		"long subject":         {"    claim: g\n    user_values: [u]\n    admin_subjects: [" + strings.Repeat("s", 256) + "]\n", "identity.access.admin_subjects"},
	} {
		t.Run(name, func(t *testing.T) { wantErrs(t, withAccess(tc.access), tc.want) })
	}
}

// withBlock appends a keys/session block to identityBase.
func withBlock(block string) string { return identityBase + block }

func TestIdentityLifetimesTighten(t *testing.T) {
	c, err := Load(write(t, withBlock(`  keys: {max_ttl: 720h, require_login_within: 168h, max_per_user: 3}
  session: {absolute_ttl: 4h}
`)))
	if err != nil {
		t.Fatalf("tightened lifetimes rejected: %v", err)
	}
	k, s := c.Identity.Keys, c.Identity.Session
	if k.MaxTTL.D() != 720*time.Hour || k.RequireLoginWithin.D() != 168*time.Hour || k.MaxPerUser != 3 {
		t.Fatalf("keys = %+v", k)
	}
	// A defaulted idle lifetime is clamped to a tighter absolute lifetime.
	if s.AbsoluteTTL.D() != 4*time.Hour || s.IdleTTL.D() != 4*time.Hour {
		t.Fatalf("session = %+v", s)
	}
	c, err = Load(write(t, withBlock("  session: {idle_ttl: 30m}\n")))
	if err != nil {
		t.Fatalf("idle 30m rejected: %v", err)
	}
	if c.Identity.Session.IdleTTL.D() != 30*time.Minute || c.Identity.Session.AbsoluteTTL.D() != 24*time.Hour {
		t.Fatalf("session = %+v", c.Identity.Session)
	}
}

func TestIdentityLifetimesCannotLoosen(t *testing.T) {
	for name, tc := range map[string]struct{ block, want string }{
		"key ttl above 90d":      {"  keys: {max_ttl: 2161h}\n", "identity.keys.max_ttl must be within (0, 2160h"},
		"key ttl negative":       {"  keys: {max_ttl: -1h}\n", "identity.keys.max_ttl must be within"},
		"login above 30d":        {"  keys: {require_login_within: 721h}\n", "identity.keys.require_login_within must be within (0, 720h"},
		"login negative":         {"  keys: {require_login_within: -1s}\n", "identity.keys.require_login_within must be within"},
		"keys per user above":    {"  keys: {max_per_user: 101}\n", "identity.keys.max_per_user must be within [1, 100]"},
		"keys per user negative": {"  keys: {max_per_user: -1}\n", "identity.keys.max_per_user must be within [1, 100]"},
		"absolute above 24h":     {"  session: {absolute_ttl: 25h}\n", "identity.session.absolute_ttl must be within (0, 24h"},
		"idle above 8h":          {"  session: {idle_ttl: 9h}\n", "identity.session.idle_ttl must be within (0, 8h"},
		"idle negative":          {"  session: {idle_ttl: -1m}\n", "identity.session.idle_ttl must be within"},
		"idle above absolute":    {"  session: {idle_ttl: 5h, absolute_ttl: 4h}\n", "identity.session.idle_ttl must not exceed absolute_ttl"},
		"bad duration":           {"  keys: {max_ttl: 90d}\n", "invalid duration"},
	} {
		t.Run(name, func(t *testing.T) { wantErrs(t, withBlock(tc.block), tc.want) })
	}
}

func TestIdentityUserBudgetDefault(t *testing.T) {
	c, err := Load(write(t, identityBase+`
budgets:
  reserve_usd: "0.05"
  users: {daily_usd: "2", monthly_usd: "0"}
`))
	if err != nil {
		t.Fatalf("user-only budget rejected: %v", err)
	}
	const uid = "u_abcdefghijklmnopqrstuvwxyz"
	got, err := c.Budgets.UserLimits(uid)
	want := []budget.Limit{
		{Scope: budget.ScopeUser, Key: uid, Period: budget.PeriodDay, Micros: 2_000_000},
		{Scope: budget.ScopeUser, Key: uid, Period: budget.PeriodMonth, Micros: 0},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("UserLimits = %+v, %v; want %+v", got, err, want)
	}
	// The global list holds only named client/account ceilings.
	if ls, err := c.Budgets.Limits(); err != nil || len(ls) != 0 {
		t.Fatalf("Limits() = %+v, %v; want none", ls, err)
	}
	if _, err := c.Budgets.UserLimits(""); err == nil {
		t.Fatal("UserLimits with empty user ID must fail")
	}
	var none *BudgetConfig
	if ls, err := none.UserLimits(uid); ls != nil || err != nil {
		t.Fatalf("nil budgets UserLimits = %v, %v", ls, err)
	}
	noUsers := &BudgetConfig{ReserveUSD: "1"}
	if ls, err := noUsers.UserLimits(uid); ls != nil || err != nil {
		t.Fatalf("no users default UserLimits = %v, %v", ls, err)
	}
}

func TestIdentityUserBudgetValidation(t *testing.T) {
	for name, tc := range map[string]struct{ users, want string }{
		"empty users only": {"  users: {}\n", "at least one client, account or user daily_usd/monthly_usd ceiling"},
		"null users only":  {"  users: {daily_usd: null}\n", "at least one client, account or user"},
		"negative":         {"  users: {daily_usd: \"-1\"}\n", "budgets.users.daily_usd"},
		"malformed":        {"  users: {monthly_usd: \"1e3\"}\n", "budgets.users.monthly_usd"},
	} {
		t.Run(name, func(t *testing.T) {
			wantErrs(t, identityBase+"budgets:\n  reserve_usd: \"0.05\"\n"+tc.users, tc.want)
		})
	}
}

func TestIdentityPerUserConcurrency(t *testing.T) {
	c, err := Load(write(t, identityBase+"limits: {max_concurrent_per_user: 3}\n"))
	if err != nil {
		t.Fatalf("per-user limit rejected: %v", err)
	}
	if c.Limits.MaxConcurrentPerUser != 3 {
		t.Fatalf("max_concurrent_per_user = %d", c.Limits.MaxConcurrentPerUser)
	}
	wantErrs(t, identityBase+"limits: {max_concurrent_per_user: -1}\n",
		"limits.max_concurrent_per_user must not be negative")
}

// Shipped legacy example configs load exactly as before: no identity block,
// and every new field stays at its zero value.
func TestLegacyExamplesUnchanged(t *testing.T) {
	for _, p := range []string{"../../config.example.yaml", "../../config.server.example.yaml", "../../config.openrouter.example.yaml"} {
		t.Run(filepath.Base(p), func(t *testing.T) {
			c, err := Load(p)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if c.MultiUser() || c.Identity != nil || c.Limits.MaxConcurrentPerUser != 0 ||
				(c.Budgets != nil && c.Budgets.Users != nil) {
				t.Fatalf("new fields set on legacy config: %+v", c)
			}
			for _, cl := range c.Clients {
				if cl.Role != "" || cl.Owner != "" {
					t.Fatalf("client %s gained role/owner", cl.Name)
				}
			}
		})
	}
}

// An empty identity block is multi-user mode with nothing configured: it is
// rejected rather than treated as absent.
func TestIdentityEmptyBlockRejected(t *testing.T) {
	wantErrs(t, "control: {require_auth: true}\nidentity: {}\n",
		"identity.public_base_url", "identity.oidc.issuer", "identity.oidc.client_id",
		"identity.oidc.client_secret_file is required", "identity.access.claim is required")
	c, err := Load(write(t, "clients: [{name: a, class: interactive, key_file: k}]\nidentity:\n"))
	if err != nil || c.Identity != nil {
		t.Fatalf("null identity block must be legacy: %+v, %v", c, err)
	}
}

// A user API key's id ("k_"…) is its client name for budgets and the ledger,
// and user IDs are "u_"…, so in multi-user mode a static client named into
// either namespace would share spend and ceilings with an identity principal.
// The real multi-user example with its service client renamed to a key id
// must fail config validation, and so `localrouter check`, not only app Build.
func TestIdentityStaticClientNamesOutsideIdentityNamespaces(t *testing.T) {
	example, err := os.ReadFile("../../config.example.multiuser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load("../../config.example.multiuser.yaml"); err != nil {
		t.Fatalf("multi-user example: %v", err)
	}
	keyID := "k_abcdefghijklmnopqrstuvwxyz"
	renamed := strings.Replace(string(example), "name: agent-laptop,", "name: "+keyID+",", 1)
	if renamed == string(example) {
		t.Fatal("example no longer has the agent-laptop client")
	}
	wantErrs(t, renamed, "clients["+keyID+"].name must not start with k_ or u_")

	for _, name := range []string{keyID, "k_x", "k_", validOwner, "u_x"} {
		t.Run(name, func(t *testing.T) {
			wantErrs(t, identityBase+"clients: [{name: "+name+", class: interactive, key_file: k, role: service}]\n",
				"clients["+name+"].name must not start with k_ or u_")
		})
	}
	for _, name := range []string{"k-agent", "kx", "K_abc", "U_abc", "agent_k_", "lrk_agent"} {
		if _, err := Load(write(t, identityBase+"clients: [{name: "+name+", class: interactive, key_file: k, role: service}]\n")); err != nil {
			t.Errorf("multi-user client %q rejected: %v", name, err)
		}
	}
	// Legacy names are unchanged: the identity namespaces do not exist there.
	for _, name := range []string{keyID, "k_x", validOwner, "u_x"} {
		if _, err := Load(write(t, "clients: [{name: "+name+", class: interactive, key_file: k}]\n")); err != nil {
			t.Errorf("legacy client %q rejected: %v", name, err)
		}
	}
}
