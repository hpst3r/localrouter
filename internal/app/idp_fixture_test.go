package app

// Test-only OpenID provider for app-level browser-login tests, adapted from
// internal/weblogin's tlsfixture_test.go and idp_test.go (test files cannot
// be imported across packages). It runs real TLS on a loopback httptest
// server with a throwaway CA for names under the reserved .test TLD, signs
// real ID tokens with an RSA key published in its JWKS, and its token
// endpoint enforces single-use codes, the exact redirect URI, client_secret
// basic authentication and the PKCE S256 verifier. The app reaches it only
// through Overrides.IdentityRootCAs and Overrides.IdentityTestDialContext;
// the dialer refuses every other host, so no test touches the network.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	fixtureIdPHost      = "idp.fixture.test"
	fixtureClientSecret = "fixture-client-secret"
	fixturePublicURL    = "https://localhost:8787"
	fixtureRedirectURI  = fixturePublicURL + "/auth/callback"
)

var (
	fixtureTLSOnce sync.Once
	fixtureTLSCert tls.Certificate
	fixtureTLSPool *x509.CertPool
	fixtureRSAOnce sync.Once
	fixtureRSAKey  *rsa.PrivateKey
)

func fixtureTLS() (tls.Certificate, *x509.CertPool) {
	fixtureTLSOnce.Do(func() {
		caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		ca := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: "app fixture CA"},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			IsCA:                  true,
			KeyUsage:              x509.KeyUsageCertSign,
			BasicConstraintsValid: true,
		}
		caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
		if err != nil {
			panic(err)
		}
		caCert, _ := x509.ParseCertificate(caDER)
		leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: fixtureIdPHost},
			DNSNames:     []string{fixtureIdPHost},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
		if err != nil {
			panic(err)
		}
		fixtureTLSCert = tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
		fixtureTLSPool = x509.NewCertPool()
		fixtureTLSPool.AddCert(caCert)
	})
	return fixtureTLSCert, fixtureTLSPool
}

func fixtureSigningKey() *rsa.PrivateKey {
	fixtureRSAOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		fixtureRSAKey = k
	})
	return fixtureRSAKey
}

// idpGrant is one authorization the fake user completed.
type idpGrant struct {
	challenge, method, redirectURI, nonce string
	subject                               string
	claims                                map[string]any
	authTime                              time.Time
	used                                  bool
}

type fakeIdP struct {
	srv    *httptest.Server
	issuer string
	clock  core.Clock
	pool   *x509.CertPool

	mu       sync.Mutex
	grants   map[string]*idpGrant
	failures []string
	hits     int
}

func newFakeIdP(t *testing.T, clock core.Clock) *fakeIdP {
	t.Helper()
	cert, pool := fixtureTLS()
	p := &fakeIdP{clock: clock, pool: pool, grants: map[string]*idpGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/o/lr/.well-known/openid-configuration", p.serveDiscovery)
	mux.HandleFunc("/o/lr/jwks", p.serveJWKS)
	mux.HandleFunc("/o/lr/token", p.serveToken)
	p.srv = httptest.NewUnstartedServer(mux)
	p.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	p.srv.StartTLS()
	t.Cleanup(p.srv.Close)
	p.issuer = p.url("/o/lr")
	return p
}

func (p *fakeIdP) url(path string) string {
	_, port, _ := net.SplitHostPort(p.srv.Listener.Addr().String())
	return "https://" + fixtureIdPHost + ":" + port + path
}

// dial maps the fixture host (any port) to the listener and refuses
// everything else.
func (p *fakeIdP) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != fixtureIdPHost {
		return nil, fmt.Errorf("fixture dial refused %q", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, p.srv.Listener.Addr().String())
}

func (p *fakeIdP) overrides(clock core.Clock) Overrides {
	return Overrides{IdentityClock: clock, IdentityRootCAs: p.pool, IdentityTestDialContext: p.dial}
}

func (p *fakeIdP) failf(format string, args ...any) {
	p.mu.Lock()
	p.failures = append(p.failures, fmt.Sprintf(format, args...))
	p.mu.Unlock()
}

func (p *fakeIdP) failureList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.failures...)
}

func (p *fakeIdP) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.hits++
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                p.issuer,
		"authorization_endpoint":                p.url("/o/lr/authorize"),
		"token_endpoint":                        p.url("/o/lr/token"),
		"jwks_uri":                              p.url("/o/lr/jwks"),
		"response_types_supported":              []string{"code"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"subject_types_supported":               []string{"public"},
	})
}

func (p *fakeIdP) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: fixtureSigningKey().Public(), KeyID: "rsa-1", Algorithm: "RS256", Use: "sig"}}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}

// authorize plays the browser and user at the authorization endpoint for
// subject with the given extra ID-token claims, and returns the callback
// query the provider would redirect to.
func (p *fakeIdP) authorize(t *testing.T, location, subject string, claims map[string]any) url.Values {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != p.url("/o/lr/authorize") {
		t.Fatalf("authorize endpoint = %q", got)
	}
	q := u.Query()
	if q.Get("max_age") != "0" {
		t.Fatalf("login does not force fresh authentication: max_age=%q", q.Get("max_age"))
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	code := base64.RawURLEncoding.EncodeToString(b)
	p.mu.Lock()
	p.grants[code] = &idpGrant{
		challenge: q.Get("code_challenge"), method: q.Get("code_challenge_method"),
		redirectURI: q.Get("redirect_uri"), nonce: q.Get("nonce"),
		subject: subject, claims: claims, authTime: p.clock.Now(),
	}
	p.mu.Unlock()
	return url.Values{"code": {code}, "state": {q.Get("state")}}
}

func (p *fakeIdP) serveToken(w http.ResponseWriter, r *http.Request) {
	fail := func(format string, args ...any) {
		p.failf(format, args...)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		fail("token request")
		return
	}
	f := r.PostForm
	id, secret, ok := r.BasicAuth()
	if !ok || id != idTestClientID || secret != fixtureClientSecret || f.Has("client_secret") {
		fail("client auth mismatch")
		return
	}
	p.mu.Lock()
	g := p.grants[f.Get("code")]
	reused := g != nil && g.used
	if g != nil {
		g.used = true
	}
	p.mu.Unlock()
	switch {
	case f.Get("grant_type") != "authorization_code":
		fail("grant_type")
		return
	case g == nil || reused:
		fail("unknown or reused code")
		return
	case f.Get("redirect_uri") != g.redirectURI || g.redirectURI != fixtureRedirectURI:
		fail("redirect_uri mismatch %q", g.redirectURI)
		return
	case g.method != "S256":
		fail("pkce method")
		return
	}
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	if f.Get("code_verifier") == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		fail("pkce verifier mismatch")
		return
	}
	now := p.clock.Now()
	claims := map[string]any{
		"iss": p.issuer, "sub": g.subject, "aud": idTestClientID,
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
		"auth_time": g.authTime.Unix(), "nonce": g.nonce,
		"email": g.subject + "@example.test", "name": "Fixture " + g.subject,
	}
	for k, v := range g.claims {
		claims[k] = v
	}
	payload, _ := json.Marshal(claims)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "access-token-marker", "token_type": "Bearer", "expires_in": 300,
		"id_token": oidctest.SignIDToken(fixtureSigningKey(), "rsa-1", "RS256", string(payload)),
	})
}
