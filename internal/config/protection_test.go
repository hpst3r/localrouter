package config

import (
	"strings"
	"testing"
	"time"
)

// Absent limits/timeouts mean unlimited concurrency and finite safe defaults.
func TestLimitsAndTimeoutDefaults(t *testing.T) {
	c, err := Load(write(t, `
clients: [{name: a, class: interactive, key_file: k}]
accounts: [{id: o, provider: openai_compat, base_url: http://x, api_key_env: K}]
routes: [{name: r, models: [m], interactive: [o]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.MaxConcurrent != 0 || len(c.Limits.MaxConcurrentPerClient) != 0 {
		t.Fatalf("limits default should be unlimited: %+v", c.Limits)
	}
	if c.Timeouts.Header.D() != 10*time.Second || c.Timeouts.Body.D() != 30*time.Second ||
		c.Timeouts.Idle.D() != 120*time.Second || c.Timeouts.Shutdown.D() != 30*time.Second {
		t.Fatalf("timeout defaults: %+v", c.Timeouts)
	}
}

func TestLimitsAndTimeoutsParsed(t *testing.T) {
	c, err := Load(write(t, `
limits:
  max_concurrent: 64
  max_concurrent_per_client: { a: 8 }
timeouts:
  header: 5s
  body: 45s
  idle: 90s
  shutdown: 20s
clients: [{name: a, class: interactive, key_file: k}]
accounts: [{id: o, provider: openai_compat, base_url: http://x, api_key_env: K}]
routes: [{name: r, models: [m], interactive: [o]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.MaxConcurrent != 64 || c.Limits.MaxConcurrentPerClient["a"] != 8 {
		t.Fatalf("limits: %+v", c.Limits)
	}
	if c.Timeouts.Header.D() != 5*time.Second || c.Timeouts.Body.D() != 45*time.Second ||
		c.Timeouts.Idle.D() != 90*time.Second || c.Timeouts.Shutdown.D() != 20*time.Second {
		t.Fatalf("timeouts: %+v", c.Timeouts)
	}
}

func TestNegativeLimitsRejected(t *testing.T) {
	_, err := Load(write(t, `
limits: {max_concurrent: -1}
clients: [{name: a, class: interactive, key_file: k}]
accounts: [{id: o, provider: openai_compat, base_url: http://x, api_key_env: K}]
routes: [{name: r, models: [m], interactive: [o]}]
`))
	if err == nil || !strings.Contains(err.Error(), "max_concurrent") {
		t.Fatalf("global negative: %v", err)
	}
	_, err = Load(write(t, `
limits: {max_concurrent_per_client: {a: -2}}
clients: [{name: a, class: interactive, key_file: k}]
accounts: [{id: o, provider: openai_compat, base_url: http://x, api_key_env: K}]
routes: [{name: r, models: [m], interactive: [o]}]
`))
	if err == nil || !strings.Contains(err.Error(), "per_client") {
		t.Fatalf("per-client negative: %v", err)
	}
}

// A per-client limit naming an unconfigured client is almost certainly a typo.
func TestPerClientLimitUnknownClientRejected(t *testing.T) {
	_, err := Load(write(t, `
limits: {max_concurrent_per_client: {ghost: 4}}
clients: [{name: a, class: interactive, key_file: k}]
accounts: [{id: o, provider: openai_compat, base_url: http://x, api_key_env: K}]
routes: [{name: r, models: [m], interactive: [o]}]
`))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown client limit: %v", err)
	}
}

func TestNegativeTimeoutsRejected(t *testing.T) {
	for _, field := range []string{"header", "body", "idle", "shutdown"} {
		_, err := Load(write(t, `
timeouts: {`+field+`: -1s}
clients: [{name: a, class: interactive, key_file: k}]
accounts: [{id: o, provider: openai_compat, base_url: http://x, api_key_env: K}]
routes: [{name: r, models: [m], interactive: [o]}]
`))
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("%s negative: %v", field, err)
		}
	}
}
