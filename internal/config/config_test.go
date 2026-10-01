package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExampleConfigLoads(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if c.Listen != "127.0.0.1:8787" || len(c.Accounts) != 4 || len(c.Routes) != 2 {
		t.Fatalf("unexpected: %+v", c)
	}
	if c.Accounts[0].BaseURL != "https://chatgpt.com/backend-api/codex" || c.Accounts[0].CostBasis != "api_equivalent" {
		t.Fatalf("codex defaults not applied: %+v", c.Accounts[0])
	}
	if !filepath.IsAbs(c.Clients[0].KeyFile) {
		t.Fatalf("key file not resolved absolute: %s", c.Clients[0].KeyFile)
	}
}

func TestRejectsNonLoopback(t *testing.T) {
	_, err := Load(write(t, `
listen: 0.0.0.0:8787
clients: [{name: a, class: interactive, key_file: k}]
`))
	if err == nil || !strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("want loopback error, got %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	_, err := Load(write(t, `
clients: [{name: a, class: sometimes, key_file: k}]
accounts:
  - {id: x, provider: codex, reserve: {daily: 0.1}}
  - {id: x, provider: ollama}
routes:
  - {name: r, models: [m], interactive: [nope]}
  - {name: r2, models: [m], interactive: [x]}
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"class must be", "reserve window", "duplicate", "api_key_file", "unknown account", "in both routes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Load(write(t, `
clients: [{name: a, class: interactive, key_file: k}]
reserves: {}
`))
	if err == nil {
		t.Fatal("unknown top-level field must be rejected")
	}
}

func TestBackgroundFallsBackToInteractive(t *testing.T) {
	c, err := Load(write(t, `
clients: [{name: a, class: interactive, key_file: k}]
accounts: [{id: o, provider: openai_compat, base_url: http://x, api_key_env: K}]
routes: [{name: r, models: [m], interactive: [o]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	r := c.CoreRoutes()[0]
	if len(r.Background) != 1 || r.Background[0] != "o" {
		t.Fatalf("background fallback: %+v", r)
	}
}
