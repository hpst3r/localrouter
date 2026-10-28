package config

import (
	"strings"
	"testing"
)

// TestR23MovingAliasVariantAndRouter (R2.3, FDC-L3): a trailing :variant is
// stripped before the -latest/-preview test, and openrouter/ router ids are
// moving aliases; all need allow_alias.
func TestR23MovingAliasVariantAndRouter(t *testing.T) {
	aliases := []string{"typesafe/jev-latest:nitro", "foo-preview:free", "openrouter/auto", "OpenRouter/auto:floor", "openrouter/some-router"}
	for _, m := range aliases {
		if !isMovingAlias(m) {
			t.Errorf("%s not reported as alias", m)
		}
		q := `"` + m + `"`
		_, err := Load(write(t, decAccounts+decRoute("systemone", q, q, "")))
		if err == nil || !strings.Contains(err.Error(), "moving alias") {
			t.Errorf("%s without allow_alias: err = %v", m, err)
		}
		if _, err := Load(write(t, decAccounts+decRoute("systemone", q, q, "        allow_alias: true"))); err != nil {
			t.Errorf("%s with allow_alias: %v", m, err)
		}
	}
	// A variant on a pin is still a pin, and a ':' before the last '/' is not
	// a variant separator.
	for _, m := range []string{"typesafe/jev-1.13:nitro", "jev-1.13:free", "a-latest:x/jev-1.13", "vendor/openrouter/auto-1.2"} {
		if isMovingAlias(m) {
			t.Errorf("%s reported as alias", m)
		}
	}
}

// TestR24DecisionRouteUpstreamModelTrimmed (R2.4, FDC-L5): a decision route's
// route-level upstream_model is projected trimmed, so what is forwarded is
// what validation compared. Non-decision routes are unchanged.
func TestR24DecisionRouteUpstreamModelTrimmed(t *testing.T) {
	body := decAccounts + `
routes:
  - name: jev
    models: [jev]
    upstream_model: "  typesafe/jev-1.14 "
    interactive: [or1]
    upstreams:
      or1: {protocols: [systemone], input_modalities: [text], question_types: [choice]}
  - name: chat
    models: [chat]
    upstream_model: " gpt-x "
    interactive: [oc]
`
	c, err := Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	rs := c.CoreRoutes()
	if rs[0].UpstreamModel != "typesafe/jev-1.14" {
		t.Fatalf("decision route upstream_model = %q, want trimmed", rs[0].UpstreamModel)
	}
	if rs[1].UpstreamModel != " gpt-x " {
		t.Fatalf("chat route upstream_model = %q, want unchanged", rs[1].UpstreamModel)
	}
}
