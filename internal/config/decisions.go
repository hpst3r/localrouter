package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/hpst3r/localrouter/internal/core"
)

// Typed-decision vocabulary (see docs/SPEC.md "Typed decisions"). Matched as
// plain strings by routing and proxy, like the capability vocabulary.
const (
	protocolSystemOne = "systemone"
	protocolDecisions = "decisions"
)

// decisionQuestionTypes are the question types a v1 decision descriptor may
// declare. box (image localization) is deliberately absent.
var decisionQuestionTypes = map[string]bool{"noul": true, "choice": true, "score": true}

// Decision bound defaults (docs/SPEC.md "Typed decisions", Bounds).
const (
	defaultMaxQuestions         = 64
	defaultMaxQuestionNameBytes = 128
	defaultMaxChoiceOptions     = 255
	defaultMaxScoreLevels       = 10
	defaultMaxNesting           = 64
)

func (d *DecisionsConfig) applyDefaults() {
	for _, f := range []struct {
		v   *int
		def int
	}{
		{&d.MaxQuestions, defaultMaxQuestions},
		{&d.MaxQuestionNameBytes, defaultMaxQuestionNameBytes},
		{&d.MaxChoiceOptions, defaultMaxChoiceOptions},
		{&d.MaxScoreLevels, defaultMaxScoreLevels},
		{&d.MaxNesting, defaultMaxNesting},
	} {
		if *f.v == 0 {
			*f.v = f.def
		}
	}
}

func (d DecisionsConfig) validate() []error {
	var errs []error
	for _, f := range []struct {
		name string
		v    int
	}{
		{"max_questions", d.MaxQuestions},
		{"max_question_name_bytes", d.MaxQuestionNameBytes},
		{"max_choice_options", d.MaxChoiceOptions},
		{"max_score_levels", d.MaxScoreLevels},
		{"max_nesting", d.MaxNesting},
	} {
		if f.v < 0 {
			errs = append(errs, fmt.Errorf("decisions.%s must not be negative (0 = default)", f.name))
		}
	}
	return errs
}

// isDecisionProtocol reports whether p names a typed-decision family.
func isDecisionProtocol(p string) bool {
	p = strings.TrimSpace(p)
	return p == protocolSystemOne || p == protocolDecisions
}

// decisionRoute reports whether any descriptor of r declares a decision
// protocol; such a route is validated by validateDecisionRoute instead of
// validateUpstreams.
func decisionRoute(r RouteConfig) bool {
	for _, s := range r.Upstreams {
		for _, p := range s.Protocols {
			if isDecisionProtocol(p) {
				return true
			}
		}
	}
	return false
}

// isMovingAlias reports whether a decision model id is a moving alias rather
// than a pin: an OpenRouter "~" alias, or an id ending in -latest or -preview
// (case-insensitive). typesafe/jev-1.14 and jev-1.13 are pins.
func isMovingAlias(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(m, "~") || strings.HasSuffix(m, "-latest") || strings.HasSuffix(m, "-preview")
}

// validateDecisionRoute enforces the typed-decision route contract: the route
// is single-family (every candidate declares exactly the same one decision
// protocol), every candidate is an openrouter account, descriptors are
// text-only with a non-empty question_types subset of {noul, choice, score}
// and no chat feature flags, all candidates share one non-empty pinned
// upstream model, a moving alias needs allow_alias on every candidate, and a
// decisions-family account's base_url path ends in /v1 (so /alpha/decisions
// can be derived from it).
func validateDecisionRoute(r RouteConfig, provider, baseURL map[string]string) []error {
	var errs []error
	cands := map[string]bool{}
	for _, id := range append(append([]string{}, r.Interactive...), r.Background...) {
		cands[id] = true
	}
	for id := range r.Upstreams {
		if !cands[id] {
			errs = append(errs, fmt.Errorf("route %s: upstreams names unknown candidate account %q", r.Name, id))
		}
	}
	for id := range cands {
		if _, ok := r.Upstreams[id]; !ok {
			errs = append(errs, fmt.Errorf("route %s: upstreams is missing a descriptor for candidate %q", r.Name, id))
		}
	}
	family := ""
	pin, pinSet := "", false
	aliasAllowed := true
	for _, id := range sortedKeys(r.Upstreams) {
		spec := r.Upstreams[id]
		pfx := fmt.Sprintf("route %s: upstreams[%s]", r.Name, id)
		if len(spec.Protocols) != 1 || !isDecisionProtocol(spec.Protocols[0]) {
			errs = append(errs, fmt.Errorf("%s: a decision route is single-family: protocols must be exactly [systemone] or [decisions] on every candidate", pfx))
		} else {
			p := strings.TrimSpace(spec.Protocols[0])
			if family == "" {
				family = p
			} else if p != family {
				errs = append(errs, fmt.Errorf("%s: route mixes decision families %s and %s", pfx, family, p))
			}
			if p == protocolDecisions && provider[id] == core.ProviderOpenRouter && !decisionsBaseURLOK(baseURL[id]) {
				errs = append(errs, fmt.Errorf("%s: account base_url path must end in /v1 to derive the /alpha/decisions endpoint", pfx))
			}
		}
		if provider[id] != core.ProviderOpenRouter {
			errs = append(errs, fmt.Errorf("%s: decision protocols are served only by openrouter accounts", pfx))
		}
		if len(spec.InputModalities) != 1 || strings.TrimSpace(spec.InputModalities[0]) != modalityText {
			errs = append(errs, fmt.Errorf("%s: input_modalities must be [text] on a decision descriptor", pfx))
		}
		if spec.Tools || spec.JSONSchema || spec.Stream {
			errs = append(errs, fmt.Errorf("%s: tools, json_schema and stream do not apply to a decision descriptor", pfx))
		}
		if len(spec.QuestionTypes) == 0 {
			errs = append(errs, fmt.Errorf("%s: question_types must be a non-empty subset of noul,choice,score", pfx))
		}
		seen := map[string]bool{}
		for _, q := range spec.QuestionTypes {
			q = strings.TrimSpace(q)
			switch {
			case !decisionQuestionTypes[q]:
				errs = append(errs, fmt.Errorf("%s: invalid question type %q (want noul, choice or score)", pfx, q))
			case seen[q]:
				errs = append(errs, fmt.Errorf("%s: duplicate question type %q", pfx, q))
			}
			seen[q] = true
		}
		m := strings.TrimSpace(spec.UpstreamModel)
		if m == "" {
			m = strings.TrimSpace(r.UpstreamModel)
		}
		if !pinSet {
			pin, pinSet = m, true
		} else if m != pin {
			errs = append(errs, fmt.Errorf("route %s: every decision candidate must share one upstream_model (%q vs %q)", r.Name, pin, m))
		}
		aliasAllowed = aliasAllowed && spec.AllowAlias
	}
	if pinSet && pin == "" {
		errs = append(errs, fmt.Errorf("route %s: a decision route requires an upstream_model (the pinned decision model)", r.Name))
	}
	if pin != "" && isMovingAlias(pin) && !aliasAllowed {
		errs = append(errs, fmt.Errorf("route %s: upstream_model %q is a moving alias; pin a version or set allow_alias: true on every candidate", r.Name, pin))
	}
	return errs
}

// decisionsBaseURLOK reports whether base's path ends in the /v1 segment the
// decisions endpoint replaces with /alpha/decisions.
func decisionsBaseURLOK(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Path == "/v1" || strings.HasSuffix(u.Path, "/v1")
}

func sortedKeys(m map[string]UpstreamSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out) // deterministic error order
	return out
}
