// Package mcpserver is a thin Model Context Protocol adapter over a running
// LocalRouter control API. It exposes exactly three read-only tools
// (quota_status, usage_summary, check_admission), forwards them to the
// configured control endpoint over HTTP, and never touches the router's
// configuration, database, ledger or provider credentials. The only
// credential it reads is the client bearer key file the operator points it at.
//
// The adapter is deliberately thin: it validates and preserves the upstream
// documents (bounded, never reformatted into invented fields), labels cost
// figures as ledger estimates rather than actual spend, and reports every
// failure as a stable, structured error code.
package mcpserver

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// DefaultURL is the loopback control endpoint a stock LocalRouter serves.
	DefaultURL = "http://127.0.0.1:8787"
	// DefaultTimeout bounds one control API round trip.
	DefaultTimeout = 10 * time.Second
	// MaxTimeout is the largest per-request timeout the adapter accepts.
	MaxTimeout = 5 * time.Minute
	// DefaultMaxResultChars bounds the bytes of the serialized tool result.
	DefaultMaxResultChars = 24000
	// MinMaxResultChars is the smallest usable result budget.
	MinMaxResultChars = 1024
	// MaxResponseBytes caps how much of an upstream response body is read.
	MaxResponseBytes = 1 << 20
	// ImplementationName identifies this server to MCP clients.
	ImplementationName = "localrouter-mcp"
	// ImplementationVersion is the adapter's own version, not the router's.
	ImplementationVersion = "1.0.0"
)

// Config is the adapter's start-up configuration. Every field is fixed by the
// operator on the command line; none of it is reachable from a tool call.
type Config struct {
	// URL is the LocalRouter control API base URL. It must carry no
	// credentials, query or fragment, and a remote host must use https.
	URL string
	// KeyFile holds the client bearer key (only needed when the server runs
	// with control.require_auth). It is read once at start-up.
	KeyFile string
	// Timeout bounds one control API round trip.
	Timeout time.Duration
	// MaxResultChars bounds the bytes of the serialized tool result object the
	// host receives (both document copies plus the metadata envelope), not the
	// bare document and not the transport framing.
	MaxResultChars int
}

// validate normalizes the configuration in place and rejects anything that
// could leak the bearer key or point the adapter at an unintended host.
func (c *Config) validate() error {
	if strings.TrimSpace(c.URL) == "" {
		c.URL = DefaultURL
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Timeout < 0 {
		return errors.New("timeout must be positive")
	}
	if c.Timeout > MaxTimeout {
		return fmt.Errorf("timeout %s exceeds the %s maximum", c.Timeout, MaxTimeout)
	}
	if c.MaxResultChars == 0 {
		c.MaxResultChars = DefaultMaxResultChars
	}
	if c.MaxResultChars < MinMaxResultChars {
		return fmt.Errorf("max-result-chars must be at least %d", MinMaxResultChars)
	}
	base, err := ValidateBaseURL(c.URL)
	if err != nil {
		return err
	}
	c.URL = base
	return nil
}

// ValidateBaseURL normalizes a control API base URL to scheme://authority and
// enforces the transport policy: no embedded credentials (userinfo), no query,
// no fragment and no path; loopback hosts may use plaintext http, every other
// host must use https. A URL that fails here is never used for a request, so a
// misconfigured --url can never send the client key in the clear to a remote
// host.
func ValidateBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// Do not wrap err: *url.Error.Error() is `parse "<raw url>": <reason>`,
		// which would print any userinfo (a credential) verbatim into stderr
		// and any supervisor log. Report a fixed, non-reflective reason.
		return "", errors.New("invalid url: not a valid http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.User != nil {
		return "", errors.New("url must not carry credentials (userinfo)")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", errors.New("url must not carry a query string")
	}
	if u.Fragment != "" {
		return "", errors.New("url must not carry a fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("url must not carry a path, got %q", u.Path)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("url must name a host")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("plaintext http is allowed only to a loopback host, got %q (use https)", u.Hostname())
	}
	return u.Scheme + "://" + u.Host, nil
}

// isLoopbackHost reports whether host names the local machine and only it.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readKeyFile reads the client bearer key. The value is trimmed and never
// logged, echoed into an error message or included in a tool result.
func readKeyFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading key file: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", errors.New("key file is empty")
	}
	return key, nil
}
