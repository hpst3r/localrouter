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
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/auth"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

const usage = `localrouter — quota-aware local LLM gateway

Usage:
  localrouter serve   [-config PATH]
  localrouter login   [-config PATH] [-force] <account-id>
  localrouter keygen  <output-file>
  localrouter pricing import [-config PATH] <litellm-prices.json>
  localrouter pricing reprice [-config PATH]
  localrouter check   [-config PATH]
  localrouter ledger relabel-host [--from ""] --to NAME [-config PATH]
  localrouter admit   [--class background] (--account ID | --model NAME) [--url URL] [--json]
                      exit 0 = allow, 1 = deny, 2 = error
  localrouter agent   [-config ~/.config/localrouter/agent.yaml] [--once]
  localrouter users   list|disable|enable|delete|keys|revoke [-config PATH] ...
                      local identity administration (multi-user mode)

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
	case "ledger":
		err = cmdLedger(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "admit":
		err = cmdAdmit(os.Args[2:])
	case "agent":
		err = cmdAgent(os.Args[2:])
	case "users":
		err = cmdUsers(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		var coded interface{ Code() int }
		if errors.As(err, &coded) {
			if coded.Code() != 1 {
				fmt.Fprintln(os.Stderr, "localrouter:", err)
			}
			os.Exit(coded.Code())
		}
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

// checkFiles runs cfg.CheckFiles against the -config path parsed into fs.
func checkFiles(fs *flag.FlagSet, cfg *config.Config) error {
	if err := cfg.CheckFiles(fs.Lookup("config").Value.String()); err != nil {
		return fmt.Errorf("file permissions:\n%w", err)
	}
	return nil
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if err := checkFiles(fs, cfg); err != nil {
		return err
	}
	keyFiles := map[string][]string{}
	for _, c := range cfg.Clients {
		keyFiles[c.Name] = c.KeyPaths()
	}
	if _, err := auth.LoadClientKeyFiles(keyFiles); err != nil {
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
	m, err := app.NewAuth(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), app.Overrides{})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return m.LoginWithOptions(ctx, id, os.Stdout, auth.LoginOptions{Force: *force})
}

func cmdPricing(args []string) error {
	const use = "usage: localrouter pricing import [-config PATH] <litellm-prices.json> | pricing reprice [-config PATH]"
	if len(args) < 1 {
		return errors.New(use)
	}
	switch args[0] {
	case "import":
		fs := flag.NewFlagSet("pricing import", flag.ExitOnError)
		cfg, err := loadConfig(fs, args[1:])
		if err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(use)
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
		fmt.Printf("hand-maintained overrides/aliases: %s (never overwritten)\n", ledger.LocalPricingPath(cfg.PricingFile))
		fmt.Println("run `localrouter pricing reprice` to apply prices to existing rows")
		return nil
	case "reprice":
		fs := flag.NewFlagSet("pricing reprice", flag.ExitOnError)
		cfg, err := loadConfig(fs, args[1:])
		if err != nil {
			return err
		}
		pricing, err := ledger.LoadPricing(cfg.PricingFile)
		if err != nil {
			return err
		}
		basis := map[string]string{}
		for _, a := range cfg.CoreAccounts() {
			basis[a.ID] = a.CostBasis
		}
		l, err := ledger.Open(filepath.Join(cfg.DataDir, "localrouter.db"), pricing,
			func(id string) string { return basis[id] })
		if err != nil {
			return err
		}
		defer l.Close()
		n, err := l.Reprice(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("repriced: %d rows now have a cost\n", n)
		return nil
	default:
		return errors.New(use)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	// Record the resolved config path the running config came from so a SIGHUP
	// re-reads exactly the same file. It is read after a successful load, so a
	// parse failure never leaves a half-resolved path behind. The path is
	// passed to Reload explicitly, so the app need not retain it.
	cfgPath := fs.Lookup("config").Value.String()
	// Fail closed on loose secret-file or config/data_dir permissions.
	if err := checkFiles(fs, cfg); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	a, err := app.Build(cfg, logger, app.Overrides{})
	if err != nil {
		return err
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a.Start(ctx)

	// SIGHUP reloads the reloadable configuration subset in place. The trigger
	// only exists on platforms that have SIGHUP; on Windows the loop is not
	// started and the in-process reload seam remains callable. Reload is
	// serialized inside the app, and the buffered channel coalesces a burst of
	// rapid signals (last-wins) under normal Go semantics.
	if sig := reloadSignal(); sig != nil {
		if r := appReloader(a, logger); r != nil {
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, sig)
			// Stop restores the default SIGHUP disposition on shutdown; the
			// loop exits with ctx, so no goroutine outlives the process.
			defer signal.Stop(hup)
			go reloadLoop(ctx, r, logger, cfgPath, hup)
		}
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	logger.Info("localrouter listening", "addr", ln.Addr().String(), "tls", cfg.TLSCertFile != "",
		"accounts", len(cfg.Accounts), "routes", len(cfg.Routes))
	// Serve owns the inbound deadlines (header, body, idle), the concurrency
	// limiter and the graceful drain; the CLI only owns the listener.
	return a.Serve(ctx, ln)
}
