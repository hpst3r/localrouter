package quota

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Default endpoints and intervals.
const (
	DefaultCodexUsageURL   = "https://chatgpt.com/backend-api/wham/usage"
	DefaultOllamaUsageURL  = "https://ollama.com/api/usage"
	DefaultClaudeUsageURL  = "https://api.anthropic.com/api/oauth/usage"
	DefaultClaudeUserAgent = "claude-code/2.1.0"
	DefaultPollInterval    = 5 * time.Minute
	DefaultMinRefreshGap   = 20 * time.Second
	defaultHTTPTimeout     = 30 * time.Second
)

// Snapshot sources.
const (
	SourceUsageAPI = "usage_api"
	SourceHeaders  = "headers"
)

// Options configures a Manager. Zero values select defaults.
type Options struct {
	PollInterval    time.Duration // default 5m
	MinRefreshGap   time.Duration // default 20s; debounce for non-urgent refreshes
	HTTPClient      *http.Client  // default: 30s timeout client
	Clock           core.Clock    // default core.SystemClock
	Logger          *slog.Logger  // default slog.Default()
	CodexUsageURL   string        // default DefaultCodexUsageURL
	OllamaUsageURL  string        // default DefaultOllamaUsageURL
	ClaudeUsageURL  string        // default DefaultClaudeUsageURL
	ClaudeUserAgent string        // default DefaultClaudeUserAgent

	// ClaudeCredentialsFile returns the Claude Code credentials file path for
	// a claude account. The file is only ever read, never written. Nil or an
	// empty result makes fetches for that account fail with a sanitized Err.
	ClaudeCredentialsFile func(accountID string) string

	// ManagementCredentials returns the separate credential source holding an
	// openrouter account's optional management key, or nil when none is
	// configured. It is used only for GET <base_url>/credits; inference and
	// GET <base_url>/key always use the account's normal credential.
	ManagementCredentials func(accountID string) core.CredentialSource
}

type accountState struct {
	acct      core.Account
	snap      *core.Snapshot
	lastFetch time.Time     // completion time of the last usage API fetch
	inflight  chan struct{} // non-nil while a fetch is running; closed when done

	// observedAt records, per window kind, when the window's current values
	// were sampled (fetch start for the usage API, receipt for headers).
	observedAt map[string]time.Time
}

// Manager implements core.QuotaSource for codex, ollama and claude accounts.
type Manager struct {
	creds core.CredentialSource // not used for claude accounts
	opts  Options

	claudeMu    sync.Mutex
	claudeCache map[string]*claudeCredCache

	mu      sync.Mutex
	state   map[string]*accountState
	baseCtx context.Context

	wg sync.WaitGroup // async refreshes (used by tests)
}

var _ core.QuotaSource = (*Manager)(nil)

// New returns a Manager for the given accounts. Accounts whose provider has
// no quota API (openai_compat) are ignored.
func New(accounts []core.Account, creds core.CredentialSource, opts Options) *Manager {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.MinRefreshGap <= 0 {
		opts.MinRefreshGap = DefaultMinRefreshGap
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if opts.Clock == nil {
		opts.Clock = core.SystemClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.CodexUsageURL == "" {
		opts.CodexUsageURL = DefaultCodexUsageURL
	}
	if opts.OllamaUsageURL == "" {
		opts.OllamaUsageURL = DefaultOllamaUsageURL
	}
	if opts.ClaudeUsageURL == "" {
		opts.ClaudeUsageURL = DefaultClaudeUsageURL
	}
	if opts.ClaudeUserAgent == "" {
		opts.ClaudeUserAgent = DefaultClaudeUserAgent
	}
	m := &Manager{
		creds:       creds,
		opts:        opts,
		claudeCache: make(map[string]*claudeCredCache),
		state:       make(map[string]*accountState),
		baseCtx:     context.Background(),
	}
	for _, a := range accounts {
		switch a.Provider {
		case core.ProviderCodex, core.ProviderOllama, core.ProviderClaude, core.ProviderOpenRouter:
			m.state[a.ID] = &accountState{acct: a, observedAt: make(map[string]time.Time)}
		}
	}
	return m
}

// Start launches one polling goroutine per quota-capable account. Each polls
// immediately and then every PollInterval until ctx is cancelled. Async
// refreshes requested via RequestRefresh after Start also use ctx.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.baseCtx = ctx
	ids := make([]string, 0, len(m.state))
	for id, st := range m.state {
		if !agentSourced(st.acct) {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		go m.poll(ctx, id)
	}
}

func (m *Manager) poll(ctx context.Context, id string) {
	t := time.NewTicker(m.opts.PollInterval)
	defer t.Stop()
	for {
		m.refresh(ctx, id, true)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Latest returns a deep copy of the account's most recent snapshot; ok=false
// if the account has no quota source or no fetch has been attempted yet.
func (m *Manager) Latest(accountID string) (core.Snapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[accountID]
	if st == nil || st.snap == nil {
		return core.Snapshot{}, false
	}
	return copySnapshot(*st.snap), true
}

// RequestRefresh asynchronously refreshes the account's snapshot. Non-urgent
// requests are dropped if the last fetch completed less than MinRefreshGap
// ago. Requests arriving while a fetch is in flight are coalesced into it.
func (m *Manager) RequestRefresh(accountID string, urgent bool) {
	m.mu.Lock()
	st, ok := m.state[accountID]
	ctx := m.baseCtx
	m.mu.Unlock()
	if !ok || agentSourced(st.acct) {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.refresh(ctx, accountID, urgent)
	}()
}

// refresh performs a single-flight fetch for the account. If a fetch is
// already running it waits for that one instead of starting another.
func (m *Manager) refresh(ctx context.Context, id string, urgent bool) {
	m.mu.Lock()
	st := m.state[id]
	if st == nil || agentSourced(st.acct) {
		m.mu.Unlock()
		return
	}
	if ch := st.inflight; ch != nil {
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return
	}
	if !urgent && !st.lastFetch.IsZero() && m.opts.Clock.Now().Sub(st.lastFetch) < m.opts.MinRefreshGap {
		m.mu.Unlock()
		return
	}
	ch := make(chan struct{})
	st.inflight = ch
	acct := st.acct
	if acct.Provider == core.ProviderOpenRouter {
		var prev *core.Snapshot
		if st.snap != nil {
			c := copySnapshot(*st.snap)
			prev = &c
		}
		m.mu.Unlock()
		// Partial results are merged part by part inside fetchOpenRouter, so
		// the outcome always replaces the snapshot. Only this single-flight
		// fetch writes openrouter snapshots (headers and ingest do not).
		snap := m.fetchOpenRouter(ctx, acct, prev)
		m.mu.Lock()
		defer m.mu.Unlock()
		defer close(ch)
		st.inflight = nil
		st.lastFetch = m.opts.Clock.Now()
		if snap.Err != "" {
			m.opts.Logger.Warn("quota fetch failed", "account", id, "provider", acct.Provider, "error", snap.Err)
		}
		st.snap = &snap
		return
	}
	m.mu.Unlock()

	snap, err := m.fetch(ctx, acct)

	m.mu.Lock()
	defer m.mu.Unlock()
	defer close(ch)
	st.inflight = nil
	st.lastFetch = m.opts.Clock.Now()
	if err != nil {
		m.opts.Logger.Warn("quota fetch failed", "account", id, "provider", acct.Provider, "error", err.Error())
		if st.snap == nil {
			st.snap = &core.Snapshot{AccountID: id, Source: SourceUsageAPI}
		}
		st.snap.Err = err.Error()
		return
	}
	st.snap = mergeFetched(st, snap)
}

// mergeFetched combines a usage API snapshot with the current one. The fetch
// counts as observed at its start time (snap.FetchedAt); a window observed
// from headers after that start is newer and is kept instead of the fetched
// value. Caller holds m.mu.
func mergeFetched(st *accountState, snap core.Snapshot) *core.Snapshot {
	started := snap.FetchedAt
	if st.snap != nil {
		for _, old := range st.snap.Windows {
			if !st.observedAt[old.Kind].After(started) {
				continue
			}
			replaced := false
			for i := range snap.Windows {
				if snap.Windows[i].Kind == old.Kind {
					snap.Windows[i] = old
					replaced = true
				}
			}
			if !replaced {
				snap.Windows = append(snap.Windows, old)
			}
			if old.UsedFrac >= 1 {
				f := false
				snap.Allowed = &f
			}
		}
	}
	for _, w := range snap.Windows {
		if !st.observedAt[w.Kind].After(started) {
			st.observedAt[w.Kind] = started
		}
	}
	return &snap
}

func (m *Manager) fetch(ctx context.Context, acct core.Account) (core.Snapshot, error) {
	switch acct.Provider {
	case core.ProviderCodex:
		return m.fetchCodex(ctx, acct.ID)
	case core.ProviderClaude:
		return m.fetchClaude(ctx, acct.ID)
	default:
		return m.fetchOllama(ctx, acct)
	}
}

// wait blocks until all async refreshes have finished (tests only).
func (m *Manager) wait() { m.wg.Wait() }

func copySnapshot(s core.Snapshot) core.Snapshot {
	if s.Windows != nil {
		s.Windows = append([]core.Window(nil), s.Windows...)
	}
	if s.Allowed != nil {
		v := *s.Allowed
		s.Allowed = &v
	}
	if s.ModelRequests != nil {
		mr := make(map[string][]core.ModelCount, len(s.ModelRequests))
		for k, v := range s.ModelRequests {
			mr[k] = append([]core.ModelCount(nil), v...)
		}
		s.ModelRequests = mr
	}
	if s.Credits != nil {
		c := *s.Credits
		s.Credits = &c
	}
	if s.Key != nil {
		k := *s.Key
		for _, p := range []**float64{&k.LimitUSD, &k.LimitRemainingUSD, &k.UsageDailyUSD, &k.UsageWeeklyUSD,
			&k.UsageMonthlyUSD, &k.BYOKUsageUSD, &k.BYOKUsageDailyUSD, &k.BYOKUsageWeeklyUSD, &k.BYOKUsageMonthlyUSD} {
			if *p != nil {
				v := **p
				*p = &v
			}
		}
		for _, p := range []**bool{&k.IncludeBYOKInLimit, &k.IsFreeTier} {
			if *p != nil {
				v := **p
				*p = &v
			}
		}
		s.Key = &k
	}
	return s
}

func clampFrac(f float64) float64 {
	if f < 0 || f != f { // negative or NaN
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
