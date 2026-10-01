// Package claudelog ingests token usage from Claude Code's local JSONL
// transcripts (~/.claude/projects/<project-slug>/<session>.jsonl, scanned
// recursively) into the ledger, so the quota-only "claude" provider has
// per-request accounting even though LocalRouter never proxies it.
//
// Public API wired by cmd/localrouter:
//
//	c := claudelog.New(ledger, claudelog.Options{
//		Dir:       "/home/u/.claude/projects", // claude_logs.dir
//		AccountID: "claude-max",               // claude_logs.account
//		StatePath: filepath.Join(dataDir, "claudelog-state.json"),
//		ScanInterval: cfg.ScanInterval,        // default 1m
//	})
//	c.Start(ctx) // non-blocking: scans now, then every ScanInterval until ctx ends
//
// ScanOnce runs a single synchronous scan and returns Stats.
//
// Each API message is recorded once its usage is final, under the ID
// "claude:" + hex(sha256(message.id + ":" + requestId))[:32], so re-ingestion
// is idempotent given the ledger's ON CONFLICT DO NOTHING. Only token counts,
// model, timestamps, session ID, sidechain flag and the project directory name
// are stored — never message content, cwd, or tool inputs.
package claudelog
