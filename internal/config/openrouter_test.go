package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

const orClients = "clients: [{name: a, class: interactive, key_file: k}]\n"

func TestOpenRouterMinimalDefaults(t *testing.T) {
	c, err := Load(write(t, orClients+`
accounts:
  - {id: or, provider: openrouter, api_key_file: keys/or.key, management_key_file: keys/or-mgmt.key}
routes:
  - {name: r, models: [anthropic/claude-sonnet-4.5], interactive: [or]}
`))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Accounts[0]
	if core.ProviderOpenRouter != "openrouter" || a.Provider != core.ProviderOpenRouter {
		t.Fatalf("provider = %q", a.Provider)
	}
	if a.BaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("base_url default = %q", a.BaseURL)
	}
	if a.CostBasis != "metered" {
		t.Errorf("cost_basis default = %q, want metered", a.CostBasis)
	}
	if !filepath.IsAbs(a.APIKeyFile) || !filepath.IsAbs(a.ManagementKeyFile) || !strings.HasSuffix(a.ManagementKeyFile, "keys/or-mgmt.key") {
		t.Errorf("paths not expanded: %q %q", a.APIKeyFile, a.ManagementKeyFile)
	}
}

func TestOpenRouterCustomBaseURLTrimmed(t *testing.T) {
	c, err := Load(write(t, orClients+`
accounts:
  - {id: or, provider: openrouter, api_key_env: OR_KEY, base_url: "http://127.0.0.1:9999/api/v1/"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Accounts[0].BaseURL; got != "http://127.0.0.1:9999/api/v1" {
		t.Errorf("base_url = %q", got)
	}
}

func TestOpenRouterValidation(t *testing.T) {
	_, err := Load(write(t, orClients+`
accounts:
  - {id: or1, provider: openrouter}
  - {id: or2, provider: openrouter, api_key_env: K, reserve: {5h: 0.1}}
  - {id: ol, provider: ollama, api_key_env: K, management_key_env: M}
  - {id: oc, provider: openai_compat, base_url: "http://x", api_key_env: K, management_key_file: m.key}
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		"account or1: api_key_file or api_key_env required",
		"account or2: reserve does not apply to prepaid openrouter accounts",
		"account ol: management_key_file/management_key_env apply only to openrouter accounts",
		"account oc: management_key_file/management_key_env apply only to openrouter accounts",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestExistingProvidersUnaffectedByManagementFields(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range c.Accounts {
		if a.ManagementKeyFile != "" || a.ManagementKeyEnv != "" {
			t.Errorf("account %s got management key fields", a.ID)
		}
	}
}

func TestOpenRouterExampleConfigLoads(t *testing.T) {
	c, err := Load("../../config.openrouter.example.yaml")
	if err != nil {
		t.Fatalf("openrouter example: %v", err)
	}
	var found bool
	for _, a := range c.Accounts {
		if a.Provider == core.ProviderOpenRouter {
			found = true
			if a.BaseURL != "https://openrouter.ai/api/v1" {
				t.Errorf("base_url = %q", a.BaseURL)
			}
		}
	}
	if !found {
		t.Fatal("no openrouter account in example")
	}
}
