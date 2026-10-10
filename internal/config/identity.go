package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

// IdentityConfig is the optional multi-user block (yaml: identity). When it
// is present, browsers sign in through one OIDC provider and applications
// use user-owned LocalRouter API keys. A nil *IdentityConfig keeps the legacy
// single-user behavior exactly. The block is restart-only.
//
// With the block present, Validate also requires control.require_auth: true,
// a role on every static client (see ClientConfig.Role), and a public origin
// the host guard admits; static clients become optional. Durations use Go
// syntax ("2160h", not "90d"). There is deliberately no YAML for the
// login service's test seams (dial override, CA pool, clock), for an inline
// or environment client secret, or for turning JIT provisioning off.
type IdentityConfig struct {
	// PublicBaseURL is the exact https origin browsers use, for example
	// https://router.example.com: no path, query or userinfo. Load
	// normalizes it (lower case, no :443, no trailing slash). The OIDC
	// redirect URI is always PublicBaseURL + "/auth/callback". Its host must
	// be loopback, or listed in allowed_hosts with allow_non_loopback: true.
	PublicBaseURL string                `yaml:"public_base_url"`
	OIDC          OIDCConfig            `yaml:"oidc"`
	Access        IdentityAccessConfig  `yaml:"access"`
	Keys          IdentityKeysConfig    `yaml:"keys"`
	Session       IdentitySessionConfig `yaml:"session"`
}

// OIDCConfig names the single OIDC provider and confidential client. The
// issuer and client ID are pinned in identity.db on first start; changing
// either later makes startup fail.
type OIDCConfig struct {
	// Issuer is the exact https issuer, compared byte for byte. Templated
	// and multi-tenant (Entra common/organizations/consumers) issuers are
	// rejected.
	Issuer string `yaml:"issuer"`
	// ClientID is 1..256 printable non-space characters.
	ClientID string `yaml:"client_id"`
	// ClientSecretFile holds the client secret and is the only source of it.
	// Relative paths resolve against the config file's directory. CheckFiles
	// requires a private regular file of at most MaxClientSecretBytes with a
	// single printable value, distinct from every other secret file.
	ClientSecretFile string `yaml:"client_secret_file"`
	// ClientAuth is client_secret_basic (default) or client_secret_post.
	ClientAuth string `yaml:"client_auth"`
	// Scopes default to [openid, profile]; openid is required and
	// offline_access is refused.
	Scopes []string `yaml:"scopes"`
	// SigningAlgs default to [RS256]; only RS/ES/PS 256-512 and EdDSA.
	SigningAlgs []string `yaml:"signing_algs"`
	// TenantID, when set, must appear in Issuer and equal the tid claim.
	TenantID string `yaml:"tenant_id"`
	// AllowPrivateNetwork lets the provider resolve to loopback, RFC 1918,
	// CGNAT or unique-local addresses (a LAN provider). Link-local and cloud
	// metadata addresses stay denied.
	AllowPrivateNetwork bool `yaml:"allow_private_network"`
	// ExtraEndpointHosts are bare host names, besides the issuer's, that
	// discovery may name for the authorization, token and JWKS endpoints.
	ExtraEndpointHosts []string `yaml:"extra_endpoint_hosts"`
	// HTTPTimeout bounds each provider request. Default 10s, at most 60s.
	HTTPTimeout Duration `yaml:"http_timeout"`
}

// IdentityAccessConfig is the login access policy. Matching is exact and
// case-sensitive. Every login, admins included, must carry an allowed Claim
// value; unknown users are provisioned just in time only after that match.
type IdentityAccessConfig struct {
	// Claim names the ID-token claim (string or string array) holding the
	// allowed values, e.g. groups or roles. Identity and profile claims
	// (sub, email, name, ...) are refused.
	Claim string `yaml:"claim"`
	// UserValues and AdminValues are the Claim values granting each role. A
	// value may not appear in both.
	UserValues  []string `yaml:"user_values"`
	AdminValues []string `yaml:"admin_values"`
	// AdminSubjects are exact sub values promoted to admin. They still need
	// an allowed Claim value to log in. At least one of AdminValues and
	// AdminSubjects is required.
	AdminSubjects []string `yaml:"admin_subjects"`
}

// IdentityKeysConfig bounds user-owned API keys. Each value may only tighten
// the identity store's ceiling.
type IdentityKeysConfig struct {
	// MaxTTL is the longest key lifetime and the default. Default and
	// ceiling 2160h (90 days).
	MaxTTL Duration `yaml:"max_ttl"`
	// RequireLoginWithin is how recent the owner's last allowed browser
	// login must be for a key to authenticate. Default and ceiling 720h
	// (30 days).
	RequireLoginWithin Duration `yaml:"require_login_within"`
	// MaxPerUser caps live keys per user. Default 10, within [1, 100].
	MaxPerUser int `yaml:"max_per_user"`
}

// IdentitySessionConfig bounds browser sessions. Each value may only tighten
// the identity store's ceiling.
type IdentitySessionConfig struct {
	// IdleTTL defaults to the lesser of 8h (the ceiling) and AbsoluteTTL; an
	// explicit value above AbsoluteTTL is rejected.
	IdleTTL Duration `yaml:"idle_ttl"`
	// AbsoluteTTL defaults to and is capped at 24h.
	AbsoluteTTL Duration `yaml:"absolute_ttl"`
}

// validateRole checks a client's role and owner. Without the identity block
// both fields are rejected: they would otherwise be silently ignored.
func (cl ClientConfig) validateRole(multiUser bool) []error {
	var errs []error
	if !multiUser {
		if cl.Role != "" {
			errs = append(errs, fmt.Errorf("clients[%s].role requires the identity block", cl.Name))
		}
		if cl.Owner != "" {
			errs = append(errs, fmt.Errorf("clients[%s].owner requires the identity block", cl.Name))
		}
		return errs
	}
	// A user API key's id ("k_"…) is its client name for budgets and the
	// ledger, and user IDs are "u_"…, so a static client named into either
	// namespace could share spend and ceilings with an identity principal.
	if strings.HasPrefix(cl.Name, "k_") || strings.HasPrefix(cl.Name, "u_") {
		errs = append(errs, fmt.Errorf("clients[%s].name must not start with k_ or u_ in multi-user mode (identity key and user ID namespaces)", cl.Name))
	}
	switch core.Role(cl.Role) {
	case core.RoleService:
		if cl.Owner != "" {
			errs = append(errs, fmt.Errorf("clients[%s].owner applies only to role user", cl.Name))
		}
	case core.RoleUser:
		switch {
		case cl.Owner == "":
			errs = append(errs, fmt.Errorf("clients[%s].owner is required for role user", cl.Name))
		case !userIDRE.MatchString(cl.Owner):
			errs = append(errs, fmt.Errorf("clients[%s].owner must be an identity user ID (u_ followed by 26 base32 characters)", cl.Name))
		}
		if cl.Ingest {
			errs = append(errs, fmt.Errorf("clients[%s].ingest is not allowed for role user", cl.Name))
		}
	case core.RoleAdmin:
		errs = append(errs, fmt.Errorf("clients[%s].role admin is not allowed: admin is browser-session only", cl.Name))
	default:
		errs = append(errs, fmt.Errorf("clients[%s].role must be service or user in multi-user mode", cl.Name))
	}
	return errs
}

// userIDRE is the identity store's opaque user ID grammar. Whether the owner
// exists and is active is checked on every request, not here: a user may be
// provisioned only after this config is written.
var userIDRE = regexp.MustCompile(`^u_[a-z2-7]{26}$`)

// MultiUser reports whether the identity block enables multi-user mode.
func (c *Config) MultiUser() bool { return c.Identity != nil }

// Defaults for the identity block. Key and session lifetimes default to the
// identity store's ceilings, which configuration may only tighten.
const (
	defaultOIDCHTTPTimeout = 10 * time.Second
	defaultOIDCClientAuth  = "client_secret_basic"
)

func (i *IdentityConfig) applyDefaults(baseDir string) {
	if u, err := publicOrigin(i.PublicBaseURL); err == nil {
		i.PublicBaseURL = u
	}
	i.OIDC.ClientSecretFile = expand(i.OIDC.ClientSecretFile, baseDir)
	if i.OIDC.ClientAuth == "" {
		i.OIDC.ClientAuth = defaultOIDCClientAuth
	}
	if i.OIDC.HTTPTimeout == 0 {
		i.OIDC.HTTPTimeout = Duration(defaultOIDCHTTPTimeout)
	}
	if i.Keys.MaxTTL == 0 {
		i.Keys.MaxTTL = Duration(identity.MaxKeyTTL)
	}
	if i.Keys.RequireLoginWithin == 0 {
		i.Keys.RequireLoginWithin = Duration(identity.MaxLoginAge)
	}
	if i.Keys.MaxPerUser == 0 {
		i.Keys.MaxPerUser = identity.DefaultMaxKeysPerUser
	}
	if i.Session.AbsoluteTTL == 0 {
		i.Session.AbsoluteTTL = Duration(identity.MaxSessionAbsolute)
	}
	// A defaulted idle lifetime never exceeds a tightened absolute one.
	if i.Session.IdleTTL == 0 {
		i.Session.IdleTTL = Duration(min(identity.MaxSessionIdle, i.Session.AbsoluteTTL.D()))
	}
}

// publicOrigin returns raw as a canonical https origin: lower-case, without
// the default port or a trailing slash. It rejects anything else a browser
// would not send as an Origin header.
func publicOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("must be a valid URL")
	}
	if u.Scheme != "https" || u.Host == "" || u.Hostname() == "" {
		return "", errors.New("must be an https URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("must not have userinfo, query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("must not have a path")
	}
	host := strings.ToLower(u.Host)
	host = strings.TrimSuffix(host, ":443")
	return "https://" + host, nil
}

// validate checks a present identity block against the rest of c. It does no
// I/O: the client secret file is checked by CheckFiles.
func (i *IdentityConfig) validate(c *Config) []error {
	var errs []error
	if !c.Control.RequireAuth {
		errs = append(errs, errors.New("identity requires control.require_auth: true"))
	}
	if origin, err := publicOrigin(i.PublicBaseURL); err != nil {
		errs = append(errs, fmt.Errorf("identity.public_base_url: %w", err))
	} else if !hostGuardAdmits(c, origin) {
		errs = append(errs, errors.New("identity.public_base_url host must be loopback or listed in allowed_hosts (with allow_non_loopback: true)"))
	}
	errs = append(errs, i.OIDC.validate()...)
	errs = append(errs, i.Access.validate()...)
	errs = append(errs, i.validateLifetimes()...)
	for _, f := range c.otherSecretFiles() {
		if filepath.Clean(f.path) == filepath.Clean(i.OIDC.ClientSecretFile) {
			errs = append(errs, fmt.Errorf("identity.oidc.client_secret_file must not be the same file as %s", f.label))
		}
	}
	if len(errs) == 0 {
		// Backstop: the login service must accept exactly what Load accepted.
		// New does no I/O; the inert secret and hooks are never called.
		if _, err := weblogin.New(i.WebLoginConfig(inertSecret, inertHooks{})); err != nil {
			errs = append(errs, fmt.Errorf("identity: %w", err))
		}
	}
	return errs
}

// Policy bounds shared with the login service.
const maxClaimValueBytes = 256

// reservedClaims may not drive the access policy: they are identity,
// protocol or mutable profile claims.
var reservedClaims = []string{
	"iss", "sub", "aud", "exp", "nbf", "iat", "auth_time", "nonce", "azp", "tid", "oid",
	"email", "email_verified", "preferred_username", "upn", "name", "unique_name",
	"_claim_names", "_claim_sources", "hasgroups",
}

var claimNameRE = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)

// validate checks the login access policy. Every login, including one by an
// admin subject, must carry an allowed value of Claim; admin_subjects only
// promote such a login to admin. At least one explicit admin mapping is
// required, since there is no first-user or domain promotion.
func (a IdentityAccessConfig) validate() []error {
	var errs []error
	switch {
	case a.Claim == "":
		errs = append(errs, errors.New("identity.access.claim is required: every login must carry an allowed claim value"))
	case !claimNameRE.MatchString(a.Claim) || slices.Contains(reservedClaims, a.Claim):
		errs = append(errs, errors.New("identity.access.claim must be a non-reserved claim name ([A-Za-z0-9_.:/-], at most 128 characters)"))
	}
	if len(a.UserValues)+len(a.AdminValues) == 0 {
		errs = append(errs, errors.New("identity.access requires at least one user_values or admin_values entry"))
	}
	if len(a.AdminValues)+len(a.AdminSubjects) == 0 {
		errs = append(errs, errors.New("identity.access requires an admin mapping (admin_values or admin_subjects)"))
	}
	for _, l := range []struct {
		name string
		vals []string
		max  int
	}{
		{"user_values", a.UserValues, maxClaimValueBytes},
		{"admin_values", a.AdminValues, maxClaimValueBytes},
		{"admin_subjects", a.AdminSubjects, identity.MaxSubjectBytes},
	} {
		seen := map[string]bool{}
		for _, v := range l.vals {
			if v == "" || len(v) > l.max || !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0 || seen[v] {
				errs = append(errs, fmt.Errorf("identity.access.%s entries must be unique, non-empty printable UTF-8 of at most %d bytes", l.name, l.max))
				break
			}
			seen[v] = true
		}
	}
	for _, v := range a.UserValues {
		if slices.Contains(a.AdminValues, v) {
			errs = append(errs, errors.New("identity.access: value listed in both user_values and admin_values"))
			break
		}
	}
	return errs
}

// validateLifetimes keeps key and session lifetimes within the identity
// store's ceilings (90-day keys, 30-day login recency, 8h idle / 24h absolute
// sessions): configuration may only tighten them.
func (i *IdentityConfig) validateLifetimes() []error {
	var errs []error
	for _, d := range []struct {
		name string
		v    Duration
		max  time.Duration
	}{
		{"keys.max_ttl", i.Keys.MaxTTL, identity.MaxKeyTTL},
		{"keys.require_login_within", i.Keys.RequireLoginWithin, identity.MaxLoginAge},
		{"session.idle_ttl", i.Session.IdleTTL, identity.MaxSessionIdle},
		{"session.absolute_ttl", i.Session.AbsoluteTTL, identity.MaxSessionAbsolute},
	} {
		if d.v <= 0 || d.v.D() > d.max {
			errs = append(errs, fmt.Errorf("identity.%s must be within (0, %s]", d.name, d.max))
		}
	}
	if i.Session.IdleTTL > i.Session.AbsoluteTTL {
		errs = append(errs, errors.New("identity.session.idle_ttl must not exceed absolute_ttl"))
	}
	if i.Keys.MaxPerUser < 1 || i.Keys.MaxPerUser > identity.MaxKeysPerUserCeiling {
		errs = append(errs, fmt.Errorf("identity.keys.max_per_user must be within [1, %d]", identity.MaxKeysPerUserCeiling))
	}
	return errs
}

// StoreOptions returns the identity store options for this block: the
// issuer/client binding pinned in identity.db and the tightened lifetimes.
// The caller adds test seams (Clock, Rand) only in tests.
func (i *IdentityConfig) StoreOptions() identity.Options {
	return identity.Options{
		Issuer:             i.OIDC.Issuer,
		ClientID:           i.OIDC.ClientID,
		KeyMaxTTL:          i.Keys.MaxTTL.D(),
		LoginMaxAge:        i.Keys.RequireLoginWithin.D(),
		SessionIdleTTL:     i.Session.IdleTTL.D(),
		SessionAbsoluteTTL: i.Session.AbsoluteTTL.D(),
		MaxKeysPerUser:     i.Keys.MaxPerUser,
	}
}

// Bounds shared with the login service and identity store.
const (
	maxClientIDBytes   = 256
	maxIssuerBytes     = 2048
	maxOIDCHTTPTimeout = time.Minute
)

// supportedSigningAlgs are the asymmetric JWS algorithms the login service
// verifies. HMAC and "none" are never accepted.
var supportedSigningAlgs = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "EdDSA"}

// validate checks the provider and client settings. Errors name the field,
// never its value.
func (o OIDCConfig) validate() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if err := checkIssuer(o.Issuer); err != nil {
		add("identity.oidc.issuer: %w", err)
	}
	if o.ClientID == "" || len(o.ClientID) > maxClientIDBytes || strings.IndexFunc(o.ClientID, notTokenChar) >= 0 {
		add("identity.oidc.client_id must be 1..%d printable non-space characters", maxClientIDBytes)
	}
	if o.ClientSecretFile == "" {
		add("identity.oidc.client_secret_file is required (the secret is read only from a file)")
	}
	if o.ClientAuth != weblogin.ClientSecretBasic && o.ClientAuth != weblogin.ClientSecretPost {
		add("identity.oidc.client_auth must be %s or %s", weblogin.ClientSecretBasic, weblogin.ClientSecretPost)
	}
	if len(o.Scopes) > 0 {
		seen := map[string]bool{}
		for _, sc := range o.Scopes {
			switch {
			case sc == "" || strings.IndexFunc(sc, notScopeChar) >= 0:
				add("identity.oidc.scopes: invalid scope token")
			case sc == "offline_access":
				add("identity.oidc.scopes: offline_access is not allowed (refresh tokens are never requested)")
			case seen[sc]:
				add("identity.oidc.scopes: duplicate scope")
			}
			seen[sc] = true
		}
		if !seen["openid"] {
			add("identity.oidc.scopes must include openid")
		}
	}
	for _, a := range o.SigningAlgs {
		if !slices.Contains(supportedSigningAlgs, a) {
			add("identity.oidc.signing_algs: only RS/ES/PS 256-512 and EdDSA are allowed")
			break
		}
	}
	if o.TenantID != "" && (strings.IndexFunc(o.TenantID, notTokenChar) >= 0 || !strings.Contains(o.Issuer, o.TenantID)) {
		add("identity.oidc.tenant_id must be printable and appear in the issuer")
	}
	for _, h := range o.ExtraEndpointHosts {
		if h == "" || strings.ContainsAny(h, ":/@?#[] ") || strings.IndexFunc(h, notTokenChar) >= 0 {
			add("identity.oidc.extra_endpoint_hosts entries must be bare host names")
			break
		}
	}
	if o.HTTPTimeout <= 0 || o.HTTPTimeout.D() > maxOIDCHTTPTimeout {
		add("identity.oidc.http_timeout must be within (0, %s]", maxOIDCHTTPTimeout)
	}
	return errs
}

// checkIssuer requires an absolute https issuer without userinfo, query,
// fragment or template placeholders, short enough to pin in the identity
// store.
func checkIssuer(raw string) error {
	if raw == "" {
		return errors.New("is required")
	}
	if len(raw) > maxIssuerBytes || strings.IndexFunc(raw, notTokenChar) >= 0 {
		return fmt.Errorf("must be at most %d printable non-space characters", maxIssuerBytes)
	}
	if strings.ContainsAny(raw, "{}") {
		return errors.New("templated issuers are not supported")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if u.Scheme != "https" || u.Host == "" || u.Hostname() == "" {
		return errors.New("must be an https URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return errors.New("must not have userinfo, query or fragment")
	}
	return nil
}

func notTokenChar(r rune) bool { return r <= ' ' || r >= 0x7f }

// notScopeChar follows RFC 6749 scope-token: %x21 / %x23-5B / %x5D-7E.
func notScopeChar(r rune) bool { return r <= ' ' || r >= 0x7f || r == '"' || r == '\\' }

// WebLoginConfig maps the block onto the login service configuration. JIT
// provisioning is always on in v1 (the claim policy still gates every
// login). The caller supplies the secret reader and hooks and may add the
// process-level Outbound.RootCAs, Clock and Logger; Outbound.TestDialContext
// is a test-only seam that configuration can never set.
func (i *IdentityConfig) WebLoginConfig(secret func() (string, error), hooks weblogin.Hooks) weblogin.Config {
	return weblogin.Config{
		PublicBaseURL: i.PublicBaseURL,
		Issuer:        i.OIDC.Issuer,
		ClientID:      i.OIDC.ClientID,
		ClientSecret:  secret,
		ClientAuth:    i.OIDC.ClientAuth,
		Scopes:        slices.Clone(i.OIDC.Scopes),
		SigningAlgs:   slices.Clone(i.OIDC.SigningAlgs),
		TenantID:      i.OIDC.TenantID,
		Policy: weblogin.Policy{
			JIT:           true,
			Claim:         i.Access.Claim,
			UserValues:    slices.Clone(i.Access.UserValues),
			AdminValues:   slices.Clone(i.Access.AdminValues),
			AdminSubjects: slices.Clone(i.Access.AdminSubjects),
		},
		Outbound: weblogin.Outbound{
			AllowPrivateNetwork: i.OIDC.AllowPrivateNetwork,
			ExtraEndpointHosts:  slices.Clone(i.OIDC.ExtraEndpointHosts),
			Timeout:             i.OIDC.HTTPTimeout.D(),
		},
		Hooks: hooks,
	}
}

var errInert = errors.New("config: validation-only login service")

func inertSecret() (string, error) { return "", errInert }

// inertHooks satisfies weblogin.Hooks for the validation backstop only.
type inertHooks struct{}

func (inertHooks) CompleteLogin(context.Context, weblogin.VerifiedLogin, bool) (weblogin.Session, error) {
	return weblogin.Session{}, errInert
}
func (inertHooks) LoginDenied(context.Context, string, string, string) error { return errInert }
func (inertHooks) Logout(context.Context, string, string) error              { return errInert }

// hostGuardAdmits mirrors the app's HostGuard: loopback names and addresses
// are always admitted, allowed_hosts only with allow_non_loopback.
func hostGuardAdmits(c *Config, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	want := hostKey(u.Hostname())
	if ip, err := netip.ParseAddr(want); err == nil && ip.IsLoopback() {
		return true
	}
	if want == "localhost" {
		return true
	}
	if !c.AllowNonLoopback {
		return false
	}
	for _, h := range c.AllowedHosts {
		if hostKey(h) == want {
			return true
		}
	}
	return false
}

// hostKey normalizes a host name or IP literal the way HostGuard compares
// them: IPs by address, names lower-cased without one trailing dot.
func hostKey(h string) string {
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.WithZone("").Unmap().String()
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// UserLimits returns the global per-user default ceilings (budgets.users) as
// budget.ScopeUser limits keyed by userID, for the gate to apply to a
// reservation owned by that user. It returns nil when spend controls or the
// user default are absent.
func (b *BudgetConfig) UserLimits(userID string) ([]budget.Limit, error) {
	if b == nil || b.Users == nil {
		return nil, nil
	}
	if userID == "" {
		return nil, errors.New("budget user limits: empty user id")
	}
	return b.Users.Limits(budget.ScopeUser, userID)
}

// MaxClientSecretBytes bounds the client secret file.
const MaxClientSecretBytes = 4096

// ReadClientSecret reads the OIDC client secret from ClientSecretFile. The
// file must be a private regular file of at most MaxClientSecretBytes
// holding one printable UTF-8 value; surrounding whitespace is trimmed.
// Errors name the file, never its content. The app passes this method as
// the login service's secret reader, so a rotated file takes effect on the
// next token exchange.
func (o OIDCConfig) ReadClientSecret() (string, error) {
	const label = "identity.oidc.client_secret_file"
	b, err := readPrivateBounded(label, o.ClientSecretFile, MaxClientSecretBytes)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	switch {
	case v == "":
		return "", fmt.Errorf("%s %s is empty", label, o.ClientSecretFile)
	case !utf8.ValidString(v) || strings.IndexFunc(v, isNonSpaceControl) >= 0:
		return "", fmt.Errorf("%s %s must hold printable UTF-8", label, o.ClientSecretFile)
	case strings.IndexFunc(v, unicode.IsSpace) >= 0:
		return "", fmt.Errorf("%s %s must hold a single value without inner whitespace", label, o.ClientSecretFile)
	}
	return v, nil
}

func isNonSpaceControl(r rune) bool { return unicode.IsControl(r) && !unicode.IsSpace(r) }

// readPrivateBounded reads a secret file that must be a private regular file
// (core.CheckPrivateFile) of at most max bytes. The open is non-blocking and
// the opened file is checked again, so a FIFO or a file swapped after the
// path check is never read. Errors carry label and path only.
func readPrivateBounded(label, path string, max int64) ([]byte, error) {
	if err := core.CheckPrivateFile(label, path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if !fi.Mode().IsRegular() || (runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0) {
		return nil, fmt.Errorf("%s %s changed while being read; must be a private regular file", label, path)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read failed", label)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s %s is larger than %d bytes", label, path, max)
	}
	return b, nil
}

// userKeyPrefix starts every identity user API key. Bearer dispatch sends any
// token with this prefix to the identity store only, so a static client key
// with it could never authenticate.
const userKeyPrefix = "lrk_"

// maxStaticKeyBytes bounds how much of a static key file CheckFiles reads.
const maxStaticKeyBytes = 64 << 10

type labeledPath struct{ label, path string }

// otherSecretFiles lists every configured secret file besides the OIDC
// client secret, labeled as CheckFiles reports them.
func (c *Config) otherSecretFiles() []labeledPath {
	var out []labeledPath
	for _, cl := range c.Clients {
		for _, p := range cl.KeyPaths() {
			out = append(out, labeledPath{"client " + cl.Name + " key file", p})
		}
	}
	for _, a := range c.Accounts {
		if a.APIKeyFile != "" {
			out = append(out, labeledPath{"account " + a.ID + " api_key_file", a.APIKeyFile})
		}
		if a.ManagementKeyFile != "" {
			out = append(out, labeledPath{"account " + a.ID + " management_key_file", a.ManagementKeyFile})
		}
	}
	if c.TLSKeyFile != "" {
		out = append(out, labeledPath{"tls_key_file", c.TLSKeyFile})
	}
	return out
}

// checkIdentityFiles is the multi-user part of CheckFiles: the client secret
// file is readable as a secret, is not another name for any other secret
// file, and no static client key uses the reserved user-key prefix. Errors
// never contain file content.
func (c *Config) checkIdentityFiles() []error {
	var errs []error
	secret := c.Identity.OIDC.ClientSecretFile
	if _, err := c.Identity.OIDC.ReadClientSecret(); err != nil {
		errs = append(errs, err)
	}
	sfi, serr := os.Stat(secret)
	for _, f := range c.otherSecretFiles() {
		if serr != nil {
			break
		}
		if fi, err := os.Stat(f.path); err == nil && os.SameFile(sfi, fi) {
			errs = append(errs, fmt.Errorf("identity.oidc.client_secret_file must not be the same file as %s", f.label))
		}
	}
	for _, cl := range c.Clients {
		for _, p := range cl.KeyPaths() {
			label := "client " + cl.Name + " key file"
			b, err := readPrivateBounded(label, p, maxStaticKeyBytes)
			if err != nil {
				// Mode and existence are reported by the private-file
				// check; the key loader rejects anything else unreadable.
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(string(b)), userKeyPrefix) {
				errs = append(errs, fmt.Errorf("%s %s starts with the reserved user API key prefix %s; generate a new static key", label, p, userKeyPrefix))
			}
		}
	}
	return errs
}
