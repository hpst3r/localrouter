package app

import (
	"net"
	"net/http"
	"strings"
)

// LocalHostGuard rejects requests whose Host header is not a loopback name.
// It blocks DNS-rebinding pages from reading the control API through the
// user's browser. It is HostGuard with no extra allowed hosts.
func LocalHostGuard(next http.Handler) http.Handler { return HostGuard(nil, next) }

// HostGuard rejects (403) requests whose Host header is neither a loopback
// name nor one of allowed. Names match case-insensitively on the host part
// (any port is ignored); IP literals are compared as IPs.
func HostGuard(allowed []string, next http.Handler) http.Handler {
	names := map[string]bool{}
	var ips []net.IP
	for _, a := range allowed {
		h := hostPart(a)
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else if h != "" {
			names[strings.ToLower(h)] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostPart(r.Host)
		ok := isLoopbackHost(host) || names[strings.ToLower(host)]
		if !ok {
			if ip := net.ParseIP(host); ip != nil {
				for _, a := range ips {
					if a.Equal(ip) {
						ok = true
						break
					}
				}
			}
		}
		if !ok {
			http.Error(w, "localrouter: forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostPart strips an optional port and IPv6 brackets.
func hostPart(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.Trim(host, "[]")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
