package config

import (
	"strings"
	"testing"
	"time"
)

// poolAccounts is the preamble of the pooled-backend (routes[].balance)
// tests: two llama-server style openai_compat accounts plus one of every
// other provider.
const poolAccounts = `
clients: [{name: a, class: interactive, key_file: k}]
accounts:
  - {id: g1, provider: openai_compat, base_url: http://g1, api_key_env: K}
  - {id: g2, provider: openai_compat, base_url: http://g2, api_key_env: K}
  - {id: g3, provider: openai_compat, base_url: http://g3, api_key_env: K, cost_basis: api_equivalent}
  - {id: ol, provider: ollama, base_url: http://ol, api_key_env: K, cost_basis: metered}
  - {id: cx, provider: codex}
  - {id: or, provider: openrouter, api_key_env: K}
  - {id: cl, provider: claude}
`

const poolRoute = `
routes:
  - name: pool
    models: [m]
    upstream_model: up
    interactive: [g1, g2]
    background: [g2, g1]
    balance:
      strategy: least_inflight
      identity: "  Qwen3.5-9B Q4_K_M  "
      sticky_ttl: 15m
      members:
        g1: {weight: 2, max_inflight: 4}
        g2: {max_inflight: 2}
`

func TestP2BalanceValidLoadsAndProjects(t *testing.T) {
	c, err := Load(write(t, poolAccounts+poolRoute))
	if err != nil {
		t.Fatal(err)
	}
	r := c.CoreRoutes()[0]
	if r.BalanceStrategy != "least_inflight" || r.BalanceIdentity != "Qwen3.5-9B Q4_K_M" || r.BalanceStickyTTL != 15*time.Minute {
		t.Fatalf("projection: %+v", r)
	}
	if r.BalanceWeight["g1"] != 2 || r.BalanceWeight["g2"] != 1 || r.BalanceMaxInflight["g1"] != 4 || r.BalanceMaxInflight["g2"] != 2 {
		t.Fatalf("members: %v %v", r.BalanceWeight, r.BalanceMaxInflight)
	}
	// Deep copy: mutating the projection never reaches the config or a second
	// projection.
	r.BalanceWeight["g1"] = 99
	if again := c.CoreRoutes()[0]; again.BalanceWeight["g1"] != 2 {
		t.Fatal("CoreRoutes aliases the balance maps")
	}
}

func TestP2NoBalanceProjectsNothing(t *testing.T) {
	c, err := Load(write(t, poolAccounts+`
routes:
  - {name: r, models: [m], interactive: [g1, g2]}
`))
	if err != nil {
		t.Fatal(err)
	}
	r := c.CoreRoutes()[0]
	if r.BalanceStrategy != "" || r.BalanceWeight != nil || r.BalanceMaxInflight != nil || r.BalanceStickyTTL != 0 || r.BalanceIdentity != "" {
		t.Fatalf("unpooled route projected balance fields: %+v", r)
	}
}

// TestP2SingleMemberAndBackgroundFallback: one member is allowed (rule 5) and
// a route without a background list pools its interactive list.
func TestP2SingleMemberAndBackgroundFallback(t *testing.T) {
	_, err := Load(write(t, poolAccounts+`
routes:
  - name: one
    models: [m]
    interactive: [g1]
    balance: {strategy: least_inflight, identity: x, members: {g1: {max_inflight: 1}}}
`))
	if err != nil {
		t.Fatal(err)
	}
}

func TestP2OllamaPoolAllowed(t *testing.T) {
	_, err := Load(write(t, poolAccounts+`
routes:
  - name: o
    models: [m]
    interactive: [ol]
    balance: {strategy: least_inflight, identity: x, members: {ol: {}}}
`))
	if err != nil {
		t.Fatal(err)
	}
}

// TestP2BalanceValidation is one failing config per rule.
func TestP2BalanceValidation(t *testing.T) {
	route := func(cands, balance string) string {
		return "\nroutes:\n  - name: pool\n    models: [m]\n    interactive: " + cands + "\n    balance: " + balance + "\n"
	}
	const ok = `{strategy: least_inflight, identity: x, members: {g1: {}, g2: {}}}`
	caps := func(g2 string) string {
		return "\nroutes:\n  - name: pool\n    models: [m]\n    interactive: [g1, g2]\n" +
			"    upstreams:\n      g1: {upstream_model: a, protocols: [chat, responses], input_modalities: [text, image], tools: true}\n" +
			"      g2: " + g2 + "\n    balance: " + ok + "\n"
	}
	cases := []struct {
		name, body, want string
	}{
		{"missing member key", route("[g1, g2]", `{strategy: least_inflight, identity: x, members: {g1: {}}}`), `members is missing candidate "g2"`},
		{"extra member key", route("[g1]", `{strategy: least_inflight, identity: x, members: {g1: {}, g2: {}}}`), `members names "g2", which is not a route candidate`},
		{"background member missing", "\nroutes:\n  - name: pool\n    models: [m]\n    interactive: [g1]\n    background: [g2]\n    balance: {strategy: least_inflight, identity: x, members: {g1: {}}}\n", `members is missing candidate "g2"`},
		{"mixed provider", route("[g1, ol]", `{strategy: least_inflight, identity: x, members: {g1: {}, ol: {}}}`), "members must share one provider"},
		{"mixed cost_basis", route("[g1, g3]", `{strategy: least_inflight, identity: x, members: {g1: {}, g3: {}}}`), "members must share one cost_basis"},
		{"codex member", route("[cx]", `{strategy: least_inflight, identity: x, members: {cx: {}}}`), `member "cx" has provider codex`},
		{"openrouter member", route("[or]", `{strategy: least_inflight, identity: x, members: {or: {}}}`), `member "or" has provider openrouter`},
		{"claude member", route("[cl]", `{strategy: least_inflight, identity: x, members: {cl: {}}}`), `member "cl" has provider claude`},
		{"differing protocols", caps(`{upstream_model: b, protocols: [responses], input_modalities: [image, text], tools: true}`), "identical upstreams capabilities"},
		{"differing modalities", caps(`{upstream_model: b, protocols: [responses, chat], input_modalities: [text], tools: true}`), "identical upstreams capabilities"},
		{"differing tools", caps(`{upstream_model: b, protocols: [responses, chat], input_modalities: [image, text]}`), "identical upstreams capabilities"},
		{"differing json_schema", caps(`{upstream_model: b, protocols: [responses, chat], input_modalities: [image, text], tools: true, json_schema: true}`), "identical upstreams capabilities"},
		{"differing stream", caps(`{upstream_model: b, protocols: [responses, chat], input_modalities: [image, text], tools: true, stream: true}`), "identical upstreams capabilities"},
		{"weight 0", route("[g1]", `{strategy: least_inflight, identity: x, members: {g1: {weight: 0}}}`), "members[g1].weight 0 must be in [1, 100]"},
		{"weight 101", route("[g1]", `{strategy: least_inflight, identity: x, members: {g1: {weight: 101}}}`), "members[g1].weight 101 must be in [1, 100]"},
		{"negative max_inflight", route("[g1]", `{strategy: least_inflight, identity: x, members: {g1: {max_inflight: -1}}}`), "members[g1].max_inflight -1 must not be negative"},
		{"unknown strategy", route("[g1]", `{strategy: round_robin, identity: x, members: {g1: {}}}`), `strategy "round_robin" must be least_inflight`},
		{"missing strategy", route("[g1]", `{identity: x, members: {g1: {}}}`), `strategy "" must be least_inflight`},
		{"empty identity", route("[g1]", `{strategy: least_inflight, identity: "   ", members: {g1: {}}}`), "identity is required"},
		{"long identity", route("[g1]", `{strategy: least_inflight, identity: "`+strings.Repeat("x", 129)+`", members: {g1: {}}}`), "identity must be at most 128 bytes"},
		{"unprintable identity", route("[g1]", `{strategy: least_inflight, identity: "a\tb", members: {g1: {}}}`), "identity must be printable"},
		{"verify not supported", route("[g1]", `{strategy: least_inflight, identity: x, verify: warn, members: {g1: {}}}`), "verify is not supported yet"},
		{"sticky_ttl 10s", route("[g1]", `{strategy: least_inflight, identity: x, sticky_ttl: 10s, members: {g1: {}}}`), "sticky_ttl 10s must be 0 or in [30s, 24h]"},
		{"sticky_ttl 25h", route("[g1]", `{strategy: least_inflight, identity: x, sticky_ttl: 25h, members: {g1: {}}}`), "sticky_ttl 25h0m0s must be 0 or in [30s, 24h]"},
		{"unknown member field", route("[g1]", `{strategy: least_inflight, identity: x, members: {g1: {priority: 1}}}`), "field priority not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, poolAccounts+tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestP2DifferingUpstreamModelPasses: only upstream_model may differ between
// member descriptors, and protocol/modality order does not matter.
func TestP2DifferingUpstreamModelPasses(t *testing.T) {
	_, err := Load(write(t, poolAccounts+`
routes:
  - name: pool
    models: [m]
    interactive: [g1, g2]
    upstreams:
      g1: {upstream_model: qwen-a, protocols: [chat, responses], input_modalities: [text, image]}
      g2: {upstream_model: qwen-b, protocols: [responses, chat], input_modalities: [image, text]}
    balance: {strategy: least_inflight, identity: x, sticky_ttl: 30s, members: {g1: {weight: 100}, g2: {weight: 1}}}
`))
	if err != nil {
		t.Fatal(err)
	}
}

// TestR1CrossRouteBalanceRules: Amendment R1.1 (one max_inflight per account
// across balance blocks) and R1.2 (unique route names once any route pools).
func TestR1CrossRouteBalanceRules(t *testing.T) {
	pool := func(name, caps string) string {
		return "  - name: " + name + "\n    models: [m-" + name + "]\n    interactive: [g1, g2]\n" +
			"    balance: {strategy: least_inflight, identity: x, members: " + caps + "}\n"
	}
	const plain = "  - {name: plain, models: [m-plain], interactive: [g1]}\n"
	cases := []struct {
		name, routes, want string
	}{
		{"same cap", pool("p1", "{g1: {max_inflight: 2}, g2: {}}") + pool("p2", "{g1: {max_inflight: 2}, g2: {}}") + plain, ""},
		{"capped member also unpooled", pool("p1", "{g1: {max_inflight: 2}, g2: {}}") + plain, ""},
		{"different caps", pool("p1", "{g1: {max_inflight: 2}, g2: {}}") + pool("p2", "{g1: {max_inflight: 3}, g2: {}}"),
			`account "g1": balance members max_inflight differs between routes p1 (2) and p2 (3)`},
		{"cap and uncapped", pool("p1", "{g1: {}, g2: {}}") + pool("p2", "{g1: {}, g2: {max_inflight: 1}}"),
			`account "g2": balance members max_inflight differs between routes p1 (0) and p2 (1)`},
		{"duplicate name with pool", pool("p1", "{g1: {}, g2: {}}") + "  - {name: p1, models: [other], interactive: [g1]}\n",
			`route name "p1" is not unique`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, poolAccounts+"\nroutes:\n"+tc.routes))
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// Without a balance block, duplicate route names stay accepted.
	if _, err := Load(write(t, poolAccounts+"\nroutes:\n  - {name: r, models: [a], interactive: [g1]}\n  - {name: r, models: [b], interactive: [g2]}\n")); err != nil {
		t.Fatalf("unpooled duplicate names: %v", err)
	}
}
