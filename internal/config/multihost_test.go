package config

import (
	"strings"
	"testing"
)

const multiBase = `
listen: %LISTEN%
%EXTRA%
clients:
  - {name: me, class: interactive, key_file: me.key}
  - {name: vm1, class: background, key_file: vm1.key, host: vm1, ingest: true}
accounts:
  - {id: cm, provider: claude, quota_source: %QS%, reserve: {5h: 0.1, weekly: 0.1}}
  - {id: ol, provider: ollama, base_url: https://ollama.com/v1, api_key_file: ol.key}
routes:
  - {name: ollama, models: [m], interactive: [ol]}
`

func multi(listen, extra, qs string) string {
	r := strings.NewReplacer("%LISTEN%", listen, "%EXTRA%", extra, "%QS%", qs)
	return r.Replace(multiBase)
}

func TestMultiHostConfigValid(t *testing.T) {
	c, err := Load(write(t, multi("100.64.0.10:8787",
		"allow_non_loopback: true\nallowed_hosts: [router.tail, 100.64.0.10]\ncontrol: {require_auth: true}", "agent")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Clients[1].Host != "vm1" || !c.Clients[1].Ingest {
		t.Fatalf("client host/ingest not parsed: %+v", c.Clients[1])
	}
	accts := c.CoreAccounts()
	if accts[0].QuotaSource != "agent" {
		t.Fatalf("QuotaSource = %q", accts[0].QuotaSource)
	}
}

func TestClaudeQuotaSourceDefaultsLocal(t *testing.T) {
	c, err := Load(write(t, strings.Replace(multi("127.0.0.1:8787", "", "local"), "quota_source: local, ", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.CoreAccounts()[0].QuotaSource != "local" {
		t.Fatalf("default QuotaSource = %q", c.CoreAccounts()[0].QuotaSource)
	}
}

func TestMultiHostValidation(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"network without auth":      {multi("0.0.0.0:8787", "allow_non_loopback: true", "local"), "require_auth"},
		"allowed_hosts on loopback": {multi("127.0.0.1:8787", "allowed_hosts: [x]", "local"), "allowed_hosts requires"},
		"tls half set":              {multi("127.0.0.1:8787", "tls_cert_file: c.pem", "local"), "together"},
		"bad quota_source":          {multi("127.0.0.1:8787", "", "remote"), "quota_source must be"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestQuotaSourceOnlyForClaude(t *testing.T) {
	body := strings.Replace(multi("127.0.0.1:8787", "", "local"),
		"api_key_file: ol.key}", "api_key_file: ol.key, quota_source: agent}", 1)
	if _, err := Load(write(t, body)); err == nil || !strings.Contains(err.Error(), "only to claude") {
		t.Fatalf("got %v", err)
	}
}
