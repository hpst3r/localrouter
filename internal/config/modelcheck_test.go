package config

import (
	"strings"
	"testing"
	"time"
)

const mcRoutes = `
routes:
  - {name: r, models: [m], interactive: [oc]}
`

// MC2: model_check is optional; absent, null or 0 disables it; a non-zero
// interval must be within [30s, 24h]; unknown keys are rejected.
func TestMC2ModelCheckConfig(t *testing.T) {
	for _, c := range []struct {
		name, yaml string
		want       time.Duration
	}{
		{"absent", "", 0},
		{"null", "model_check:\n", 0},
		{"zero", "model_check: {interval: 0s}\n", 0},
		{"min", "model_check: {interval: 30s}\n", 30 * time.Second},
		{"max", "model_check: {interval: 24h}\n", 24 * time.Hour},
	} {
		cfg, err := Load(write(t, capAccounts+c.yaml+mcRoutes))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := cfg.ModelCheckInterval(); got != c.want {
			t.Fatalf("%s: interval %v, want %v", c.name, got, c.want)
		}
	}
	for _, c := range []struct{ name, yaml, want string }{
		{"negative", "model_check: {interval: -1m}\n", "model_check.interval"},
		{"too short", "model_check: {interval: 29s}\n", "model_check.interval"},
		{"too long", "model_check: {interval: 25h}\n", "model_check.interval"},
		{"bad duration", "model_check: {interval: soon}\n", "invalid duration"},
		{"unknown key", "model_check: {interval: 1m, strict: true}\n", "strict"},
	} {
		_, err := Load(write(t, capAccounts+c.yaml+mcRoutes))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}
