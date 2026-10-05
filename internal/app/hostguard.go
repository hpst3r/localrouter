package app

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// LocalHostGuard rejects requests whose Host header is not a loopback name.
// It blocks DNS-rebinding pages from reading the control API through the
// user's browser. It is HostGuard with no extra allowed hosts.
func LocalHostGuard(next http.Handler) http.Handler { return HostGuard(nil, next) }

// HostGuard rejects (403) requests whose Host header is neither a loopback
// name nor one of allowed. Names match case-insensitively on the host part
// (any port and a single trailing dot are ignored); IP literals are compared
// as IPs (brackets and any IPv6 zone are ignored). An empty Host is rejected.
func HostGuard(allowed []string, next http.Handler) http.Handler {
	names := map[string]bool{}
	ips := map[netip.Addr]bool{}
	for _, a := range allowed {
		if ip, name := parseHost(a); ip.IsValid() {
			ips[ip] = true
		} else if name != "" {
			names[name] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, name := parseHost(r.Host)
		var ok bool
		if ip.IsValid() {
			ok = ip.IsLoopback() || ips[ip]
		} else {
			ok = name != "" && (name == "localhost" || names[name])
		}
		if !ok {
			http.Error(w, "localrouter: forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// parseHost strips an optional port and IPv6 brackets from hostport. It
// returns the IP (unmapped, zone dropped) for an IP literal, otherwise the
// lower-cased name with one trailing dot removed (FQDN form "host." is the
// same host). Trailing-dot stripping applies to names only, so "127.0.0.1."
// is a name and never matches as loopback.
func parseHost(hostport string) (netip.Addr, string) {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.WithZone("").Unmap(), ""
	}
	return netip.Addr{}, strings.ToLower(strings.TrimSuffix(host, "."))
}
