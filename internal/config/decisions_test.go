package config

import (
	"reflect"
	"strings"
	"testing"
)

// decAccounts: two openrouter accounts (default base_url), one openrouter
// account whose base_url path does not end in /v1, and one openai_compat.
const decAccounts = `
clients: [{name: a, class: interactive, key_file: k}]
accounts:
  - {id: or1, provider: openrouter, api_key_env: K1}
  - {id: or2, provider: openrouter, api_key_env: K2}
  - {id: orx, provider: openrouter, base_url: https://proxy.example/openrouter, api_key_env: K3}
  - {id: oc, provider: openai_compat, base_url: http://oc/v1, api_key_env: K4}
`

func decRoute(proto, model1, model2, extra string) string {
	return `
routes:
  - name: jev
    models: [jev]
    interactive: [or1, or2]
    upstreams:
      or1:
        upstream_model: ` + model1 + `
        protocols: [` + proto + `]
        input_modalities: [text]
        question_types: [noul, choice, score]
` + extra + `
      or2:
        upstream_model: ` + model2 + `
        protocols: [` + proto + `]
        input_modalities: [text]
        question_types: [noul]
` + extra + `
`
}

func TestT2DecisionConfigValid(t *testing.T) {
	for _, proto := range []string{"systemone", "decisions"} {
		c, err := Load(write(t, decAccounts+decRoute(proto, "typesafe/jev-1.13", "typesafe/jev-1.13", "")))
		if err != nil {
			t.Fatalf("%s route rejected: %v", proto, err)
		}
		up := c.CoreRoutes()[0].Upstreams["or1"]
		if !reflect.DeepEqual(up.QuestionTypes, []string{"noul", "choice", "score"}) || up.Protocols[0] != proto || up.AllowAlias {
			t.Fatalf("projection = %#v", up)
		}
	}
	// Bare TypeSafe ids are pins too; aliases load with allow_alias everywhere.
	if _, err := Load(write(t, decAccounts+decRoute("systemone", "jev-1.13", "jev-1.13", ""))); err != nil {
		t.Fatalf("bare pin rejected: %v", err)
	}
	c, err := Load(write(t, decAccounts+decRoute("decisions", "~typesafe/jev-latest", "~typesafe/jev-latest", "        allow_alias: true")))
	if err != nil {
		t.Fatalf("allowed alias rejected: %v", err)
	}
	if !c.CoreRoutes()[0].Upstreams["or2"].AllowAlias {
		t.Fatal("allow_alias not projected")
	}
	// Route-level upstream_model is the shared pin when descriptors omit it.
	body := decAccounts + `
routes:
  - name: jev
    models: [jev]
    upstream_model: typesafe/jev-1.14
    interactive: [or1]
    upstreams:
      or1: {protocols: [systemone], input_modalities: [text], question_types: [choice]}
`
	if _, err := Load(write(t, body)); err != nil {
		t.Fatalf("route-level pin rejected: %v", err)
	}
	// Defaults for the decisions block, and explicit values survive.
	c, err = Load(write(t, decAccounts+"decisions: {max_questions: 8}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := DecisionsConfig{MaxQuestions: 8, MaxQuestionNameBytes: 128, MaxChoiceOptions: 255, MaxScoreLevels: 10, MaxNesting: 64}
	if c.Decisions != want {
		t.Fatalf("decisions = %#v", c.Decisions)
	}
}

func TestT2DecisionConfigRejected(t *testing.T) {
	route := func(upstreams string) string {
		return decAccounts + `
routes:
  - name: jev
    models: [jev]
    interactive: [or1, or2]
    upstreams:
` + upstreams
	}
	desc := func(id, body string) string { return "      " + id + ": " + body + "\n" }
	ok := `{upstream_model: typesafe/jev-1.13, protocols: [systemone], input_modalities: [text], question_types: [noul]}`
	cases := map[string]struct{ cfg, want string }{
		"mixed chat and systemone route": {route(desc("or1", ok) +
			desc("or2", `{upstream_model: typesafe/jev-1.13, protocols: [chat], input_modalities: [text]}`)), "single-family"},
		"systemone plus chat on one descriptor": {route(desc("or1", ok) +
			desc("or2", `{upstream_model: typesafe/jev-1.13, protocols: [systemone, chat], input_modalities: [text], question_types: [noul]}`)), "single-family"},
		"mixed decision families": {route(desc("or1", ok) +
			desc("or2", `{upstream_model: typesafe/jev-1.13, protocols: [decisions], input_modalities: [text], question_types: [noul]}`)), "mixes decision families"},
		"box question type": {route(desc("or1", ok) +
			desc("or2", `{upstream_model: typesafe/jev-1.13, protocols: [systemone], input_modalities: [text], question_types: [box]}`)), "invalid question type"},
		"unknown question type": {route(desc("or1", ok) +
			desc("or2", `{upstream_model: typesafe/jev-1.13, protocols: [systemone], input_modalities: [text], question_types: [rank]}`)), "invalid question type"},
		"missing question types": {route(desc("or1", ok) +
			desc("or2", `{upstream_model: typesafe/jev-1.13, protocols: [systemone], input_modalities: [text]}`)), "question_types must be a non-empty"},
		"duplicate question type": {route(desc("or1", ok) +
			desc("or2", `{upstream_model: typesafe/jev-1.13, protocols: [systemone], input_modalities: [text], question_types: [noul, noul]}`)), "duplicate question type"},
		"image modality": {route(desc("or1", ok) +
			desc("or2", `{upstream_model: typesafe/jev-1.13, protocols: [systemone], input_modalities: [text, image], question_types: [noul]}`)), "input_modalities must be [text]"},
		"stream flag": {route(desc("or1", ok) +
			desc("or2", `{upstream_model: typesafe/jev-1.13, protocols: [systemone], input_modalities: [text], question_types: [noul], stream: true}`)), "do not apply to a decision descriptor"},
		"mismatched pins": {route(desc("or1", ok) +
			desc("or2", `{upstream_model: typesafe/jev-1.14, protocols: [systemone], input_modalities: [text], question_types: [noul]}`)), "share one upstream_model"},
		"no pin": {route(desc("or1", `{protocols: [systemone], input_modalities: [text], question_types: [noul]}`) +
			desc("or2", `{protocols: [systemone], input_modalities: [text], question_types: [noul]}`)), "requires an upstream_model"},
		"missing descriptor": {route(desc("or1", ok)), "missing a descriptor"},
		"question_types on chat descriptor": {decAccounts + `
routes:
  - name: c
    models: [c]
    interactive: [oc]
    upstreams:
      oc: {protocols: [chat], input_modalities: [text], question_types: [noul]}
`, "apply only to decision descriptors"},
		"allow_alias on chat descriptor": {decAccounts + `
routes:
  - name: c
    models: [c]
    interactive: [oc]
    upstreams:
      oc: {protocols: [chat], input_modalities: [text], allow_alias: true}
`, "apply only to decision descriptors"},
		"systemone on openai_compat": {decAccounts + `
routes:
  - name: c
    models: [c]
    interactive: [oc]
    upstreams:
      oc: {upstream_model: jev-1.13, protocols: [systemone], input_modalities: [text], question_types: [noul]}
`, "only by openrouter accounts"},
		"decisions on non-/v1 base_url": {decAccounts + `
routes:
  - name: c
    models: [c]
    interactive: [orx]
    upstreams:
      orx: {upstream_model: typesafe/jev-1.13, protocols: [decisions], input_modalities: [text], question_types: [noul]}
`, "must end in /v1"},
		"negative bound": {decAccounts + "decisions: {max_nesting: -1}\n", "decisions.max_nesting must not be negative"},
	}
	for name, c := range cases {
		_, err := Load(write(t, c.cfg))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
	}
	// A /v1-less base_url is fine for systemone (it appends /systemone).
	body := decAccounts + `
routes:
  - name: c
    models: [c]
    interactive: [orx]
    upstreams:
      orx: {upstream_model: typesafe/jev-1.13, protocols: [systemone], input_modalities: [text], question_types: [noul]}
`
	if _, err := Load(write(t, body)); err != nil {
		t.Fatalf("systemone on non-/v1 base_url: %v", err)
	}
}

// TestT2MovingAliasRequiresAllowAlias covers design F2a: ~ aliases and
// -latest/-preview ids need allow_alias on every candidate.
func TestT2MovingAliasRequiresAllowAlias(t *testing.T) {
	for _, m := range []string{"~typesafe/jev-latest", "jev-latest", "typesafe/jev-preview", "JEV-LATEST"} {
		_, err := Load(write(t, decAccounts+decRoute("systemone", m, m, "")))
		if err == nil || !strings.Contains(err.Error(), "moving alias") {
			t.Errorf("%s without allow_alias: err = %v", m, err)
		}
	}
	// allow_alias on only one candidate is not enough.
	body := decAccounts + `
routes:
  - name: jev
    models: [jev]
    interactive: [or1, or2]
    upstreams:
      or1: {upstream_model: jev-latest, allow_alias: true, protocols: [systemone], input_modalities: [text], question_types: [noul]}
      or2: {upstream_model: jev-latest, protocols: [systemone], input_modalities: [text], question_types: [noul]}
`
	if _, err := Load(write(t, body)); err == nil || !strings.Contains(err.Error(), "moving alias") {
		t.Fatalf("partial allow_alias: %v", err)
	}
	for _, m := range []string{"typesafe/jev-1.14", "jev-1.13", "openai/gpt-6-luna-decisions"} {
		if isMovingAlias(m) {
			t.Errorf("%s reported as alias", m)
		}
	}
}
