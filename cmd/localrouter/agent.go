package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/hpst3r/localrouter/internal/agent"
	"github.com/hpst3r/localrouter/internal/claudelog"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/quota"
)

// cmdAgent runs `localrouter agent -config PATH [--once]`: it pushes Claude
// Code transcript usage and Claude quota snapshots to the central server.
func cmdAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	path := fs.String("config", agent.DefaultConfigPath(), "agent config file")
	once := fs.Bool("once", false, "one scan and one quota push, then exit")
	verbose := fs.Bool("v", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: localrouter agent [-config PATH] [--once]")
	}
	cfg, err := agent.LoadConfig(*path)
	if err != nil {
		return err
	}
	key, err := agent.ReadKey(cfg.KeyFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("state_dir: %w", err)
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	client := agent.NewClient(cfg.Server, cfg.Host, key, &http.Client{Timeout: 30 * time.Second})
	col := claudelog.New(agent.NewRemoteLedger(client), claudelog.Options{
		Dir:       cfg.ClaudeProjectsDir,
		AccountID: cfg.Account,
		Host:      cfg.Host,
		StatePath: filepath.Join(cfg.StateDir, "claudelog-state.json"),
		Logger:    logger,
	})
	creds, err := agent.NewCredentialReader(cfg.Credentials, runtime.GOOS, nil)
	if err != nil {
		return err
	}
	usageHTTP := &http.Client{Timeout: 20 * time.Second}
	fetch := func(ctx context.Context, raw []byte, now time.Time) (core.Snapshot, error) {
		cred, err := quota.ParseClaudeCredentials(raw)
		if err != nil {
			return core.Snapshot{}, err
		}
		if cred.ExpiresAt > 0 && !now.Before(time.UnixMilli(cred.ExpiresAt)) {
			return core.Snapshot{}, agent.ErrTokenExpired
		}
		return quota.FetchClaudeSnapshot(ctx, usageHTTP, quota.DefaultClaudeUsageURL,
			quota.DefaultClaudeUserAgent, cred, cfg.Account, now)
	}
	a := agent.New(agent.Options{
		Client:    client,
		AccountID: cfg.Account,
		Scan: func(ctx context.Context) error {
			st, err := col.ScanOnce(ctx)
			if err == nil && st.Recorded > 0 {
				logger.Info("agent: pushed usage", "records", st.Recorded)
			}
			return err
		},
		Credentials:   creds,
		FetchQuota:    fetch,
		PushInterval:  cfg.PushInterval,
		QuotaInterval: cfg.QuotaInterval,
		Logger:        logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		return a.RunOnce(ctx)
	}
	logger.Info("localrouter agent started", "host", cfg.Host, "account", cfg.Account,
		"push_interval", cfg.PushInterval, "quota_interval", cfg.QuotaInterval)
	return a.Run(ctx)
}
