package app

import (
	"net"
	"net/http"
	"strings"
)

// LocalHostGuard rejects requests whose Host header is not a loopback name.
// It blocks DNS-rebinding pages from reading the unauthenticated control API
// through the user's browser. Disabled when non-loopback binding is allowed.
func LocalHostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "localrouter: forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
