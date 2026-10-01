// Command localrouter is a loopback OpenAI-compatible proxy that routes LLM
// requests across subscription accounts, enforces quota reserves for
// background work, and records token usage and estimated cost.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hpst3r/localrouter/internal/auth"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/control"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
	"github.com/hpst3r/localrouter/internal/policy"
	"github.com/hpst3r/localrouter/internal/proxy"
	"github.com/hpst3r/localrouter/internal/quota"
)

const usage = `localrouter — quota-aware local LLM gateway

Usage:
  localrouter serve   [-config PATH]
  localrouter login   [-config PATH] [-force] <account-id>
  localrouter keygen  <output-file>
  localrouter pricing import [-config PATH] <litellm-prices.json>
  localrouter check   [-config PATH]

Default config: ~/.config/localrouter/config.yaml
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "login":
		err = cmdLogin(os.Args[2:])
	case "keygen":
		err = cmdKeygen(os.Args[2:])
	case "pricing":
		err = cmdPricing(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "localrouter:", err)
		os.Exit(1)
	}
}

func defaultConfigPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(d, "localrouter", "config.yaml")
}

func loadConfig(fs *flag.FlagSet, args []string) (*config.Config, error) {
	path := fs.String("config", defaultConfigPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return config.Load(*path)
}

func staticKeys(cfg *config.Config) map[string]auth.StaticKey {
	keys := map[string]auth.StaticKey{}
	for _, a := range cfg.Accounts {
		if a.Provider != core.ProviderCodex {
			keys[a.ID] = auth.StaticKey{File: a.APIKeyFile, Env: a.APIKeyEnv}
		}
	}
	return keys
}

func newAuth(cfg *config.Config, logger *slog.Logger) (*auth.Manager, error) {
	store, err := auth.NewStore(filepath.Join(cfg.DataDir, "tokens"))
	if err != nil {
		return nil, err
	}
	return auth.New(cfg.CoreAccounts(), staticKeys(cfg), store, auth.Options{Logger: logger}), nil
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	keyFiles := map[string]string{}
	for _, c := range cfg.Clients {
		keyFiles[c.Name] = c.KeyFile
	}
	if _, err := auth.LoadClientKeys(keyFiles); err != nil {
		return fmt.Errorf("client keys: %w", err)
	}
	fmt.Printf("config OK: %d clients, %d accounts, %d routes; data_dir=%s\n",
		len(cfg.Clients), len(cfg.Accounts), len(cfg.Routes), cfg.DataDir)
	return nil
}

func cmdKeygen(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: localrouter keygen <output-file>")
	}
	k, err := auth.GenerateKey()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(args[0]), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, k); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("wrote new client key to %s (mode 0600)\n", args[0])
	return nil
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	path := fs.String("config", defaultConfigPath(), "config file")
	force := fs.Bool("force", false, "overwrite a token for a different ChatGPT account")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: localrouter login [-force] <account-id>")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	found := false
	for _, a := range cfg.Accounts {
		if a.ID == id {
			if a.Provider != core.ProviderCodex {
				return fmt.Errorf("account %s is %s; only codex accounts use login", id, a.Provider)
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("unknown account %q", id)
	}
	m, err := newAuth(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return m.LoginWithOptions(ctx, id, os.Stdout, auth.LoginOptions{Force: *force})
}

func cmdPricing(args []string) error {
	if len(args) < 1 || args[0] != "import" {
		return errors.New("usage: localrouter pricing import [-config PATH] <litellm-prices.json>")
	}
	fs := flag.NewFlagSet("pricing import", flag.ExitOnError)
	cfg, err := loadConfig(fs, args[1:])
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: localrouter pricing import [-config PATH] <litellm-prices.json>")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	prices, err := ledger.ImportLiteLLM(f)
	if err != nil {
		return err
	}
	if err := ledger.WritePricing(cfg.PricingFile, prices); err != nil {
		return err
	}
	fmt.Printf("imported %d model prices into %s\n", len(prices), cfg.PricingFile)
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}

	keyFiles := map[string]string{}
	classes := map[string]core.Class{}
	for _, c := range cfg.Clients {
		keyFiles[c.Name] = c.KeyFile
		classes[c.Name] = core.Class(c.Class)
	}
	clientKeys, err := auth.LoadClientKeys(keyFiles)
	if err != nil {
		return fmt.Errorf("client keys: %w", err)
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
	clock := core.SystemClock{}

	creds, err := newAuth(cfg, logger)
	if err != nil {
		return err
	}

	pricing, err := ledger.LoadPricing(cfg.PricingFile)
	if err != nil {
		return fmt.Errorf("pricing: %w", err)
	}
	led, err := ledger.Open(filepath.Join(cfg.DataDir, "localrouter.db"), pricing,
		func(id string) string { return acctMap[id].CostBasis })
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	defer led.Close()

	qm := quota.New(accounts, creds, quota.Options{
		PollInterval: cfg.Quota.PollInterval.D(),
		Clock:        clock,
		Logger:       logger,
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	qm.Start(ctx)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	logger.Info("localrouter listening", "addr", ln.Addr().String(), "accounts", len(accounts), "routes", len(routes))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		logger.Info("shutting down; draining in-flight requests")
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
