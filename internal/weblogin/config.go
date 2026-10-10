package weblogin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Clock supplies the current time; core.SystemClock satisfies it.
type Clock interface{ Now() time.Time }

// Client authentication methods at the token endpoint. Auto-detection is
// never used (it retries and sends the secret twice).
const (
	ClientSecretBasic = "client_secret_basic"
	ClientSecretPost  = "client_secret_post"
)

// Config is the resolved identity configuration. The app builds it from its
// own config types; weblogin does not import internal/config.
type Config struct {
	// PublicBaseURL is the exact https origin users browse to, for example
	// https://router.example.com. The redirect URI is always
	// PublicBaseURL + "/auth/callback".
	PublicBaseURL string
	// Issuer is compared byte-exact with discovery and the ID token iss.
	Issuer   string
	ClientID string
	// ClientSecret is called at each token exchange; the value is never
	// stored or logged.
	ClientSecret func() (string, error)
	// ClientAuth is ClientSecretBasic (default) or ClientSecretPost.
	ClientAuth string
	// Scopes default to openid, profile. openid is required and
	// offline_access is refused: no refresh tokens are ever requested.
	Scopes []string
	// SigningAlgs default to RS256; only RS/ES/PS 256-512 and EdDSA.
	SigningAlgs []string
	// TenantID, when set, must equal the ID token tid claim (Entra).
	TenantID string
	// Policy is evaluated on every verified login before any hook runs.
	Policy Policy
	// LoginTimeout bounds a pending authorization flow. Default 10m, [1m,30m].
	LoginTimeout time.Duration
	// Outbound bounds IdP traffic.
	Outbound Outbound
	// MaxRedeemedLogins caps the callback states remembered against replay
	// (default 256, at most 65536). A state is remembered from the start of
	// its token exchange; it is kept until its flow expires (LoginTimeout)
	// only once the IdP returned an ID token signed for its nonce, and is
	// forgotten at once if the exchange or verification fails. When the
	// cache is full, callbacks get 503 busy rather than forgetting a state.
	// Pending logins are not stored at all: they live in the browser's
	// sealed login cookie.
	MaxRedeemedLogins int
	// MaxConcurrentExchanges caps concurrent token exchanges (default 4).
	MaxConcurrentExchanges int
	// DiscoveryRetry is the minimum gap between failed discovery attempts
	// (default 30s). A successful discovery is kept for the process lifetime.
	DiscoveryRetry time.Duration
	// Clock defaults to the system clock.
	Clock Clock
	// Logger defaults to discarding output.
	Logger *slog.Logger
	// Hooks receives verified logins and logouts.
	Hooks Hooks
}

// VerifiedLogin is a login whose ID token was verified and whose claims the
// Policy admitted. Issuer and Subject are the identity; Role is RoleUser or
// RoleAdmin; AuthTime is the fresh IdP authentication time. Email and
// DisplayName are informational only and must never be used for identity or
// authorization.
type VerifiedLogin struct {
	Issuer      string
	Subject     string
	Role        string
	AuthTime    time.Time
	Email       string
	DisplayName string
}

// Session is what Hooks.CompleteLogin returns: the opaque cookie secret
// (base64url, 16..256 bytes) and the session's absolute expiry.
type Session struct {
	Secret    string
	ExpiresAt time.Time
}

// Hooks is implemented by the app adapter over the identity store. Each call
// carries the request context. Implementations must be safe for concurrent
// use.
type Hooks interface {
	// CompleteLogin runs once per callback whose ID token verified and whose
	// claims Policy admitted. It must resolve the user by (Issuer, Subject),
	// creating it only when jit is true, record the login (Role, AuthTime)
	// and create a fresh session. Return ErrNotProvisioned for an unknown
	// user without JIT and ErrUserDisabled for a disabled or deleted user
	// (wrapping is fine).
	CompleteLogin(ctx context.Context, login VerifiedLogin, jit bool) (Session, error)
	// LoginDenied runs when a verified, freshly authenticated subject was
	// denied by Policy (reason is a Reason* constant). An existing user
	// should be treated as an access-policy denial (revoke sessions/keys);
	// an unknown subject must not be created. The error is logged only.
	LoginDenied(ctx context.Context, issuer, subject, reason string) error
	// Logout revokes the session whose cookie secret is given, after
	// checking csrfToken against that session (return ErrCSRF on mismatch).
	// An unknown or expired session must return nil.
	Logout(ctx context.Context, sessionSecret, csrfToken string) error
}

// Errors a Hooks implementation returns to select the user-facing outcome.
var (
	ErrNotProvisioned = errors.New("weblogin: user not provisioned")
	ErrUserDisabled   = errors.New("weblogin: user disabled")
	ErrCSRF           = errors.New("weblogin: csrf token mismatch")
)

// Fixed endpoint paths and cookie names. The __Host- prefix makes browsers
// require Secure, Path=/ and no Domain.
const (
	LoginPath         = "/auth/login"
	CallbackPath      = "/auth/callback"
	LogoutPath        = "/auth/logout"
	LoginCookieName   = "__Host-lr_login"
	SessionCookieName = "__Host-lr_session"
	// LandingPath is the only post-login redirect target; no caller-supplied
	// return URL is ever honoured.
	LandingPath = "/ui/"
	// CSRFHeader carries the session CSRF token on POST /auth/logout.
	CSRFHeader = "X-LocalRouter-CSRF"
)

// Service serves the login endpoints. Build it with New.
type Service struct {
	cfg        Config
	client     *http.Client
	publicHost string
	issuerHost string
	issuerPort string
	sealer     *flowSealer
	redeemed   *redeemTable
	// exchanges is a semaphore over concurrent token exchanges; a callback
	// waits at most exchangeWait for a slot.
	exchanges    chan struct{}
	exchangeWait time.Duration

	prov          atomic.Pointer[provider]
	discMu        sync.Mutex
	lastDiscovery time.Time
}

// New validates cfg, applies defaults and returns a Service. It performs no
// network I/O; discovery happens lazily on the first login.
func New(cfg Config) (*Service, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	iss, _ := url.Parse(cfg.Issuer)
	s := &Service{
		cfg:        cfg,
		publicHost: strings.TrimPrefix(cfg.PublicBaseURL, "https://"),
		sealer:     newFlowSealer(cfg.PublicBaseURL),
		redeemed:   newRedeemTable(cfg.MaxRedeemedLogins),
		exchanges:  make(chan struct{}, cfg.MaxConcurrentExchanges),
		// Waiting longer than this only stacks up browsers behind a slow IdP.
		exchangeWait: min(cfg.Outbound.Timeout, 2*time.Second),
		issuerHost:   strings.ToLower(iss.Hostname()),
		issuerPort:   effectivePort(iss),
	}
	hosts := append([]string{s.issuerHost}, cfg.Outbound.ExtraEndpointHosts...)
	s.client = newOutboundClient(cfg.Outbound, hosts)
	return s, nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

var supportedAlgs = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "EdDSA"}

// reservedClaims may not drive the access policy: they are identity,
// protocol or mutable profile claims.
var reservedClaims = []string{
	"iss", "sub", "aud", "exp", "nbf", "iat", "auth_time", "nonce", "azp", "tid", "oid",
	"email", "email_verified", "preferred_username", "upn", "name", "unique_name",
	"_claim_names", "_claim_sources", "hasgroups",
}

var claimNameRE = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)

// normalize validates c in place and fills defaults. Errors name the field,
// never its value.
func (c *Config) normalize() error {
	pub, err := checkHTTPSURL(c.PublicBaseURL)
	if err != nil {
		return fmt.Errorf("weblogin: public base url: %w", err)
	}
	if pub.Path != "" && pub.Path != "/" {
		return errors.New("weblogin: public base url: must not have a path")
	}
	c.PublicBaseURL = "https://" + canonicalHost(pub.Host)

	iss, err := checkHTTPSURL(c.Issuer)
	if err != nil {
		return fmt.Errorf("weblogin: issuer: %w", err)
	}
	if strings.EqualFold(iss.Hostname(), "login.microsoftonline.com") {
		seg, _, _ := strings.Cut(strings.TrimPrefix(iss.Path, "/"), "/")
		switch strings.ToLower(seg) {
		case "common", "organizations", "consumers":
			return errors.New("weblogin: issuer: multi-tenant Entra issuers are not supported")
		}
	}
	if strings.ContainsAny(c.Issuer, "{}") {
		return errors.New("weblogin: issuer: templated issuer is not supported")
	}
	if c.ClientID == "" || len(c.ClientID) > 256 || strings.IndexFunc(c.ClientID, notTokenChar) >= 0 {
		return errors.New("weblogin: client id: must be 1..256 printable non-space characters")
	}
	if c.ClientSecret == nil {
		return errors.New("weblogin: client secret: required")
	}
	if c.Hooks == nil {
		return errors.New("weblogin: hooks: required")
	}
	switch c.ClientAuth {
	case "":
		c.ClientAuth = ClientSecretBasic
	case ClientSecretBasic, ClientSecretPost:
	default:
		return errors.New("weblogin: client auth: must be client_secret_basic or client_secret_post")
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{"openid", "profile"}
	}
	for _, s := range c.Scopes {
		if s == "" || strings.IndexFunc(s, notScopeChar) >= 0 {
			return errors.New("weblogin: scopes: invalid scope token")
		}
		if s == "offline_access" {
			return errors.New("weblogin: scopes: offline_access is not allowed")
		}
	}
	if !slices.Contains(c.Scopes, "openid") {
		return errors.New("weblogin: scopes: openid is required")
	}
	c.Scopes = slices.Clone(c.Scopes)
	if len(c.SigningAlgs) == 0 {
		c.SigningAlgs = []string{"RS256"}
	}
	for _, a := range c.SigningAlgs {
		if !slices.Contains(supportedAlgs, a) {
			return errors.New("weblogin: signing algs: only RS/ES/PS 256-512 and EdDSA are allowed")
		}
	}
	c.SigningAlgs = slices.Clone(c.SigningAlgs)
	if c.TenantID != "" && !strings.Contains(c.Issuer, c.TenantID) {
		return errors.New("weblogin: tenant id: must appear in the issuer")
	}
	if err := c.Policy.validate(); err != nil {
		return err
	}
	if c.LoginTimeout == 0 {
		c.LoginTimeout = 10 * time.Minute
	}
	if c.LoginTimeout < time.Minute || c.LoginTimeout > 30*time.Minute {
		return errors.New("weblogin: login timeout: must be within [1m, 30m]")
	}
	if c.Outbound.Timeout == 0 {
		c.Outbound.Timeout = defaultHTTPTimeout
	}
	if c.Outbound.Timeout < 0 || c.Outbound.Timeout > time.Minute {
		return errors.New("weblogin: http timeout: must be within (0, 60s]")
	}
	if c.Outbound.MaxBodyBytes == 0 {
		c.Outbound.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.Outbound.MaxBodyBytes < 0 || c.Outbound.MaxBodyBytes > 16<<20 {
		return errors.New("weblogin: max body bytes: must be within (0, 16 MiB]")
	}
	hosts := make([]string, 0, len(c.Outbound.ExtraEndpointHosts))
	for _, h := range c.Outbound.ExtraEndpointHosts {
		if h == "" || strings.ContainsAny(h, ":/@?#[] ") {
			return errors.New("weblogin: extra endpoint hosts: must be bare host names")
		}
		hosts = append(hosts, strings.ToLower(h))
	}
	c.Outbound.ExtraEndpointHosts = hosts
	if c.MaxRedeemedLogins == 0 {
		c.MaxRedeemedLogins = 256
	}
	if c.MaxRedeemedLogins < 1 || c.MaxRedeemedLogins > 1<<16 {
		return errors.New("weblogin: max redeemed logins: must be within [1, 65536]")
	}
	if c.MaxConcurrentExchanges == 0 {
		c.MaxConcurrentExchanges = 4
	}
	if c.MaxConcurrentExchanges < 1 || c.MaxConcurrentExchanges > 64 {
		return errors.New("weblogin: max concurrent exchanges: must be within [1, 64]")
	}
	if c.DiscoveryRetry == 0 {
		c.DiscoveryRetry = 30 * time.Second
	}
	if c.DiscoveryRetry < 0 {
		return errors.New("weblogin: discovery retry: must be positive")
	}
	if c.Clock == nil {
		c.Clock = systemClock{}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return nil
}

func (p *Policy) validate() error {
	lists := [][]string{p.UserValues, p.AdminValues, p.AdminSubjects}
	for _, l := range lists {
		seen := map[string]bool{}
		for _, v := range l {
			if v == "" || len(v) > maxClaimValueLen || seen[v] {
				return errors.New("weblogin: policy: values and subjects must be unique, non-empty and at most 256 bytes")
			}
			seen[v] = true
		}
	}
	if len(p.UserValues)+len(p.AdminValues) > 0 {
		if !claimNameRE.MatchString(p.Claim) || slices.Contains(reservedClaims, p.Claim) {
			return errors.New("weblogin: policy: claim must be a non-reserved claim name")
		}
	}
	if len(p.AdminValues) == 0 && len(p.AdminSubjects) == 0 {
		return errors.New("weblogin: policy: an admin value or admin subject is required")
	}
	if len(p.UserValues) == 0 && len(p.AdminValues) == 0 {
		return errors.New("weblogin: policy: a user or admin claim value is required")
	}
	p.UserValues = slices.Clone(p.UserValues)
	p.AdminValues = slices.Clone(p.AdminValues)
	p.AdminSubjects = slices.Clone(p.AdminSubjects)
	return nil
}

// checkHTTPSURL requires an absolute https URL with a host and no userinfo,
// query or fragment.
func checkHTTPSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("must be a valid URL")
	}
	if u.Scheme != "https" || u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("must be an https URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("must not have userinfo, query or fragment")
	}
	return u, nil
}

func notTokenChar(r rune) bool { return r <= ' ' || r >= 0x7f }

// notScopeChar follows RFC 6749 scope-token: %x21 / %x23-5B / %x5D-7E.
func notScopeChar(r rune) bool { return r <= ' ' || r >= 0x7f || r == '"' || r == '\\' }
