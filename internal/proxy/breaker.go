package proxy

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// denialBreakerOpen is the "denial" attribute of the policy denied log line
// for a request refused only by the reachability breaker.
const denialBreakerOpen = "breaker_open"

// breakerPolicy is implemented by policies with a reachability breaker
// (internal/policy.Policy). AcquireWithBreaker is Acquire that additionally
// reports, for a denial in which every rejected candidate was refused by the
// breaker, those accounts and the time until the earliest half-opens.
type breakerPolicy interface {
	AcquireWithBreaker(class core.Class, candidates []string, exclude map[string]bool) (core.Lease, core.Decision, []string, time.Duration)
}

// acquire calls the policy, using AcquireWithBreaker when available. With
// any other policy unreachable is always nil.
func (p *Proxy) acquire(class core.Class, candidates []string, exclude map[string]bool) (core.Lease, core.Decision, []string, time.Duration) {
	if bp, ok := p.deps.Policy.(breakerPolicy); ok {
		return bp.AcquireWithBreaker(class, candidates, exclude)
	}
	l, dec := p.deps.Policy.Acquire(class, candidates, exclude)
	return l, dec, nil, 0
}

// writeUnreachable answers a request whose every candidate is held open by
// the reachability breaker: 503 upstream_unavailable with Retry-After set to
// the whole seconds until the earliest half-open (at least 1).
func writeUnreachable(w http.ResponseWriter, accounts []string, retryIn time.Duration) {
	secs := int64((retryIn + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.FormatInt(max(secs, 1), 10))
	writeError(w, http.StatusServiceUnavailable,
		"localrouter: upstream unreachable: "+strings.Join(accounts, ", "), "upstream_unavailable")
}

// unreachableClass classifies a send error (client cancellation already
// excluded) for core.Outcome.Unreachable. Errors from before the request
// reached the transport (body rewrite) return "": they say nothing about the
// account. Every transport failure got no response headers, so it counts.
func unreachableClass(err error) string {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return ""
	}
	var (
		dnsErr   *net.DNSError
		opErr    *net.OpError
		certErr  *tls.CertificateVerificationError
		recErr   tls.RecordHeaderError
		alertErr tls.AlertError
		netErr   net.Error
	)
	switch {
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.As(err, &certErr), errors.As(err, &recErr), errors.As(err, &alertErr):
		return "tls"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "dial"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.EPIPE):
		return "reset"
	default:
		return "transport"
	}
}
