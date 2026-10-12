package weblogin

import (
	"context"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// errProviderUnavailable is returned while discovery has not succeeded.
var errProviderUnavailable = errors.New("weblogin: identity provider unavailable")

// errMetadataInvalid marks a discovery document that failed validation.
var errMetadataInvalid = errors.New("weblogin: provider metadata invalid")

// provider is the validated, process-lifetime result of discovery.
type provider struct {
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config // ClientSecret is filled per exchange on a copy
}

// Discover runs provider discovery now if it has not succeeded yet and the
// retry gap has passed. The app may call it at startup; logins call it
// lazily. The returned error is a fixed sentinel, never the provider's body.
func (s *Service) Discover(ctx context.Context) error {
	_, err := s.provider(ctx)
	return err
}

// Ready reports whether discovery has succeeded and been validated.
func (s *Service) Ready() bool {
	return s.prov.Load() != nil
}

// provider returns the discovered provider, discovering at most once per
// DiscoveryRetry. Concurrent callers share one attempt.
func (s *Service) provider(ctx context.Context) (*provider, error) {
	if p := s.prov.Load(); p != nil {
		return p, nil
	}
	s.discMu.Lock()
	defer s.discMu.Unlock()
	if p := s.prov.Load(); p != nil {
		return p, nil
	}
	now := s.cfg.Clock.Now()
	if !s.lastDiscovery.IsZero() && now.Sub(s.lastDiscovery) < s.cfg.DiscoveryRetry {
		return nil, errProviderUnavailable
	}
	s.lastDiscovery = now

	// Detach from the triggering request: a browser that disconnects must
	// not fail the shared attempt and push everyone into the retry gap. The
	// attempt stays bounded by the outbound timeout.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Outbound.Timeout)
	defer cancel()
	op, err := oidc.NewProvider(oidc.ClientContext(ctx, s.client), s.cfg.Issuer)
	if err == nil {
		err = s.checkMetadata(op)
	}
	if err != nil {
		// go-oidc errors embed response bodies and URLs: log a class only.
		s.cfg.Logger.Warn("weblogin: provider discovery failed", "reason", errorClass(err))
		return nil, errProviderUnavailable
	}
	ep := op.Endpoint()
	ep.AuthStyle = oauth2.AuthStyleInHeader
	if s.cfg.ClientAuth == ClientSecretPost {
		ep.AuthStyle = oauth2.AuthStyleInParams
	}
	p := &provider{
		verifier: op.Verifier(s.verifierConfig()),
		oauth: oauth2.Config{
			ClientID:    s.cfg.ClientID,
			Endpoint:    ep,
			RedirectURL: s.cfg.PublicBaseURL + CallbackPath,
			Scopes:      s.cfg.Scopes,
		},
	}
	s.prov.Store(p)
	s.cfg.Logger.Info("weblogin: provider discovered")
	return p, nil
}

// verifierConfig is the only go-oidc verifier configuration ever built. No
// Skip* or Insecure* option is set.
func (s *Service) verifierConfig() *oidc.Config {
	return &oidc.Config{
		ClientID:             s.cfg.ClientID,
		SupportedSigningAlgs: s.cfg.SigningAlgs,
		Now:                  s.cfg.Clock.Now,
	}
}

// checkMetadata validates the endpoints discovery named. go-oidc has already
// required the discovered issuer to equal the configured one exactly.
func (s *Service) checkMetadata(op *oidc.Provider) error {
	var md struct {
		AuthURL       string   `json:"authorization_endpoint"`
		TokenURL      string   `json:"token_endpoint"`
		JWKSURL       string   `json:"jwks_uri"`
		ResponseTypes []string `json:"response_types_supported"`
		Algs          []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := op.Claims(&md); err != nil {
		return errMetadataInvalid
	}
	for _, raw := range []string{md.AuthURL, md.TokenURL, md.JWKSURL} {
		if !s.endpointAllowed(raw) {
			return errMetadataInvalid
		}
	}
	if md.ResponseTypes != nil && !slices.Contains(md.ResponseTypes, "code") {
		return errMetadataInvalid
	}
	if md.Algs != nil && !slices.ContainsFunc(md.Algs, func(a string) bool { return slices.Contains(s.cfg.SigningAlgs, a) }) {
		return errMetadataInvalid
	}
	return nil
}

// endpointAllowed requires https, no userinfo, and either the issuer's host
// and port or an explicitly listed extra host.
func (s *Service) endpointAllowed(raw string) bool {
	u, err := checkHTTPSURL(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if slices.Contains(s.cfg.Outbound.ExtraEndpointHosts, host) {
		return true
	}
	return host == s.issuerHost && effectivePort(u) == s.issuerPort
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}

// errorClass maps an outbound error to a fixed log value.
func errorClass(err error) string {
	var mismatch *oidc.IssuerMismatchError
	var netErr net.Error
	switch {
	case errors.Is(err, errDialDenied):
		return "dial_denied"
	case errors.Is(err, errRequestDenied):
		return "request_denied"
	case errors.Is(err, errBodyTooLarge):
		return "body_too_large"
	case errors.Is(err, errMetadataInvalid):
		return "metadata_invalid"
	case errors.As(err, &mismatch):
		return "issuer_mismatch"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	}
	return "unavailable"
}
