package discovery

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Defaults for Options.
const (
	DefaultTimeout        = 5 * time.Second
	DefaultRefreshWait    = 3 * time.Second
	DefaultRefreshSpacing = 30 * time.Second
	DefaultMaxBodyBytes   = 1 << 20
	DefaultMaxIDs         = 1000
	DefaultTick           = time.Second
	// DefaultMaxHeaderBytes bounds the response headers of one /models poll.
	DefaultMaxHeaderBytes = 64 << 10
	// RetryBase is the delay before retrying a failed poll. It doubles on
	// each consecutive failure, capped at the account's interval.
	RetryBase = 30 * time.Second
)

// Options tune a Manager. Zero values select the defaults above.
type Options struct {
	// HTTPClient performs GET /models. Nil uses a client that never follows
	// redirects (a redirect is then a non-2xx "status" failure) and reads at
	// most DefaultMaxHeaderBytes of response headers (an overflow is a
	// "transport" failure).
	HTTPClient *http.Client
	// Clock drives poll scheduling and refresh spacing; default
	// core.SystemClock.
	Clock  core.Clock
	Logger *slog.Logger
	// Timeout bounds one poll end to end (credential, request, body).
	Timeout time.Duration
	// RefreshWait is the longest Refresh blocks its caller.
	RefreshWait time.Duration
	// RefreshSpacing is the minimum spacing between on-demand refreshes.
	RefreshSpacing time.Duration
	// MaxBodyBytes caps a listing body; MaxIDs caps ids kept per account.
	MaxBodyBytes int64
	MaxIDs       int
	// Tick is how often the background loop checks for due polls (real time).
	Tick time.Duration
}

// Manager polls model listings for the accounts the current generation's
// discovering routes reference and holds them keyed by account id. It is safe
// for concurrent use. Create it with New.
type Manager struct {
	creds core.CredentialSource
	opts  Options
	clock core.Clock
	log   *slog.Logger

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}

	mu       sync.RWMutex
	closed   bool
	startCtx context.Context // set by Start; nil before
	running  bool            // the background loop goroutine exists
	targets  map[string]*entry
	// polled is closed and replaced each time any poll finishes, so refresh
	// waiters can re-check resolution per account.
	polled chan struct{}

	refreshMu   sync.Mutex
	refreshing  chan struct{} // non-nil while an on-demand refresh runs
	lastRefresh time.Time
}

// entry is one polled account. Fields are guarded by Manager.mu; ids and
// sorted are replaced wholesale, never mutated in place.
type entry struct {
	account  core.Account
	interval time.Duration
	nextDue  time.Time
	inflight chan struct{} // non-nil while a poll of this account runs

	ids         map[string]struct{}
	sorted      []string
	lastSuccess time.Time
	lastAttempt time.Time
	dropped     int
	errClass    string
	failures    int    // consecutive failed polls
	loggedClass string // last error class logged ("" = healthy)
}

// delay is the time from the last attempt to the next scheduled poll: the
// interval, or after consecutive failures RetryBase doubled per extra
// failure, capped at the interval.
func (e *entry) delay() time.Duration {
	if e.failures == 0 {
		return e.interval
	}
	d := RetryBase
	for i := 1; i < e.failures && d < e.interval; i++ {
		d *= 2
	}
	return min(d, e.interval)
}

// New builds a Manager. creds supplies the same bearer the proxy sends.
func New(creds core.CredentialSource, opts Options) *Manager {
	if opts.HTTPClient == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = (&net.Dialer{Timeout: DefaultTimeout, KeepAlive: 30 * time.Second}).DialContext
		tr.MaxResponseHeaderBytes = DefaultMaxHeaderBytes
		opts.HTTPClient = &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.RefreshWait <= 0 {
		opts.RefreshWait = DefaultRefreshWait
	}
	if opts.RefreshSpacing <= 0 {
		opts.RefreshSpacing = DefaultRefreshSpacing
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.MaxIDs <= 0 {
		opts.MaxIDs = DefaultMaxIDs
	}
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	m := &Manager{creds: creds, opts: opts, clock: opts.Clock, log: opts.Logger,
		wake: make(chan struct{}, 1), targets: map[string]*entry{}, polled: make(chan struct{})}
	if m.clock == nil {
		m.clock = core.SystemClock{}
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m
}

// SetTargets re-targets polling to routes (one generation's routing table).
// An account is polled while at least one route with DiscoverModels lists it
// as a candidate, at the smallest DiscoverInterval among those routes.
// Listings of accounts still referenced are kept; an account no longer
// referenced stops being polled and its listing is dropped. Newly referenced
// accounts are polled on the next loop pass. After Start, the background loop
// exists only while there is a target: the first target starts it and an
// empty target set ends it.
func (m *Manager) SetTargets(accounts map[string]core.Account, routes []core.Route) {
	want := map[string]time.Duration{}
	for _, rt := range routes {
		if !rt.DiscoverModels {
			continue
		}
		for _, id := range candidates(rt) {
			if _, ok := accounts[id]; !ok {
				continue
			}
			if cur, ok := want[id]; !ok || rt.DiscoverInterval < cur {
				want[id] = rt.DiscoverInterval
			}
		}
	}
	m.mu.Lock()
	for id := range m.targets {
		if _, ok := want[id]; !ok {
			delete(m.targets, id)
		}
	}
	for id, iv := range want {
		if iv <= 0 {
			iv = 5 * time.Minute
		}
		e, ok := m.targets[id]
		if !ok {
			m.targets[id] = &entry{account: accounts[id], interval: iv}
			continue
		}
		e.account = accounts[id]
		if e.interval != iv {
			e.interval = iv
			if !e.lastAttempt.IsZero() {
				e.nextDue = e.lastAttempt.Add(e.delay())
			}
		}
	}
	m.startLoopLocked()
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Start enables background polling until ctx is cancelled or Close is called.
// It does not block: the first poll runs on the background goroutine, which
// exists only while there is a target, so a configuration without a
// discovering route starts no goroutine. Calls after the first, or after
// Close, do nothing.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startCtx != nil || m.closed {
		return
	}
	m.startCtx = ctx
	m.startLoopLocked()
}

// startLoopLocked starts the background loop if Start was called, the manager
// is open, there is a target and the loop is not already running. Caller
// holds m.mu.
func (m *Manager) startLoopLocked() {
	if m.startCtx == nil || m.startCtx.Err() != nil || m.closed || m.running || len(m.targets) == 0 {
		return
	}
	m.running = true
	m.wg.Add(1)
	go m.loop(m.startCtx)
}

// track registers one manager goroutine unless the manager is closed.
func (m *Manager) track() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.wg.Add(1)
	return true
}

func (m *Manager) loop(ctx context.Context) {
	defer m.wg.Done()
	pollCtx, cancel := context.WithCancel(m.ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	t := time.NewTicker(m.opts.Tick)
	defer t.Stop()
	for {
		// The exit check and SetTargets' restart both run under m.mu, so a
		// target added while the loop exits always finds running false.
		m.mu.Lock()
		if len(m.targets) == 0 || pollCtx.Err() != nil {
			m.running = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		m.PollDue(pollCtx)
		select {
		case <-pollCtx.Done():
		case <-t.C:
		case <-m.wake:
		}
	}
}

// Close stops polling and waits for every goroutine the manager started. A
// later Refresh returns immediately. It is idempotent.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
	m.opts.HTTPClient.CloseIdleConnections()
}

// PollDue polls, concurrently, every target whose next poll is due by the
// manager clock, and returns when they finish. The background loop calls it;
// tests call it directly with a controlled clock.
func (m *Manager) PollDue(ctx context.Context) {
	now := m.clock.Now()
	var due []string
	m.mu.RLock()
	for id, e := range m.targets {
		if !e.nextDue.After(now) {
			due = append(due, id)
		}
	}
	m.mu.RUnlock()
	m.pollEach(ctx, due)
}

// pollAll polls every current target regardless of schedule.
func (m *Manager) pollAll(ctx context.Context) {
	m.mu.RLock()
	ids := make([]string, 0, len(m.targets))
	for id := range m.targets {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	m.pollEach(ctx, ids)
}

func (m *Manager) pollEach(ctx context.Context, ids []string) {
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.poll(ctx, id)
		}()
	}
	wg.Wait()
}

// poll fetches one account's listing, single-flight per account: a caller
// that finds a poll already running waits for it instead of sending another.
func (m *Manager) poll(ctx context.Context, id string) {
	m.mu.Lock()
	e := m.targets[id]
	if e == nil || m.closed {
		m.mu.Unlock()
		return
	}
	if ch := e.inflight; ch != nil {
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return
	}
	done := make(chan struct{})
	e.inflight = done
	account := e.account
	m.mu.Unlock()
	defer close(done)

	l, class := m.fetch(ctx, account)
	now := m.clock.Now()

	m.mu.Lock()
	e.inflight = nil
	close(m.polled)
	m.polled = make(chan struct{})
	// Discard the result if the account was dropped (or re-added as a new
	// entry) by a reload meanwhile, or the manager is closing.
	if m.targets[id] != e || m.closed || ctx.Err() != nil && class != "" {
		m.mu.Unlock()
		return
	}
	e.lastAttempt = now
	e.errClass = class
	if class == "" {
		ids := make(map[string]struct{}, len(l.ids))
		for _, s := range l.ids {
			ids[s] = struct{}{}
		}
		e.ids, e.sorted, e.dropped, e.lastSuccess = ids, l.ids, l.dropped, now
		e.failures = 0
	} else {
		e.failures++
	}
	e.nextDue = now.Add(e.delay())
	logIt := class != e.loggedClass
	prev := e.loggedClass
	e.loggedClass = class
	m.mu.Unlock()

	switch {
	case logIt && class != "":
		m.log.Warn("model discovery poll failed", "account", id, "error_class", class)
	case logIt && prev != "":
		m.log.Info("model discovery poll recovered", "account", id)
	}
}

// Refresh runs an on-demand refresh of every target, single-flight and at most
// once per RefreshSpacing (by the manager clock). The caller waits until the
// refresh finishes, until done reports true (re-checked each time any
// account's poll finishes; nil never does), at most RefreshWait, or until ctx
// is done; ctx never cancels the shared refresh itself, which other callers
// may be waiting on. Inside the spacing window, or after Close, it returns at
// once.
func (m *Manager) Refresh(ctx context.Context, done func() bool) {
	m.refreshMu.Lock()
	ch := m.refreshing
	if ch == nil {
		now := m.clock.Now()
		if !m.lastRefresh.IsZero() && now.Sub(m.lastRefresh) < m.opts.RefreshSpacing || !m.track() {
			m.refreshMu.Unlock()
			return
		}
		ch = make(chan struct{})
		m.refreshing, m.lastRefresh = ch, now
		go func() {
			defer m.wg.Done()
			m.pollAll(m.ctx)
			m.refreshMu.Lock()
			m.refreshing = nil
			m.refreshMu.Unlock()
			close(ch)
		}()
	}
	m.refreshMu.Unlock()
	t := time.NewTimer(m.opts.RefreshWait)
	defer t.Stop()
	for {
		// Take the poll signal before checking done, so a poll finishing in
		// between is never missed.
		m.mu.RLock()
		polled := m.polled
		m.mu.RUnlock()
		if done != nil && done() {
			return
		}
		select {
		case <-ch:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case <-polled:
		}
	}
}

// candidates is interactive ∪ background in first-seen order.
func candidates(rt core.Route) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range append(append([]string{}, rt.Interactive...), rt.Background...) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
