package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// discAccounts adds a claude and an openrouter account to capAccounts.
const discAccounts = `
  - {id: cl, provider: claude}
  - {id: or, provider: openrouter, api_key_env: ORKEY}
`

func loadDisc(t *testing.T, routes string) (*Config, error) {
	t.Helper()
	return Load(write(t, capAccounts+discAccounts+routes))
}

func TestDiscoveryValidRouteProjection(t *testing.T) {
	c, err := loadDisc(t, `
routes:
  - {name: legacy, models: [m], interactive: [cx]}
  - name: lab
    discover_models: true
    interactive: [ol]
    background: [oc]
  - name: lab2
    discover_models: true
    discover_interval: 30s
    discover_match: ["unsloth/*", "x?"]
    models: [static-on-discovering]
    interactive: [oc]
    upstreams:
      oc: {protocols: [chat], input_modalities: [text]}
`)
	if err != nil {
		t.Fatalf("valid discovering routes rejected: %v", err)
	}
	rs := c.CoreRoutes()
	if rs[0].DiscoverModels || rs[0].DiscoverInterval != 0 || rs[0].DiscoverMatch != nil {
		t.Fatalf("legacy route gained discovery fields: %+v", rs[0])
	}
	if !rs[1].DiscoverModels || rs[1].DiscoverInterval != DefaultDiscoverInterval || rs[1].DiscoverMatch != nil || len(rs[1].Models) != 0 {
		t.Fatalf("default discovering route: %+v", rs[1])
	}
	if rs[2].DiscoverInterval != 30*time.Second || !slices.Equal(rs[2].DiscoverMatch, []string{"unsloth/*", "x?"}) {
		t.Fatalf("explicit discovering route: %+v", rs[2])
	}
	// Deep copy: mutating the projection never reaches the loaded config.
	rs[2].DiscoverMatch[0] = "mutated"
	if c.Routes[2].DiscoverMatch[0] != "unsloth/*" {
		t.Fatal("CoreRoutes aliases discover_match")
	}
}

// D2: one failing config per validation rule.
func TestDiscoveryValidationRules(t *testing.T) {
	cases := []struct{ name, routes, want string }{
		{"rule1 codex candidate", `
routes: [{name: r, discover_models: true, interactive: [ol], background: [cx]}]`, `account "cx" is codex`},
		{"rule1 openrouter candidate", `
routes: [{name: r, discover_models: true, interactive: [or]}]`, `account "or" is openrouter`},
		{"rule1 claude candidate", `
routes: [{name: r, discover_models: true, interactive: [cl]}]`, `account "cl" is claude`},
		{"rule2 route upstream_model", `
routes: [{name: r, discover_models: true, upstream_model: x, interactive: [ol]}]`, "upstream_model must be empty"},
		{"rule2 descriptor upstream_model", `
routes:
  - name: r
    discover_models: true
    interactive: [ol]
    upstreams: {ol: {upstream_model: x, protocols: [chat], input_modalities: [text]}}`, "upstreams[ol].upstream_model must be empty"},
		{"rule3 models still required without discovery", `
routes: [{name: r, interactive: [ol]}]`, "name and models required"},
		{"rule4 interval without discovery", `
routes: [{name: r, models: [m], discover_interval: 1m, interactive: [ol]}]`, "discover_interval requires discover_models"},
		{"rule4 match without discovery", `
routes: [{name: r, models: [m], discover_match: ["a*"], interactive: [ol]}]`, "discover_match requires discover_models"},
		{"rule4 interval too short", `
routes: [{name: r, discover_models: true, discover_interval: 29s, interactive: [ol]}]`, "within [30s, 24h]"},
		{"rule4 interval too long", `
routes: [{name: r, discover_models: true, discover_interval: 25h, interactive: [ol]}]`, "within [30s, 24h]"},
		{"rule4 interval syntax", `
routes: [{name: r, discover_models: true, discover_interval: soon, interactive: [ol]}]`, "invalid duration"},
		{"rule4 empty pattern", `
routes: [{name: r, discover_models: true, discover_match: [""], interactive: [ol]}]`, "patterns must be non-empty"},
		{"rule4 bad pattern", `
routes: [{name: r, discover_models: true, discover_match: ["[unclosed"], interactive: [ol]}]`, "is invalid"},
		{"rule4 empty match list", `
routes: [{name: r, discover_models: true, discover_match: [], interactive: [ol]}]`, "at least one pattern"},
		{"rule5 unknown key stays strict", `
routes: [{name: r, discover_models: true, discover_modelz: true, interactive: [ol]}]`, "discover_modelz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadDisc(t, c.routes)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}
