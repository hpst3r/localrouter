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
// Options.Host is copied to every RequestRecord.Host (the per-host agent sets
// it; the server-local collector leaves it ""). If the ledger also implements
// core.BatchLedger (e.g. the agent's remote ledger), each file's final
// messages are sent via RecordBatch in chunks of at most MaxBatch, and the
// file's offset is persisted only after every chunk succeeded; on error the
// file is re-read next scan and IDs dedupe. An error implementing
// PermanentError with Permanent() == true (e.g. *agent.HTTPError for
// 400/413/422) instead bisects the chunk, records the accepted records, and
// drops only the rejected ones (Stats.Dropped), so the file still advances.
// Lines the server would reject (negative or >1e12 usage, missing timestamp
// or one outside [now-400d, now+5m]) are skipped at parse time. Labels
// (model, session, project, client) are bounded with core.TruncateLabel.
//
// Each API message is recorded once its usage is final, under the ID
// "claude:" + hex(sha256(message.id + ":" + requestId))[:32], so re-ingestion
// is idempotent given the ledger's ON CONFLICT DO NOTHING. Only token counts,
// model, timestamps, session ID, sidechain flag and the project directory name
// are stored — never message content, cwd, or tool inputs.
package claudelog
