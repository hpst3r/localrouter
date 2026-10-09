// Package connlim enforces limits on how many inference requests may be
// active at once, globally and per authenticated client. It is deliberately
// free of any knowledge of accounts, models or routing: it governs the
// inbound edge only, so it stays valid when admission policy and the worker
// pool change.
//
// A Controller is safe for concurrent use. The zero global limit and a zero
// or absent per-client limit both mean "unlimited", which keeps the feature
// backwards compatible with configurations written before it existed.
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
type Controller struct {
	mu      sync.Mutex
	global  int
	clients map[string]int
	// activeGlobal counts every active request; active[c] counts one client.
	activeGlobal int
	active       map[string]int
	peak         int
}

// New builds a Controller. A global limit of 0 means unlimited; clientLimits
// may be nil or empty. It returns ErrNegativeLimit for any negative value.
func New(global int, clientLimits map[string]int) (*Controller, error) {
	c := &Controller{}
	if err := c.Reconfigure(global, clientLimits); err != nil {
		return nil, err
	}
	return c, nil
}

// Reconfigure atomically replaces the configured limits. Active requests are
// preserved: a request already admitted keeps its slot, and the new limits
// apply to subsequent admissions. This is the atomic-reload seam.
func (c *Controller) Reconfigure(global int, clientLimits map[string]int) error {
	if global < 0 {
		return ErrNegativeLimit
	}
	for name, lim := range clientLimits {
		if lim < 0 {
			return ErrNegativeLimit
		}
		_ = name
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.global = global
	c.clients = make(map[string]int, len(clientLimits))
	for name, lim := range clientLimits {
		if lim > 0 {
			c.clients[name] = lim
		}
	}
	return nil
}

// Acquire reserves a slot for client, or reports false when the global limit
// or the client's limit is already saturated. The returned release function is
// idempotent and must be called exactly once per successful Acquire, however
// the request ends (completion, disconnect, upstream failure or failover).
func (c *Controller) Acquire(client string) (release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.global > 0 && c.activeGlobal >= c.global {
		return nil, false
	}
	if lim, limited := c.clients[client]; limited && c.active[client] >= lim {
		return nil, false
	}
	if c.active == nil {
		c.active = map[string]int{}
	}
	c.activeGlobal++
	c.active[client]++
	if c.activeGlobal > c.peak {
		c.peak = c.activeGlobal
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.activeGlobal--
			if n := c.active[client] - 1; n <= 0 {
				delete(c.active, client)
			} else {
				c.active[client] = n
			}
		})
	}, true
}

// Stats reports current capacity and usage for authenticated diagnostics. It
// never includes client keys or request content.
func (c *Controller) Stats() core.InflightStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := core.InflightStats{
		GlobalLimit:  c.global,
		GlobalActive: c.activeGlobal,
		GlobalPeak:   c.peak,
	}
	seen := map[string]bool{}
	for name := range c.clients {
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
			Limit:  c.clients[name], // 0 = unlimited
			Active: c.active[name],
		})
	}
	return st
}
