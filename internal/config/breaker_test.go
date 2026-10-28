package config

import (
	"strings"
	"testing"
	"time"
)

// B1: policy.breaker is opt-in, defaults only when enabled, and rejects
// negative values and max < base.
func TestB1BreakerConfig(t *testing.T) {
	const base = `
clients: [{name: a, class: interactive, key_file: k}]
`
	c, err := Load(write(t, base))
	if err != nil {
		t.Fatal(err)
	}
	if c.Policy.Breaker != (BreakerConfig{}) {
		t.Fatalf("absent block must stay disabled and zero: %+v", c.Policy.Breaker)
	}

	c, err = Load(write(t, base+"policy: {breaker: {threshold: 3}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if b := c.Policy.Breaker; b.Threshold != 3 || b.Base.D() != 5*time.Second || b.Max.D() != 60*time.Second {
		t.Fatalf("enabled defaults: %+v", b)
	}

	c, err = Load(write(t, base+"policy: {breaker: {threshold: 2, base: 2s, max: 2s}}\n"))
	if err != nil || c.Policy.Breaker.Base.D() != 2*time.Second || c.Policy.Breaker.Max.D() != 2*time.Second {
		t.Fatalf("explicit knobs: %+v %v", c.Policy.Breaker, err)
	}

	for body, want := range map[string]string{
		"policy: {breaker: {threshold: -1}}\n":                    "must not be negative",
		"policy: {breaker: {threshold: 1, base: -1s}}\n":          "must not be negative",
		"policy: {breaker: {threshold: 1, max: -1s}}\n":           "must not be negative",
		"policy: {breaker: {threshold: 1, base: 10s, max: 5s}}\n": "max must not be less than",
		"policy: {breaker: {threshold: 1, base: 2m}}\n":           "max must not be less than",
		"policy: {breaker: {threshold: 1, window: 2m}}\n":         "field window not found",
	} {
		if _, err := Load(write(t, base+body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err=%v, want %q", body, err, want)
		}
	}
}
