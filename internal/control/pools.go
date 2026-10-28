package control

import (
	"net/http"

	"github.com/hpst3r/localrouter/internal/core"
)

// balancedDryRunner is implemented by policies that pool routes
// (internal/policy.Policy). POST /control/v1/admit uses it for a route with a
// balance block, so the dry run reports the member AcquireBalanced would pick
// without creating a lease, an affinity entry or a breaker probe.
type balancedDryRunner interface {
	DryRunBalanced(class core.Class, candidates []string, route core.Route, session string) core.Decision
}

// affinitySource reports a pooled route's live session bindings for
// diagnostics.
type affinitySource interface {
	AffinityEntries(route string) int
}

// dryRun evaluates candidates for an admit request: the pooled dry run for a
// route with a balance block when the policy supports it, else DryRun. The
// session label is the request's X-LocalRouter-Session header, as on the
// inference path.
func (s *Server) dryRun(r *http.Request, class core.Class, candidates []string, route *core.Route) core.Decision {
	if route != nil && route.BalanceStrategy != "" {
		if bp, ok := s.deps.Policy.(balancedDryRunner); ok {
			return bp.DryRunBalanced(class, candidates, *route, core.TruncateLabel(r.Header.Get("X-LocalRouter-Session")))
		}
	}
	return s.deps.Policy.DryRun(class, candidates)
}

// diagPool is one pooled route in the diagnostics document. It carries
// configured names and counters only, never URLs or keys.
type diagPool struct {
	Route         string           `json:"route"`
	Strategy      string           `json:"strategy"`
	Identity      string           `json:"identity"`
	StickyTTLS    int64            `json:"sticky_ttl_s"`
	StickyEntries int              `json:"sticky_entries"`
	Members       []diagPoolMember `json:"members"`
}

type diagPoolMember struct {
	Account     string `json:"account"`
	Weight      int    `json:"weight"`
	MaxInflight int    `json:"max_inflight"`
	Inflight    int    `json:"inflight"`
	// Breaker is the member's breaker state; omitted when the breaker is
	// disabled.
	Breaker string `json:"breaker,omitempty"`
}

// pools builds the diagnostics pools block: nil (omitted) unless some route
// has a balance block. Members are listed in candidate order (interactive,
// then background-only members).
func (s *Server) pools() []diagPool {
	var out []diagPool
	for _, rt := range s.deps.Routes {
		if rt.BalanceStrategy == "" {
			continue
		}
		p := diagPool{Route: rt.Name, Strategy: rt.BalanceStrategy, Identity: rt.BalanceIdentity,
			StickyTTLS: int64(rt.BalanceStickyTTL.Seconds()), Members: []diagPoolMember{}}
		if as, ok := s.deps.Policy.(affinitySource); ok {
			p.StickyEntries = as.AffinityEntries(rt.Name)
		}
		seen := map[string]bool{}
		for _, id := range append(append([]string(nil), rt.Interactive...), rt.Background...) {
			if seen[id] {
				continue
			}
			seen[id] = true
			m := diagPoolMember{Account: id, Weight: rt.BalanceWeight[id], MaxInflight: rt.BalanceMaxInflight[id]}
			if s.deps.Policy != nil {
				m.Inflight = s.deps.Policy.Status(id).Inflight
			}
			if b := s.breakerView(id); b != nil {
				m.Breaker = b.State
			}
			p.Members = append(p.Members, m)
		}
		out = append(out, p)
	}
	return out
}
