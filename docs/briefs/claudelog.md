Package: internal/claudelog (NEW package) — implement the "Claude transcript
accounting" section of docs/SPEC.md exactly.

API:
  type Options struct { Dir, AccountID, StatePath string; ScanInterval, QuietPeriod
    time.Duration /* default 1m, 30s */; Clock core.Clock; Logger *slog.Logger }
  func New(ledger core.Ledger, opts Options) *Collector
  func (c *Collector) ScanOnce(ctx context.Context) (Stats, error)  // Stats{Files, Lines, Recorded, Pending, Skipped int}
  func (c *Collector) Start(ctx context.Context)   // ScanOnce at start, then every ScanInterval; stops on ctx
Only import stdlib + internal/core.

Use the REAL-shape fixture at internal/claudelog/testdata/real-shape.jsonl (already
committed; sanitized from real transcripts — it contains non-assistant entry types
and one message id repeated with growing output_tokens 144→167→910→910, then a
second message 113→2104). Expected from that file once final: exactly 2 records,
with output_tokens 910 and 2104, and InputTokens summed per SPEC.

Tests (fake ledger implementing core.Ledger, recording rows; fake clock; temp dirs):
- fixture → 2 rows, correct usage mapping incl. cache_creation & thinking tokens.
- idempotence: two ScanOnce calls, and a new Collector with the same StatePath → no new
  rows; also new Collector with a DELETED state file → Record called again with the same
  IDs (ledger dedupes) and row IDs identical.
- incremental: append lines to a file between scans; only new lines read (count reads or
  bytes), partial last line (no trailing newline) not consumed until completed.
- finality: last key in a still-recent file is Pending, recorded after QuietPeriod
  passes (advance fake clock + file mtime via os.Chtimes).
- truncation → rescan from 0. recursive subdirectories. lines > 8 MiB skipped.
  "<synthetic>" model skipped. Entries without usage skipped. Malformed JSON skipped.
- privacy: rows contain no content/cwd; Task == project dir name only; state file 0600.
