package weblogin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test-only TLS fixture. The certificate is issued by a throwaway CA for
// names under the reserved .test TLD plus 127.0.0.1, so tests run real TLS
// against a loopback httptest server without touching system roots. Hosts
// are mapped to the listener by testDial; unknown hosts fail, so no test can
// reach the network.

const (
	idpHost   = "idp.example.test"
	otherHost = "other.example.test"
)

var (
	tlsOnce sync.Once
	tlsCert tls.Certificate
	tlsPool *x509.CertPool
)

func testTLS(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	tlsOnce.Do(func() {
		caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		ca := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: "weblogin test CA"},
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
			Subject:      pkix.Name{CommonName: idpHost},
			DNSNames:     []string{idpHost, otherHost},
			IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
		if err != nil {
			panic(err)
		}
		tlsCert = tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
		tlsPool = x509.NewCertPool()
		tlsPool.AddCert(caCert)
	})
	return tlsCert, tlsPool
}

// newTestTLSServer starts a loopback TLS server presenting the fixture cert.
func newTestTLSServer(t *testing.T, h http.Handler) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	cert, pool := testTLS(t)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, pool
}

// testDial maps the fixture host names (any port) to srv's listener and
// refuses everything else.
func testDial(srv *httptest.Server) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || (host != idpHost && host != otherHost) {
			return nil, fmt.Errorf("test dial refused %q", addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
}

// serverPort returns the listener port of srv as a string.
func serverPort(srv *httptest.Server) string {
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return port
}

func hostURL(host string, srv *httptest.Server, path string) string {
	return "https://" + host + ":" + serverPort(srv) + "/" + strings.TrimPrefix(path, "/")
}
