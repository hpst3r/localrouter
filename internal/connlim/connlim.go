// Package connlim enforces limits on how many inference requests may be
// active at once, globally and per authenticated client. It is deliberately
// free of any knowledge of accounts, models or routing: it governs the
// inbound edge only, so it stays valid when admission policy and the worker
// pool change.
//
// A Controller is safe for concurrent use. The zero global limit and a zero
// or absent per-client limit both mean "unlimited", which keeps the feature
// backwards compatible with configurations written before it existed.
//
// For hot reload the Controller also hands out generation-scoped Views
// (Controller.View). A View carries one immutable concurrency-limit generation
// while sharing the Controller's live counters and leases, so a reload can
// adopt new limits without losing or double-counting in-flight requests.
//
// In multi-user mode a request is admitted for its authenticated principal
// (AcquirePrincipal) and additionally counted against its owning user, keyed by
// the opaque user id, so one user's limit spans all of their API keys and owned
// static clients. Per-user counters are shared runtime state like the others.
package connlim

import (
	"errors"
	"sort"
	"sync"

	"github.com/hpst3r/localrouter/internal/core"
)

// ErrNegativeLimit rejects a negative limit, which is always a mistake: it
// is neither a real cap nor the "unlimited" spelling of zero.
var ErrNegativeLimit = errors.New("connlim: limits must not be negative")

// Controller tracks active inference requests against a global limit and
// per-client limits. Create one with New; a nil Controller is also a valid
// no-op, so wiring may omit it.
//
// The counters (activeGlobal, active, peak) are the persistent runtime state
// shared by the Controller and every View derived from it; they are never
// rebuilt by a reload. mu is a pointer so the same lock guards all of them.
type Controller struct {
	mu *sync.Mutex
	// global/clients/perUser are this generation's configured limits.
	global  int
	clients map[string]int
	perUser int
	// activeGlobal counts every active request; active[c] counts one client
	// and activeUser[u] one user (across all of that user's credentials).
	activeGlobal int
	active       map[string]int
	activeUser   map[string]int
	peak         int
}

// New builds a Controller. A global limit of 0 means unlimited; clientLimits
// may be nil or empty. It returns ErrNegativeLimit for any negative value.
func New(global int, clientLimits map[string]int) (*Controller, error) {
	if err := validateLimits(global, clientLimits); err != nil {
		return nil, err
	}
	c := &Controller{mu: &sync.Mutex{}}
	c.mu.Lock()
	c.storeLocked(global, clientLimits)
	c.mu.Unlock()
	return c, nil
}

// Reconfigure atomically replaces the configured limits. Active requests are
// preserved: a request already admitted keeps its slot, and the new limits
// apply to subsequent admissions. This is the atomic-reload seam.
func (c *Controller) Reconfigure(global int, clientLimits map[string]int) error {
	if err := validateLimits(global, clientLimits); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(global, clientLimits)
	return nil
}

// View derives a generation-scoped view of the limits (one hot-reload
// generation) that shares this Controller's runtime state: the same lock and
// the same active/peak counters. A lease taken through any view is visible in
// every other view and is released exactly once against the shared counters.
//
// The View's limits are cloned and validated, so the caller may freely reuse
// or mutate clientLimits. A negative limit returns ErrNegativeLimit and
// mutates nothing. View never mutates the Controller's own configuration.
func (c *Controller) View(global int, clientLimits map[string]int) (*View, error) {
	return c.ViewWithUsers(global, clientLimits, 0)
}

// ViewWithUsers is View with a per-user limit for this generation: at most
// perUser requests admitted through AcquirePrincipal may be active at once for
// one user, counted across every view of the Controller. 0 means unlimited; a
// negative value returns ErrNegativeLimit.
func (c *Controller) ViewWithUsers(global int, clientLimits map[string]int, perUser int) (*View, error) {
	if err := validateLimits(global, clientLimits); err != nil {
		return nil, err
	}
	if perUser < 0 {
		return nil, ErrNegativeLimit
	}
	v := &View{c: c, global: global, perUser: perUser, clients: make(map[string]int, len(clientLimits))}
	for name, lim := range clientLimits {
		if lim > 0 {
			v.clients[name] = lim
		}
	}
	return v, nil
}

// Acquire reserves a slot for client, or reports false when the global limit
// or the client's limit is already saturated. The returned release function is
// idempotent and must be called exactly once per successful Acquire, however
// the request ends (completion, disconnect, upstream failure or failover).
func (c *Controller) Acquire(client string) (release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserveLocked(c.global, c.clients, 0, admission{client: client, countClient: true})
}

// AcquirePrincipal reserves a slot for an authenticated principal under the
// Controller's own limits, which carry no per-user limit; the user is still
// counted so views see it. See View.AcquirePrincipal for the rules.
func (c *Controller) AcquirePrincipal(p core.Principal) (release func(), ok bool) {
	a, valid := admissionFor(p)
	if !valid {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserveLocked(c.global, c.clients, c.perUser, a)
}

// UserActive reports how many requests of userID are active across every
// view. It is for diagnostics and tests; it never enumerates users.
func (c *Controller) UserActive(userID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeUser[userID]
}

// Stats reports current capacity and usage for authenticated diagnostics. It
// never includes client keys or request content.
func (c *Controller) Stats() core.InflightStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statsLocked(c.global, c.clients)
}

// storeLocked installs a validated generation's configuration. c.mu must be
// held (or the Controller not yet shared).
func (c *Controller) storeLocked(global int, clientLimits map[string]int) {
	c.global = global
	c.clients = make(map[string]int, len(clientLimits))
	for name, lim := range clientLimits {
		if lim > 0 {
			c.clients[name] = lim
		}
	}
}

// admission names the counters one request occupies: the client counter
// (static clients and legacy callers only) and the user counter (when the
// request has an owning user).
type admission struct {
	client      string
	countClient bool
	user        string
}

// admissionFor maps a principal onto its counters, reporting false for a
// principal that may not run inference or is malformed: sessions, unknown
// kinds, a static client without a name, or a user key without a well-formed
// owner and key id. A user key is counted per user, never in the per-client
// map, so its key id can never match (or show up as) a configured client.
func admissionFor(p core.Principal) (admission, bool) {
	switch p.Kind {
	case core.PrincipalStaticClient:
		if p.Client.Name == "" || (p.UserID != "" && !validOpaqueID(p.UserID)) {
			return admission{}, false
		}
		return admission{client: p.Client.Name, countClient: true, user: p.UserID}, true
	case core.PrincipalUserKey:
		if p.Role != core.RoleUser || !validOpaqueID(p.UserID) || !validOpaqueID(p.KeyID) {
			return admission{}, false
		}
		return admission{user: p.UserID}, true
	}
	return admission{}, false
}

// maxOpaqueID bounds an opaque user or key id.
const maxOpaqueID = 128

// validOpaqueID reports whether id is a well-formed opaque identifier: 1 to
// 128 bytes of ASCII letters, digits, '_' and '-'. Identity user and key ids
// ("u_…", "k_…") satisfy it; it is a shape check only and authenticates
// nothing.
func validOpaqueID(id string) bool {
	if id == "" || len(id) > maxOpaqueID {
		return false
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		if !('a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || b == '_' || b == '-') {
			return false
		}
	}
	return true
}

// reserveLocked admits one request under the given generation's limits and
// increments the shared counters: the global, client and user checks and
// increments happen together under c.mu, so they are atomic. c.mu must be held.
func (c *Controller) reserveLocked(global int, clients map[string]int, perUser int, a admission) (release func(), ok bool) {
	if global > 0 && c.activeGlobal >= global {
		return nil, false
	}
	if a.countClient {
		if lim, limited := clients[a.client]; limited && c.active[a.client] >= lim {
			return nil, false
		}
	}
	if a.user != "" && perUser > 0 && c.activeUser[a.user] >= perUser {
		return nil, false
	}
	c.activeGlobal++
	if a.countClient {
		if c.active == nil {
			c.active = map[string]int{}
		}
		c.active[a.client]++
	}
	if a.user != "" {
		if c.activeUser == nil {
			c.activeUser = map[string]int{}
		}
		c.activeUser[a.user]++
	}
	if c.activeGlobal > c.peak {
		c.peak = c.activeGlobal
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.activeGlobal--
			if a.countClient {
				decrement(c.active, a.client)
			}
			if a.user != "" {
				decrement(c.activeUser, a.user)
			}
		})
	}, true
}

// decrement lowers m[k] by one, deleting the entry at zero.
func decrement(m map[string]int, k string) {
	if n := m[k] - 1; n <= 0 {
		delete(m, k)
	} else {
		m[k] = n
	}
}

// statsLocked renders a diagnostics snapshot for the given generation's
// limits against the shared counters. c.mu must be held.
func (c *Controller) statsLocked(global int, clients map[string]int) core.InflightStats {
	st := core.InflightStats{
		GlobalLimit:  global,
		GlobalActive: c.activeGlobal,
		GlobalPeak:   c.peak,
	}
	seen := map[string]bool{}
	for name := range clients {
		seen[name] = true
	}
	for name := range c.active {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		st.Clients = append(st.Clients, core.ClientInflight{
			Name:   name,
			Limit:  clients[name], // 0 = unlimited
			Active: c.active[name],
		})
	}
	return st
}

// View is one generation-scoped concurrency configuration over a Controller's
// persistent counters. It is safe for concurrent use. Create one with
// Controller.View.
type View struct {
	c       *Controller
	global  int
	clients map[string]int
	perUser int
}

// Acquire reserves a slot for client under this view's limits. Its contract
// matches Controller.Acquire: the release is idempotent and must be called
// exactly once per successful Acquire.
func (v *View) Acquire(client string) (release func(), ok bool) {
	c := v.c
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserveLocked(v.global, v.clients, 0, admission{client: client, countClient: true})
}

// AcquirePrincipal reserves a slot for an authenticated principal under this
// view's limits: the global limit, the per-client limit for a static client
// (by Client.Name), and this view's per-user limit for a principal with an
// owning user, counted across all of that user's credentials and every view.
// All checks and increments are one atomic step. It reports false when any
// limit is saturated, and also for a principal that may not run inference
// (see admissionFor). The release contract matches Acquire.
func (v *View) AcquirePrincipal(p core.Principal) (release func(), ok bool) {
	a, valid := admissionFor(p)
	if !valid {
		return nil, false
	}
	c := v.c
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserveLocked(v.global, v.clients, v.perUser, a)
}

// UserLimit reports this view's per-user limit (0 = unlimited).
func (v *View) UserLimit() int { return v.perUser }

// Stats reports this view's configured limits against the shared live usage.
func (v *View) Stats() core.InflightStats {
	c := v.c
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statsLocked(v.global, v.clients)
}

func validateLimits(global int, clientLimits map[string]int) error {
	if global < 0 {
		return ErrNegativeLimit
	}
	for _, lim := range clientLimits {
		if lim < 0 {
			return ErrNegativeLimit
		}
	}
	return nil
}
