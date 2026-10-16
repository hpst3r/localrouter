package discovery

import (
	"context"
	"path"
	"slices"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Resolver resolves model ids that no route lists statically against one
// generation's discovering routes and the Manager's shared listings. It is
// immutable apart from the listings it reads and safe for concurrent use.
type Resolver struct {
	m      *Manager
	routes []core.Route // discovering routes, config order
	static map[string]bool
}

// Resolver returns the resolver for one generation's routes, or nil when no
// route discovers (callers then keep the static-only path unchanged).
func (m *Manager) Resolver(routes []core.Route) *Resolver {
	r := &Resolver{m: m, static: map[string]bool{}}
	for _, rt := range routes {
		for _, s := range rt.Models {
			r.static[s] = true
		}
		if rt.DiscoverModels {
			r.routes = append(r.routes, rt)
		}
	}
	if len(r.routes) == 0 {
		return nil
	}
	return r
}

// Resolve returns the discovering route that serves model: the only one whose
// discover_match admits model and of whose candidates at least one currently
// lists it, with Interactive and Background narrowed to the listing accounts
// (order preserved; either may be empty). If two or more discovering routes
// claim model, no route serves it and ambiguous is true. A statically listed
// model never resolves here: static routes win and are looked up by the
// caller first.
func (r *Resolver) Resolve(model string) (route core.Route, ok, ambiguous bool) {
	if r == nil || r.static[model] {
		return core.Route{}, false, false
	}
	r.m.mu.RLock()
	defer r.m.mu.RUnlock()
	for _, rt := range r.routes {
		if !matches(rt.DiscoverMatch, model) {
			continue
		}
		in, bg := r.narrow(rt.Interactive, model), r.narrow(rt.Background, model)
		if len(in) == 0 && len(bg) == 0 {
			continue
		}
		if ok {
			return core.Route{}, false, true
		}
		route, ok = rt, true
		route.Interactive, route.Background = in, bg
	}
	return route, ok, false
}

// narrow keeps the ids whose listing contains model. Caller holds m.mu.
func (r *Resolver) narrow(ids []string, model string) []string {
	var out []string
	for _, id := range ids {
		if e := r.m.targets[id]; e != nil {
			if _, ok := e.ids[model]; ok {
				out = append(out, id)
			}
		}
	}
	return out
}

// claims maps every non-static discovered id to the indexes (into r.routes,
// ascending) of the discovering routes whose discover_match admits it and
// whose candidates list it. Caller holds m.mu.
func (r *Resolver) claims() map[string][]int {
	out := map[string][]int{}
	for i, rt := range r.routes {
		for _, id := range candidates(rt) {
			e := r.m.targets[id]
			if e == nil {
				continue
			}
			for _, s := range e.sorted {
				if r.static[s] || !matches(rt.DiscoverMatch, s) {
					continue
				}
				if c := out[s]; len(c) == 0 || c[len(c)-1] != i {
					out[s] = append(c, i)
				}
			}
		}
	}
	return out
}

// Models lists every currently resolvable discovered id (after discover_match,
// excluding static models and ids claimed by more than one discovering
// route), deduplicated and sorted.
func (r *Resolver) Models() []string {
	if r == nil {
		return nil
	}
	r.m.mu.RLock()
	claims := r.claims()
	r.m.mu.RUnlock()
	var out []string
	for s, routes := range claims {
		if len(routes) == 1 {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// Refresh triggers the manager's on-demand refresh (see Manager.Refresh) for
// a request naming model, and returns as soon as model resolves. It does
// nothing when no discovering route's discover_match admits model, since no
// listing could make it resolve.
func (r *Resolver) Refresh(ctx context.Context, model string) {
	if r == nil || r.static[model] || !slices.ContainsFunc(r.routes, func(rt core.Route) bool {
		return matches(rt.DiscoverMatch, model)
	}) {
		return
	}
	r.m.Refresh(ctx, func() bool {
		_, ok, _ := r.Resolve(model)
		return ok
	})
}

// Diagnostics is the diagnostics "discovery" block. It never carries URLs,
// keys or upstream bodies.
type Diagnostics struct {
	Accounts []AccountDiagnostics `json:"accounts"`
	Routes   []RouteDiagnostics   `json:"routes"`
}

// AccountDiagnostics describes one polled account. Times are UTC and null
// until the first attempt/success; ModelCount is the full listing size before
// any discover_match filtering; ErrorClass is the last attempt's failure
// class, omitted when it succeeded.
type AccountDiagnostics struct {
	Account       string     `json:"account"`
	Routes        []string   `json:"routes"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastAttemptAt *time.Time `json:"last_attempt_at"`
	ModelCount    int        `json:"model_count"`
	Dropped       int        `json:"dropped"`
	ErrorClass    string     `json:"error_class,omitempty"`
}

// RouteDiagnostics lists the discovered ids a route serves and, under
// Ambiguous, the ids it claims together with at least one other discovering
// route, which no route serves.
type RouteDiagnostics struct {
	Route     string   `json:"route"`
	Models    []string `json:"models"`
	Ambiguous []string `json:"ambiguous,omitempty"`
}

// Snapshot builds the diagnostics block for this generation.
func (r *Resolver) Snapshot() Diagnostics {
	d := Diagnostics{Accounts: []AccountDiagnostics{}, Routes: []RouteDiagnostics{}}
	if r == nil {
		return d
	}
	r.m.mu.RLock()
	defer r.m.mu.RUnlock()
	for _, rt := range r.routes {
		d.Routes = append(d.Routes, RouteDiagnostics{Route: rt.Name, Models: []string{}})
	}
	for s, routes := range r.claims() {
		for _, i := range routes {
			rd := &d.Routes[i]
			if len(routes) == 1 {
				rd.Models = append(rd.Models, s)
			} else {
				rd.Ambiguous = append(rd.Ambiguous, s)
			}
		}
	}
	for i := range d.Routes {
		slices.Sort(d.Routes[i].Models)
		slices.Sort(d.Routes[i].Ambiguous)
	}
	byAccount := map[string][]string{}
	var order []string
	for _, rt := range r.routes {
		for _, id := range candidates(rt) {
			if _, ok := byAccount[id]; !ok {
				order = append(order, id)
			}
			byAccount[id] = append(byAccount[id], rt.Name)
		}
	}
	slices.Sort(order)
	for _, id := range order {
		ad := AccountDiagnostics{Account: id, Routes: byAccount[id]}
		if e := r.m.targets[id]; e != nil {
			ad.LastSuccessAt, ad.LastAttemptAt = utcPtr(e.lastSuccess), utcPtr(e.lastAttempt)
			ad.ModelCount, ad.Dropped, ad.ErrorClass = len(e.sorted), e.dropped, e.errClass
		}
		d.Accounts = append(d.Accounts, ad)
	}
	return d
}

// Diagnostics returns Snapshot as the value control encodes verbatim.
func (r *Resolver) Diagnostics() any { return r.Snapshot() }

func utcPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// matches reports whether id passes the globs (none = all). Patterns were
// validated at config load, so a match error is treated as no match. Globs use
// path.Match, so "*" does not cross "/": "unsloth/*" or "*/*", not "*".
func matches(globs []string, id string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if ok, err := path.Match(g, id); err == nil && ok {
			return true
		}
	}
	return false
}
