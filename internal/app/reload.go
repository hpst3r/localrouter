package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"

	"github.com/hpst3r/localrouter/internal/auth"
	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/connlim"
	"github.com/hpst3r/localrouter/internal/control"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
	"github.com/hpst3r/localrouter/internal/policy"
	"github.com/hpst3r/localrouter/internal/proxy"
	"github.com/hpst3r/localrouter/internal/quota"
)

var ErrRestartRequired = errors.New("configuration change requires restart")

type runtimeGeneration struct {
	handler http.Handler
	pricing *ledger.PricingView
	status  core.ReloadStatus
}

func cloneConfig(c *config.Config) (*config.Config, error) {
	if c == nil {
		return nil, errors.New("nil configuration")
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var out config.Config
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// restartFields ignores only fields explicitly allowed to reload. New fields
// are restart-only by default, rather than accidentally entering the live set.
func restartFields(base, candidate *config.Config) []string {
	left, _ := cloneConfig(base)
	right, _ := cloneConfig(candidate)
	for _, c := range []*config.Config{left, right} {
		c.Clients = nil
		c.Routes = nil
		c.Limits = config.LimitsConfig{}
		c.Policy = config.PolicyConfig{}
		for i := range c.Accounts {
			c.Accounts[i].Reserve = nil
		}
		c.Budgets = budgetPresence(c.Budgets)
	}
	l := reflect.ValueOf(*left)
	r := reflect.ValueOf(*right)
	typ := l.Type()
	var fields []string
	for i := 0; i < l.NumField(); i++ {
		if !reflect.DeepEqual(l.Field(i).Interface(), r.Field(i).Interface()) {
			fields = append(fields, typ.Field(i).Tag.Get("yaml"))
		}
	}
	sort.Strings(fields)
	return fields
}

// budgetPresence normalizes the optional budgets block to a single bit before
// the restart-only comparison: nil (disabled) or an empty &BudgetConfig{}
// (enabled). Enabling or disabling spend controls requires a restart — the
// process-wide store and its ownership lock exist only when Build saw the block
// — but everything inside it (reserve_usd and every client/account ceiling) is
// live-reloadable, so the whole block is erased for the comparison. A future
// field added to BudgetConfig is therefore reloadable by default; to make it
// restart-only, zero it explicitly here as well.
func budgetPresence(b *config.BudgetConfig) *config.BudgetConfig {
	if b == nil {
		return nil
	}
	return &config.BudgetConfig{}
}

// budgetGate builds a generation's immutable budget adapter over the shared
// store. It returns a nil gate only when spend controls are disabled for the
// process (no budgets block and no store). The gate pins this generation's
// limits and fixed reserve, so a reload publishes a fresh gate without
// disturbing a request still holding the previous generation's.
//
// A configuration that enables budgets is never allowed to publish a nil gate:
// the proxy treats a nil gate as "no enforcement", so resolving budgets-enabled
// to nil would fail open. If the block is present but the process has no store
// (released by Close, or the block appeared without the restart it requires),
// that is a hard error. budgetStore is read here on a goroutine that holds
// reloadMu, the same lock Close holds while releasing it.
func (a *App) budgetGate(c *config.Config, prices *ledger.Pricing) (proxy.Budget, string, error) {
	if c.Budgets != nil && a.budgetStore == nil {
		return nil, "budgets invalid", errors.New("budgets are configured but the budget store is not open")
	}
	if a.budgetStore == nil {
		return nil, "", nil
	}
	if c.Budgets == nil {
		return nil, "budgets invalid", errors.New("budget store is open but this configuration has no budgets block")
	}
	limits, err := c.Budgets.Limits()
	if err != nil {
		return nil, "budgets invalid", err
	}
	reserve, err := c.Budgets.ReservationMicros()
	if err != nil {
		return nil, "budgets invalid", err
	}
	return budget.NewGate(a.budgetStore, prices, limits, reserve), "", nil
}

func (a *App) generationStatus(g *runtimeGeneration) core.ReloadStatus {
	st := g.status
	if last := a.lastReload.Load(); last != nil && last.Generation == g.status.Generation {
		st = *last
	}
	st.RestartOnly = append([]string(nil), st.RestartOnly...)
	return st
}

func (a *App) ReloadStatus() core.ReloadStatus {
	if g := a.current.Load(); g != nil {
		return a.generationStatus(g)
	}
	return core.ReloadStatus{}
}

// Reload serializes both file loading and publication. Failure leaves the last
// good handler and all mutable runtime state untouched. Errors returned to the
// trusted caller may include paths; diagnostics and signal logs use only status.
func (a *App) Reload(path string) (core.ReloadStatus, error) {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	c, err := config.Load(path)
	if err != nil {
		return a.reloadFailure("configuration invalid", nil, err)
	}
	return a.reloadConfigLocked(c)
}
func (a *App) ReloadConfig(c *config.Config) (core.ReloadStatus, error) {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	return a.reloadConfigLocked(c)
}
func (a *App) reloadFailure(reason string, fields []string, err error) (core.ReloadStatus, error) {
	st := a.ReloadStatus()
	st.OK = false
	st.At = a.reloadClock.Now()
	st.Reason = reason
	st.RestartOnly = append([]string(nil), fields...)
	a.lastReload.Store(&st)
	return st, err
}
func (a *App) reloadConfigLocked(c *config.Config) (core.ReloadStatus, error) {
	if !a.Serving() {
		return a.reloadFailure("router shutting down", nil, ErrNotServing)
	}
	c, err := cloneConfig(c)
	if err != nil {
		return a.reloadFailure("configuration invalid", nil, err)
	}
	if err = c.Validate(); err != nil {
		return a.reloadFailure("configuration invalid", nil, err)
	}
	if fields := restartFields(a.reloadBase, c); len(fields) > 0 {
		return a.reloadFailure("restart required", fields, ErrRestartRequired)
	}
	old := a.current.Load()
	if old == nil {
		return core.ReloadStatus{}, errors.New("reload not initialized")
	}
	g, reason, err := a.makeGeneration(c, old.status.Generation+1)
	if err != nil {
		return a.reloadFailure(reason, nil, err)
	}
	// This is the sole configuration cutover. Handler, auth, policy knobs,
	// capacity, pricing and generation number all belong to this same pointer.
	// Serialize the final check/publication with BeginShutdown, not just with
	// other reloads: generation preparation may race with a terminal stop.
	a.srvMu.Lock()
	if !a.Serving() {
		a.srvMu.Unlock()
		return a.reloadFailure("router shutting down", nil, ErrNotServing)
	}
	a.current.Store(g)
	a.srvMu.Unlock()
	return a.generationStatus(g), nil
}

type viewInflight struct{ view *connlim.View }

func (v viewInflight) InflightStats() core.InflightStats { return v.view.Stats() }

func (a *App) makeGeneration(c *config.Config, number uint64) (*runtimeGeneration, string, error) {
	files := map[string][]string{}
	clients := map[string]core.Client{}
	for _, cl := range c.Clients {
		files[cl.Name] = cl.KeyPaths()
		host := cl.Host
		if host == "" {
			host = c.HostName
		}
		clients[cl.Name] = core.Client{Name: cl.Name, Class: core.Class(cl.Class), Host: host, Ingest: cl.Ingest}
	}
	keys, err := auth.LoadClientKeyFiles(files)
	if err != nil {
		return nil, "client keys invalid", err
	}
	prices, err := ledger.LoadPricing(c.PricingFile)
	if err != nil {
		return nil, "pricing invalid", err
	}
	accounts := c.CoreAccounts()
	acctMap := map[string]core.Account{}
	for _, acct := range accounts {
		acctMap[acct.ID] = acct
	}
	pol, err := a.Policy.WithConfig(accounts, policy.Options{StaleAfter: c.Policy.StaleAfter.D(), SafetyMargin: c.Policy.SafetyMargin, InflightEstimate: c.Policy.InflightEstimate, Clock: a.reloadClock, Logger: a.Logger})
	if err != nil {
		return nil, "policy invalid", err
	}
	lim, err := a.Limiter.View(c.Limits.MaxConcurrent, c.Limits.MaxConcurrentPerClient)
	if err != nil {
		return nil, "limits invalid", err
	}
	priceView := a.Ledger.WithPricing(prices)
	gate, reason, err := a.budgetGate(c, prices)
	if err != nil {
		return nil, reason, err
	}
	authenticate := func(bearer string) (core.Client, bool) {
		name, ok := keys.Lookup(bearer)
		if !ok {
			return core.Client{}, false
		}
		return clients[name], true
	}
	routes := c.CoreRoutes()
	px := proxy.New(proxy.Deps{Accounts: acctMap, Routes: routes, Creds: a.Auth, Quota: a.Quota, Policy: pol, Ledger: priceView, Budget: gate, Authenticate: authenticate, Clock: a.reloadClock, Logger: a.Logger, Limiter: lim}, proxy.Options{MaxFailovers: c.Policy.MaxFailovers})
	g := &runtimeGeneration{pricing: priceView, status: core.ReloadStatus{Generation: number, OK: true, At: a.reloadClock.Now()}}
	deps := control.Deps{Accounts: accounts, Quota: a.Quota, Policy: pol, Ledger: priceView, Routes: routes, Authenticate: authenticate, Clock: a.reloadClock, Storage: a.Ledger, Inflight: viewInflight{lim}, Ready: a.Serving, ReloadStatus: func() core.ReloadStatus { return a.generationStatus(g) }, Budgets: a.budgetSource(c.Budgets)}
	for _, cl := range c.Clients {
		deps.Clients = append(deps.Clients, control.ClientInfo{Name: cl.Name, Class: cl.Class, Host: clients[cl.Name].Host, Ingest: cl.Ingest})
	}
	deps.Ingester = a.Quota
	deps.IsSnapshotStale = func(err error) bool { return errors.Is(err, quota.ErrSnapshotStale) }
	ctl := control.New(deps, control.Options{RequireAuth: c.Control.RequireAuth, StaleAfter: c.Policy.StaleAfter.D()})
	mux := http.NewServeMux()
	mux.Handle("/v1/", px.Handler())
	mux.Handle("/", ctl.Handler())
	var allowed []string
	if c.AllowNonLoopback {
		allowed = c.AllowedHosts
	}
	g.handler = HostGuard(allowed, mux)
	return g, "", nil
}

// collectorLedger selects pricing once per local collector write/batch, while
// HTTP handlers retain their request generation's frozen pricing view. It is
// the single write path of the server-local collectors, so it also applies a
// final sanitation pass (see sanitizeCollectorRecord).
type collectorLedger struct {
	*ledger.Ledger
	app *App
}

func (l collectorLedger) Record(ctx context.Context, r core.RequestRecord) error {
	g := l.app.current.Load()
	if g == nil {
		return fmt.Errorf("runtime unavailable")
	}
	r, err := sanitizeCollectorRecord(r)
	if err != nil {
		return err
	}
	return g.pricing.Record(ctx, r)
}
func (l collectorLedger) RecordBatch(ctx context.Context, rs []core.RequestRecord) error {
	g := l.app.current.Load()
	if g == nil {
		return fmt.Errorf("runtime unavailable")
	}
	out := make([]core.RequestRecord, len(rs))
	for i, r := range rs {
		var err error
		if out[i], err = sanitizeCollectorRecord(r); err != nil {
			return err
		}
	}
	return g.pricing.RecordBatch(ctx, out)
}

// errInvalidCollectorRecord rejects a record whose usage is out of range. It
// is permanent (claudelog.PermanentError) so collectors drop the record
// instead of retrying it forever.
type errInvalidCollectorRecord struct{ id string }

func (e errInvalidCollectorRecord) Error() string {
	return "collector record " + core.TruncateLabel(e.id) + ": usage out of range"
}
func (errInvalidCollectorRecord) Permanent() bool { return true }

// sanitizeCollectorRecord bounds free-form labels (core.TruncateLabel,
// core.TruncateError) and rejects any token count outside
// [0, core.MaxRecordTokens]. Valid records are returned unchanged.
func sanitizeCollectorRecord(r core.RequestRecord) (core.RequestRecord, error) {
	u := r.Usage
	for _, v := range []int64{u.InputTokens, u.CachedInputTokens, u.CacheCreationInputTokens, u.OutputTokens, u.ReasoningTokens} {
		if v < 0 || v > core.MaxRecordTokens {
			return r, errInvalidCollectorRecord{id: r.ID}
		}
	}
	for _, p := range []*string{&r.Client, &r.Route, &r.Model, &r.Provider, &r.Session, &r.Task, &r.Agent, &r.Host} {
		*p = core.TruncateLabel(*p)
	}
	r.Class = core.Class(core.TruncateLabel(string(r.Class)))
	r.Error = core.TruncateError(r.Error)
	return r, nil
}
func (l collectorLedger) Close() error { return nil }
