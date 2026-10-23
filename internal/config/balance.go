package config

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hpst3r/localrouter/internal/core"
)

// Pooled-backend vocabulary and bounds (routes[].balance).
const (
	balanceLeastInflight = "least_inflight"
	balanceMaxWeight     = 100
	balanceMaxIdentity   = 128
	balanceMinStickyTTL  = 30 * time.Second
	balanceMaxStickyTTL  = 24 * time.Hour
)

// BalanceConfig pools a route's candidates (routes[].balance): the keys of
// Members must equal interactive ∪ background, every member must be a
// self-hosted replica of the same model, and the policy picks the member with
// the fewest weighted in-flight requests instead of the first admissible one.
type BalanceConfig struct {
	// Strategy is required; the only value is "least_inflight".
	Strategy string `yaml:"strategy"`
	// Identity is the operator's declaration of the pooled model and quant
	// (required, printable, at most 128 bytes after trimming).
	Identity string `yaml:"identity"`
	// Verify is reserved for identity verification, which is not supported
	// yet: any value is rejected so adding it later is not a silent change.
	Verify *string `yaml:"verify"`
	// StickyTTL enables X-LocalRouter-Session affinity; 0/absent disables it,
	// otherwise it must be in [30s, 24h].
	StickyTTL Duration `yaml:"sticky_ttl"`
	// Members maps every candidate account id to its pool parameters.
	Members map[string]BalanceMember `yaml:"members"`
}

// BalanceMember is one pool member's parameters.
type BalanceMember struct {
	// Weight is an integer in [1, 100]; absent means 1.
	Weight *int `yaml:"weight"`
	// MaxInflight caps the member account's open leases on every route
	// (llama-server -np); 0 or absent means uncapped. Every balance block
	// listing the account must give the same value.
	MaxInflight int `yaml:"max_inflight"`
}

// weight returns the member's effective weight.
func (m BalanceMember) weight() int {
	if m.Weight == nil {
		return 1
	}
	return *m.Weight
}

// validateBalance enforces the pooled-backend rules for one route. A route
// without a balance block is skipped entirely.
func validateBalance(r RouteConfig, accounts []AccountConfig) []error {
	b := r.Balance
	if b == nil {
		return nil
	}
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("route %s: balance: "+format, append([]any{r.Name}, a...)...))
	}
	if b.Strategy != balanceLeastInflight {
		add("strategy %q must be %s", b.Strategy, balanceLeastInflight)
	}
	id := strings.TrimSpace(b.Identity)
	switch {
	case id == "":
		add("identity is required")
	case len(id) > balanceMaxIdentity:
		add("identity must be at most %d bytes", balanceMaxIdentity)
	case !printable(id):
		add("identity must be printable UTF-8")
	}
	if b.Verify != nil {
		add("verify is not supported yet (identity verification is a later slice); remove the key")
	}
	if ttl := b.StickyTTL.D(); ttl != 0 && (ttl < balanceMinStickyTTL || ttl > balanceMaxStickyTTL) {
		add("sticky_ttl %v must be 0 or in [30s, 24h]", ttl)
	}

	// Rule 1: members == interactive ∪ background, in candidate order.
	var cands []string
	for _, id := range append(slices.Clone(r.Interactive), r.Background...) {
		if !slices.Contains(cands, id) {
			cands = append(cands, id)
		}
	}
	for _, id := range cands {
		if _, ok := b.Members[id]; !ok {
			add("members is missing candidate %q (members must equal interactive ∪ background)", id)
		}
	}
	for _, id := range sortedKeys(b.Members) {
		m := b.Members[id]
		if !slices.Contains(cands, id) {
			add("members names %q, which is not a route candidate (members must equal interactive ∪ background)", id)
		}
		if w := m.weight(); w < 1 || w > balanceMaxWeight {
			add("members[%s].weight %d must be in [1, %d]", id, w, balanceMaxWeight)
		}
		if m.MaxInflight < 0 {
			add("members[%s].max_inflight %d must not be negative (0 = uncapped)", id, m.MaxInflight)
		}
	}

	// Rule 2: one self-hosted provider and one cost basis across members.
	byID := map[string]AccountConfig{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	var first *AccountConfig
	for _, id := range cands {
		a, ok := byID[id]
		if !ok {
			continue // unknown accounts are reported by Validate
		}
		if a.Provider != core.ProviderOpenAICompat && a.Provider != core.ProviderOllama {
			add("member %q has provider %s; pools accept only openai_compat or ollama accounts", id, a.Provider)
			continue
		}
		switch {
		case first == nil:
			first = &a
		case a.Provider != first.Provider:
			add("members must share one provider (%q is %s, %q is %s)", first.ID, first.Provider, id, a.Provider)
		case a.CostBasis != first.CostBasis:
			add("members must share one cost_basis (%q is %s, %q is %s)", first.ID, first.CostBasis, id, a.CostBasis)
		}
	}

	// Rule 3: identical capability descriptors (upstream_model may differ).
	if len(r.Upstreams) > 0 {
		var ref string
		for _, id := range cands {
			spec, ok := r.Upstreams[id]
			if !ok {
				continue // reported by validateUpstreams
			}
			if ref == "" {
				ref = id
				continue
			}
			if !sameCapabilities(r.Upstreams[ref], spec) {
				add("members %q and %q must declare identical upstreams capabilities (protocols, input_modalities, tools, json_schema, stream); only upstream_model may differ", ref, id)
			}
		}
	}
	return errs
}

// validateBalanceRoutes enforces the cross-route pooled-backend rules. With no
// balance block anywhere it reports nothing. Otherwise route names must be
// unique (affinity bindings and diagnostics are keyed by name), and an account
// whose max_inflight is non-zero in any balance block must have the same value
// in every balance block that lists it: the cap belongs to the server
// (llama-server -np) and is enforced on every route.
func validateBalanceRoutes(routes []RouteConfig) []error {
	if !slices.ContainsFunc(routes, func(r RouteConfig) bool { return r.Balance != nil }) {
		return nil
	}
	var errs []error
	names := map[string]bool{}
	for _, r := range routes {
		if r.Name != "" && names[r.Name] {
			errs = append(errs, fmt.Errorf("route name %q is not unique (required when any route has a balance block)", r.Name))
		}
		names[r.Name] = true
	}
	type capAt struct {
		route string
		n     int
	}
	first := map[string]capAt{}
	for _, r := range routes {
		if r.Balance == nil {
			continue
		}
		for _, id := range sortedKeys(r.Balance.Members) {
			n := r.Balance.Members[id].MaxInflight
			prev, ok := first[id]
			if !ok {
				first[id] = capAt{r.Name, n}
				continue
			}
			if prev.n != n {
				errs = append(errs, fmt.Errorf("account %q: balance members max_inflight differs between routes %s (%d) and %s (%d); the cap is per account and must be the same in every balance block", id, prev.route, prev.n, r.Name, n))
			}
		}
	}
	return errs
}

// sameCapabilities compares two descriptors, protocols and modalities as sets.
func sameCapabilities(a, b UpstreamSpec) bool {
	return sameSet(a.Protocols, b.Protocols) && sameSet(a.InputModalities, b.InputModalities) &&
		a.Tools == b.Tools && a.JSONSchema == b.JSONSchema && a.Stream == b.Stream
}

func sameSet(a, b []string) bool {
	set := func(in []string) []string {
		out := copyTrimmed(in)
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(set(a), set(b))
}

func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// project copies a validated balance block onto its core route. A nil block
// leaves the route unpooled. Weights are resolved (absent = 1) and both maps
// are freshly allocated so the core route never aliases the config.
func (b *BalanceConfig) project(r *core.Route) {
	if b == nil {
		return
	}
	r.BalanceStrategy = b.Strategy
	r.BalanceIdentity = strings.TrimSpace(b.Identity)
	r.BalanceStickyTTL = b.StickyTTL.D()
	r.BalanceWeight = make(map[string]int, len(b.Members))
	r.BalanceMaxInflight = make(map[string]int, len(b.Members))
	for id, m := range b.Members {
		r.BalanceWeight[id] = m.weight()
		r.BalanceMaxInflight[id] = m.MaxInflight
	}
}
