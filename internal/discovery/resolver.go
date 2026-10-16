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

// Resolve returns the first discovering route (config order) whose
// discover_match admits model and of whose candidates at least one currently
// lists it, with Interactive and Background narrowed to the listing accounts
// (order preserved). A statically listed model never resolves here: static
// routes win and are looked up by the caller first.
func (r *Resolver) Resolve(model string) (core.Route, bool) {
	if r == nil || r.static[model] {
		return core.Route{}, false
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
		rt.Interactive, rt.Background = in, bg
		return rt, true
	}
	return core.Route{}, false
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

// Models lists every currently resolvable discovered id (after discover_match,
// excluding static models), deduplicated and sorted.
func (r *Resolver) Models() []string {
	if r == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	r.m.mu.RLock()
	for _, rt := range r.routes {
		for _, id := range candidates(rt) {
			e := r.m.targets[id]
			if e == nil {
				continue
			}
			for _, s := range e.sorted {
				if !seen[s] && !r.static[s] && matches(rt.DiscoverMatch, s) {
					seen[s] = true
					out = append(out, s)
				}
			}
		}
	}
	r.m.mu.RUnlock()
	slices.Sort(out)
	return out
}

// Refresh triggers the manager's on-demand refresh (see Manager.Refresh).
func (r *Resolver) Refresh(ctx context.Context) {
	if r != nil {
		r.m.Refresh(ctx)
	}
}

// Diagnostics is the diagnostics "discovery" block. It never carries URLs,
// keys or upstream bodies.
type Diagnostics struct {
	Accounts []AccountDiagnostics `json:"accounts"`
	Routes   []RouteDiagnostics   `json:"routes"`
}

// AccountDiagnostics describes one polled account. Times are UTC and null
// until the first attempt/success; ErrorClass is the last attempt's failure
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
// Shadowed, the ids its candidates list that a static entry or an earlier
// discovering route serves instead.
type RouteDiagnostics struct {
	Route    string   `json:"route"`
	Models   []string `json:"models"`
	Shadowed []string `json:"shadowed,omitempty"`
}

// Snapshot builds the diagnostics block for this generation.
func (r *Resolver) Snapshot() Diagnostics {
	d := Diagnostics{Accounts: []AccountDiagnostics{}, Routes: []RouteDiagnostics{}}
	if r == nil {
		return d
	}
	byAccount := map[string][]string{}
	var order []string
	claimed := map[string]bool{}
	for s := range r.static {
		claimed[s] = true
	}
	r.m.mu.RLock()
	defer r.m.mu.RUnlock()
	for _, rt := range r.routes {
		rd := RouteDiagnostics{Route: rt.Name, Models: []string{}}
		listed := map[string]bool{}
		for _, id := range candidates(rt) {
			if _, ok := byAccount[id]; !ok {
				order = append(order, id)
			}
			byAccount[id] = append(byAccount[id], rt.Name)
			if e := r.m.targets[id]; e != nil {
				for _, s := range e.sorted {
					if matches(rt.DiscoverMatch, s) {
						listed[s] = true
					}
				}
			}
		}
		ids := make([]string, 0, len(listed))
		for s := range listed {
			ids = append(ids, s)
		}
		slices.Sort(ids)
		for _, s := range ids {
			if claimed[s] {
				rd.Shadowed = append(rd.Shadowed, s)
				continue
			}
			claimed[s] = true
			rd.Models = append(rd.Models, s)
		}
		d.Routes = append(d.Routes, rd)
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
// validated at config load, so a match error is treated as no match.
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
