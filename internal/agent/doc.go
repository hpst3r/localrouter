// Package agent implements `localrouter agent`, the per-host process that
// pushes Claude Code transcript usage and Claude quota snapshots to a central
// LocalRouter server via POST /control/v1/ingest (see docs/SPEC.md,
// "Multi-host").
//
// It depends only on core; the concrete transcript collector and quota
// fetcher are injected as functions so cmd/localrouter does the wiring:
//
//	cfg, _ := agent.LoadConfig(path)            // defaults, ~ expansion, validation
//	key, _ := agent.ReadKey(cfg.KeyFile)
//	client := agent.NewClient(cfg.Server, cfg.Host, key, httpClient)
//	ledger := agent.NewRemoteLedger(client)     // core.Ledger + core.BatchLedger
//	col := claudelog.New(ledger, claudelog.Options{...})
//	creds, _ := agent.NewCredentialReader(cfg.Credentials, runtime.GOOS, nil)
//	a := agent.New(agent.Options{
//		Client: client, AccountID: cfg.Account,
//		Scan: func(ctx context.Context) error { _, err := col.ScanOnce(ctx); return err },
//		Credentials: creds, FetchQuota: fetch, // fetch returns ErrTokenExpired to skip a push
//		PushInterval: cfg.PushInterval, QuotaInterval: cfg.QuotaInterval,
//	})
//	err := a.Run(ctx) // or a.RunOnce(ctx) for --once
//
// Nothing in this package logs or returns tokens, client keys, or transcript
// content; server error bodies are truncated and redacted.
package agent
