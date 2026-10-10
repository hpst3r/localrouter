package weblogin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Outbound bounds every request made to the identity provider.
type Outbound struct {
	// AllowPrivateNetwork admits loopback, RFC 1918, CGNAT and unique-local
	// addresses, for a LAN provider such as a self-hosted Authentik.
	// Link-local, cloud metadata, multicast and reserved addresses stay denied.
	AllowPrivateNetwork bool
	// ExtraEndpointHosts are host names, besides the issuer's, that discovery
	// may name for the authorization, token and JWKS endpoints.
	ExtraEndpointHosts []string
	// Timeout bounds each request end to end. Default 10s, at most 60s.
	Timeout time.Duration
	// MaxBodyBytes caps every response body. Default 1 MiB.
	MaxBodyBytes int64
	// RootCAs replaces the system roots when non-nil (a private provider CA).
	RootCAs *x509.CertPool

	// TestDialContext is a TEST-ONLY seam and must be nil in production. When
	// set it replaces the IP-guarded dialer so fixture tests can reach an
	// httptest issuer on loopback under a .test host name. Every other guard
	// (https only, host allowlist, no redirects, no proxy, body cap, timeouts,
	// TLS verification against RootCAs) stays in force.
	TestDialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// errDialDenied is returned when the guard refuses a resolved address.
var errDialDenied = errors.New("weblogin: outbound address denied")

// newOutboundClient builds the guarded client used for discovery, JWKS and
// the token exchange. allowedHosts are lower-case host names.
func newOutboundClient(o Outbound, allowedHosts []string) *http.Client {
	dial := o.TestDialContext
	if dial == nil {
		d := &net.Dialer{
			Timeout: 5 * time.Second,
			// Control runs after DNS resolution, so the guard sees the
			// address actually dialled and DNS rebinding cannot bypass it.
			Control: func(_, address string, _ syscall.RawConn) error {
				ap, err := netip.ParseAddrPort(address)
				if err != nil {
					return errDialDenied
				}
				return checkDialAddr(ap.Addr(), o.AllowPrivateNetwork)
			},
		}
		dial = d.DialContext
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         dial,
		TLSClientConfig:     &tls.Config{RootCAs: o.RootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	maxBody := o.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	tr.ResponseHeaderTimeout = min(timeout, 5*time.Second)
	return &http.Client{
		Timeout:   timeout,
		Transport: &guardTransport{next: tr, hosts: allowedHosts, maxBody: maxBody},
		// A 3xx is returned as is and then fails as a non-200 response.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

const (
	defaultMaxBodyBytes = 1 << 20
	defaultHTTPTimeout  = 10 * time.Second
)

// errRequestDenied is returned for a request outside the allowlist.
var errRequestDenied = errors.New("weblogin: outbound request denied")

// errBodyTooLarge is returned by a response body read past MaxBodyBytes.
var errBodyTooLarge = errors.New("weblogin: response body too large")

// guardTransport refuses anything but https to an allowlisted host, as a
// second line behind discovery validation in case a library builds a URL of
// its own.
type guardTransport struct {
	next    http.RoundTripper
	hosts   []string
	maxBody int64
}

func (g *guardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u := r.URL
	if u.Scheme != "https" || u.User != nil || !slices.Contains(g.hosts, strings.ToLower(u.Hostname())) {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, errRequestDenied
	}
	resp, err := g.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	// go-oidc reads discovery and JWKS bodies with an unbounded io.ReadAll,
	// so the cap must live here.
	resp.Body = &cappedBody{ReadCloser: resp.Body, left: g.maxBody}
	return resp, nil
}

// cappedBody fails a read once more than its budget would be returned.
type cappedBody struct {
	io.ReadCloser
	left int64
}

func (c *cappedBody) Read(p []byte) (int, error) {
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.ReadCloser.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return 0, errBodyTooLarge
	}
	return n, err
}

// checkDialAddr classifies one resolved IP. allowPrivate admits loopback,
// RFC 1918, CGNAT and unique-local addresses (a LAN identity provider) but
// never link-local, metadata, multicast, unspecified or reserved addresses,
// nor IPv6 forms embedding an IPv4 address it does not decode.
func checkDialAddr(ip netip.Addr, allowPrivate bool) error {
	ip = embeddedIPv4(ip.Unmap())
	switch {
	case !ip.IsValid(), ip.IsUnspecified(), ip.IsMulticast(),
		ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return errDialDenied
	}
	if inAny(ip, alwaysDenied) || (inAny(ip, undecodedIPv4) && !ip.IsLoopback()) {
		return errDialDenied
	}
	if ip.IsLoopback() || ip.IsPrivate() || inAny(ip, privateExtra) {
		if !allowPrivate {
			return errDialDenied
		}
	}
	return nil
}

// alwaysDenied lists ranges refused even with the private-network exception:
// "this network", reserved/broadcast, and cloud metadata/host services that
// sit outside link-local.
var alwaysDenied = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("240.0.0.0/4"), // includes 255.255.255.255
	netip.MustParsePrefix("100.100.100.200/32"),
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

// privateExtra are non-public ranges netip does not report as private.
var privateExtra = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fec0::/10"),
}

// undecodedIPv4 are IPv6 forms that carry an IPv4 address embeddedIPv4 does
// not decode. They are refused outright, apart from ::1 (loopback): no
// identity provider is reached through them.
var undecodedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("::/96"),           // IPv4-compatible (deprecated)
	netip.MustParsePrefix("::ffff:0:0:0/96"), // IPv4-translated (SIIT)
	netip.MustParsePrefix("2001::/32"),       // Teredo
	netip.MustParsePrefix("64:ff9b:1::/48"),  // NAT64 local-use
}

var (
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

// embeddedIPv4 returns the IPv4 address carried by a NAT64 or 6to4 address so
// it is classified by the address it ultimately reaches.
func embeddedIPv4(ip netip.Addr) netip.Addr {
	if !ip.Is6() {
		return ip
	}
	b := ip.As16()
	switch {
	case nat64Prefix.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16]))
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6]))
	}
	return ip
}

func inAny(ip netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
