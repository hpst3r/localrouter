// Package app wires LocalRouter's packages into a runnable server. It exists
// separately from cmd/localrouter so end-to-end tests can build the real
// object graph with fake upstream endpoints.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hpst3r/localrouter/internal/auth"
	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/claudelog"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/connlim"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/hermeslog"
	"github.com/hpst3r/localrouter/internal/httpguard"
	"github.com/hpst3r/localrouter/internal/ledger"
	"github.com/hpst3r/localrouter/internal/policy"
	"github.com/hpst3r/localrouter/internal/quota"
)

// Overrides replaces production endpoints; zero values keep defaults.
type Overrides struct {
	Issuer         string
	CodexUsageURL  string
	OllamaUsageURL string
	ClaudeUsageURL string
	HTTPClient     *http.Client
	Clock          core.Clock
}

// Timeouts are the resolved inbound server deadlines. They are read from
// config (with finite defaults) and applied by Serve.
type Timeouts struct {
	Header   time.Duration // request line + headers
	Body     time.Duration // reading a request body (inference, ingest, admit)
	Idle     time.Duration // keep-alive idle between requests
	Shutdown time.Duration // graceful drain budget
}

// App is a fully wired LocalRouter instance.
type App struct {
	Handler http.Handler
	Quota   *quota.Manager
	Policy  *policy.Policy
	Ledger  *ledger.Ledger
	Auth    *auth.Manager
	// ClaudeLog is nil unless claude_logs.enabled.
	ClaudeLog *claudelog.Collector
	// HermesLog is nil unless hermes_logs.enabled.
	HermesLog *hermeslog.Collector

	// Limiter is the shared inference concurrency controller. It bounds active
	// requests at the proxy and is reported to diagnostics. Non-nil even when
	// no limit is configured (then unlimited).
	Limiter *connlim.Controller
	// BodyGuard applies the configured body-read deadline around Handler.
	BodyGuard *httpguard.BodyTimeout
	// Timeouts are the deadlines Serve applies.
	Timeouts Timeouts
	// TLSCertFile and TLSKeyFile, when set, make Serve terminate TLS.
	TLSCertFile, TLSKeyFile string
	// Logger is the server logger.
	Logger *slog.Logger

	serving atomic.Bool
	// budgetStore and budgetOwnership are set by Build when cfg.Budgets is
	// present, and left nil otherwise. Every runtime generation shares the one
	// store (each builds its own immutable Gate over it); the ownership lock is
	// held for the whole process, so a second Build against the same DataDir is
	// denied and no other process may own or reconcile this database. Both are
	// written in Build before the App is published, and released (set nil) by
	// Close under reloadMu; every other access is on a goroutine that holds
	// reloadMu (Build is single-threaded and pre-publication), so a reload's
	// budgetGate read and Close's release are ordered on that one lock.
	budgetStore     *budget.Store
	budgetOwnership *budget.Ownership
	current         atomic.Pointer[runtimeGeneration]
	lastReload      atomic.Pointer[core.ReloadStatus]
	reloadMu        sync.Mutex
	reloadBase      *config.Config
	reloadClock     core.Clock

	// srvMu guards the one-shot Serve lifecycle fields below. Serve publishes
	// an active server to at most one caller; Shutdown may run concurrently on
	// any goroutine, so a.srv is never read, and started/stopped are never
	// mutated, without holding srvMu.
	srvMu   sync.Mutex
	srv     *http.Server
	started bool
	stopped bool
}

// StaticKeys maps non-codex accounts to their key sources.
func StaticKeys(cfg *config.Config) map[string]auth.StaticKey {
	keys := map[string]auth.StaticKey{}
	for _, a := range cfg.Accounts {
		if a.Provider != core.ProviderCodex {
			keys[a.ID] = auth.StaticKey{File: a.APIKeyFile, Env: a.APIKeyEnv}
		}
	}
	return keys
}

// ManagementKeys maps openrouter accounts that configure a management key to
// its source. Those keys are held by a separate credential manager used only
// for GET /credits, so they can never be attached to inference or GET /key.
func ManagementKeys(cfg *config.Config) map[string]auth.StaticKey {
	keys := map[string]auth.StaticKey{}
	for _, a := range cfg.Accounts {
		if a.Provider == core.ProviderOpenRouter && (a.ManagementKeyFile != "" || a.ManagementKeyEnv != "") {
			keys[a.ID] = auth.StaticKey{File: a.ManagementKeyFile, Env: a.ManagementKeyEnv}
		}
	}
	return keys
}

// managementCredentials returns the quota.Options.ManagementCredentials
// lookup: the management-key manager for accounts with one, nil otherwise.
func managementCredentials(cfg *config.Config, logger *slog.Logger, ov Overrides) func(string) core.CredentialSource {
	keys := ManagementKeys(cfg)
	if len(keys) == 0 {
		return nil
	}
	var accts []core.Account
	for _, a := range cfg.CoreAccounts() {
		if _, ok := keys[a.ID]; ok {
			accts = append(accts, a)
		}
	}
	m := auth.New(accts, keys, nil, auth.Options{HTTPClient: ov.HTTPClient, Clock: ov.Clock, Logger: logger})
	return func(id string) core.CredentialSource {
		if _, ok := keys[id]; ok {
			return m
		}
		return nil
	}
}

// NewAuth builds the credential manager (used by serve and login).
func NewAuth(cfg *config.Config, logger *slog.Logger, ov Overrides) (*auth.Manager, error) {
	store, err := auth.NewStore(filepath.Join(cfg.DataDir, "tokens"))
	if err != nil {
		return nil, err
	}
	return auth.New(cfg.CoreAccounts(), StaticKeys(cfg), store, auth.Options{
		Issuer: ov.Issuer, HTTPClient: ov.HTTPClient, Clock: ov.Clock, Logger: logger,
	}), nil
}

// Build wires every component. Call Start to begin quota polling and Close
// when done.
func Build(cfg *config.Config, logger *slog.Logger, ov Overrides) (*App, error) {
	cfg, err := cloneConfig(cfg)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	var clock core.Clock = core.SystemClock{}
	if ov.Clock != nil {
		clock = ov.Clock
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}

	accounts := cfg.CoreAccounts()
	acctMap := map[string]core.Account{}
	for _, account := range accounts {
		acctMap[account.ID] = account
	}

	creds, err := NewAuth(cfg, logger, ov)
	if err != nil {
		return nil, err
	}
	pricing, err := ledger.LoadPricing(cfg.PricingFile)
	if err != nil {
		return nil, fmt.Errorf("pricing: %w", err)
	}
	led, err := ledger.Open(filepath.Join(cfg.DataDir, "localrouter.db"), pricing,
		func(id string) string { return acctMap[id].CostBasis })
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}

	claudeCreds := map[string]string{}
	for _, a := range cfg.Accounts {
		if a.Provider == core.ProviderClaude {
			claudeCreds[a.ID] = a.CredentialsFile
		}
	}
	qm := quota.New(accounts, creds, quota.Options{
		PollInterval:          cfg.Quota.PollInterval.D(),
		HTTPClient:            ov.HTTPClient,
		Clock:                 clock,
		Logger:                logger,
		CodexUsageURL:         ov.CodexUsageURL,
		OllamaUsageURL:        ov.OllamaUsageURL,
		ClaudeUsageURL:        ov.ClaudeUsageURL,
		ClaudeCredentialsFile: func(id string) string { return claudeCreds[id] },
		ManagementCredentials: managementCredentials(cfg, logger, ov),
	})
	pol := policy.New(accounts, qm, policy.Options{
		StaleAfter:       cfg.Policy.StaleAfter.D(),
		SafetyMargin:     cfg.Policy.SafetyMargin,
		InflightEstimate: cfg.Policy.InflightEstimate,
		Clock:            clock,
		Logger:           logger,
	})
	lim, err := connlim.New(cfg.Limits.MaxConcurrent, cfg.Limits.MaxConcurrentPerClient)
	if err != nil {
		return nil, fmt.Errorf("concurrency limits: %w", err)
	}
	a := &App{
		Quota: qm, Policy: pol, Ledger: led, Auth: creds,
		Limiter: lim, BodyGuard: httpguard.NewBodyTimeout(cfg.Timeouts.Body.D()),
		Timeouts: Timeouts{
			Header:   cfg.Timeouts.Header.D(),
			Body:     cfg.Timeouts.Body.D(),
			Idle:     cfg.Timeouts.Idle.D(),
			Shutdown: cfg.Timeouts.Shutdown.D(),
		},
		TLSCertFile: cfg.TLSCertFile, TLSKeyFile: cfg.TLSKeyFile, Logger: logger,
	}
	a.serving.Store(true)
	a.reloadBase = cfg
	a.reloadClock = clock
	if cfg.Budgets != nil {
		owner, store, err := openBudgetStore(cfg)
		if err != nil {
			_ = led.Close()
			return nil, err
		}
		a.budgetOwnership, a.budgetStore = owner, store
	}
	generation, _, err := a.makeGeneration(cfg, 1)
	if err != nil {
		a.closeBudget()
		_ = led.Close()
		return nil, err
	}
	a.current.Store(generation)
	a.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g := a.current.Load()
		g.handler.ServeHTTP(w, r)
	})
	collector := collectorLedger{Ledger: led, app: a}
	if cfg.ClaudeLogs.Enabled {
		a.ClaudeLog = claudelog.New(collector, claudelog.Options{
			Dir:          cfg.ClaudeLogs.Dir,
			AccountID:    cfg.ClaudeLogs.Account,
			Host:         cfg.HostName,
			Client:       cfg.ClaudeLogs.Client,
			StatePath:    filepath.Join(cfg.DataDir, "claudelog-state.json"),
			ScanInterval: cfg.ClaudeLogs.ScanInterval.D(),
			Clock:        clock,
			Logger:       logger,
		})
	}
	if cfg.HermesLogs.Enabled {
		a.HermesLog = hermeslog.New(collector, hermeslog.Options{
			Home:         cfg.HermesLogs.Home,
			Accounts:     cfg.HermesLogs.Accounts,
			Client:       cfg.HermesLogs.Client,
			Host:         cfg.HostName,
			SelfHosts:    selfHosts(cfg.Listen),
			StatePath:    filepath.Join(cfg.DataDir, "hermeslog-state.json"),
			ScanInterval: cfg.HermesLogs.ScanInterval.D(),
			Clock:        clock,
			Logger:       logger,
		})
	}
	return a, nil
}

// selfHosts lists host:port spellings of this router as clients might write
// them in a base URL (so Hermes rows already proxied here are skipped).
func selfHosts(listen string) []string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil
	}
	out := []string{net.JoinHostPort(host, port)}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() || host == "" || host == "0.0.0.0" || host == "::" {
		for _, h := range []string{"127.0.0.1", "localhost", "::1"} {
			out = append(out, net.JoinHostPort(h, port))
		}
	}
	return out
}

// Start begins background quota polling until ctx is cancelled.
func (a *App) Start(ctx context.Context) {
	a.Quota.Start(ctx)
	if a.ClaudeLog != nil {
		a.ClaudeLog.Start(ctx)
	}
	if a.HermesLog != nil {
		a.HermesLog.Start(ctx)
	}
}

// budgetStartupTimeout bounds the one-shot orphan reconciliation Build performs
// while it holds the process-wide budget lock, before any request is served.
const budgetStartupTimeout = 5 * time.Second

// openBudgetStore takes process-wide ownership of the budget database, opens
// it, and reconciles orphan reservations a previous process left behind.
//
// Order matters. Ownership is acquired BEFORE the store is opened and
// reconciliation runs BEFORE the store is shared with any generation, so no
// other process can be reconciling this file concurrently and this process
// never opens a database another owner is still driving. Any failure releases
// exactly what was acquired, so a failed Build leaves ownership free for the
// next attempt.
func openBudgetStore(cfg *config.Config) (*budget.Ownership, *budget.Store, error) {
	owner, err := budget.AcquireOwnership(filepath.Join(cfg.DataDir, "budgets.lock"))
	if err != nil {
		return nil, nil, fmt.Errorf("budget ownership: %w", err)
	}
	store, err := budget.Open(filepath.Join(cfg.DataDir, "budgets.db"))
	if err != nil {
		_ = owner.Close()
		return nil, nil, fmt.Errorf("budget store: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budgetStartupTimeout)
	defer cancel()
	if err := store.ReconcileOrphans(ctx); err != nil {
		_ = store.Close()
		_ = owner.Close()
		return nil, nil, fmt.Errorf("budget reconcile: %w", err)
	}
	return owner, store, nil
}

// closeBudget releases the budget store and then its ownership lock, in that
// order: the store is closed first so none of our writes are still on the file
// at the instant the lock drops and another process could become owner and
// reconcile. It is idempotent and safe when both fields are nil.
//
// Callers: Build's failure cleanup, before the App is published and while no
// handler can be running, and Close, which holds reloadMu so the nil-ing of the
// fields is serialized with the reload path that reads them.
func (a *App) closeBudget() {
	if a.budgetStore != nil {
		_ = a.budgetStore.Close()
		a.budgetStore = nil
	}
	if a.budgetOwnership != nil {
		_ = a.budgetOwnership.Close()
		a.budgetOwnership = nil
	}
}

// Close releases the budget store and its ownership lock, then the ledger, and
// makes the lifecycle terminal.
//
// Lifecycle: Close is a terminal transition, not merely a resource release. It
// takes reloadMu with the same lock order Reload uses (reloadMu -> srvMu) and,
// under srvMu, marks the app both not-serving and stopped. That single latch is
// what makes the release safe:
//
//   - no later Reload can pass the serving gate in reloadConfigLocked, so no
//     generation whose budget gate was built from a released (nil) store is ever
//     published (the fail-open the review found on the undrained path);
//   - no later Serve can start, because Serve rejects a stopped app.
//
// The budget store is then released while reloadMu is still held, so the write
// that nils a.budgetStore and a concurrent reload's read in budgetGate are
// ordered on the same lock, and concurrent Close calls serialize on it too.
// Releasing the store before its ownership keeps the closeBudget order (store
// then owner): nothing of ours is still writing the file when the lock drops.
//
// Close does not stop the server and does not wait for in-flight handlers to
// drain; draining remains the caller's responsibility (cmd/localrouter drives
// Serve/Shutdown first), exactly as it already is for the ledger. What Close
// guarantees is only that no new generation is published and no Serve starts
// after it, and that once the store is closed no later write goes through it
// (the closed store fails closed). It is idempotent and nil-safe, and its
// ledger release keeps the existing ledger.Close semantics.
func (a *App) Close() error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	a.srvMu.Lock()
	a.serving.Store(false)
	a.stopped = true
	a.srvMu.Unlock()
	a.closeBudget()
	if a.Ledger == nil {
		return nil
	}
	return a.Ledger.Close()
}
