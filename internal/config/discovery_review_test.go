package config

import (
	"os"
	"testing"
)

func reviewBug(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("LOCALROUTER_REVIEW") == "" {
		t.Skip("BUG: " + msg)
	}
}

// Validation rule 4 rejects discover_interval/discover_match on a route
// without discover_models, and the implementation rejects an explicit empty
// discover_match because it "would otherwise read as all ids". A YAML null
// value (`key: ~` or a bare `key:`) slips past both: the pointer/slice stays
// nil, so the key is silently accepted on a non-discovering route and, on a
// discovering route, `discover_match: ~` projects to "all ids".
func TestReviewDiscoverNullKeysAccepted(t *testing.T) {
	reviewBug(t, "null-valued discover_interval/discover_match pass validation")
	cases := []struct{ name, routes string }{
		{"interval null on non-discovering route", `
routes: [{name: r, models: [m], discover_interval: ~, interactive: [ol]}]`},
		{"match null on non-discovering route", `
routes: [{name: r, models: [m], discover_match: ~, interactive: [ol]}]`},
		{"match null on discovering route reads as all ids", `
routes: [{name: r, discover_models: true, discover_match: ~, interactive: [ol]}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := loadDisc(t, c.routes)
			if err == nil {
				t.Errorf("config accepted (routes[0] = %+v); want a validation error like the non-null/empty forms", cfg.CoreRoutes()[0])
			}
		})
	}
}
