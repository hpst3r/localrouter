// Package app wires LocalRouter's packages into a runnable server. It exists
// separately from cmd/localrouter so end-to-end tests can build the real
// object graph with fake upstream endpoints.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/hpst3r/localrouter/internal/auth"
	"github.com/hpst3r/localrouter/internal/claudelog"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/control"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
	"github.com/hpst3r/localrouter/internal/policy"
	"github.com/hpst3r/localrouter/internal/proxy"
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

// App is a fully wired LocalRouter instance.
type App struct {
	Handler http.Handler
	Quota   *quota.Manager
	Policy  *policy.Policy
	Ledger  *ledger.Ledger
	Auth    *auth.Manager
	// ClaudeLog is nil unless claude_logs.enabled.
	ClaudeLog *claudelog.Collector
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

	keyFiles := map[string]string{}
	classes := map[string]core.Class{}
	for _, c := range cfg.Clients {
		keyFiles[c.Name] = c.KeyFile
		classes[c.Name] = core.Class(c.Class)
	}
	clientKeys, err := auth.LoadClientKeys(keyFiles)
	if err != nil {
		return nil, fmt.Errorf("client keys: %w", err)
	}
	authenticate := func(bearer string) (core.Client, bool) {
		name, ok := clientKeys.Lookup(bearer)
		if !ok {
			return core.Client{}, false
		}
		return core.Client{Name: name, Class: classes[name]}, true
	}

	accounts := cfg.CoreAccounts()
	acctMap := map[string]core.Account{}
	for _, a := range accounts {
		acctMap[a.ID] = a
	}
	routes := cfg.CoreRoutes()

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
	})
	pol := policy.New(accounts, qm, policy.Options{
		StaleAfter:       cfg.Policy.StaleAfter.D(),
		SafetyMargin:     cfg.Policy.SafetyMargin,
		InflightEstimate: cfg.Policy.InflightEstimate,
		Clock:            clock,
		Logger:           logger,
	})
	px := proxy.New(proxy.Deps{
		Accounts: acctMap, Routes: routes, Creds: creds, Quota: qm, Policy: pol,
		Ledger: led, Authenticate: authenticate, Clock: clock, Logger: logger,
	}, proxy.Options{MaxFailovers: cfg.Policy.MaxFailovers})
	ctl := control.New(control.Deps{
		Accounts: accounts, Quota: qm, Policy: pol, Ledger: led, Routes: routes,
		Authenticate: authenticate, Clock: clock,
	}, control.Options{RequireAuth: cfg.Control.RequireAuth, StaleAfter: cfg.Policy.StaleAfter.D()})

	mux := http.NewServeMux()
	mux.Handle("/v1/", px.Handler())
	mux.Handle("/", ctl.Handler())
	var h http.Handler = mux
	if !cfg.AllowNonLoopback {
		h = LocalHostGuard(mux)
	}
	a := &App{Handler: h, Quota: qm, Policy: pol, Ledger: led, Auth: creds}
	if cfg.ClaudeLogs.Enabled {
		a.ClaudeLog = claudelog.New(led, claudelog.Options{
			Dir:          cfg.ClaudeLogs.Dir,
			AccountID:    cfg.ClaudeLogs.Account,
			StatePath:    filepath.Join(cfg.DataDir, "claudelog-state.json"),
			ScanInterval: cfg.ClaudeLogs.ScanInterval.D(),
			Clock:        clock,
			Logger:       logger,
		})
	}
	return a, nil
}

// Start begins background quota polling until ctx is cancelled.
func (a *App) Start(ctx context.Context) {
	a.Quota.Start(ctx)
	if a.ClaudeLog != nil {
		a.ClaudeLog.Start(ctx)
	}
}

// Close releases the ledger.
func (a *App) Close() error { return a.Ledger.Close() }
