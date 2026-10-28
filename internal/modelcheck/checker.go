package modelcheck

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// SharedListings is a source of listings another component already polls. It
// is satisfied by *internal/discovery.Manager.
type SharedListings interface {
	// Listing returns accountID's last good ids and their dropped count, the
	// last attempt's time and error class, and whether the account is polled
	// at all.
	Listing(accountID string) (ids []string, dropped int, attemptAt time.Time, errClass string, targeted bool)
}

// Checker periodically re-runs the check against the current generation's
// routes and keeps the last Result for diagnostics. It never changes routing.
// Create it with NewChecker.
type Checker struct {
	creds    core.CredentialSource
	shared   SharedListings
	interval time.Duration
	opts     Options
	log      *slog.Logger

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}

	mu       sync.Mutex
	started  bool
	closed   bool
	accounts map[string]core.Account
	routes   []core.Route
	result   Result

	checkMu sync.Mutex             // serializes Check
	cache   map[string]cachedFetch // own fetches by account; guarded by checkMu
	state   map[string]pairState   // last logged state per (route, account); checkMu
}

type cachedFetch struct {
	baseURL string
	at      time.Time
	listing Listing
}

// NewChecker builds a checker that re-checks every interval (> 0). shared,
// when non-nil, supplies listings for accounts it already polls; those
// accounts are never fetched by the checker. logger nil uses slog.Default.
func NewChecker(creds core.CredentialSource, shared SharedListings, interval time.Duration, logger *slog.Logger, opts Options) *Checker {
	if logger == nil {
		logger = slog.Default()
	}
	c := &Checker{creds: creds, shared: shared, interval: interval, opts: opts.withDefaults(), log: logger,
		wake: make(chan struct{}, 1), cache: map[string]cachedFetch{}, state: map[string]pairState{},
		result: Result{Accounts: []AccountResult{}, Verdicts: []Verdict{}}}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	return c
}

// SetTargets re-targets the checker to one generation's accounts and routes
// and, once started, triggers a re-check. Accounts the checker fetched
// recently are not fetched again until the next interval.
func (c *Checker) SetTargets(accounts map[string]core.Account, routes []core.Route) {
	c.mu.Lock()
	c.accounts, c.routes = maps.Clone(accounts), slices.Clone(routes)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Start runs the first check in the background and then one every interval
// until ctx is cancelled or Close is called. It does not block. Calls after
// the first, or after Close, do nothing.
func (c *Checker) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.closed {
		return
	}
	c.started = true
	c.wg.Add(1)
	go c.loop(ctx)
}

func (c *Checker) loop(ctx context.Context) {
	defer c.wg.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	t := time.NewTicker(c.interval)
	defer t.Stop()
	// retry re-checks (without re-fetching) while a shared listing is still
	// pending, so the first discovery poll is picked up before the next
	// interval.
	retry := time.NewTimer(0)
	retry.Stop()
	defer retry.Stop()
	res := c.check(ctx, true)
	for {
		if res.pending() {
			retry.Reset(PendingRetry)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			res = c.check(ctx, true)
		case <-c.wake:
			res = c.check(ctx, false)
		case <-retry.C:
			res = c.check(ctx, false)
		}
	}
}

// PendingRetry is how soon the checker re-reads a shared listing that was
// still pending at the last check.
const PendingRetry = 10 * time.Second

func (r Result) pending() bool {
	return slices.ContainsFunc(r.Accounts, func(a AccountResult) bool { return a.ErrorClass == ErrClassPending })
}

// Close stops the checker and waits for its goroutine. It is idempotent.
func (c *Checker) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
	c.opts.Fetch.HTTPClient.CloseIdleConnections()
}

// Check runs one check now, fetching every account the checker owns, and
// returns its result. The background loop calls it; tests call it directly.
func (c *Checker) Check(ctx context.Context) Result { return c.check(ctx, true) }

// check runs one check. With refetch false, an own listing fetched for the
// same base URL is reused (a reload re-check).
func (c *Checker) check(ctx context.Context, refetch bool) Result {
	c.checkMu.Lock()
	defer c.checkMu.Unlock()
	c.mu.Lock()
	accounts, routes := c.accounts, c.routes
	c.mu.Unlock()

	exps := Expectations(accounts, routes)
	ids := eligibleAccounts(exps)
	listings := map[string]Listing{}
	results := map[string]AccountResult{}
	var fetch []string
	for _, id := range ids {
		if c.shared != nil {
			if got, dropped, at, class, ok := c.shared.Listing(id); ok {
				ar := AccountResult{Account: id, Source: "discovery", FetchedAt: utcPtr(at), ModelCount: len(got), ErrorClass: class}
				switch {
				case at.IsZero():
					ar.ErrorClass = ErrClassPending
					listings[id] = Listing{ErrorClass: ErrClassPending}
				default:
					listings[id] = Listing{IDs: got, Dropped: dropped, ErrorClass: class}
				}
				results[id] = ar
				continue
			}
		}
		if cf, ok := c.cache[id]; ok && !refetch && cf.baseURL == accounts[id].BaseURL {
			listings[id] = cf.listing
			continue
		}
		fetch = append(fetch, id)
	}
	fetched := fetchAll(ctx, c.creds, accounts, fetch, c.opts)
	now := c.opts.Clock.Now()
	for id, l := range fetched {
		c.cache[id] = cachedFetch{baseURL: accounts[id].BaseURL, at: now, listing: l}
	}
	for id := range c.cache {
		if _, ok := results[id]; ok || !slices.Contains(ids, id) {
			delete(c.cache, id)
		}
	}
	for id, cf := range c.cache {
		listings[id] = cf.listing
		results[id] = AccountResult{Account: id, Source: "fetch", FetchedAt: utcPtr(cf.at), ModelCount: len(cf.listing.IDs), ErrorClass: cf.listing.ErrorClass}
	}

	res := Result{CheckedAt: utcPtr(now), Accounts: []AccountResult{}}
	for _, id := range ids {
		res.Accounts = append(res.Accounts, results[id])
	}
	res.Verdicts = Evaluate(exps, listings)
	res.Summary = summarize(res.Verdicts)
	if ctx.Err() != nil {
		// A cancelled check (shutdown) is neither published nor logged.
		return res
	}
	c.logChanges(res.Verdicts)
	c.mu.Lock()
	c.result = res
	c.mu.Unlock()
	return res
}

// logChanges logs one warn line per verdict change into missing or
// single_model and one info line per return to ok. Unchecked and ineligible
// verdicts keep the last known state. State is tracked per (route, account,
// expected id). A pair that now expects a single id but expected several
// before inherits the pair's previous bad state; a pair that expected a single
// id before carries its state to its first verdict when no current expected id
// has state. So changing upstream_model (or switching between one and several
// expected ids) on reload still logs the return to ok, once.
func (c *Checker) logChanges(vs []Verdict) {
	next := map[string]pairState{}
	known := map[string]bool{} // pairs with state for a current expected id
	for _, v := range vs {
		pair := v.Route + "\x00" + v.Account
		ps := next[pair]
		ps.n++
		next[pair] = ps
		if _, ok := c.state[pair].st[v.Expected]; ok {
			known[pair] = true
		}
	}
	carried := map[string]bool{}
	for _, v := range vs {
		pair := v.Route + "\x00" + v.Account
		old, ps := c.state[pair], next[pair]
		if ps.st == nil {
			ps.st = map[string]string{}
			next[pair] = ps
		}
		prev := old.st[v.Expected]
		if !carried[pair] && (ps.n == 1 && old.n > 1 || old.n == 1 && !known[pair]) {
			carried[pair] = true
			prev = badState(old.st)
		}
		st := v.Status
		if v.SingleModel {
			st = "single_model"
		}
		switch st {
		case StatusOK:
			if prev == StatusMissing || prev == "single_model" {
				c.log.Info("backend model check ok", "route", v.Route, "account", v.Account, "expected", v.Expected)
			}
		case StatusMissing:
			if prev != st {
				c.log.Warn("backend model missing", "route", v.Route, "account", v.Account, "expected", v.Expected,
					"suggestions", strings.Join(v.Suggestions, ","))
			}
		case "single_model":
			if prev != st {
				c.log.Warn("backend serves a different single model", "route", v.Route, "account", v.Account, "expected", v.Expected,
					"loaded", v.Loaded)
			}
		default:
			if prev != "" {
				ps.st[v.Expected] = prev
			}
			continue
		}
		ps.st[v.Expected] = st
	}
	c.state = next
}

// pairState is the logged state of one (route, account) pair: its verdict
// count at the last check and the state per expected id.
type pairState struct {
	n  int
	st map[string]string
}

// badState returns a missing or single_model state among a pair's states
// (missing first), or "".
func badState(states map[string]string) string {
	var out string
	for _, st := range states {
		if st == StatusMissing {
			return st
		}
		if st == "single_model" {
			out = st
		}
	}
	return out
}

// Diagnostics is the diagnostics "model_check" block: the interval and the
// last Result (checked_at null and empty lists before the first check).
type Diagnostics struct {
	Interval string `json:"interval"`
	Result
}

// Snapshot returns the current diagnostics block.
func (c *Checker) Snapshot() Diagnostics {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Diagnostics{Interval: c.interval.String(), Result: c.result}
}

// Diagnostics returns Snapshot as the value control encodes verbatim.
func (c *Checker) Diagnostics() any { return c.Snapshot() }

func utcPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
